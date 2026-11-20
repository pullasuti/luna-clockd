package web

import (
	"encoding/json"
	"errors"
	"net/http"
)

// DLNAer reports the DLNA devices (Audio > DLNA; switched with the config).
type DLNAer interface {
	DLNAStatus() map[string]any
}

// SSHer manages the clock's ssh keys (System > SSH).
type SSHer interface {
	SSHStatus() map[string]any
	SSHAddKey(text string) (map[string]any, error)
	SSHRemoveKey(fingerprint string) (map[string]any, error)
	SSHEnable(on bool) (map[string]any, error)
}

func (s *Server) sshRoutes(mux *http.ServeMux) {
	get := func(w http.ResponseWriter) SSHer {
		x, ok := s.Backend.(SSHer)
		if !ok {
			httpError(w, http.StatusNotFound, errors.New("ssh not supported"))
		}
		return x
	}
	reply := func(w http.ResponseWriter, m map[string]any, err error) {
		if err != nil {
			httpError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, m)
	}
	mux.HandleFunc("GET /api/dlna", func(w http.ResponseWriter, r *http.Request) {
		x, ok := s.Backend.(DLNAer)
		if !ok {
			httpError(w, http.StatusNotFound, errors.New("dlna not supported"))
			return
		}
		writeJSON(w, x.DLNAStatus())
	})
	mux.HandleFunc("GET /api/ssh", func(w http.ResponseWriter, r *http.Request) {
		if x := get(w); x != nil {
			writeJSON(w, x.SSHStatus())
		}
	})
	mux.HandleFunc("POST /api/ssh", func(w http.ResponseWriter, r *http.Request) {
		x := get(w)
		if x == nil {
			return
		}
		var m struct {
			Key     *string `json:"key"`
			Remove  *string `json:"remove"`
			Enabled *bool   `json:"enabled"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<10)).Decode(&m); err != nil {
			httpError(w, http.StatusBadRequest, err)
			return
		}
		switch {
		case m.Key != nil:
			s.Log.Info("web: ssh key add", "peer", r.RemoteAddr)
			res, err := x.SSHAddKey(*m.Key)
			reply(w, res, err)
		case m.Remove != nil:
			s.Log.Info("web: ssh key remove", "fingerprint", *m.Remove, "peer", r.RemoteAddr)
			res, err := x.SSHRemoveKey(*m.Remove)
			reply(w, res, err)
		case m.Enabled != nil:
			s.Log.Info("web: ssh", "enabled", *m.Enabled, "peer", r.RemoteAddr)
			res, err := x.SSHEnable(*m.Enabled)
			reply(w, res, err)
		default:
			httpError(w, http.StatusBadRequest, errors.New("key, remove or enabled"))
		}
	})
}
