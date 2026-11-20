package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"lenovo-satellite/internal/web"
	"lenovo-satellite/internal/wifi"
)

// Setup mode: a hotspot ("LenovoClock-1A2B") with a small setup page for
// joining a Wi-Fi network (device/setup-ap.sh runs hostapd + dnsmasq on uap0,
// behind a firewall). The hotspot is open unless the installer left a key for
// it (hotspot.key next to the script: WPA2, see install-clock.sh
// --hotspot-password); either way the page works only after the action button
// was held (confirm). It starts by itself when no network is saved, after
// setupLostAfter without a connection, or by holding both volume buttons for
// 5 s (which also stops it).
//
// While wlan0 is lost the hotspot gives wpa_supplicant room to reconnect: it
// goes away when nobody has used it for setupIdle, and comes back only after
// another setupLostAfter without a connection. If the radio can't run the
// hotspot and a client at once, a failed connect restarts the hotspot.

const (
	setupLostAfter = 2 * time.Minute
	setupIdle      = 10 * time.Minute
	setupAfterOK   = 90 * time.Second // the hotspot stays so the phone can read the result
	setupConnectTO = 40 * time.Second
)

type setupMode struct {
	d      *daemon
	wifi   *wifi.Manager
	script string
	ssid   string
	// confirm: the button press the setup page waits for
	confirm *web.SetupConfirm

	mu         sync.Mutex
	active     bool
	reason     string // "no network saved", "no connection", "button", "web UI"
	since      time.Time
	lastSeen   time.Time // last request from the setup network
	lastErr    string
	connecting bool
	doneAt     time.Time // connected: stop after setupAfterOK
	lostSince  time.Time
	lastTry    time.Time
	lastBanner time.Time
	dns        setupDNSCtl // catch-all DNS while the hotspot is on
}

func newSetupMode(d *daemon, wm *wifi.Manager, script, ssid string) *setupMode {
	return &setupMode{d: d, wifi: wm, script: script, ssid: ssid, confirm: &web.SetupConfirm{ShowCode: d.face.setCode}}
}

// secured: the hotspot asks for a password (setup-ap.sh reads the key).
func (s *setupMode) secured() bool {
	_, err := os.Stat(filepath.Join(filepath.Dir(s.script), "hotspot.key"))
	return err == nil
}

// ownDNS: clockd answers DNS on the hotspot (else the flag file use-dnsmasq
// keeps the old route: setup-ap.sh's dnsmasq does DHCP and DNS).
func (s *setupMode) ownDNS() bool {
	_, err := os.Stat(filepath.Join(filepath.Dir(s.script), "use-dnsmasq"))
	return err != nil
}

func (s *setupMode) available() bool {
	_, err := os.Stat(s.script)
	return err == nil
}

func (s *setupMode) sh(ctx context.Context, args ...string) (string, error) {
	c, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(c, "/sbin/busybox", append([]string{"sh", s.script}, args...)...)
	// a daemon the script starts may inherit the output pipe: don't wait for it
	cmd.WaitDelay = 2 * time.Second
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// Start brings the hotspot up (no-op when it is up).
func (s *setupMode) Start(reason string) error {
	if !s.available() {
		return errors.New("setup hotspot not installed (" + s.script + ")")
	}
	s.mu.Lock()
	if s.active {
		s.mu.Unlock()
		return nil
	}
	s.lastTry = time.Now()
	s.mu.Unlock()
	out, err := s.sh(context.Background(), "start", s.ssid)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.lastErr = out
		s.d.log.Warn("setup hotspot failed to start", "reason", reason, "out", out)
		return errors.New("the hotspot did not start: " + out)
	}
	if s.ownDNS() {
		s.dns.start(s.d.log)
	}
	s.active, s.reason, s.since, s.lastSeen, s.lastErr, s.doneAt = true, reason, time.Now(), time.Time{}, "", time.Time{}
	s.lastBanner = time.Time{}
	s.d.log.Info("setup hotspot on", "ssid", s.ssid, "reason", reason, "password", s.secured())
	return nil
}

// Stop takes the hotspot down.
func (s *setupMode) Stop(why string) {
	s.mu.Lock()
	was := s.active
	s.active, s.connecting, s.doneAt = false, false, time.Time{}
	s.lastTry = time.Now() // give wpa_supplicant setupLostAfter before trying again
	s.mu.Unlock()
	s.confirm.Reset()
	s.dns.stop()
	out, err := s.sh(context.Background(), "stop")
	if err != nil {
		s.d.log.Warn("setup hotspot stop", "out", out)
	}
	if was {
		s.d.log.Info("setup hotspot off", "why", why)
	}
}

// Toggle is the button combination.
func (s *setupMode) Toggle() {
	if s.Active() {
		s.Stop("button")
		s.d.face.flash("off", 1500*time.Millisecond)
		return
	}
	s.d.face.flash("AP", 1500*time.Millisecond)
	if err := s.Start("button"); err != nil {
		s.d.face.flash("err", 2*time.Second)
	}
}

func (s *setupMode) Active() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active
}

// Touch: a phone on the setup network asked for something.
func (s *setupMode) Touch() {
	s.mu.Lock()
	s.lastSeen = time.Now()
	s.mu.Unlock()
}

// Connect tries a network from the setup page (in the background; the page
// polls Info). The previous network, if any, stays as a fallback.
func (s *setupMode) Connect(ssid, password string, hidden bool) error {
	if err := s.wifi.Connect(ssid, password, hidden, setupConnectTO); err != nil {
		return err
	}
	s.mu.Lock()
	s.connecting = true
	s.mu.Unlock()
	s.d.log.Info("setup: connecting", "ssid", ssid)
	return nil
}

// Info is the setup page's view (and the web UI's).
func (s *setupMode) Info() map[string]any {
	s.mu.Lock()
	m := map[string]any{
		"available": s.available(), "active": s.active, "reason": s.reason, "hotspot": s.ssid,
		"error": s.lastErr, "name": s.d.name, "friendly": s.d.friendly, "secured": s.secured(),
	}
	if s.active {
		m["since"] = s.since
		if !s.doneAt.IsZero() { // connected: the hotspot closes after setupAfterOK
			m["closes_in"] = max(0, int((setupAfterOK - time.Since(s.doneAt)).Seconds()))
		}
	}
	s.mu.Unlock()
	m["wifi"] = s.wifi.Status()
	return m
}

func (s *setupMode) run(ctx context.Context) {
	if !s.available() {
		s.d.log.Info("setup hotspot not installed; setup mode off", "script", s.script)
		return
	}
	s.sh(ctx, "stop") // a leftover from a crash
	// let wpa_supplicant come up and try the saved networks first; with none
	// saved there is nothing to wait for: a first install opens the hotspot as
	// soon as wpa_supplicant answers (a PC that tried to join earlier gave up)
	deadline := time.After(20 * time.Second)
	poll := time.NewTicker(time.Second)
wait:
	for {
		select {
		case <-ctx.Done():
			poll.Stop()
			return
		case <-deadline:
			break wait
		case <-poll.C:
			if saved, err := s.wifi.SavedNetworks(); err == nil && len(saved) == 0 {
				break wait
			}
		}
	}
	poll.Stop()
	if saved, err := s.wifi.SavedNetworks(); err == nil && len(saved) == 0 {
		s.Start("no network saved")
	}
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			s.Stop("shutting down")
			return
		case <-t.C:
			s.tick(ctx)
		}
	}
}

func (s *setupMode) tick(ctx context.Context) {
	st := s.wifi.Status()
	connected := st.State == "COMPLETED" && st.IP != ""
	now := time.Now()

	s.mu.Lock()
	active, reason, connecting, doneAt := s.active, s.reason, s.connecting, s.doneAt
	idle := now.Sub(maxTime(s.lastSeen, s.since))
	if connected {
		s.lostSince = time.Time{}
	} else if s.lostSince.IsZero() {
		s.lostSince = now
	}
	lost := now.Sub(s.lostSince)
	retry := now.Sub(s.lastTry) > setupLostAfter
	banner := active && now.Sub(s.lastBanner) > 12*time.Second
	if banner {
		s.lastBanner = now
	}
	s.mu.Unlock()

	if connecting {
		switch j := s.wifi.Job(); j.State {
		case "connected":
			s.mu.Lock()
			s.connecting, s.doneAt = false, now
			s.mu.Unlock()
			s.d.log.Info("setup: connected", "ssid", j.SSID, "ip", st.IP)
			s.d.face.showText("ok "+st.IP, 2)
		case "failed":
			s.mu.Lock()
			s.connecting = false
			s.mu.Unlock()
			s.d.log.Info("setup: connect failed", "ssid", j.SSID, "msg", j.Message)
			// the radio may have dropped the hotspot to try the network
			if out, _ := s.sh(ctx, "status"); active && !strings.HasPrefix(out, "running") {
				s.mu.Lock()
				s.active = false
				s.mu.Unlock()
				s.Start(reason)
			}
		}
		return
	}
	switch {
	case active && !doneAt.IsZero() && now.Sub(doneAt) > setupAfterOK:
		s.Stop("connected")
	case active && reason == "no connection" && connected && idle > 2*time.Minute:
		s.Stop("connection is back")
	case active && reason != "no network saved" && idle > setupIdle:
		s.Stop("nobody used it")
	case !active && !connected && lost > setupLostAfter && retry:
		if saved, err := s.wifi.SavedNetworks(); err == nil {
			why := "no connection"
			if len(saved) == 0 {
				why = "no network saved"
			}
			s.Start(why)
		}
	}
	if banner && s.Active() && s.d.face.textIdle() {
		s.d.face.showText("setup", 1)
	}
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
