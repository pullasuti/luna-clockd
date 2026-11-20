package web

// Login by physical presence: a browser asks for a login, the clock shows a
// 4-digit code, and a long press of the action button approves it. The
// browser then gets a long-lived session cookie. Only the SHA-256 of each
// session token is stored on the device.
//
// Transport is plain HTTP for now (accepted risk on the LAN): the cookie can be
// sniffed there. Cross-site use is blocked with SameSite=Strict, an Origin
// check on state-changing requests and a Host check against DNS rebinding.

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	cookieName     = "clockd_session"
	sessionMaxIdle = 180 * 24 * time.Hour
	maxSessions    = 20
	pendingTTL     = 60 * time.Second
	requestPause   = 3 * time.Second // between a finished request and the next one
	// One address may have this many login requests that were not approved
	// inside the window; then it waits. The button login has a single slot and
	// shows a code on the face, so a device asking over and over would keep the
	// face busy and everyone else out.
	requestWindow = 10 * time.Minute
	requestMax    = 6
)

// Session is one approved browser.
type Session struct {
	ID       string    `json:"id"`   // public short id (for listing / revoking)
	Hash     string    `json:"hash"` // sha256 of the token
	Label    string    `json:"label"`
	IP       string    `json:"ip"`
	Created  time.Time `json:"created"`
	LastSeen time.Time `json:"last_seen"`
	// Method is how the login was approved: "button", "browser", "home
	// assistant" or "password" (empty for sessions from before v0.29).
	Method string `json:"method,omitempty"`
}

type pending struct {
	id       string // secret, known only to the requesting browser
	code     string
	ip, ua   string
	created  time.Time
	approved bool
	method   string // who approved it
}

// Auth holds sessions and the (single) pending login request.
type Auth struct {
	Path string // sessions file
	// ShowCode is called with the code to display ("" = clear).
	ShowCode func(code string)
	// Hosts are extra accepted Host names besides IP literals and LAN-style
	// names (single label, .local, .lan, .home, .internal, .home.arpa).
	Hosts []string
	// PassPath is the optional password file (see password.go).
	PassPath string
	// OnApprove is called after a pending login was approved (method as in
	// Session.Method), e.g. to confirm it on the clock face.
	OnApprove func(method string)
	// OnPendingChange is called when a login starts waiting or stops waiting.
	OnPendingChange func()

	mu       sync.Mutex
	sessions []Session
	pend     *pending
	deniedID string     // the last denied request, so its browser can be told
	pwMu     sync.Mutex // one password attempt at a time (checkPassword)
	pwFails  []time.Time
	reqs     map[string][]time.Time // login requests per address, not (yet) approved
	lastDone time.Time
	dirty    time.Time
	loaded   bool
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func hashToken(t string) string {
	s := sha256.Sum256([]byte(t))
	return hex.EncodeToString(s[:])
}

func (a *Auth) load() {
	if a.loaded {
		return
	}
	a.loaded = true
	if b, err := os.ReadFile(a.Path); err == nil {
		json.Unmarshal(b, &a.sessions)
	}
}

// noSessions reports whether no browser is approved (a clock that was never set up).
func (a *Auth) noSessions() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.load()
	return len(a.sessions) == 0
}

func (a *Auth) saveLocked() error {
	b, _ := json.MarshalIndent(a.sessions, "", "  ")
	tmp := a.Path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	a.dirty = time.Time{}
	return os.Rename(tmp, a.Path)
}

// TimeFloor: a clock before this hasn't got the real time yet. The clocks have
// no battery clock and boot in 2015 until Home Assistant (or NTP) sets it.
var TimeFloor = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// expireLocked drops idle sessions and an expired request.
func (a *Auth) expireLocked(now time.Time) {
	// a login made before the clock knew the time (a first setup: the time
	// arrives with Home Assistant, and clockd restarts for its timezone) was
	// stamped in 2015: restamp it, or it would count as idle for years
	if !now.Before(TimeFloor) {
		fixed := false
		for i := range a.sessions {
			s := &a.sessions[i]
			if s.LastSeen.Before(TimeFloor) {
				s.LastSeen, fixed = now, true
			}
			if s.Created.Before(TimeFloor) {
				s.Created, fixed = now, true
			}
		}
		if fixed {
			a.saveLocked()
		}
	}
	kept := a.sessions[:0]
	for _, s := range a.sessions {
		if now.Sub(s.LastSeen) < sessionMaxIdle {
			kept = append(kept, s)
		}
	}
	a.sessions = kept
	// also an approved login nobody collects (the browser closed after the
	// button): otherwise it blocks every new request until a restart
	if a.pend != nil && now.Sub(a.pend.created) > pendingTTL {
		a.clearPendingLocked(now)
	}
}

// requestAllowedLocked counts a login request from ip, or returns how long the
// address has to wait (requestMax unapproved requests in requestWindow).
func (a *Auth) requestAllowedLocked(ip string, now time.Time) time.Duration {
	for k, ts := range a.reqs {
		kept := ts[:0]
		for _, t := range ts {
			if now.Sub(t) < requestWindow {
				kept = append(kept, t)
			}
		}
		if len(kept) == 0 {
			delete(a.reqs, k)
		} else {
			a.reqs[k] = kept
		}
	}
	if ts := a.reqs[ip]; len(ts) >= requestMax {
		return requestWindow - now.Sub(ts[0])
	}
	if a.reqs == nil {
		a.reqs = map[string][]time.Time{}
	}
	a.reqs[ip] = append(a.reqs[ip], now)
	return 0
}

func (a *Auth) clearPendingLocked(now time.Time) {
	a.pend = nil
	a.lastDone = now
	if a.ShowCode != nil {
		go a.ShowCode("")
	}
	if a.OnPendingChange != nil {
		go a.OnPendingChange()
	}
}

// Pending reports whether a login request is waiting for the button.
func (a *Auth) Pending() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.expireLocked(time.Now())
	return a.pend != nil && !a.pend.approved
}

// Approve accepts the pending request. method says who did it ("button",
// "browser", "home assistant"); code, if not empty, must match the request's
// code (so a remote approver confirms the request it was shown).
func (a *Auth) Approve(method, code string) bool {
	a.mu.Lock()
	a.expireLocked(time.Now())
	if a.pend == nil || a.pend.approved || (code != "" && subtle.ConstantTimeCompare([]byte(code), []byte(a.pend.code)) != 1) {
		a.mu.Unlock()
		return false
	}
	a.pend.approved = true
	a.pend.method = method
	a.pend.created = time.Now() // the browser has another TTL to collect it
	a.mu.Unlock()
	if a.ShowCode != nil {
		go a.ShowCode("")
	}
	if a.OnPendingChange != nil {
		go a.OnPendingChange()
	}
	if a.OnApprove != nil {
		go a.OnApprove(method)
	}
	return true
}

// Deny refuses the pending request.
func (a *Auth) Deny() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.pend == nil || a.pend.approved {
		return false
	}
	a.deniedID = a.pend.id
	a.clearPendingLocked(time.Now())
	return true
}

// PendingLogin describes the waiting request for approvers (no secrets).
type PendingLogin struct {
	Code    string `json:"code"`
	Browser string `json:"browser"`
	IP      string `json:"ip"`
	Age     int    `json:"age_s"`
}

// PendingInfo returns the waiting request, if any.
func (a *Auth) PendingInfo() (PendingLogin, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	a.expireLocked(now)
	if a.pend == nil || a.pend.approved {
		return PendingLogin{}, false
	}
	return PendingLogin{Code: a.pend.code, Browser: browserLabel(a.pend.ua), IP: a.pend.ip,
		Age: int(now.Sub(a.pend.created).Seconds())}, true
}

// newSessionLocked adds a session and returns its token.
func (a *Auth) newSessionLocked(ua, ip, method string, now time.Time) (string, Session) {
	token := randHex(32)
	s := Session{ID: randHex(4), Hash: hashToken(token), Label: browserLabel(ua), IP: ip, Created: now, LastSeen: now, Method: method}
	a.sessions = append(a.sessions, s)
	if len(a.sessions) > maxSessions {
		sort.Slice(a.sessions, func(i, j int) bool { return a.sessions[i].LastSeen.After(a.sessions[j].LastSeen) })
		a.sessions = a.sessions[:maxSessions]
	}
	return token, s
}

// sessionFor returns the session matching the request cookie.
func (a *Auth) sessionFor(r *http.Request) (*Session, bool) {
	c, err := r.Cookie(cookieName)
	if err != nil || len(c.Value) != 64 {
		return nil, false
	}
	h := hashToken(c.Value)
	a.mu.Lock()
	defer a.mu.Unlock()
	a.load()
	now := time.Now()
	a.expireLocked(now)
	for i := range a.sessions {
		if subtle.ConstantTimeCompare([]byte(a.sessions[i].Hash), []byte(h)) == 1 {
			s := &a.sessions[i]
			if now.Sub(s.LastSeen) > time.Minute {
				s.LastSeen = now
				if a.dirty.IsZero() {
					a.dirty = now
				}
			}
			// persist last-seen at most every 10 minutes
			if !a.dirty.IsZero() && now.Sub(a.dirty) > 10*time.Minute {
				a.saveLocked()
			}
			cp := *s
			return &cp, true
		}
	}
	return nil, false
}

func setCookie(w http.ResponseWriter, token string, maxAge time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: token, Path: "/",
		MaxAge: int(maxAge.Seconds()), HttpOnly: true, SameSite: http.SameSiteStrictMode,
	})
}

func clientIP(r *http.Request) string {
	h, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return h
}

// browserLabel makes a short label from the User-Agent.
func browserLabel(ua string) string {
	name, plat := "Browser", ""
	for _, b := range []string{"Firefox", "Edg", "OPR", "Chrome", "Safari"} {
		if strings.Contains(ua, b+"/") {
			name = map[string]string{"Edg": "Edge", "OPR": "Opera"}[b]
			if name == "" {
				name = b
			}
			break
		}
	}
	for _, o := range []string{"Android", "iPhone", "iPad", "Windows", "Mac OS X", "Linux"} {
		if strings.Contains(ua, o) {
			plat = map[string]string{"Mac OS X": "macOS"}[o]
			if plat == "" {
				plat = o
			}
			break
		}
	}
	if plat != "" {
		return name + " on " + plat
	}
	return name
}

func newCode() string {
	n, _ := rand.Int(rand.Reader, big.NewInt(10000))
	return fmt.Sprintf("%04d", n.Int64())
}

// ---- request checks ----

func (a *Auth) hostAllowed(host string) bool {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.ToLower(strings.Trim(host, "[]")), ".")
	if net.ParseIP(host) != nil || host == "localhost" || (host != "" && !strings.Contains(host, ".")) {
		return true
	}
	for _, suf := range []string{".local", ".lan", ".home", ".internal", ".home.arpa"} {
		if strings.HasSuffix(host, suf) {
			return true
		}
	}
	for _, h := range a.Hosts {
		if strings.EqualFold(h, host) {
			return true
		}
	}
	return false
}

// originOK: state-changing requests must come from our own page.
func originOK(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	o := r.Header.Get("Origin")
	if o == "" {
		// non-browser clients (curl) send no Origin; browsers always do on POST
		return r.Header.Get("Sec-Fetch-Site") == "" || r.Header.Get("Sec-Fetch-Site") == "same-origin"
	}
	return strings.TrimPrefix(strings.TrimPrefix(o, "http://"), "https://") == r.Host
}

var openPaths = map[string]bool{
	"/api/auth/request": true,
	"/api/auth/poll":    true,
	"/api/auth/cancel":  true,
	"/api/auth/me":      true,
	// password login (only works when a password is set; rate-limited)
	"/api/auth/methods":        true,
	"/api/auth/password/login": true,
}

// Wrap protects next: Host and Origin checks for everything, a session for
// /api/* except the login endpoints. Static files (the page itself) are public.
func (a *Auth) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !a.hostAllowed(r.Host) {
			http.Error(w, "unknown host name", http.StatusMisdirectedRequest)
			return
		}
		if !originOK(r) {
			httpError(w, http.StatusForbidden, errors.New("cross-site request refused"))
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") && !openPaths[r.URL.Path] {
			if _, ok := a.sessionFor(r); !ok {
				httpError(w, http.StatusUnauthorized, errors.New("login required"))
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// ---- endpoints ----

func (a *Auth) register(mux *http.ServeMux, log func(msg string, args ...any)) {
	mux.HandleFunc("POST /api/auth/request", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		now := time.Now()
		a.expireLocked(now)
		ip := clientIP(r)
		// the same browser asking again (a reload or closed tab lost the old id) replaces its
		// own request, approved or not: nobody else can collect it. The new code needs the
		// button again; the per-address limit below still counts every request.
		own := a.pend != nil && a.pend.ip == ip && a.pend.ua == r.UserAgent()
		if a.pend != nil && !own {
			a.mu.Unlock()
			httpError(w, http.StatusConflict, errors.New("another login is waiting for the button; try again in a minute"))
			return
		}
		if !own && now.Sub(a.lastDone) < requestPause {
			a.mu.Unlock()
			httpError(w, http.StatusTooManyRequests, errors.New("too soon, try again in a few seconds"))
			return
		}
		if wait := a.requestAllowedLocked(ip, now); wait > 0 {
			a.mu.Unlock()
			httpError(w, http.StatusTooManyRequests, fmt.Errorf("too many login requests from this device; try again in %d minutes", int(wait.Minutes())+1))
			return
		}
		p := &pending{id: randHex(16), code: newCode(), ip: ip, ua: r.UserAgent(), created: now}
		a.pend = p
		a.mu.Unlock()
		if a.ShowCode != nil {
			a.ShowCode(p.code)
		}
		if a.OnPendingChange != nil {
			go a.OnPendingChange()
		}
		log("web: login requested", "ip", p.ip, "browser", browserLabel(p.ua), "replaced", own)
		writeJSON(w, map[string]any{"id": p.id, "code": p.code, "expires_in": int(pendingTTL.Seconds())})
	})

	mux.HandleFunc("GET /api/auth/poll", func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("id")
		a.mu.Lock()
		a.load()
		now := time.Now()
		a.expireLocked(now)
		p := a.pend
		if p == nil || subtle.ConstantTimeCompare([]byte(p.id), []byte(id)) != 1 {
			denied := a.deniedID != "" && subtle.ConstantTimeCompare([]byte(a.deniedID), []byte(id)) == 1
			a.mu.Unlock()
			if denied {
				writeJSON(w, map[string]any{"state": "denied"})
				return
			}
			writeJSON(w, map[string]any{"state": "expired"})
			return
		}
		if !p.approved {
			left := pendingTTL - now.Sub(p.created)
			a.mu.Unlock()
			writeJSON(w, map[string]any{"state": "pending", "expires_in": int(left.Seconds())})
			return
		}
		token, s := a.newSessionLocked(p.ua, p.ip, p.method, now)
		delete(a.reqs, p.ip) // approved: its requests were real
		a.pend = nil
		a.lastDone = now
		err := a.saveLocked()
		a.mu.Unlock()
		if err != nil {
			httpError(w, http.StatusInternalServerError, err)
			return
		}
		setCookie(w, token, sessionMaxIdle)
		log("web: login approved", "ip", s.IP, "browser", s.Label, "session", s.ID, "by", s.Method)
		writeJSON(w, map[string]any{"state": "approved"})
	})

	mux.HandleFunc("POST /api/auth/cancel", func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("id")
		a.mu.Lock()
		if a.pend != nil && subtle.ConstantTimeCompare([]byte(a.pend.id), []byte(id)) == 1 {
			a.clearPendingLocked(time.Now())
		}
		a.mu.Unlock()
		writeJSON(w, map[string]any{"ok": true})
	})

	mux.HandleFunc("GET /api/auth/me", func(w http.ResponseWriter, r *http.Request) {
		s, ok := a.sessionFor(r)
		if !ok {
			httpError(w, http.StatusUnauthorized, errors.New("login required"))
			return
		}
		writeJSON(w, map[string]any{"session": s.ID, "label": s.Label})
	})

	mux.HandleFunc("POST /api/auth/logout", func(w http.ResponseWriter, r *http.Request) {
		if s, ok := a.sessionFor(r); ok {
			a.revoke(s.ID)
		}
		setCookie(w, "", -time.Second)
		writeJSON(w, map[string]any{"ok": true})
	})

	mux.HandleFunc("GET /api/auth/sessions", func(w http.ResponseWriter, r *http.Request) {
		cur, _ := a.sessionFor(r)
		a.mu.Lock()
		list := make([]map[string]any, 0, len(a.sessions))
		for _, s := range a.sessions {
			list = append(list, map[string]any{"id": s.ID, "label": s.Label, "ip": s.IP, "method": s.Method,
				"created": s.Created, "last_seen": s.LastSeen, "current": cur != nil && cur.ID == s.ID})
		}
		a.mu.Unlock()
		writeJSON(w, map[string]any{"sessions": list})
	})

	a.registerApprovals(mux, log)
	a.registerPassword(mux, log)

	mux.HandleFunc("DELETE /api/auth/sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
		if !a.revoke(r.PathValue("id")) {
			httpError(w, http.StatusNotFound, errors.New("no such session"))
			return
		}
		log("web: session revoked", "session", r.PathValue("id"))
		writeJSON(w, map[string]any{"ok": true})
	})
}

func (a *Auth) revoke(id string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i, s := range a.sessions {
		if s.ID == id {
			a.sessions = append(a.sessions[:i], a.sessions[i+1:]...)
			a.saveLocked()
			return true
		}
	}
	return false
}
