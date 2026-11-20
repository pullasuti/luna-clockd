package web

// Logins without the button on the clock:
//   - an already logged-in browser approves (or denies) the waiting request
//   - Home Assistant approves it (button entity, see cmd/clockd)
//   - an optional password (off until set from a logged-in browser)

import (
	"crypto/pbkdf2"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"
)

// ---- approve / deny from a logged-in browser ----

func (a *Auth) registerApprovals(mux *http.ServeMux, log func(msg string, args ...any)) {
	mux.HandleFunc("GET /api/auth/pending", func(w http.ResponseWriter, r *http.Request) {
		p, ok := a.PendingInfo()
		if !ok {
			writeJSON(w, map[string]any{"pending": nil})
			return
		}
		writeJSON(w, map[string]any{"pending": p})
	})
	mux.HandleFunc("POST /api/auth/approve", func(w http.ResponseWriter, r *http.Request) {
		by, _ := a.sessionFor(r)
		if !a.Approve("browser", r.URL.Query().Get("code")) {
			httpError(w, http.StatusConflict, errors.New("no waiting login with that code (it may have expired)"))
			return
		}
		log("web: login approved from another browser", "by_session", by.ID, "by_browser", by.Label)
		writeJSON(w, map[string]any{"ok": true})
	})
	mux.HandleFunc("POST /api/auth/deny", func(w http.ResponseWriter, r *http.Request) {
		by, _ := a.sessionFor(r)
		if !a.Deny() {
			httpError(w, http.StatusConflict, errors.New("no waiting login"))
			return
		}
		log("web: login denied", "by_session", by.ID)
		writeJSON(w, map[string]any{"ok": true})
	})
}

// ---- optional password ----

type passwordFile struct {
	Salt string    `json:"salt"`
	Hash string    `json:"hash"` // PBKDF2-HMAC-SHA256
	Iter int       `json:"iter"`
	Set  time.Time `json:"set"`
}

const (
	passwordIter    = 200_000 // ~0.5 s on the clock's Cortex-A53; stored per file
	passwordMinLen  = 8
	passwordMaxLen  = 128
	pwFailWindow    = 15 * time.Minute
	pwMaxFails      = 5 // in the window, from anywhere: then locked until the window passes
	pwFailurePause  = time.Second
	passwordKeySize = 32
)

func (a *Auth) passwordLoad() (passwordFile, bool) {
	var pf passwordFile
	if a.PassPath == "" {
		return pf, false
	}
	b, err := os.ReadFile(a.PassPath)
	if err != nil || json.Unmarshal(b, &pf) != nil || pf.Hash == "" || pf.Salt == "" || pf.Iter <= 0 {
		return pf, false
	}
	return pf, true
}

func derive(password, saltHex string, iter int) (string, error) {
	salt, err := hex.DecodeString(saltHex)
	if err != nil {
		return "", err
	}
	k, err := pbkdf2.Key(sha256.New, password, salt, iter, passwordKeySize)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(k), nil
}

// SetPassword stores a new password ("" removes it).
func (a *Auth) SetPassword(password string) error {
	if a.PassPath == "" {
		return errors.New("password login is not available")
	}
	if password == "" {
		if err := os.Remove(a.PassPath); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if n := len([]rune(password)); n < passwordMinLen || len(password) > passwordMaxLen {
		return fmt.Errorf("the password must be %d-%d characters", passwordMinLen, passwordMaxLen)
	}
	pf := passwordFile{Salt: randHex(16), Iter: passwordIter, Set: time.Now()}
	h, err := derive(password, pf.Salt, pf.Iter)
	if err != nil {
		return err
	}
	pf.Hash = h
	b, _ := json.MarshalIndent(pf, "", "  ")
	tmp := a.PassPath + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, a.PassPath)
}

// checkPassword reports whether password is right, enforcing the lockout.
// locked > 0 means no attempt was made.
//
// One attempt at a time: the lockout count is read and the miss recorded under
// the same lock as the ~0.5 s derivation, so parallel requests can neither
// slip past the count nor run the CPU several derivations at once.
func (a *Auth) checkPassword(password string, now time.Time) (ok bool, locked time.Duration) {
	a.pwMu.Lock()
	defer a.pwMu.Unlock()
	a.mu.Lock()
	kept := a.pwFails[:0]
	for _, t := range a.pwFails {
		if now.Sub(t) < pwFailWindow {
			kept = append(kept, t)
		}
	}
	a.pwFails = kept
	if len(a.pwFails) >= pwMaxFails {
		locked = pwFailWindow - now.Sub(a.pwFails[0])
		a.mu.Unlock()
		return false, locked
	}
	a.mu.Unlock()

	pf, set := a.passwordLoad()
	if set && len(password) <= passwordMaxLen {
		if h, err := derive(password, pf.Salt, pf.Iter); err == nil && subtle.ConstantTimeCompare([]byte(h), []byte(pf.Hash)) == 1 {
			return true, 0
		}
	}
	a.mu.Lock()
	a.pwFails = append(a.pwFails, now)
	a.mu.Unlock()
	return false, 0
}

func (a *Auth) registerPassword(mux *http.ServeMux, log func(msg string, args ...any)) {
	// public: which login methods the login page can offer
	mux.HandleFunc("GET /api/auth/methods", func(w http.ResponseWriter, r *http.Request) {
		_, set := a.passwordLoad()
		// first: no browser approved and no password yet (a new clock): the login screen
		// shows the welcome; nothing else to reveal on a clock that has never been set up
		writeJSON(w, map[string]any{"password": set, "https": r.TLS != nil, "first": !set && a.noSessions()})
	})
	// public, rate-limited
	mux.HandleFunc("POST /api/auth/password/login", func(w http.ResponseWriter, r *http.Request) {
		var m struct {
			Password string `json:"password"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&m); err != nil {
			httpError(w, http.StatusBadRequest, err)
			return
		}
		if _, set := a.passwordLoad(); !set {
			httpError(w, http.StatusNotFound, errors.New("no password is set; log in with the button on the clock"))
			return
		}
		now := time.Now()
		ok, locked := a.checkPassword(m.Password, now)
		if locked > 0 {
			httpError(w, http.StatusTooManyRequests, fmt.Errorf("too many wrong passwords; try again in %d minutes", int(locked.Minutes())+1))
			return
		}
		if !ok {
			log("web: wrong password", "ip", clientIP(r), "browser", browserLabel(r.UserAgent()))
			time.Sleep(pwFailurePause)
			httpError(w, http.StatusUnauthorized, errors.New("wrong password"))
			return
		}
		a.mu.Lock()
		a.load()
		token, s := a.newSessionLocked(r.UserAgent(), clientIP(r), "password", now)
		err := a.saveLocked()
		a.mu.Unlock()
		if err != nil {
			httpError(w, http.StatusInternalServerError, err)
			return
		}
		setCookie(w, token, sessionMaxIdle)
		log("web: login with password", "ip", s.IP, "browser", s.Label, "session", s.ID)
		writeJSON(w, map[string]any{"ok": true})
	})
	// logged in: set / remove
	mux.HandleFunc("POST /api/auth/password", func(w http.ResponseWriter, r *http.Request) {
		var m struct {
			Password string `json:"password"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&m); err != nil {
			httpError(w, http.StatusBadRequest, err)
			return
		}
		if m.Password == "" {
			httpError(w, http.StatusBadRequest, errors.New("empty password (use Remove to turn password login off)"))
			return
		}
		if err := a.SetPassword(m.Password); err != nil {
			httpError(w, http.StatusBadRequest, err)
			return
		}
		by, _ := a.sessionFor(r)
		log("web: password set", "by_session", by.ID)
		writeJSON(w, map[string]any{"ok": true})
	})
	mux.HandleFunc("DELETE /api/auth/password", func(w http.ResponseWriter, r *http.Request) {
		if err := a.SetPassword(""); err != nil {
			httpError(w, http.StatusInternalServerError, err)
			return
		}
		by, _ := a.sessionFor(r)
		log("web: password removed", "by_session", by.ID)
		writeJSON(w, map[string]any{"ok": true})
	})
}
