package web

import (
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"strings"
)

// SetupBackend is the first-time setup hotspot (clockd setup.go).
type SetupBackend interface {
	Active() bool
	Touch()
	Info() map[string]any
	Connect(ssid, password string, hidden bool) error
}

// SetupIP is the clock's address on its setup hotspot (uap0, set by
// setup-ap.sh; the same fixed address the vendor net_mgr uses).
const SetupIP = "192.168.255.249"

// onSetupNet: the request came in through the setup hotspot.
func onSetupNet(r *http.Request) bool {
	a, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if !ok {
		return false
	}
	host, _, err := net.SplitHostPort(a.String())
	return err == nil && host == SetupIP
}

// setupIDHeader carries the page's confirm id (SetupConfirm).
const setupIDHeader = "X-Setup-Id"

// serveSetup is everything a device on the setup network can reach: the setup
// page and its endpoints. No login, but scanning and sending a network need
// the button on the clock first (SetupConfirm): joining the hotspot only takes
// being in radio range. Anything else, including the phones' "is there
// internet" checks, is sent to the page, which makes them open it.
func (s *Server) serveSetup(w http.ResponseWriter, r *http.Request) {
	if s.Setup == nil || !s.Setup.Active() {
		http.Error(w, "setup mode is off", http.StatusServiceUnavailable)
		return
	}
	s.Setup.Touch()
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if host != SetupIP {
		http.Redirect(w, r, "http://"+SetupIP+"/", http.StatusFound)
		return
	}
	if r.Method == http.MethodPost {
		if o := r.Header.Get("Origin"); o != "" && o != "http://"+SetupIP {
			httpError(w, http.StatusForbidden, errors.New("wrong origin"))
			return
		}
	}
	switch {
	case r.Method == http.MethodGet && (r.URL.Path == "/" || r.URL.Path == "/setup.html"):
		ui, _ := fs.Sub(uiFiles, "ui")
		b, err := fs.ReadFile(ui, "setup.html")
		if err != nil {
			httpError(w, http.StatusInternalServerError, err)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(b)
	case r.Method == http.MethodGet && r.URL.Path == "/api/setup":
		m := s.Setup.Info()
		m["confirm"] = s.setupState(r)
		writeJSON(w, m)
	case r.Method == http.MethodPost && r.URL.Path == "/api/setup/confirm":
		if s.SetupConfirm == nil {
			writeJSON(w, map[string]any{"confirm": "confirmed"})
			return
		}
		id, code, err := s.SetupConfirm.Ask(clientIP(r))
		if err != nil {
			httpError(w, http.StatusConflict, err)
			return
		}
		s.Log.Info("setup: a device asks for the button", "ip", clientIP(r))
		writeJSON(w, map[string]any{"id": id, "code": code, "expires_in": int(setupAskTTL.Seconds())})
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/setup/") && s.setupState(r) != "confirmed":
		httpError(w, http.StatusForbidden, errors.New("confirm with the button on the clock first"))
	case r.Method == http.MethodPost && r.URL.Path == "/api/setup/scan":
		list, err := s.Wifi.Scan(r.Context())
		if err != nil {
			httpError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, map[string]any{"networks": list})
	case r.Method == http.MethodPost && r.URL.Path == "/api/setup/connect":
		var m struct {
			SSID     string `json:"ssid"`
			Password string `json:"password"`
			Hidden   bool   `json:"hidden"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&m); err != nil {
			httpError(w, http.StatusBadRequest, err)
			return
		}
		if err := s.Setup.Connect(strings.TrimSpace(m.SSID), m.Password, m.Hidden); err != nil {
			httpError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	default:
		http.Redirect(w, r, "http://"+SetupIP+"/", http.StatusFound)
	}
}

// setupState: has the button confirmed the page that sent r?
func (s *Server) setupState(r *http.Request) string {
	if s.SetupConfirm == nil { // no button to ask (tests, a build without one)
		return "confirmed"
	}
	return s.SetupConfirm.State(r.Header.Get(setupIDHeader), clientIP(r))
}
