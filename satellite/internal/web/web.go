// Package web serves clockd's management UI: one embedded page and a small
// JSON API. Access control is deliberately absent for now (LAN, during
// development); see the notes' security TODO.
package web

import (
	"compress/gzip"
	"context"
	"crypto/subtle"
	"embed"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"math"
	"net"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
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

// audioType: the Content-Type of a drive's audio file (by its extension).
func audioType(name string) string {
	switch strings.ToLower(path.Ext(name)) {
	case ".flac":
		return "audio/flac"
	case ".mp3":
		return "audio/mpeg"
	case ".wav":
		return "audio/wav"
	}
	return "application/octet-stream"
}

// radioStall: how long a radio stream write may block before the listener
// counts as gone (a variable for the tests).
var radioStall = 10 * time.Second

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
	// skip (or stop skipping) an alarm's next ring: the clock works out which
	// day that is, in its own time zone
	mux.HandleFunc("POST /api/alarms/skip", func(w http.ResponseWriter, r *http.Request) {
		a, ok := s.Backend.(interface {
			Alarms() any
			SkipAlarm(id string, skip bool) error
		})
		if !ok {
			httpError(w, http.StatusNotFound, errors.New("no alarms on this clock"))
			return
		}
		var m struct {
			ID   string `json:"id"`
			Skip bool   `json:"skip"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&m); err != nil {
			httpError(w, http.StatusBadRequest, err)
			return
		}
		if err := a.SkipAlarm(m.ID, m.Skip); err != nil {
			httpError(w, http.StatusBadRequest, err)
			return
		}
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
	// a drive file for the browser to play itself (Player › USB drive on This
	// device): ranges, so it can seek; behind the login like all of /api/
	mux.HandleFunc("GET /api/usb/file", func(w http.ResponseWriter, r *http.Request) {
		u, ok := s.Backend.(interface {
			USBOpen(path string) (*os.File, string, error)
		})
		if !ok {
			httpError(w, http.StatusNotFound, errors.New("no USB socket on this clock"))
			return
		}
		f, p, err := u.USBOpen(r.URL.Query().Get("path"))
		if err != nil {
			httpError(w, http.StatusNotFound, err)
			return
		}
		defer f.Close()
		fi, err := f.Stat()
		if err != nil {
			httpError(w, http.StatusInternalServerError, err)
			return
		}
		if rg := r.Header.Get("Range"); rg == "" || strings.HasPrefix(rg, "bytes=0-") {
			s.Log.Info("web: USB file to a browser", "path", p, "peer", r.RemoteAddr)
		}
		w.Header().Set("Content-Type", audioType(p))
		w.Header().Set("Cache-Control", "private, max-age=3600")
		http.ServeContent(w, r, "", fi.ModTime(), f)
	})
	// the drive's music library: ?brief=1 the scan's state only, else the
	// whole index (gzipped: a big drive's runs to megabytes)
	mux.HandleFunc("GET /api/usb/library", func(w http.ResponseWriter, r *http.Request) {
		u, ok := s.Backend.(interface {
			USBLibrary(full bool) map[string]any
		})
		if !ok {
			httpError(w, http.StatusNotFound, errors.New("no USB socket on this clock"))
			return
		}
		m := u.USBLibrary(r.URL.Query().Get("brief") == "")
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			writeJSON(w, m)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		zw := gzip.NewWriter(w)
		json.NewEncoder(zw).Encode(m)
		zw.Close()
	})
	// a drive file's cover (embedded, else the folder's image; scaled down,
	// ?size=64..600 for thumbnails)
	mux.HandleFunc("GET /api/usb/art", func(w http.ResponseWriter, r *http.Request) {
		u, ok := s.Backend.(interface {
			USBArt(path string, side int) ([]byte, string, error)
		})
		if !ok {
			httpError(w, http.StatusNotFound, errors.New("no USB socket on this clock"))
			return
		}
		side, _ := strconv.Atoi(r.URL.Query().Get("size"))
		data, mime, err := u.USBArt(r.URL.Query().Get("path"), side)
		if err != nil {
			httpError(w, http.StatusNotFound, err)
			return
		}
		w.Header().Set("Content-Type", mime)
		w.Header().Set("Cache-Control", "private, max-age=86400")
		w.Write(data)
	})
	mux.HandleFunc("GET /api/usb/meta", func(w http.ResponseWriter, r *http.Request) {
		u, ok := s.Backend.(interface {
			USBMeta(path string) (map[string]any, error)
		})
		if !ok {
			httpError(w, http.StatusNotFound, errors.New("no USB socket on this clock"))
			return
		}
		m, err := u.USBMeta(r.URL.Query().Get("path"))
		if err != nil {
			httpError(w, http.StatusNotFound, err)
			return
		}
		writeJSON(w, m)
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
	// FM radio (an RTL-SDR stick on the USB socket)
	type radioStreamer interface {
		RadioToken(renew bool) string
		RadioListen(hz int) (<-chan []byte, func(), error)
	}
	type radioBackend interface {
		Radio() map[string]any
		RadioPlay(hz int) error
		RadioRetune(hz int) error // for the listeners only (speaker: false)
		RadioStop()
		RadioSearch() error
	}
	mux.HandleFunc("GET /api/radio", func(w http.ResponseWriter, r *http.Request) {
		rb, ok := s.Backend.(radioBackend)
		if !ok {
			httpError(w, http.StatusNotFound, errors.New("no radio on this clock"))
			return
		}
		writeJSON(w, rb.Radio())
	})
	mux.HandleFunc("POST /api/radio/{op}", func(w http.ResponseWriter, r *http.Request) {
		rb, ok := s.Backend.(radioBackend)
		if !ok {
			httpError(w, http.StatusNotFound, errors.New("no radio on this clock"))
			return
		}
		var err error
		switch r.PathValue("op") {
		case "play":
			var m struct {
				Hz      int   `json:"hz"`
				Speaker *bool `json:"speaker"` // false: only for those listening (Listen here)
			}
			if err = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&m); err == nil {
				if m.Speaker != nil && !*m.Speaker {
					err = rb.RadioRetune(m.Hz)
				} else {
					err = rb.RadioPlay(m.Hz)
				}
			}
			s.Log.Info("web: radio play", "hz", m.Hz, "peer", r.RemoteAddr)
		case "stop":
			rb.RadioStop()
		case "search":
			err = rb.RadioSearch()
			s.Log.Info("web: radio station search", "peer", r.RemoteAddr)
		case "favs":
			fb, ok := s.Backend.(interface{ RadioSetFavs(favs []int) error })
			if !ok {
				httpError(w, http.StatusNotFound, errors.New("no favourites on this clock"))
				return
			}
			var m struct {
				Favs []int `json:"favs"`
			}
			if err = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&m); err == nil {
				err = fb.RadioSetFavs(m.Favs)
			}
		case "newlink":
			if st, ok := s.Backend.(radioStreamer); ok {
				st.RadioToken(true)
				s.Log.Info("web: radio stream link renewed", "peer", r.RemoteAddr)
			}
		default:
			httpError(w, http.StatusNotFound, errors.New("unknown radio action"))
			return
		}
		if err != nil {
			httpError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, rb.Radio())
	})
	// the radio as an endless stream for other players (Home Assistant, Music
	// Assistant, a browser): <token>.flac (32 kHz FLAC, about half the size;
	// FM audio stops at 15 kHz) or <token>.wav (16-bit 48 kHz, 1.5 Mbit/s).
	// Outside /api: such players have no session, so the token in the path is
	// the key. ?mhz=91.2 picks the station when the clock plays none (or only
	// for listeners).
	mux.HandleFunc("GET /radio/{file}", func(w http.ResponseWriter, r *http.Request) {
		rs, ok := s.Backend.(radioStreamer)
		file := r.PathValue("file")
		tok, isFLAC := strings.CutSuffix(file, ".flac")
		if !isFLAC {
			var wav bool
			if tok, wav = strings.CutSuffix(file, ".wav"); !wav {
				tok = ""
			}
		}
		if !ok || tok == "" || subtle.ConstantTimeCompare([]byte(tok), []byte(rs.RadioToken(false))) != 1 {
			http.NotFound(w, r)
			return
		}
		hz := 0
		if m := r.URL.Query().Get("mhz"); m != "" {
			f, err := strconv.ParseFloat(m, 64)
			if err != nil {
				httpError(w, http.StatusBadRequest, errors.New("mhz: a frequency such as 91.2"))
				return
			}
			hz = int(math.Round(f*10)) * 100_000
		}
		w.Header().Set("Content-Type", "audio/wav")
		if isFLAC {
			w.Header().Set("Content-Type", "audio/flac")
		}
		w.Header().Set("Cache-Control", "no-store")
		if r.Method == http.MethodHead {
			return
		}
		c, done, err := rs.RadioListen(hz)
		if err != nil {
			httpError(w, http.StatusConflict, err)
			return
		}
		defer done()
		s.Log.Info("web: radio listener", "peer", r.RemoteAddr, "flac", isFLAC)
		defer s.Log.Info("web: radio listener left", "peer", r.RemoteAddr)
		// each block of 48 kHz 16-bit stereo goes out as it is (WAV) or
		// through the resampler into the FLAC encoder (a frame per 128 ms)
		send := func(b []byte) error { _, err := w.Write(b); return err }
		if isFLAC {
			fs, err := newFLACStream(w, 32_000)
			if err != nil {
				return
			}
			res := newResampler32()
			var sl, sr []int16
			send = func(b []byte) error {
				sl, sr = res.process(b, sl[:0], sr[:0])
				return fs.write(sl, sr)
			}
		} else if _, err := w.Write(streamWAVHeader(48_000, 2)); err != nil {
			return
		}
		// a listener that stops reading (a phone asleep, Wi-Fi gone) would block
		// a write forever without the connection ever closing, and keep the
		// radio running for nobody: each block must go out within radioStall
		rc := http.NewResponseController(w)
		for {
			select {
			case b, ok := <-c:
				if !ok {
					return
				}
				rc.SetWriteDeadline(time.Now().Add(radioStall))
				if err := send(b); err != nil {
					return
				}
				if err := rc.Flush(); err != nil {
					return
				}
			case <-r.Context().Done():
				return
			}
		}
	})
	mux.HandleFunc("POST /api/usb/play", func(w http.ResponseWriter, r *http.Request) {
		u, ok := s.Backend.(interface {
			USBPlay(path string, queue []string) error
		})
		if !ok {
			httpError(w, http.StatusNotFound, errors.New("no USB socket on this clock"))
			return
		}
		var m struct {
			Path  string   `json:"path"`
			Queue []string `json:"queue"` // from the library: the order to play in
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&m); err != nil {
			httpError(w, http.StatusBadRequest, err)
			return
		}
		if err := u.USBPlay(m.Path, m.Queue); err != nil {
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

// streamWAVHeader is a 16-bit PCM WAV header for a stream of unknown length
// (sizes at their maximum, as players expect for live streams).
func streamWAVHeader(rate, channels int) []byte {
	h := make([]byte, 44)
	copy(h, "RIFF")
	binary.LittleEndian.PutUint32(h[4:], 0xFFFFFFFF)
	copy(h[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(h[16:], 16)
	binary.LittleEndian.PutUint16(h[20:], 1) // PCM
	binary.LittleEndian.PutUint16(h[22:], uint16(channels))
	binary.LittleEndian.PutUint32(h[24:], uint32(rate))
	binary.LittleEndian.PutUint32(h[28:], uint32(rate*channels*2))
	binary.LittleEndian.PutUint16(h[32:], uint16(channels*2))
	binary.LittleEndian.PutUint16(h[34:], 16)
	copy(h[36:], "data")
	binary.LittleEndian.PutUint32(h[40:], 0xFFFFFFFF-36)
	return h
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
