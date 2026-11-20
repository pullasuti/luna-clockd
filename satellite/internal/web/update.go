package web

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

// Updater installs signed clockd release packages (System > Update). The
// backend checks the signature itself; the page only uploads and confirms.
type Updater interface {
	// UpdateStatus: running version, the last update's outcome and any
	// uploaded package waiting for confirmation.
	UpdateStatus() map[string]any
	// UpdateUpload stores and checks a package; nothing is installed yet.
	UpdateUpload(r io.Reader) (map[string]any, error)
	// UpdateInstall installs the checked package and restarts clockd.
	UpdateInstall(allowDowngrade bool) (map[string]any, error)
	UpdateCancel()
	UpdateDismiss() error
	// UpdateRevert goes back to the version the last update replaced and
	// restarts clockd.
	UpdateRevert() (map[string]any, error)
	// UpdateCheckOnline asks for the latest release now (the check must be on).
	UpdateCheckOnline() (map[string]any, error)
	// UpdateDownload fetches the latest release's package and checks it like
	// an upload; nothing is installed yet.
	UpdateDownload() (map[string]any, error)
}

// maxPackage bounds an upload (a full package is ~9 MB).
const maxPackage = 32 << 20

func (s *Server) updateRoutes(mux *http.ServeMux) {
	up := func(w http.ResponseWriter) Updater {
		u, ok := s.Backend.(Updater)
		if !ok {
			httpError(w, http.StatusNotFound, errors.New("updates not supported"))
		}
		return u
	}
	reply := func(w http.ResponseWriter, m map[string]any, err error) {
		if err != nil {
			httpError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, m)
	}
	mux.HandleFunc("GET /api/update", func(w http.ResponseWriter, r *http.Request) {
		if u := up(w); u != nil {
			writeJSON(w, u.UpdateStatus())
		}
	})
	// the package is the raw request body (no multipart: it's streamed to flash)
	mux.HandleFunc("POST /api/update/upload", func(w http.ResponseWriter, r *http.Request) {
		u := up(w)
		if u == nil {
			return
		}
		s.Log.Info("web: update package upload", "bytes", r.ContentLength, "peer", r.RemoteAddr)
		m, err := u.UpdateUpload(http.MaxBytesReader(w, r.Body, maxPackage))
		reply(w, m, err)
	})
	mux.HandleFunc("POST /api/update/install", func(w http.ResponseWriter, r *http.Request) {
		u := up(w)
		if u == nil {
			return
		}
		var m struct {
			AllowDowngrade bool `json:"allow_downgrade"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&m); err != nil {
			httpError(w, http.StatusBadRequest, err)
			return
		}
		s.Log.Info("web: update install requested", "allow_downgrade", m.AllowDowngrade, "peer", r.RemoteAddr)
		res, err := u.UpdateInstall(m.AllowDowngrade)
		reply(w, res, err)
	})
	mux.HandleFunc("POST /api/update/cancel", func(w http.ResponseWriter, r *http.Request) {
		if u := up(w); u != nil {
			u.UpdateCancel()
			writeJSON(w, u.UpdateStatus())
		}
	})
	mux.HandleFunc("POST /api/update/revert", func(w http.ResponseWriter, r *http.Request) {
		if u := up(w); u != nil {
			s.Log.Info("web: going back to the previous version", "peer", r.RemoteAddr)
			m, err := u.UpdateRevert()
			reply(w, m, err)
		}
	})
	mux.HandleFunc("POST /api/update/check", func(w http.ResponseWriter, r *http.Request) {
		if u := up(w); u != nil {
			s.Log.Info("web: online update check", "peer", r.RemoteAddr)
			m, err := u.UpdateCheckOnline()
			reply(w, m, err)
		}
	})
	mux.HandleFunc("POST /api/update/download", func(w http.ResponseWriter, r *http.Request) {
		if u := up(w); u != nil {
			s.Log.Info("web: update download requested", "peer", r.RemoteAddr)
			m, err := u.UpdateDownload()
			reply(w, m, err)
		}
	})
	mux.HandleFunc("POST /api/update/dismiss", func(w http.ResponseWriter, r *http.Request) {
		if u := up(w); u != nil {
			err := u.UpdateDismiss()
			reply(w, u.UpdateStatus(), err)
		}
	})
}
