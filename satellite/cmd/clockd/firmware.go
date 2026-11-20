package main

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	esphome "github.com/ygelfand/go-esphome-device"

	"lenovo-satellite/internal/config"
)

// firmware is the ESPHome update entity showing clockd's version in Home
// Assistant. It reports a newer version only when both the online check
// (update_check) and "Offer updates in Home Assistant" (update_ha) are on:
// HA's voice setup wizard installs any update it sees, so a fresh install must
// not offer one. Install from HA runs the same path as System > Update's
// Download + Install: the signed package from the release, checked file by
// file, then the restart with automatic rollback.
//
// The entity also steadies that wizard: without an update entity its first
// step skips at once and the connection test starts while HA is still settling
// the new satellite entity, which ran the test twice (the second refused as
// busy, "can't connect"). With one, the wizard refreshes it and waits 10 s first.
type firmware struct {
	log *slog.Logger
	cfg *config.Store
	ent *esphome.Update
	wb  atomic.Pointer[webBackend] // unset without the web UI: then always "up to date"

	mu         sync.Mutex
	shown      esphome.UpdateState
	installing atomic.Bool
}

func newFirmware(log *slog.Logger, cfg *config.Store) *firmware {
	f := &firmware{log: log, cfg: cfg}
	f.ent = &esphome.Update{
		Base:        esphome.Base{ObjectID: "firmware", Name: "Firmware", Category: esphome.CategoryConfig},
		DeviceClass: "firmware",
	}
	f.ent.OnCommand = func(c esphome.UpdateCommand) {
		switch c {
		case esphome.UpdateCheck:
			f.refresh()
		case esphome.UpdateInstall:
			go f.install()
		}
	}
	f.sync()
	return f
}

// offered is the release Home Assistant may show and install, if any.
func (f *firmware) offered() (latestRelease, bool) {
	wb := f.wb.Load()
	if wb == nil || wb.online == nil || !f.cfg.Get().UpdateInHA {
		return latestRelease{}, false
	}
	return wb.online.newer()
}

// state is what the entity should show now (not while installing).
func (f *firmware) state() esphome.UpdateState {
	st := esphome.UpdateState{CurrentVersion: version, LatestVersion: version, Title: "clockd"}
	if r, ok := f.offered(); ok {
		st.LatestVersion, st.ReleaseSummary, st.ReleaseURL = r.version, r.summary, r.url
	}
	return st
}

// set publishes st when it differs from what Home Assistant has (or always, force).
func (f *firmware) set(st esphome.UpdateState, force bool) {
	f.mu.Lock()
	same := st == f.shown
	f.shown = st
	f.mu.Unlock()
	if !same || force {
		f.ent.Set(st)
	}
}

// sync shows the current state unless an install is running.
func (f *firmware) sync() {
	if !f.installing.Load() {
		f.set(f.state(), false)
	}
}

// run follows the online check and the switches (a check result, a setting
// changed in the web UI).
func (f *firmware) run(ctx context.Context) {
	t := time.NewTicker(20 * time.Second)
	defer t.Stop()
	for {
		f.sync()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// refresh: Home Assistant asked (its check button, the voice wizard). Answer
// at once with the state as it is; when updates are offered in HA, also ask
// GitHub and report again.
func (f *firmware) refresh() {
	if !f.installing.Load() {
		f.set(f.state(), true)
	}
	wb := f.wb.Load()
	if wb != nil && wb.online != nil && wb.online.enabled() && f.cfg.Get().UpdateInHA {
		go func() {
			wb.online.check(context.Background())
			f.sync()
		}()
	}
}

// install downloads, checks and installs the offered release (clockd then
// restarts into it). Progress: the download is 0-90 %.
func (f *firmware) install() {
	r, ok := f.offered()
	if !ok {
		f.log.Warn("update requested from Home Assistant, but none is offered")
		f.sync()
		return
	}
	if !f.installing.CompareAndSwap(false, true) {
		return
	}
	wb := f.wb.Load()
	f.log.Info("update requested from Home Assistant", "to", r.version)
	st := f.state()
	st.InProgress = true
	f.set(st, false)
	fail := func(err error) {
		f.log.Warn("update from Home Assistant failed", "err", err)
		f.installing.Store(false)
		st := f.state()
		st.ReleaseSummary = "Install failed: " + err.Error()
		f.set(st, false)
	}
	body, _, err := wb.online.download(context.Background())
	if err != nil {
		fail(err)
		return
	}
	defer body.Close()
	pr := &progressReader{r: io.LimitReader(body, maxDownload), size: r.size, report: func(p float32) {
		st.Progress = p * 90
		f.set(st, false)
	}}
	if _, err := wb.UpdateUpload(pr); err != nil {
		fail(err)
		return
	}
	st.Progress = 95
	f.set(st, false)
	m, err := wb.UpdateInstall(false)
	if err != nil {
		fail(err)
		return
	}
	if m["restarting"] != true { // nothing changed: already this version
		f.installing.Store(false)
		f.sync()
		return
	}
	st.Progress = 100
	f.set(st, false) // clockd restarts in a moment; the new one reports itself
}

// progressReader reports the share of size read so far, in steps of 5 %.
type progressReader struct {
	r      io.Reader
	size   int64
	n      int64
	last   int64
	report func(float32)
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.n += int64(n)
	if p.size > 0 {
		if step := min(p.n*20/p.size, 20); step > p.last {
			p.last = step
			p.report(float32(step) / 20)
		}
	}
	return n, err
}
