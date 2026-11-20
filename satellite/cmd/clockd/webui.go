package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"path"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	esphome "github.com/ygelfand/go-esphome-device"

	"lenovo-satellite/internal/config"
	"lenovo-satellite/internal/haapi"
	"lenovo-satellite/internal/output"
	"lenovo-satellite/internal/release"
	"lenovo-satellite/internal/timesync"
	"lenovo-satellite/internal/web"
)

// webBackend implements web.Backend on top of the daemon.
type webBackend struct {
	procs   procSampler
	d       *daemon
	tk      *timeKeeper
	started time.Time
	restart func()
	dir     string // clockd's directory (state, updates)

	updMu  sync.Mutex
	sshMu  sync.Mutex    // ssh keys file (ssh.go)
	upd    *release.Plan // uploaded package waiting for Install (System > Update)
	online *onlineCheck  // the optional online update check (onlineupdate.go)

	name, friendly, apiAddr string
	apiKey                  string // ESPHome API encryption key (base64)

	usb *usbSock // the USB-A socket: adb or host mode, drive listing
}

// USB is the System > USB panel's status (mode, drive, space).
func (b *webBackend) USB() map[string]any { return b.usb.Status() }

// Sleep sounds and timer (sleep.go).
func (b *webBackend) Sleep() map[string]any { return b.d.sleep.Status() }

func (b *webBackend) SleepSound(id string) error {
	if id == "" {
		b.d.sleep.stopSound()
		return nil
	}
	return b.d.sleep.play(id)
}

func (b *webBackend) SleepTimer(minutes int) error { return b.d.sleep.setTimer(minutes) }

// SetUSBMode switches the socket between adb (a cable to a computer) and
// host (a USB drive). Takes a few seconds in host mode: the stick has to
// enumerate before it can mount.
func (b *webBackend) SetUSBMode(host bool) error { return b.usb.SetHost(host) }

// USBFiles lists one directory of the mounted drive (confined to the mount).
func (b *webBackend) USBFiles(path string) (map[string]any, error) { return b.usb.Files(path) }

// USBPlay plays a FLAC, MP3 or WAV file from the mounted drive (the USB panel's
// play buttons) through the media path, so the stop word and volume apply.
// The file's folder becomes the queue: when a file ends on its own, the next
// one starts, until the folder is through or something stops it.
func (b *webBackend) USBPlay(file string) error {
	f, p, err := b.usb.OpenAudio(file)
	if err != nil {
		return err
	}
	f.Close() // playUSB reopens it: one owner at a time
	list, _ := b.usb.Files(path.Dir(p))
	q := queueFromList(entriesOf(list), p)
	b.loadMeta(q)
	d := b.d
	d.mu.Lock()
	q.repeat = d.usbRepeat
	shuffle := d.usbShuffle
	d.mu.Unlock()
	if shuffle {
		q.setShuffle(true, shuffledOrder)
	}
	b.playUSBAt(q, q.oi, 0)
	return nil
}

// USBSkip jumps within the USB folder queue: ⏭ next, ⏮ previous — a track that
// has played for over 5 s restarts instead of going back (usbRestartAfter).
// Skipping while paused resumes at the target.
func (b *webBackend) USBSkip(steps int) error {
	d := b.d
	d.mu.Lock()
	q, paused := d.usbQ, d.usbPaused
	d.mu.Unlock()
	var elapsed time.Duration
	switch {
	case q != nil:
		elapsed = d.usbElapsed()
	case paused != nil:
		q, elapsed = paused.q, paused.elapsed
	default:
		return errors.New("nothing is playing from the drive")
	}
	d.mu.Lock()
	var target int
	if steps > 0 {
		var ok bool
		target, ok = q.nextOrder()
		if !ok {
			d.usbPaused = nil
			d.mu.Unlock()
			d.stopMedia() // past the last track
			return nil
		}
	} else {
		target = q.prevOrder(elapsed, usbRestartAfter)
	}
	d.usbPaused = nil
	d.mu.Unlock()
	b.playUSBAt(q, target, 0)
	return nil
}

// USBPause pauses (on) or resumes (off) the folder queue. Pause stops
// playback but keeps the queue and the position; resume reopens the file and
// seeks to where it stopped.
func (b *webBackend) USBPause(on bool) error {
	d := b.d
	if on {
		d.mu.Lock()
		q, already := d.usbQ, d.usbPaused
		d.mu.Unlock()
		if q == nil || already != nil {
			return errors.New("nothing is playing from the drive")
		}
		el := d.usbElapsed()
		d.mu.Lock()
		v, buf := d.media, d.mediaBuf
		d.media, d.mediaBuf, d.mediaName = nil, nil, ""
		d.usbQ = nil
		d.usbPaused = &usbPaused{q: q, elapsed: el}
		d.mu.Unlock()
		if buf != nil {
			buf.Abort()
		}
		d.out.Stop(v)
		d.player.SetState(esphome.MediaPlayerPaused)
		d.log.Info("usb playback paused", "file", q.track().path, "at", el.Round(time.Second))
		return nil
	}
	d.mu.Lock()
	p := d.usbPaused
	d.mu.Unlock()
	if p == nil {
		return errors.New("nothing is paused")
	}
	d.mu.Lock()
	d.usbPaused = nil
	d.mu.Unlock()
	b.playUSBAt(p.q, p.q.oi, p.elapsed)
	return nil
}

// usbPaused is a paused folder queue: what is loaded and where it stopped.
type usbPaused struct {
	q       *usbQueue
	elapsed time.Duration
}

// loadMeta reads each track's header for its length and tags (best effort; a
// track that fails to open keeps zeros and shows elapsed and the file name).
func (b *webBackend) loadMeta(q *usbQueue) {
	for i := range q.tracks {
		f, _, err := b.usb.OpenAudio(q.tracks[i].path)
		if err != nil {
			continue
		}
		info, err := output.TrackMeta(f)
		f.Close()
		if err == nil {
			q.tracks[i].dur, q.tracks[i].name = info.Duration, info.Name
		}
	}
}

// USBOpts sets the player's shuffle and repeat. Both apply to the queue that
// is playing (shuffle keeps the current track and scatters the rest) and are
// remembered for later folders. nil leaves a setting unchanged.
func (b *webBackend) USBOpts(shuffle *bool, repeat *string) error {
	d := b.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if repeat != nil {
		m, ok := parseRepeat(*repeat)
		if !ok {
			return errors.New("repeat must be off, all or one")
		}
		d.usbRepeat = m
		if q := d.usbQ; q != nil {
			q.repeat = m
		}
		if p := d.usbPaused; p != nil {
			p.q.repeat = m
		}
	}
	if shuffle != nil {
		d.usbShuffle = *shuffle
		if q := d.usbQ; q != nil {
			q.setShuffle(*shuffle, shuffledOrder)
		}
		if p := d.usbPaused; p != nil {
			p.q.setShuffle(*shuffle, shuffledOrder)
		}
	}
	return nil
}

// playUSBAt plays the queue's order[oi] starting skip into the audio (resume).
func (b *webBackend) playUSBAt(q *usbQueue, oi int, skip time.Duration) {
	d := b.d
	if oi >= len(q.order) {
		d.stopMedia() // through the folder
		return
	}
	track := q.tracks[q.order[oi]]
	f, p, err := b.usb.OpenAudio(track.path)
	if err != nil {
		d.log.Warn("usb playback", "file", track.path, "err", err)
		d.stopMedia()
		return
	}
	d.log.Info("play media", "file", p, "group", "media", "track", q.order[oi]+1, "of", len(q.tracks),
		"from", skip.Round(time.Second))
	d.stopMedia() // also clears the previous queue and any pause
	d.mu.Lock()
	q.oi = oi
	d.usbQ = q
	d.mu.Unlock()
	if track.name != "" && d.cfg.Get().Sendspin.ShowNowPlaying {
		d.face.showText(track.name, 1) // the same scroll Music Assistant's titles get
	}
	d.startMedia(p, func(buf *output.Buffer) error {
		defer f.Close()
		return output.DecodeFrom(f, skip, buf)
	}, func() { // ended on its own: continue where the order says
		d.mu.Lock()
		mine := d.usbQ == q && q.oi == oi
		next, ok := q.endOrder()
		d.mu.Unlock()
		if mine && ok {
			b.playUSBAt(q, next, 0)
		} else if mine {
			d.stopMedia()
		}
	})
}

// usbElapsed is how long the current USB track has actually been heard: the
// decode runs up to the buffer's length ahead of the speaker, so subtract
// what is still waiting.
func (d *daemon) usbElapsed() time.Duration {
	d.mu.Lock()
	buf := d.mediaBuf
	d.mu.Unlock()
	if buf == nil {
		return 0
	}
	w, buffered := buf.Progress()
	frames := max(0, w-buffered) / output.Channels
	return time.Duration(frames) * time.Second / time.Duration(output.Rate)
}

func entriesOf(list map[string]any) []ent {
	if e, ok := list["entries"].([]ent); ok {
		return e
	}
	return nil
}

// WakeHistory is the recent wake word / stop word detections and near misses,
// with the thresholds of each sensitivity so the UI can suggest one.
func (b *webBackend) WakeHistory() map[string]any {
	v := b.d.cfg.Get().Voice
	// threshold per sensitivity, per wake word phrase (from each model's cutoff)
	sens := map[string]map[string]float64{}
	for _, m := range b.d.models() {
		sens[m.Phrase] = map[string]float64{}
		for name := range wakeSensitivity {
			sens[m.Phrase][name] = wakeThreshold(m, name)
		}
	}
	return map[string]any{"events": b.d.wakeHist.list(), "sensitivity": v.WakeSensitivity,
		"thresholds": sens, "stop_threshold": v.StopThreshold()}
}

// MicLevel is the live microphone level for the web UI meter.
// Face is the clock face as it is lit right now (for the dashboard picture):
// raw TM1652 grid bytes (see internal/display), brightness 1-16, the status
// LEDs and the night light. "moving" is true while the digits animate faster than the
// half-second blinks (a scrolling message, the assistant's eyes), so the page polls fast
// only then.
func (b *webBackend) Face() map[string]any {
	fr, bright, ok := b.d.face.snapshot()
	a, bb := make([]int, 6), make([]int, 6)
	for i := range fr.A {
		a[i], bb[i] = int(fr.A[i]), int(fr.B[i])
	}
	leds := [][3]int{}
	for _, c := range b.d.hw.leds.Last() {
		leds = append(leds, [3]int{int(c.R), int(c.G), int(c.B)})
	}
	return map[string]any{"live": ok, "a": a, "b": bb, "brightness": bright,
		"leds": leds, "night_light": b.d.nightLevel(),
		"ms":     time.Now().Nanosecond() / 1e6,
		"moving": !b.d.face.textIdle() || b.d.face.teaserOn() || (b.d.face.mood != nil && b.d.face.mood() != moodNone)}
}

func (b *webBackend) Processes() map[string]any { return b.procs.Processes() }

// Capture streams a test capture (capture.go).
func (b *webBackend) Capture(ctx context.Context, max time.Duration, w io.Writer, flush func()) error {
	return b.d.mic.capture(ctx, max, w, flush)
}

// Clip validates an attempt clip name (clips.go) and returns its path for
// web.go to serve.
func (b *webBackend) Clip(name string) (string, bool) { return b.d.clips.path(name) }

func (b *webBackend) MicLevel() map[string]any {
	l := b.d.micGain.level()
	b.d.mu.Lock()
	muted := b.d.muted
	b.d.mu.Unlock()
	return map[string]any{"peak_db": l.PeakDB, "rms_db": l.RMSDB, "clipped": l.Clipped,
		"gain_db": l.GainDB, "muted": muted, "fresh": time.Since(l.At) < time.Second}
}

func (b *webBackend) Status() map[string]any {
	d, tk := b.d, b.tk
	d.mu.Lock()
	st, muted, ringing := d.st, d.muted, d.alarm != nil
	hwMuted, softMuted := d.hwMuted, d.softMuted
	mediaPlaying, mediaName := d.media != nil, d.mediaName
	// the USB transport line: what is loaded (playing or paused), where it is
	var (
		usbTitle           string
		usbName            string
		usbPaused          bool
		usbTrack, usbTotal int
		usbElapsed, usbDur time.Duration
	)
	if q := d.usbQ; q != nil {
		t := q.track()
		usbTitle, usbName, usbTrack, usbTotal = t.path, t.name, q.idx()+1, len(q.tracks)
		usbDur = t.dur
		if buf := d.mediaBuf; buf != nil {
			w, buffered := buf.Progress()
			usbElapsed = time.Duration(max(0, w-buffered)/output.Channels) * time.Second / time.Duration(output.Rate)
		}
	} else if p := d.usbPaused; p != nil {
		t := p.q.track()
		usbTitle, usbName, usbPaused = t.path, t.name, true
		usbTrack, usbTotal, usbElapsed, usbDur = p.q.idx()+1, len(p.q.tracks), p.elapsed, t.dur
	}
	usbShuffle, usbRepeatMode := d.usbShuffle, string(d.usbRepeat)
	var active []string
	for _, det := range d.detectors {
		active = append(active, det.Model.Phrase)
	}
	d.mu.Unlock()

	tk.mu.Lock()
	loc, last, lastSrc := time.Local, tk.lastSync, tk.lastSrc
	haClient := ""
	if tk.conn != nil {
		haClient = tk.conn.ClientInfo()
	}
	tk.mu.Unlock()
	now := time.Now().In(loc)

	// the HA entity is the source of truth (HA can change it too)
	night := 0
	if l := d.hw.nightLight; l != nil {
		if ls := l.Get(); ls.On {
			night = max(1, int(ls.Brightness*255+0.5))
		}
	}

	s := map[string]any{
		"version":          version,
		"local_time":       now.Format("2006-01-02 15:04:05 MST"),
		"timezone":         loc.String(),
		"timezone_known":   startZone != "",
		"time_source":      b.d.cfg.Get().TimeSource,
		"last_sync":        !last.IsZero(),
		"last_sync_source": lastSrc,
		"last_sync_age":    time.Since(last).Seconds(),
		"ha_connected":     d.sat.Subscribed(),
		"state":            st.String(),
		"mic_muted":        muted,
		"mic_muted_switch": hwMuted,
		"mic_muted_soft":   softMuted,
		"stop_word_ok":     d.stopDet != nil,
		"wake_words":       active,
		"wake_ids":         b.d.cfg.Get().WakeWords, // the page reloads its picker when HA changes them
		"last_wake":        b.lastWake(),
		"alarm_ringing":    ringing,
		"volume":           d.player.Volume(),
		"night_light":      night,
		"clockd_uptime":    time.Since(b.started).Seconds(),
		"ip":               ipAddrs(),
		"esphome_name":     b.name,
		"esphome_friendly": b.friendly,
		"esphome_addr":     b.apiAddr,
		"ha_client":        haClient,
		"sendspin_playing": d.sendspin != nil && d.sendspin.Playing(),
		"sendspin_enabled": d.sendspin != nil,
		"sendspin_server":  d.sendspin != nil && d.sendspin.Connected(),
		"media_playing":    mediaPlaying,
		"media_title":      mediaName,
		"usb_title":        usbTitle,
		"usb_name":         usbName,
		"usb_paused":       usbPaused,
		"usb_track":        usbTrack,
		"usb_total":        usbTotal,
		"usb_elapsed":      usbElapsed.Seconds(),
		"usb_duration":     usbDur.Seconds(),
		"usb_shuffle":      usbShuffle,
		"usb_repeat":       usbRepeatMode,
		"cpu_cores":        runtime.NumCPU(),
		"weather_age":      -1.0,
	}
	if b.d.bt != nil {
		st := b.d.bt.status()
		s["bt_enabled"] = b.d.cfg.Get().Bluetooth.Proxy
		s["bt_state"] = st.State
		s["bt_error"] = st.Err
		s["bt_addr"] = st.Addr
		s["bt_adv_total"] = st.AdvTotal
		s["bt_adv_min"] = st.PerMin
		s["bt_ha"] = b.d.bt.proxy.Subscribed()
	}
	// the mic delivers audio (the meter got a window in the last 2 s)
	s["mic_alive"] = !muted && time.Since(d.micGain.level().At) < 2*time.Second
	if lvl, ok := d.amb.Level(); ok {
		s["ambient_light"] = math.Round(lvl * 100)
		s["ambient_raw"] = math.Round(d.amb.Raw()*10) / 10
	}
	d.face.mu.Lock()
	s["display_brightness"] = d.face.brightness
	s["display_off"] = d.face.blank
	d.face.mu.Unlock()
	if set, rise, ok := d.night.sun.times(); ok {
		s["sunset"], s["sunrise"] = set.In(loc).Format("15:04"), rise.In(loc).Format("15:04")
	}
	if d.mic != nil {
		act, merr, filt, nf, np := d.mic.status()
		s["mic_source"], s["mic_source_error"], s["aec_filtering"] = act, merr, filt
		if nf+np > 0 {
			s["aec_filtered_share"] = math.Round(float64(nf)/float64(nf+np)*1000) / 10
		}
		if l := d.mic.tapTarget(); l != nil {
			s["mic_headroom_db"] = math.Round(l.HeadroomDB()*10) / 10
			if d.cfg.Get().Voice.MicNull {
				s["mic_null"] = l.NullStats()
			}
		}
		s["webrtc_installed"] = d.mic.installed("webrtc")
	}
	nActive, nDark := d.night.State()
	s["night_active"], s["room_dark"] = nActive, nDark
	wv := d.wx.view()
	if !wv.Updated.IsZero() {
		s["weather_age"] = time.Since(wv.Updated).Seconds()
	}
	s["weather_entity"] = d.cfg.Get().WeatherEntity
	s["weather_condition"] = wv.Condition
	if wv.Valid {
		s["weather_temp"] = fmt.Sprintf("%d °%s", wv.Temp, wv.Unit)
	}
	if f := readFields("/proc/uptime"); len(f) > 0 {
		s["uptime"], _ = strconv.ParseFloat(f[0], 64)
	}
	if f := readFields("/proc/loadavg"); len(f) >= 3 {
		s["load"] = strings.Join(f[:3], " ")
	}
	mem := meminfo()
	s["mem_total_mb"], s["mem_available_mb"] = mem["MemTotal"]/1024, mem["MemAvailable"]/1024
	if v, ok := procStatus("VmRSS"); ok {
		s["clockd_rss_mb"] = v / 1024
	}
	if t, err := os.ReadFile("/sys/class/thermal/thermal_zone0/temp"); err == nil {
		if v, err := strconv.ParseFloat(strings.TrimSpace(string(t)), 64); err == nil {
			s["soc_temp"] = v / 1000
		}
	}
	if dbm, ok := wifiSignal(); ok {
		s["wifi_dbm"] = dbm
	}
	// CPU governor and speed (start.sh sets "interactive"; stock is "performance")
	cf := func(f string) string {
		b, _ := os.ReadFile("/sys/devices/system/cpu/cpu0/cpufreq/" + f)
		return strings.TrimSpace(string(b))
	}
	if g := cf("scaling_governor"); g != "" {
		s["cpu_governor"] = g
		khz := func(f string) int { v, _ := strconv.Atoi(cf(f)); return v / 1000 }
		s["cpu_mhz"], s["cpu_min_mhz"], s["cpu_max_mhz"] = khz("scaling_cur_freq"), khz("scaling_min_freq"), khz("scaling_max_freq")
		s["cpu_freqs"] = d.freqs.shares() // rolling ~10-minute share per speed
	}
	if d.setup != nil {
		si := d.setup.Info()
		delete(si, "wifi")
		s["setup"] = si
	}
	return s
}

func (b *webBackend) Config() config.Config { return b.d.cfg.Get() }

func (b *webBackend) APIKey() string { return b.apiKey }

// WakeModels lists the installed wake word models for the picker.
// lastWake is the newest wake word detection or near miss (not the stop
// word), for the Status page: shows at a glance that the mic and model work.
func (b *webBackend) lastWake() map[string]any {
	for _, e := range b.d.wakeHist.list() {
		if !e.Stop {
			return map[string]any{"word": e.Word, "kind": e.Kind, "score": e.Score, "age": time.Since(e.At).Seconds()}
		}
	}
	return nil
}

// Clocks lists this clock and the others found on the network.
func (b *webBackend) Clocks() any {
	self := peer{ID: b.d.name, Name: b.d.friendly, Version: version, Self: true, Online: true}
	return append([]peer{self}, b.d.peers.list(time.Now())...)
}

func (b *webBackend) WakeModels() map[string]any {
	active := b.d.activeWakeIDs()
	var out []map[string]any
	for _, m := range b.d.models() {
		size := int64(0)
		if fi, err := os.Stat(m.Path); err == nil {
			size = fi.Size()
		}
		out = append(out, map[string]any{"id": m.ID, "phrase": m.Phrase, "cutoff": m.Cutoff, "languages": m.Languages,
			"author": m.Author, "source": m.Source, "size": size, "active": slices.Contains(active, m.ID)})
	}
	return map[string]any{"models": out, "active": active, "max_active": 2}
}

func (b *webBackend) InstallWakeModel(id string, manifest, model []byte, source string) error {
	_, err := b.d.installModel(id, manifest, model, source)
	return err
}

func (b *webBackend) DeleteWakeModel(id string) error { return b.d.deleteModel(id) }

func (b *webBackend) WakeWords() []web.WakeWord {
	var out []web.WakeWord
	for _, m := range b.d.models() {
		out = append(out, web.WakeWord{ID: m.ID, Phrase: m.Phrase})
	}
	return out
}

func (b *webBackend) ApplyConfig(c config.Config) error {
	switch c.TimeSource {
	case config.TimeHomeAssistant, config.TimeNTP, config.TimeBoth, config.TimeNone:
	default:
		return fmt.Errorf("unknown time source %q", c.TimeSource)
	}
	if (c.TimeSource == config.TimeNTP) && c.NTPServer == "" {
		return errors.New("time source NTP needs an NTP server")
	}
	if c.NTPIntervalMinutes < 1 {
		return errors.New("NTP interval must be at least 1 minute")
	}
	if c.Display.ScrollMs < 80 || c.Display.ScrollMs > 2000 {
		return errors.New("scroll speed must be 80-2000 ms per step")
	}
	if c.Display.BrightnessMin < 1 || c.Display.BrightnessMin > 16 {
		return errors.New("minimum brightness must be 1-16")
	}
	if c.Display.Brightness < 1 || c.Display.Brightness > 16 {
		return errors.New("brightness must be 1-16")
	}
	if c.Timezone != "" {
		if _, err := timesync.LoadZone(c.Timezone); err != nil {
			return fmt.Errorf("timezone: %v", err)
		}
	}
	c.HA.URL = strings.TrimRight(strings.TrimSpace(c.HA.URL), "/")
	if c.HA.URL != "" {
		if _, err := haapi.ValidateURL(c.HA.URL, c.HA.AllowHTTP); err != nil {
			return err
		}
	}
	if c.HA.TLSFingerprint != "" {
		fp := haapi.NormalizeFingerprint(c.HA.TLSFingerprint)
		if fp == "" {
			return errors.New("certificate fingerprint must be 64 hex digits (SHA-256)")
		}
		c.HA.TLSFingerprint = fp
	}
	if c.Night.Enabled {
		if _, err := parseClock(c.Night.Start); err != nil {
			return fmt.Errorf("night start: %v", err)
		}
		if _, err := parseClock(c.Night.End); err != nil {
			return fmt.Errorf("night end: %v", err)
		}
	}
	switch c.Night.Schedule {
	case "times", "sun":
	default:
		return fmt.Errorf("unknown night schedule %q", c.Night.Schedule)
	}
	if c.Night.SunsetOffset < -180 || c.Night.SunsetOffset > 180 || c.Night.SunriseOffset < -180 || c.Night.SunriseOffset > 180 {
		return errors.New("sunset/sunrise offsets must be -180..180 minutes")
	}
	switch c.Night.Display {
	case "normal", "dim", "off", "off_dark":
	default:
		return fmt.Errorf("unknown night display mode %q", c.Night.Display)
	}
	switch c.Night.NightLight {
	case "none", "on", "dark":
	default:
		return fmt.Errorf("unknown night light mode %q", c.Night.NightLight)
	}
	if c.Night.DisplayBrightness < 1 || c.Night.DisplayBrightness > 16 || c.Night.NightLightLevel < 1 || c.Night.NightLightLevel > 255 ||
		c.Night.FadeMinutes < 0 || c.Night.FadeMinutes > 60 || c.Night.DarkLevel < 1 || c.Night.DarkLevel > 50 {
		return errors.New("night: brightness 1-16, night light level 1-255, fade 0-60 minutes, dark level 1-50 %")
	}
	switch c.Display.HALostHint {
	case "off", "icon", "icon_blink", "small", "message":
	default:
		return fmt.Errorf("unknown connection-lost hint %q", c.Display.HALostHint)
	}
	if c.Voice.StopCutoff != 0 && (c.Voice.StopCutoff < 0.1 || c.Voice.StopCutoff > 0.9) {
		return errors.New("stop word threshold must be 0.1-0.9")
	}
	if _, ok := wakeSensitivity[c.Voice.WakeSensitivity]; !ok && c.Voice.WakeSensitivity != "" {
		return fmt.Errorf("unknown wake word sensitivity %q", c.Voice.WakeSensitivity)
	}
	if c.Voice.MicSource != "" && c.Voice.MicSource != "vendor" && c.Voice.MicSource != "local" && c.Voice.MicSource != "webrtc" {
		return fmt.Errorf("unknown microphone source %q", c.Voice.MicSource)
	}
	if _, ok := tuneByID(c.Voice.StartupSound); !ok && c.Voice.StartupSound != "" {
		return fmt.Errorf("unknown startup sound %q", c.Voice.StartupSound)
	}
	if _, ok := chimeByID(c.Chime.Sound); !ok && c.Chime.Sound != "" {
		return fmt.Errorf("unknown chime %q", c.Chime.Sound)
	}
	switch c.Chime.Quiet {
	case "night", "times", "none":
	default:
		return fmt.Errorf("unknown chime quiet time %q", c.Chime.Quiet)
	}
	if c.Chime.Volume < 10 || c.Chime.Volume > 100 {
		return errors.New("chime volume must be 10-100 %")
	}
	if _, err := parseClock(c.Chime.QuietStart); err != nil {
		return err
	}
	if _, err := parseClock(c.Chime.QuietEnd); err != nil {
		return err
	}
	if _, ok := alarmSounds[c.Voice.AlarmSound]; !ok && c.Voice.AlarmSound != "" {
		return fmt.Errorf("unknown alarm sound %q", c.Voice.AlarmSound)
	}
	if c.Voice.SnoozeMinutes < 0 || c.Voice.SnoozeMinutes > 30 {
		return errors.New("snooze must be 1-30 minutes")
	}
	if c.Voice.MicGainDB < -12 || c.Voice.MicGainDB > 24 {
		return errors.New("microphone gain must be -12..+24 dB")
	}
	if c.Sendspin.DelayMs < 0 || c.Sendspin.DelayMs > 1000 {
		return errors.New("Sendspin delay must be 0-1000 ms")
	}
	if c.LEDs.Brightness < 0 || c.LEDs.Brightness > 100 {
		return errors.New("LED brightness must be 0-100")
	}
	for _, col := range []string{c.LEDs.Listening, c.LEDs.Thinking, c.LEDs.Responding, c.LEDs.Muted, c.LEDs.Ringing} {
		if !config.ValidColour(col) {
			return fmt.Errorf("bad LED colour %q", col)
		}
	}
	if len(c.WakeWords) == 0 || len(c.WakeWords) > 2 {
		return errors.New("choose one or two wake words")
	}
	if err := b.d.setActiveWakeWords(c.WakeWords); err != nil {
		return err
	}
	b.d.sat.ActiveWakeWords = c.WakeWords
	if err := b.d.cfg.Update(func(x *config.Config) { *x = c }); err != nil {
		return err
	}
	b.d.applyBluetooth(c)                                             // starts/stops the BLE proxy; no-op when unchanged
	b.d.dlna.apply(c.DLNA)                                            // DLNA server/renderer on or off
	b.tk.checkZone()                                                  // a changed zone setting restarts clockd
	b.d.wx.sel.Set(b.d.wx.option(strings.TrimSpace(c.WeatherEntity))) // the HA dropdown
	b.d.syncSettings()
	b.d.updateLEDs()
	b.d.wx.subscribe() // no-op unless the entity changed
	return nil
}

func (b *webBackend) PlayURL(u string) error {
	if u == "" {
		b.d.stopMedia()
		return nil
	}
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		return errors.New("play: http(s) URL required")
	}
	b.d.playMediaURL(u, false)
	return nil
}

func (b *webBackend) Action(name string, v float64) error {
	d := b.d
	if id, ok := strings.CutPrefix(name, "sound_"); ok { // an alarm editor's preview
		if _, known := alarmSounds[id]; !known {
			return fmt.Errorf("unknown sound %q", id)
		}
		d.previewSound(id)
		return nil
	}
	switch name {
	case "volume":
		d.setVolume(float32(v))
	case "chime":
		d.out.Play(output.NewSamples(d.chimeWake, 0), 1, "alert")
	case "alarm_preview": // the selected timer sound
		d.previewSound(d.cfg.Get().Voice.AlarmSound)
	case "night_light":
		d.setNightLightByHand(int(v))
	case "sync_time":
		b.tk.syncNow()
	case "night_mode":
		d.night.setOverride(v != 0)
	case "mute":
		d.setSoftMuted(v != 0)
	case "setup_mode": // the setup hotspot on (1) / off (0)
		if d.setup == nil {
			return errors.New("setup mode not available")
		}
		if v != 0 {
			return d.setup.Start("web UI")
		}
		d.setup.Stop("web UI")
	case "led_preview":
		// 0 listening, 1 thinking, 2 speaking, 3 muted, 4 ringing (animated, 3 s)
		d.previewLEDs(int(v))
	case "face_test": // every segment on for v seconds (0 = stop)
		d.face.testPattern(time.Duration(v * float64(time.Second)))
		d.log.Info("clock face test pattern", "seconds", v)
	case "face_teaser": // the luna teaser on the face (teaser.go)
		d.face.teaser()
		d.log.Info("clock face teaser")
	case "wake_test": // value = seconds of test mode (wake/stop words logged, no turns, no stopping); 0 ends it
		if v < 0 || v > 900 {
			return errors.New("wake_test: 0-900 s")
		}
		d.testUntil.Store(time.Now().Add(time.Duration(v * float64(time.Second))).UnixNano())
		d.log.Info("wake word test mode", "seconds", v)
	case "startup_preview": // value = index into tunes (the web UI lists them in the same order)
		i := int(v)
		if i < 0 || i >= len(tunes) {
			return errors.New("no such tune")
		}
		d.playTune(tunes[i].ID)
	case "chime_preview": // value = index into chimes; plays the chime for the current hour at the chime volume
		i := int(v)
		if i < 0 || i >= len(chimes) {
			return errors.New("no such chime")
		}
		d.playChime(chimes[i], d.face.tk.Now().Hour(), 0, d.cfg.Get().Chime.Volume)
	case "ref_record": // record v seconds of the speaker signal to /data/local/tmp/ref.wav (AEC tests)
		path := "/data/local/tmp/ref.wav"
		d.out.Tap(time.Duration(v*float64(time.Second)), func(s []float32) {
			if err := writeWAVFloat(path, s, output.Rate, output.Channels); err != nil {
				d.log.Warn("ref_record", "err", err)
				return
			}
			d.log.Info("ref_record written", "path", path, "seconds", float64(len(s))/output.Rate/output.Channels)
		})
		d.log.Info("ref_record started", "seconds", v)
	case "clear_text":
		d.face.clearText()
	case "restart":
		d.log.Info("restart requested from the web UI")
		go func() { time.Sleep(300 * time.Millisecond); b.restart() }()
	default:
		return fmt.Errorf("unknown action %q", name)
	}
	return nil
}

func (b *webBackend) ShowText(text string, repeat int) { b.d.face.showText(text, repeat) }

// ---- system info helpers ----

func readFields(path string) []string {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return strings.Fields(string(b))
}

func meminfo() map[string]int {
	out := map[string]int{}
	b, _ := os.ReadFile("/proc/meminfo")
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.Fields(l)
		if len(f) >= 2 {
			v, _ := strconv.Atoi(f[1])
			out[strings.TrimSuffix(f[0], ":")] = v
		}
	}
	return out
}

func procStatus(key string) (int, bool) {
	b, _ := os.ReadFile("/proc/self/status")
	for _, l := range strings.Split(string(b), "\n") {
		if f := strings.Fields(l); len(f) >= 2 && f[0] == key+":" {
			v, err := strconv.Atoi(f[1])
			return v, err == nil
		}
	}
	return 0, false
}

func wifiSignal() (int, bool) {
	b, _ := os.ReadFile("/proc/net/wireless")
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.Fields(l)
		if len(f) >= 4 && f[0] == "wlan0:" {
			v, err := strconv.ParseFloat(strings.TrimSuffix(f[3], "."), 64)
			return int(v), err == nil && v != 0
		}
	}
	return 0, false
}

func ipAddrs() []string {
	var out []string
	ifs, _ := net.Interfaces()
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil {
				out = append(out, ipn.IP.String())
			}
		}
	}
	return out
}

// previewSound plays a timer/alarm sound for about 6 seconds (whole cycles).
func (d *daemon) previewSound(id string) {
	pcm := alarmTone(id)
	ms := len(pcm) / output.Channels * 1000 / output.Rate
	d.out.Play(output.NewSamples(pcm, max(0, 6000/ms-1)), 1, "alert")
}
