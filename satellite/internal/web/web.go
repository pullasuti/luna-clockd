// Package web serves clockd's management UI: one embedded page and a small
// JSON API. Access control is deliberately absent for now (LAN, during
// development); see the notes' security TODO.
package web

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"lenovo-satellite/internal/config"
	"lenovo-satellite/internal/haapi"
	"lenovo-satellite/internal/wifi"
)

//go:embed ui
var uiFiles embed.FS

// Backend is what clockd exposes to the UI.
type Backend interface {
	Status() map[string]any
	Config() config.Config
	ApplyConfig(config.Config) error
	WakeWords() []WakeWord
	Action(name string, value float64) error
	ShowText(text string, repeat int)
	// PlayURL plays a WAV URL as media (as Home Assistant would); "" stops it.
	PlayURL(url string) error
	APIKey() string
}

// sleepBackend: sleep sounds and the sleep timer (optional).
type sleepBackend interface {
	Sleep() map[string]any
	SleepSound(id string) error
	SleepTimer(minutes int) error
}

type WakeWord struct {
	ID     string `json:"id"`
	Phrase string `json:"phrase"`
}

type Server struct {
	Addr    string
	Backend Backend
	Logs    *Ring
	Log     *slog.Logger
	HA      *haapi.Manager
	Auth    *Auth
	Wifi    *wifi.Manager
	Setup   SetupBackend // optional: the first-time setup hotspot
	// SetupConfirm, when set, makes the setup page wait for the button.
	SetupConfirm *SetupConfirm
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	ui, _ := fs.Sub(uiFiles, "ui")
	// no-cache: the browser re-checks the page on every load, so after an
	// update nobody keeps running an old copy (the UI is small).
	files := http.FileServerFS(ui)
	mux.Handle("GET /", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	}))
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, s.Backend.Status())
	})
	mux.HandleFunc("GET /api/config", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"config": s.Backend.Config(), "wake_words": s.Backend.WakeWords()})
	})
	mux.HandleFunc("POST /api/config", func(w http.ResponseWriter, r *http.Request) {
		var c config.Config
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&c); err != nil {
			httpError(w, http.StatusBadRequest, err)
			return
		}
		if err := s.Backend.ApplyConfig(c); err != nil {
			httpError(w, http.StatusBadRequest, err)
			return
		}
		s.Log.Info("web: configuration saved", "peer", r.RemoteAddr)
		writeJSON(w, map[string]any{"config": s.Backend.Config()})
	})
	mux.HandleFunc("POST /api/action/{name}", func(w http.ResponseWriter, r *http.Request) {
		v, _ := strconv.ParseFloat(r.URL.Query().Get("value"), 64)
		if err := s.Backend.Action(r.PathValue("name"), v); err != nil {
			httpError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	})
	mux.HandleFunc("POST /api/play", func(w http.ResponseWriter, r *http.Request) {
		var m struct {
			URL string `json:"url"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&m); err != nil {
			httpError(w, http.StatusBadRequest, err)
			return
		}
		if err := s.Backend.PlayURL(m.URL); err != nil {
			httpError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	})
	mux.HandleFunc("POST /api/text", func(w http.ResponseWriter, r *http.Request) {
		var m struct {
			Text   string `json:"text"`
			Repeat int    `json:"repeat"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&m); err != nil {
			httpError(w, http.StatusBadRequest, err)
			return
		}
		s.Backend.ShowText(m.Text, m.Repeat)
		writeJSON(w, map[string]any{"ok": true})
	})
	mux.HandleFunc("GET /api/ha", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"token_set": s.HA.Token() != "", "configured": s.HA.Configured()})
	})
	mux.HandleFunc("POST /api/ha/token", func(w http.ResponseWriter, r *http.Request) {
		var m struct {
			Token string `json:"token"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&m); err != nil {
			httpError(w, http.StatusBadRequest, err)
			return
		}
		if err := s.HA.SetToken(m.Token); err != nil {
			httpError(w, http.StatusBadRequest, err)
			return
		}
		s.Log.Info("web: Home Assistant token updated", "set", m.Token != "", "peer", r.RemoteAddr)
		writeJSON(w, map[string]any{"token_set": s.HA.Token() != ""})
	})
	mux.HandleFunc("POST /api/ha/test", func(w http.ResponseWriter, r *http.Request) {
		err := s.HA.Test(r.Context())
		var ce *haapi.CertError
		switch {
		case errors.As(err, &ce):
			writeJSON(w, map[string]any{"ok": false, "error": err.Error(), "fingerprint": ce.Fingerprint, "subject": ce.Subject})
		case err != nil:
			writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
		default:
			writeJSON(w, map[string]any{"ok": true})
		}
	})
	mux.HandleFunc("GET /api/alarms", func(w http.ResponseWriter, r *http.Request) {
		a, ok := s.Backend.(interface{ Alarms() any })
		if !ok {
			httpError(w, http.StatusNotFound, errors.New("no alarms on this clock"))
			return
		}
		writeJSON(w, a.Alarms())
	})
	mux.HandleFunc("POST /api/alarms", func(w http.ResponseWriter, r *http.Request) {
		a, ok := s.Backend.(interface {
			Alarms() any
			SetAlarms(json.RawMessage) error
		})
		if !ok {
			httpError(w, http.StatusNotFound, errors.New("no alarms on this clock"))
			return
		}
		var raw json.RawMessage
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&raw); err != nil {
			httpError(w, http.StatusBadRequest, err)
			return
		}
		if err := a.SetAlarms(raw); err != nil {
			httpError(w, http.StatusBadRequest, err)
			return
		}
		s.Log.Info("web: alarms saved", "peer", r.RemoteAddr)
		writeJSON(w, a.Alarms())
	})
	mux.HandleFunc("GET /api/ha/discover", func(w http.ResponseWriter, r *http.Request) {
		f, ok := s.Backend.(interface{ FindHomeAssistant() any })
		if !ok {
			writeJSON(w, map[string]any{"found": []any{}})
			return
		}
		writeJSON(w, map[string]any{"found": f.FindHomeAssistant()})
	})
	mux.HandleFunc("GET /api/ha/entities", func(w http.ResponseWriter, r *http.Request) {
		list, err := s.HA.Entities(r.Context(), r.URL.Query().Get("domain"))
		if err != nil {
			httpError(w, http.StatusBadGateway, err)
			return
		}
		writeJSON(w, map[string]any{"entities": list})
	})
	mux.HandleFunc("GET /api/sleep", func(w http.ResponseWriter, r *http.Request) {
		sl, ok := s.Backend.(sleepBackend)
		if !ok {
			httpError(w, http.StatusNotFound, errors.New("no sleep sounds"))
			return
		}
		writeJSON(w, sl.Sleep())
	})
	mux.HandleFunc("POST /api/sleep", func(w http.ResponseWriter, r *http.Request) {
		sl, ok := s.Backend.(sleepBackend)
		if !ok {
			httpError(w, http.StatusNotFound, errors.New("no sleep sounds"))
			return
		}
		var m struct {
			Sound *string `json:"sound"` // "" stops the sleep sound
			Timer *int    `json:"timer"` // minutes, 0 cancels
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&m); err != nil {
			httpError(w, http.StatusBadRequest, err)
			return
		}
		if m.Sound != nil {
			if err := sl.SleepSound(*m.Sound); err != nil {
				httpError(w, http.StatusBadRequest, err)
				return
			}
			s.Log.Info("web: sleep sound", "sound", *m.Sound, "peer", r.RemoteAddr)
		}
		if m.Timer != nil {
			if err := sl.SleepTimer(*m.Timer); err != nil {
				httpError(w, http.StatusBadRequest, err)
				return
			}
			s.Log.Info("web: sleep timer", "minutes", *m.Timer, "peer", r.RemoteAddr)
		}
		writeJSON(w, sl.Sleep())
	})
	mux.HandleFunc("GET /api/usb", func(w http.ResponseWriter, r *http.Request) {
		u, ok := s.Backend.(interface{ USB() map[string]any })
		if !ok {
			httpError(w, http.StatusNotFound, errors.New("no USB socket on this clock"))
			return
		}
		writeJSON(w, u.USB())
	})
	mux.HandleFunc("POST /api/usb/mode", func(w http.ResponseWriter, r *http.Request) {
		u, ok := s.Backend.(interface {
			USB() map[string]any
			SetUSBMode(host bool) error
		})
		if !ok {
			httpError(w, http.StatusNotFound, errors.New("no USB socket on this clock"))
			return
		}
		var m struct {
			Host bool `json:"host"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&m); err != nil {
			httpError(w, http.StatusBadRequest, err)
			return
		}
		if err := u.SetUSBMode(m.Host); err != nil {
			httpError(w, http.StatusConflict, err)
			return
		}
		s.Log.Info("web: USB mode change requested", "host", m.Host, "peer", r.RemoteAddr)
		writeJSON(w, u.USB())
	})
	mux.HandleFunc("GET /api/usb/files", func(w http.ResponseWriter, r *http.Request) {
		u, ok := s.Backend.(interface {
			USBFiles(path string) (map[string]any, error)
		})
		if !ok {
			httpError(w, http.StatusNotFound, errors.New("no USB socket on this clock"))
			return
		}
		files, err := u.USBFiles(r.URL.Query().Get("path"))
		if err != nil {
			httpError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, files)
	})
	mux.HandleFunc("POST /api/usb/play", func(w http.ResponseWriter, r *http.Request) {
		u, ok := s.Backend.(interface {
			USBPlay(path string) error
		})
		if !ok {
			httpError(w, http.StatusNotFound, errors.New("no USB socket on this clock"))
			return
		}
		var m struct {
			Path string `json:"path"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&m); err != nil {
			httpError(w, http.StatusBadRequest, err)
			return
		}
		if err := u.USBPlay(m.Path); err != nil {
			httpError(w, http.StatusBadRequest, err)
			return
		}
		s.Log.Info("web: USB playback requested", "path", m.Path, "peer", r.RemoteAddr)
		writeJSON(w, map[string]any{"ok": true})
	})
	mux.HandleFunc("POST /api/usb/skip", func(w http.ResponseWriter, r *http.Request) {
		u, ok := s.Backend.(interface {
			USBSkip(steps int) error
		})
		if !ok {
			httpError(w, http.StatusNotFound, errors.New("no USB socket on this clock"))
			return
		}
		var m struct {
			Steps int `json:"steps"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&m); err != nil {
			httpError(w, http.StatusBadRequest, err)
			return
		}
		if m.Steps == 0 || m.Steps < -1 || m.Steps > 1 {
			httpError(w, http.StatusBadRequest, errors.New("steps must be 1 or -1"))
			return
		}
		if err := u.USBSkip(m.Steps); err != nil {
			httpError(w, http.StatusConflict, err)
			return
		}
		s.Log.Info("web: USB skip requested", "steps", m.Steps, "peer", r.RemoteAddr)
		writeJSON(w, map[string]any{"ok": true})
	})
	mux.HandleFunc("POST /api/usb/pause", func(w http.ResponseWriter, r *http.Request) {
		u, ok := s.Backend.(interface {
			USBPause(on bool) error
		})
		if !ok {
			httpError(w, http.StatusNotFound, errors.New("no USB socket on this clock"))
			return
		}
		var m struct {
			Paused bool `json:"paused"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&m); err != nil {
			httpError(w, http.StatusBadRequest, err)
			return
		}
		if err := u.USBPause(m.Paused); err != nil {
			httpError(w, http.StatusConflict, err)
			return
		}
		s.Log.Info("web: USB pause requested", "paused", m.Paused, "peer", r.RemoteAddr)
		writeJSON(w, map[string]any{"ok": true})
	})
	mux.HandleFunc("POST /api/usb/opts", func(w http.ResponseWriter, r *http.Request) {
		u, ok := s.Backend.(interface {
			USBOpts(shuffle *bool, repeat *string) error
		})
		if !ok {
			httpError(w, http.StatusNotFound, errors.New("no USB socket on this clock"))
			return
		}
		var m struct {
			Shuffle *bool   `json:"shuffle"`
			Repeat  *string `json:"repeat"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&m); err != nil {
			httpError(w, http.StatusBadRequest, err)
			return
		}
		if m.Shuffle == nil && m.Repeat == nil {
			httpError(w, http.StatusBadRequest, errors.New("nothing to set"))
			return
		}
		if err := u.USBOpts(m.Shuffle, m.Repeat); err != nil {
			httpError(w, http.StatusBadRequest, err)
			return
		}
		s.Log.Info("web: USB player options", "shuffle", m.Shuffle, "repeat", m.Repeat, "peer", r.RemoteAddr)
		writeJSON(w, map[string]any{"ok": true})
	})
	s.Auth.register(mux, s.Log.Info)
	mux.HandleFunc("GET /api/clocks", func(w http.ResponseWriter, r *http.Request) {
		c, ok := s.Backend.(interface{ Clocks() any })
		if !ok {
			writeJSON(w, map[string]any{"clocks": []any{}})
			return
		}
		writeJSON(w, map[string]any{"clocks": c.Clocks()})
	})
	mux.HandleFunc("GET /api/wake/history", func(w http.ResponseWriter, r *http.Request) {
		h, ok := s.Backend.(interface{ WakeHistory() map[string]any })
		if !ok {
			httpError(w, http.StatusNotFound, errors.New("no wake word history"))
			return
		}
		writeJSON(w, h.WakeHistory())
	})
	// wake word models: list, upload (manifest + .tflite, downloaded by the browser), delete
	type wakeModels interface {
		WakeModels() map[string]any
		InstallWakeModel(id string, manifest, model []byte, source string) error
		DeleteWakeModel(id string) error
	}
	mux.HandleFunc("GET /api/wake/models", func(w http.ResponseWriter, r *http.Request) {
		if m, ok := s.Backend.(wakeModels); ok {
			writeJSON(w, m.WakeModels())
			return
		}
		httpError(w, http.StatusNotFound, errors.New("no wake word models"))
	})
	mux.HandleFunc("POST /api/wake/models", func(w http.ResponseWriter, r *http.Request) {
		m, ok := s.Backend.(wakeModels)
		if !ok {
			httpError(w, http.StatusNotFound, errors.New("no wake word models"))
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 3<<20)
		if err := r.ParseMultipartForm(3 << 20); err != nil {
			httpError(w, http.StatusBadRequest, err)
			return
		}
		read := func(field string) ([]byte, error) {
			f, _, err := r.FormFile(field)
			if err != nil {
				return nil, fmt.Errorf("missing %s file", field)
			}
			defer f.Close()
			return io.ReadAll(f)
		}
		man, err := read("manifest")
		if err == nil {
			var model []byte
			if model, err = read("model"); err == nil {
				err = m.InstallWakeModel(r.FormValue("id"), man, model, r.FormValue("source"))
			}
		}
		if err != nil {
			httpError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, m.WakeModels())
	})
	mux.HandleFunc("DELETE /api/wake/models/{id}", func(w http.ResponseWriter, r *http.Request) {
		m, ok := s.Backend.(wakeModels)
		if !ok {
			httpError(w, http.StatusNotFound, errors.New("no wake word models"))
			return
		}
		if err := m.DeleteWakeModel(r.PathValue("id")); err != nil {
			httpError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, m.WakeModels())
	})
	s.updateRoutes(mux)
	s.sshRoutes(mux)
	mux.HandleFunc("GET /api/face", func(w http.ResponseWriter, r *http.Request) {
		f, ok := s.Backend.(interface{ Face() map[string]any })
		if !ok {
			httpError(w, http.StatusNotFound, errors.New("no clock face"))
			return
		}
		writeJSON(w, f.Face())
	})
	mux.HandleFunc("GET /api/procs", func(w http.ResponseWriter, r *http.Request) {
		p, ok := s.Backend.(interface{ Processes() map[string]any })
		if !ok {
			httpError(w, http.StatusNotFound, errors.New("no process list"))
			return
		}
		writeJSON(w, p.Processes())
	})
	// a test capture as a live WAV stream (clockd capture.go), ?seconds= (max 1800)
	mux.HandleFunc("GET /api/capture", func(w http.ResponseWriter, r *http.Request) {
		c, ok := s.Backend.(interface {
			Capture(ctx context.Context, max time.Duration, w io.Writer, flush func()) error
		})
		if !ok {
			httpError(w, http.StatusNotFound, errors.New("no test captures"))
			return
		}
		sec, _ := strconv.ParseFloat(r.URL.Query().Get("seconds"), 64)
		if sec <= 0 || sec > 1800 {
			sec = 1800
		}
		started := false
		fw := &firstWrite{w: w, start: func() {
			started = true
			w.Header().Set("Content-Type", "audio/wav")
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Content-Disposition", `attachment; filename="capture-`+time.Now().Format("20060102-150405")+`.wav"`)
		}}
		flush := func() {
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
		if err := c.Capture(r.Context(), time.Duration(sec*float64(time.Second)), fw, flush); err != nil {
			if !started {
				httpError(w, http.StatusConflict, err)
			} else {
				s.Log.Warn("test capture", "err", err)
			}
		}
	})
	// an attempt clip (16 kHz mono WAV, cmd/clockd clips.go), ?name=
	mux.HandleFunc("GET /api/clip", func(w http.ResponseWriter, r *http.Request) {
		c, ok := s.Backend.(interface {
			Clip(name string) (string, bool)
		})
		if !ok {
			httpError(w, http.StatusNotFound, errors.New("no attempt clips"))
			return
		}
		p, ok := c.Clip(r.URL.Query().Get("name"))
		if !ok {
			httpError(w, http.StatusNotFound, errors.New("no such clip"))
			return
		}
		w.Header().Set("Content-Type", "audio/wav")
		w.Header().Set("Cache-Control", "no-store")
		http.ServeFile(w, r, p)
	})
	mux.HandleFunc("GET /api/mic", func(w http.ResponseWriter, r *http.Request) {
		m, ok := s.Backend.(interface{ MicLevel() map[string]any })
		if !ok {
			httpError(w, http.StatusNotFound, errors.New("no microphone meter"))
			return
		}
		writeJSON(w, m.MicLevel())
	})
	mux.HandleFunc("GET /api/wifi", func(w http.ResponseWriter, r *http.Request) {
		saved, err := s.Wifi.SavedNetworks()
		if err != nil {
			httpError(w, http.StatusBadGateway, err)
			return
		}
		writeJSON(w, map[string]any{"status": s.Wifi.Status(), "saved": saved})
	})
	mux.HandleFunc("POST /api/wifi/scan", func(w http.ResponseWriter, r *http.Request) {
		list, err := s.Wifi.Scan(r.Context())
		if err != nil {
			httpError(w, http.StatusBadGateway, err)
			return
		}
		writeJSON(w, map[string]any{"networks": list})
	})
	mux.HandleFunc("POST /api/wifi/connect", func(w http.ResponseWriter, r *http.Request) {
		var m struct {
			SSID     string `json:"ssid"`
			Password string `json:"password"`
			Hidden   bool   `json:"hidden"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&m); err != nil {
			httpError(w, http.StatusBadRequest, err)
			return
		}
		if err := s.Wifi.Connect(m.SSID, m.Password, m.Hidden, 40*time.Second); err != nil {
			httpError(w, http.StatusBadRequest, err)
			return
		}
		s.Log.Info("web: Wi-Fi change requested", "ssid", m.SSID, "peer", r.RemoteAddr)
		writeJSON(w, map[string]any{"ok": true})
	})
	mux.HandleFunc("POST /api/wifi/switch", func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.Atoi(r.URL.Query().Get("id"))
		if err == nil {
			err = s.Wifi.Switch(id, 40*time.Second)
		}
		if err != nil {
			httpError(w, http.StatusBadRequest, err)
			return
		}
		s.Log.Info("web: Wi-Fi switch to a saved network requested", "id", id, "peer", r.RemoteAddr)
		writeJSON(w, map[string]any{"ok": true})
	})
	mux.HandleFunc("POST /api/wifi/forget", func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.Atoi(r.URL.Query().Get("id"))
		if err == nil {
			err = s.Wifi.Forget(id)
		}
		if err != nil {
			httpError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	})
	mux.HandleFunc("GET /api/esphome/key", func(w http.ResponseWriter, r *http.Request) {
		s.Log.Info("web: ESPHome encryption key shown", "peer", r.RemoteAddr)
		writeJSON(w, map[string]any{"key": s.Backend.APIKey()})
	})
	mux.HandleFunc("GET /api/logs", func(w http.ResponseWriter, r *http.Request) {
		n, _ := strconv.Atoi(r.URL.Query().Get("n"))
		if n <= 0 {
			n = 300
		}
		writeJSON(w, map[string]any{"lines": s.Logs.Last(n)})
	})
	main := s.Auth.Wrap(mux)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if onSetupNet(r) {
			s.serveSetup(w, r)
			return
		}
		main.ServeHTTP(w, r)
	})
}

// Run serves until the listener fails.
func (s *Server) Run() error {
	ln, err := net.Listen("tcp", s.Addr)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}
	s.Log.Info("web UI listening", "addr", s.Addr)
	return srv.Serve(ln)
}

// firstWrite calls start before the first byte (headers can still change
// until then, so an error can still be a JSON reply).
type firstWrite struct {
	w     io.Writer
	start func()
	done  bool
}

func (f *firstWrite) Write(p []byte) (int, error) {
	if !f.done {
		f.done = true
		f.start()
	}
	return f.w.Write(p)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}

func httpError(w http.ResponseWriter, code int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

// Ring keeps the last lines written to it (tee the logger into it).
type Ring struct {
	mu    sync.Mutex
	lines []string
	max   int
	part  []byte
}

func NewRing(max int) *Ring { return &Ring{max: max} }

func (r *Ring) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.part = append(r.part, p...)
	for {
		i := indexByte(r.part, '\n')
		if i < 0 {
			break
		}
		r.lines = append(r.lines, string(r.part[:i]))
		r.part = r.part[i+1:]
	}
	if len(r.lines) > r.max {
		r.lines = append([]string(nil), r.lines[len(r.lines)-r.max:]...)
	}
	return len(p), nil
}

func (r *Ring) Last(n int) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n > len(r.lines) {
		n = len(r.lines)
	}
	return append([]string(nil), r.lines[len(r.lines)-n:]...)
}

func indexByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}
