package web

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeSetup struct {
	active  bool
	touched int
	ssid    string
}

func (f *fakeSetup) Active() bool         { return f.active }
func (f *fakeSetup) Touch()               { f.touched++ }
func (f *fakeSetup) Info() map[string]any { return map[string]any{"active": f.active} }
func (f *fakeSetup) Connect(ssid, password string, hidden bool) error {
	f.ssid = ssid
	return nil
}

func TestSetupNetwork(t *testing.T) {
	fs := &fakeSetup{active: true}
	s := &Server{Auth: &Auth{Path: filepath.Join(t.TempDir(), "sessions.json")}, Setup: fs,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	h := s.Handler()
	req := func(local, method, url, body, origin string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, url, strings.NewReader(body))
		r = r.WithContext(context.WithValue(r.Context(), http.LocalAddrContextKey,
			&net.TCPAddr{IP: net.ParseIP(local), Port: 80}))
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	const ap, lan = SetupIP, "10.0.0.5"

	if w := req(ap, "GET", "http://192.168.255.249/", "", ""); w.Code != 200 || !strings.Contains(w.Body.String(), "<html") {
		t.Errorf("setup page: %d", w.Code)
	}
	// phones' internet checks and any other name land on the page
	for _, u := range []string{"http://connectivitycheck.gstatic.com/generate_204", "http://captive.apple.com/hotspot-detect.html"} {
		if w := req(ap, "GET", u, "", ""); w.Code != http.StatusFound || w.Header().Get("Location") != "http://192.168.255.249/" {
			t.Errorf("%s: %d %q", u, w.Code, w.Header().Get("Location"))
		}
	}
	// the main UI and its API are not reachable from the setup network
	for _, p := range []string{"/api/status", "/api/config", "/api/auth/request", "/index.html", "/api/action/reboot"} {
		if w := req(ap, "GET", "http://192.168.255.249"+p, "", ""); w.Code != http.StatusFound {
			t.Errorf("%s from the setup network: %d", p, w.Code)
		}
	}
	if w := req(ap, "POST", "http://192.168.255.249/api/setup/connect", `{"ssid":"x","password":"12345678"}`, "http://evil.example"); w.Code != http.StatusForbidden {
		t.Errorf("foreign origin: %d", w.Code)
	}
	if w := req(ap, "POST", "http://192.168.255.249/api/setup/connect", `{"ssid":" home ","password":"12345678"}`, "http://192.168.255.249"); w.Code != 200 || fs.ssid != "home" {
		t.Errorf("connect: %d %q", w.Code, fs.ssid)
	}
	if fs.touched == 0 {
		t.Error("setup activity not reported")
	}

	// the setup endpoints don't exist on the home network
	n := fs.touched
	if w := req(lan, "GET", "http://10.0.0.5/api/setup", "", ""); w.Code == 200 || fs.touched != n {
		t.Errorf("setup API on the LAN: %d", w.Code)
	}
	// setup mode off: nothing on the hotspot address
	fs.active = false
	if w := req(ap, "GET", "http://192.168.255.249/", "", ""); w.Code != http.StatusServiceUnavailable {
		t.Errorf("inactive: %d", w.Code)
	}
}

// The setup page does nothing before the button on the clock confirmed it.
func TestSetupNeedsTheButton(t *testing.T) {
	fs := &fakeSetup{active: true}
	shown := &shownCode{}
	c := &SetupConfirm{ShowCode: func(s string) { shown.v.Store(s) }}
	s := &Server{Auth: &Auth{Path: filepath.Join(t.TempDir(), "sessions.json")}, Setup: fs, SetupConfirm: c,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	h := s.Handler()
	req := func(from, method, path, body, id string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://"+SetupIP+path, strings.NewReader(body))
		r = r.WithContext(context.WithValue(r.Context(), http.LocalAddrContextKey,
			&net.TCPAddr{IP: net.ParseIP(SetupIP), Port: 80}))
		r.RemoteAddr = from + ":4000"
		r.Header.Set(setupIDHeader, id)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	const phone, other = "192.168.255.250", "192.168.255.251"
	const net1 = `{"ssid":"home","password":"12345678"}`

	if w := req(phone, "GET", "/", "", ""); w.Code != 200 {
		t.Fatalf("the page itself: %d", w.Code)
	}
	for _, p := range []string{"/api/setup/scan", "/api/setup/connect"} {
		if w := req(phone, "POST", p, net1, ""); w.Code != http.StatusForbidden {
			t.Errorf("%s without the button: %d", p, w.Code)
		}
	}
	w := req(phone, "POST", "/api/setup/confirm", "{}", "")
	var ask struct{ ID, Code string }
	json.Unmarshal(w.Body.Bytes(), &ask)
	if w.Code != 200 || ask.ID == "" || len(ask.Code) != 4 {
		t.Fatalf("confirm: %d %s", w.Code, w.Body.String())
	}
	if w := req(other, "POST", "/api/setup/confirm", "{}", ""); w.Code != http.StatusConflict {
		t.Errorf("a second device while one waits: %d", w.Code)
	}
	if w := req(phone, "POST", "/api/setup/connect", net1, ask.ID); w.Code != http.StatusForbidden {
		t.Errorf("asked but not pressed: %d", w.Code)
	}
	if !strings.Contains(req(phone, "GET", "/api/setup", "", ask.ID).Body.String(), `"confirm":"waiting"`) {
		t.Error("state should be waiting")
	}
	if !c.Pending() || !c.Approve() || c.Pending() {
		t.Fatal("the button should confirm the waiting page, once")
	}
	if c.Approve() {
		t.Error("nothing waits: the button confirms nothing")
	}
	// the id alone is not enough from another address, nor a wrong id from this one
	if w := req(other, "POST", "/api/setup/connect", net1, ask.ID); w.Code != http.StatusForbidden {
		t.Errorf("the id from another address: %d", w.Code)
	}
	if w := req(phone, "POST", "/api/setup/connect", net1, "0000"); w.Code != http.StatusForbidden {
		t.Errorf("a wrong id: %d", w.Code)
	}
	if fs.ssid != "" {
		t.Fatalf("a network was accepted without the button: %q", fs.ssid)
	}
	if w := req(phone, "POST", "/api/setup/connect", net1, ask.ID); w.Code != 200 || fs.ssid != "home" {
		t.Errorf("confirmed: %d %q", w.Code, fs.ssid)
	}
	// the hotspot stopping ends it
	c.Reset()
	if w := req(phone, "POST", "/api/setup/connect", net1, ask.ID); w.Code != http.StatusForbidden {
		t.Errorf("after the hotspot stopped: %d", w.Code)
	}
	// an unanswered request runs out and clears the face
	req(phone, "POST", "/api/setup/confirm", "{}", "")
	c.mu.Lock()
	c.pend.at = c.pend.at.Add(-2 * setupAskTTL)
	c.mu.Unlock()
	if c.Pending() {
		t.Error("the request should have run out")
	}
	for i := 0; i < 100 && shown.get() != ""; i++ {
		time.Sleep(time.Millisecond)
	}
	if shown.get() != "" {
		t.Error("code still shown")
	}
}
