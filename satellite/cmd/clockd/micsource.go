package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"lenovo-satellite/internal/capture"
	"lenovo-satellite/internal/localmic"
)

// Microphone sources (config voice.mic_source):
//   - webrtc: the raw PDM microphones + the open WebRTC audio processing (apmd:
//     the modules iot_audio_capture is built on: AEC3, noise suppression, AGC2)
//     with clockd's own mix as the reference; needs <state>/aec/apmd (the default)
//   - vendor: iot_audio_capture's echo-cancelled stream (as shipped)
//
// The web UI has no choice since v0.84.4: webrtc is the pipeline and vendor the
// automatic fallback. The setting stays in the config and the API for the test
// rig (tools/wwtest compares the two) and as the manual way back.
//
// A third one, LocalVQE (vqed), is disabled since v0.84.3: behind the two-mic
// null it measured no better than WebRTC (Engineering Notes, 2026-11-10), so it
// left the web UI and the package. The code path stays: with the flag file
// <state>/use-localvqe, and <state>/aec/vqed + model.gguf on the clock, the
// "webrtc" setting runs LocalVQE instead. An old "local" setting means webrtc.
//
// The capture is clockd's own; the vendor service is the fallback only: start.sh
// stops it at boot (flag file use-iot_audio keeps it running until clockd takes
// the microphones), and clockd starts it whenever it leaves its own source
// (the "vendor" setting, a failure, clockd exiting), so going back to
// iot_audio_capture is always one setting away.
type micSource struct {
	d        *daemon
	aecDir   string
	mu       sync.Mutex
	active   string // what really runs now
	err      string // why local is not running, if it was asked for
	local    *localmic.Mic
	vendorOn bool // we know iot_audio_capture is running (or we started it)
}

func (ms *micSource) status() (active, err string, filtering bool, filtered, passed uint64) {
	ms.mu.Lock()
	active, err, l := ms.active, ms.err, ms.local
	ms.mu.Unlock()
	if l != nil && active != "vendor" {
		filtering, filtered, passed = l.Stats()
	}
	return
}

func (ms *micSource) set(active, err string, l *localmic.Mic) {
	ms.mu.Lock()
	ms.active, ms.err, ms.local = active, err, l
	ms.mu.Unlock()
}

func initService(verb string) error {
	return exec.Command("/system/bin/"+verb, "iot_audio").Run()
}

// vendorRunning: init still has iot_audio_capture up (unknown counts as yes).
func vendorRunning() bool {
	out, err := exec.Command("/system/bin/getprop", "init.svc.iot_audio").Output()
	return err != nil || strings.TrimSpace(string(out)) != "stopped"
}

func (ms *micSource) want() string {
	switch s := ms.d.cfg.Get().Voice.MicSource; s {
	case "local", "webrtc":
		return "webrtc"
	}
	return "vendor"
}

// installed reports whether a source's helper files are present.
func (ms *micSource) installed(src string) bool {
	files := map[string][]string{"local": {"vqed", "model.gguf"}, "webrtc": {"apmd"}}[src]
	for _, f := range files {
		if _, err := os.Stat(filepath.Join(ms.aecDir, f)); err != nil {
			return false
		}
	}
	return len(files) > 0
}

// useLocalVQE: the flag file that brings the disabled LocalVQE source back.
func (ms *micSource) useLocalVQE() bool {
	_, err := os.Stat(filepath.Join(filepath.Dir(ms.aecDir), "use-localvqe"))
	return err == nil && ms.installed("local")
}

func (ms *micSource) run(ctx context.Context) {
	log := ms.d.log
	ms.vendorOn = false // "start" it once anyway: a hard crash on local may have left it stopped
	defer func() {
		if !ms.vendorOn {
			log.Info("starting iot_audio_capture again (clockd exiting)")
			initService("start")
		}
	}()
	failed := false // the local source failed: stay on vendor until the setting changes
	for ctx.Err() == nil {
		asked := ms.want()
		run := asked
		if run != "vendor" && (failed || !ms.installed(run)) {
			run = "vendor" // not installed on this clock: the original, without an error
		}
		// Muted: our own sources don't run at all (no capture, no apmd/vqed), which
		// saves most of their CPU; they start again on unmute (about a second).
		if run != "vendor" && ms.d.isMuted() {
			ms.set(run, "", nil)
			for ctx.Err() == nil && ms.d.isMuted() && ms.want() == asked {
				time.Sleep(300 * time.Millisecond)
			}
			continue
		}
		sctx, cancel := context.WithCancel(ctx)
		go func() { // end this round when the setting changes
			t := time.NewTicker(time.Second)
			defer t.Stop()
			for {
				select {
				case <-sctx.Done():
					return
				case <-t.C:
					if ms.want() != asked || (run != "vendor" && ms.d.isMuted()) {
						cancel()
						return
					}
				}
			}
		}()
		if run != "vendor" {
			if err := ms.runLocal(sctx, run); err != nil && sctx.Err() == nil && ms.want() == run { // not when we stopped it (setting change, mute)
				log.Error("local audio processing failed; back to iot_audio_capture", "source", run, "err", err)
				ms.set("vendor", err.Error(), nil)
				failed = true
			}
		} else {
			if !ms.vendorOn {
				log.Info("starting iot_audio_capture")
				if err := initService("start"); err != nil {
					log.Error("start iot_audio", "err", err)
				}
				ms.vendorOn = true
			}
			if !failed {
				ms.set("vendor", "", nil)
			}
			mic := &capture.Client{Stream: capture.StreamEchoCancelled, Log: log}
			mic.Run(sctx, ms.d.onMic)
		}
		cancel()
		if ms.want() != asked {
			failed = false
		}
	}
}

func (ms *micSource) runLocal(ctx context.Context, src string) error {
	l := &localmic.Mic{Log: ms.d.log, SetReference: ms.d.out.SetReference,
		NullOn:   func() bool { return ms.d.cfg.Get().Voice.MicNull },
		NullHold: ms.d.nullHold.Load,
		NullPath: filepath.Join(ms.aecDir, "null.bin")}
	if ms.useLocalVQE() {
		src = "local"
	}
	switch src {
	case "webrtc":
		// raw PDM -> +40 dB -> WebRTC (it brings its own AGC); runs on every block
		l.Helper, l.PreGainDB, l.Always = filepath.Join(ms.aecDir, "apmd"), 40, true
		l.ErrLog = func(line string) { ms.d.log.Info("apmd", "msg", line) }
		// developer hook: extra apmd options (e.g. "--no-aec") from <aec>/apmd.args
		if b, err := os.ReadFile(filepath.Join(ms.aecDir, "apmd.args")); err == nil {
			l.Args = strings.Fields(string(b))
			ms.d.log.Info("apmd extra options", "args", l.Args)
		}
	default:
		l.Helper, l.Model = filepath.Join(ms.aecDir, "vqed"), filepath.Join(ms.aecDir, "model.gguf")
		l.PreGainDB, l.PostGainDB, l.AGCMaxDB = 6, 18, 12
	}
	for _, p := range []string{l.Helper, l.Model} {
		if p == "" {
			continue
		}
		if _, err := os.Stat(p); err != nil {
			return err
		}
	}
	if vendorRunning() {
		ms.d.log.Info("stopping iot_audio_capture for local audio processing", "source", src)
		if err := initService("stop"); err != nil {
			return err
		}
		time.Sleep(time.Second) // let it release the microphones
	}
	ms.vendorOn = false
	ms.set(src, "", l)
	err := l.Run(ctx, ms.d.onMic)
	if ctx.Err() != nil {
		return nil
	}
	return err
}
