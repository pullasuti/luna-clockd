package web

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type shownCode struct{ v atomic.Value }

func (s *shownCode) get() string { c, _ := s.v.Load().(string); return c }

func newTestAuth(t *testing.T) (*Auth, http.Handler, *shownCode) {
	shown := &shownCode{}
	a := &Auth{Path: filepath.Join(t.TempDir(), "sessions.json"), ShowCode: func(c string) { shown.v.Store(c) }}
	mux := http.NewServeMux()
	a.register(mux, func(string, ...any) {})
	mux.HandleFunc("GET /api/secret", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("POST /api/secret", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("page")) })
	return a, a.Wrap(mux), shown
}

func do(h http.Handler, method, url string, mod func(*http.Request)) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, url, nil)
	r.Host = "10.0.0.219"
	if mod != nil {
		mod(r)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestLoginFlow(t *testing.T) {
	a, h, shown := newTestAuth(t)
	if w := do(h, "GET", "/api/secret", nil); w.Code != 401 {
		t.Fatalf("no session: %d", w.Code)
	}
	if w := do(h, "GET", "/", nil); w.Code != 200 {
		t.Fatalf("page should be public: %d", w.Code)
	}
	w := do(h, "POST", "/api/auth/request", nil)
	var req struct{ ID, Code string }
	json.Unmarshal(w.Body.Bytes(), &req)
	if w.Code != 200 || len(req.Code) != 4 || len(req.ID) != 32 {
		t.Fatalf("request: %d %s", w.Code, w.Body)
	}
	if shown.get() != req.Code || !a.Pending() {
		t.Fatalf("code not shown / not pending")
	}
	other := func(r *http.Request) { r.RemoteAddr = "192.0.2.99:1234" }
	if w := do(h, "POST", "/api/auth/request", other); w.Code != 409 {
		t.Fatalf("another device's request while pending: %d", w.Code)
	}
	// the same browser asking again (a page reload lost the id) replaces its own request
	old := req.ID
	w = do(h, "POST", "/api/auth/request", nil)
	json.Unmarshal(w.Body.Bytes(), &req)
	if w.Code != 200 || req.ID == old || shown.get() != req.Code {
		t.Fatalf("same browser re-request: %d %s", w.Code, w.Body)
	}
	if w := do(h, "GET", "/api/auth/poll?id="+old, nil); !strings.Contains(w.Body.String(), "expired") {
		t.Fatalf("replaced request still alive: %s", w.Body)
	}
	if w := do(h, "GET", "/api/auth/poll?id=wrong", nil); !strings.Contains(w.Body.String(), "expired") {
		t.Fatalf("wrong id: %s", w.Body)
	}
	if w := do(h, "GET", "/api/auth/poll?id="+req.ID, nil); !strings.Contains(w.Body.String(), "pending") {
		t.Fatalf("poll: %s", w.Body)
	}
	if !a.Approve("button", "") {
		t.Fatal("approve")
	}
	w = do(h, "GET", "/api/auth/poll?id="+req.ID, nil)
	cookies := w.Result().Cookies()
	if !strings.Contains(w.Body.String(), "approved") || len(cookies) != 1 || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatalf("approved poll: %s %+v", w.Body, cookies)
	}
	ck := cookies[0]
	withCookie := func(r *http.Request) { r.AddCookie(ck) }
	if w := do(h, "GET", "/api/secret", withCookie); w.Code != 200 {
		t.Fatalf("with session: %d", w.Code)
	}
	// the token is handed out once
	if w := do(h, "GET", "/api/auth/poll?id="+req.ID, nil); len(w.Result().Cookies()) != 0 {
		t.Fatal("token issued twice")
	}
	// cross-site POST refused even with the cookie; same-origin accepted
	if w := do(h, "POST", "/api/secret", func(r *http.Request) { withCookie(r); r.Header.Set("Origin", "http://evil.example") }); w.Code != 403 {
		t.Fatalf("cross-site POST: %d", w.Code)
	}
	if w := do(h, "POST", "/api/secret", func(r *http.Request) { withCookie(r); r.Header.Set("Origin", "http://10.0.0.219") }); w.Code != 200 {
		t.Fatalf("same-origin POST: %d", w.Code)
	}
	// DNS rebinding: unknown host name refused
	if w := do(h, "GET", "/", func(r *http.Request) { r.Host = "attacker.example.com" }); w.Code != http.StatusMisdirectedRequest {
		t.Fatalf("foreign host: %d", w.Code)
	}
	for _, host := range []string{"lenovo-clock", "lenovo-clock.local", "10.0.0.219:80", "[fe80::1]"} {
		if w := do(h, "GET", "/", func(r *http.Request) { r.Host = host }); w.Code != 200 {
			t.Errorf("host %s refused", host)
		}
	}
	// sessions survive a restart (new Auth on the same file) and can be revoked
	b := &Auth{Path: a.Path}
	mux := http.NewServeMux()
	b.register(mux, func(string, ...any) {})
	mux.HandleFunc("GET /api/secret", func(w http.ResponseWriter, r *http.Request) {})
	h2 := b.Wrap(mux)
	if w := do(h2, "GET", "/api/secret", withCookie); w.Code != 200 {
		t.Fatalf("after restart: %d", w.Code)
	}
	w = do(h2, "GET", "/api/auth/sessions", withCookie)
	var sl struct {
		Sessions []struct {
			ID      string
			Current bool
		}
	}
	json.Unmarshal(w.Body.Bytes(), &sl)
	if len(sl.Sessions) != 1 || !sl.Sessions[0].Current {
		t.Fatalf("sessions: %s", w.Body)
	}
	if w := do(h2, "DELETE", "/api/auth/sessions/"+sl.Sessions[0].ID, withCookie); w.Code != 200 {
		t.Fatalf("revoke: %d", w.Code)
	}
	if w := do(h2, "GET", "/api/secret", withCookie); w.Code != 401 {
		t.Fatalf("revoked session still works: %d", w.Code)
	}
}

// A login made before the clock had the time (first setup: it boots in 2015) is
// kept after the real time arrives and clockd restarts, not dropped as idle.
func TestSessionFromBeforeTheTimeSurvives(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	token := strings.Repeat("ab", 32)
	boot := time.Date(2015, 1, 1, 0, 8, 0, 0, time.UTC)
	raw, _ := json.Marshal([]Session{{ID: "s1", Hash: hashToken(token), Created: boot, LastSeen: boot, Method: "button"}})
	os.WriteFile(path, raw, 0o600)
	a := &Auth{Path: path}
	r := httptest.NewRequest("GET", "/", nil)
	r.AddCookie(&http.Cookie{Name: cookieName, Value: token})
	s, ok := a.sessionFor(r)
	if !ok {
		t.Fatal("the session from before the time sync was dropped")
	}
	if s.LastSeen.Before(TimeFloor) || s.Created.Before(TimeFloor) {
		t.Errorf("not restamped: %+v", s)
	}
	var saved []Session
	b, _ := os.ReadFile(path)
	json.Unmarshal(b, &saved)
	if len(saved) != 1 || saved[0].LastSeen.Before(TimeFloor) {
		t.Errorf("restamp not saved: %s", b)
	}
	// a real old session still expires
	old := time.Now().Add(-sessionMaxIdle - time.Hour)
	raw, _ = json.Marshal([]Session{{ID: "s2", Hash: hashToken(token), Created: old, LastSeen: old}})
	os.WriteFile(path, raw, 0o600)
	if _, ok := (&Auth{Path: path}).sessionFor(r); ok {
		t.Error("an idle session survived")
	}
}

func TestCancelClearsCode(t *testing.T) {
	_, h, shown := newTestAuth(t)
	w := do(h, "POST", "/api/auth/request", nil)
	var req struct{ ID string }
	json.Unmarshal(w.Body.Bytes(), &req)
	do(h, "POST", "/api/auth/cancel?id="+req.ID, nil)
	for i := 0; i < 100 && shown.get() != ""; i++ {
		time.Sleep(time.Millisecond) // ShowCode("") runs in a goroutine
	}
	if shown.get() != "" {
		t.Fatal("code not cleared")
	}
	if w := do(h, "POST", "/api/auth/request", nil); w.Code != 429 {
		t.Fatalf("immediate re-request should wait: %d", w.Code)
	}
}

// login helper: request, approve with fn, collect the cookie
func loginWith(t *testing.T, a *Auth, h http.Handler, approve func(code string) bool) *http.Cookie {
	t.Helper()
	for i := 0; i < 50; i++ { // wait out requestPause from a previous login
		a.mu.Lock()
		a.lastDone = time.Time{}
		a.mu.Unlock()
		w := do(h, "POST", "/api/auth/request", func(r *http.Request) { r.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux) Firefox/140.0") })
		if w.Code == 200 {
			var req struct{ ID, Code string }
			json.Unmarshal(w.Body.Bytes(), &req)
			if !approve(req.Code) {
				t.Fatal("approve failed")
			}
			w = do(h, "GET", "/api/auth/poll?id="+req.ID, nil)
			if c := w.Result().Cookies(); len(c) == 1 {
				return c[0]
			}
			t.Fatalf("no cookie: %s", w.Body)
		}
	}
	t.Fatal("could not request a login")
	return nil
}

func TestApproveFromBrowserAndDeny(t *testing.T) {
	a, h, _ := newTestAuth(t)
	first := loginWith(t, a, h, func(string) bool { return a.Approve("button", "") })
	withFirst := func(r *http.Request) { r.AddCookie(first) }

	// a second browser asks; the first one sees it and approves with the code
	a.mu.Lock()
	a.lastDone = time.Time{}
	a.mu.Unlock()
	w := do(h, "POST", "/api/auth/request", nil)
	var req struct{ ID, Code string }
	json.Unmarshal(w.Body.Bytes(), &req)
	w = do(h, "GET", "/api/auth/pending", withFirst)
	if !strings.Contains(w.Body.String(), `"code":"`+req.Code+`"`) {
		t.Fatalf("pending not visible: %s", w.Body)
	}
	if w := do(h, "GET", "/api/auth/pending", nil); w.Code != 401 {
		t.Fatalf("pending visible without a session: %d", w.Code)
	}
	wrong := "0000"
	if req.Code == wrong {
		wrong = "1111"
	}
	if w := do(h, "POST", "/api/auth/approve?code="+wrong, withFirst); w.Code != 409 {
		t.Fatalf("approved with the wrong code: %d", w.Code)
	}
	if w := do(h, "POST", "/api/auth/approve?code="+req.Code, withFirst); w.Code != 200 {
		t.Fatalf("approve: %d %s", w.Code, w.Body)
	}
	w = do(h, "GET", "/api/auth/poll?id="+req.ID, nil)
	if len(w.Result().Cookies()) != 1 {
		t.Fatalf("second browser got no session: %s", w.Body)
	}
	a.mu.Lock()
	method := a.sessions[len(a.sessions)-1].Method
	a.mu.Unlock()
	if method != "browser" {
		t.Fatalf("method %q", method)
	}

	// deny: the asking browser is told so
	a.mu.Lock()
	a.lastDone = time.Time{}
	a.mu.Unlock()
	w = do(h, "POST", "/api/auth/request", nil)
	json.Unmarshal(w.Body.Bytes(), &req)
	if w := do(h, "POST", "/api/auth/deny", withFirst); w.Code != 200 {
		t.Fatalf("deny: %d", w.Code)
	}
	if w := do(h, "GET", "/api/auth/poll?id="+req.ID, nil); !strings.Contains(w.Body.String(), "denied") {
		t.Fatalf("denied poll: %s", w.Body)
	}
}

func TestPasswordLogin(t *testing.T) {
	a, h, _ := newTestAuth(t)
	a.PassPath = filepath.Join(t.TempDir(), "password.json")
	body := func(pw string) func(*http.Request) {
		return func(r *http.Request) {
			r.Body = io.NopCloser(strings.NewReader(`{"password":"` + pw + `"}`))
		}
	}
	if w := do(h, "GET", "/api/auth/methods", nil); !strings.Contains(w.Body.String(), `"password":false`) {
		t.Fatalf("methods: %s", w.Body)
	}
	if w := do(h, "POST", "/api/auth/password/login", body("whatever1")); w.Code != 404 {
		t.Fatalf("login with no password set: %d", w.Code)
	}
	// setting it needs a session
	if w := do(h, "POST", "/api/auth/password", body("correct horse")); w.Code != 401 {
		t.Fatalf("set without session: %d", w.Code)
	}
	ck := loginWith(t, a, h, func(string) bool { return a.Approve("button", "") })
	withCk := func(r *http.Request) { r.AddCookie(ck) }
	if w := do(h, "POST", "/api/auth/password", func(r *http.Request) { withCk(r); body("short")(r) }); w.Code != 400 {
		t.Fatalf("short password accepted: %d", w.Code)
	}
	if w := do(h, "POST", "/api/auth/password", func(r *http.Request) { withCk(r); body("correct horse")(r) }); w.Code != 200 {
		t.Fatalf("set: %d %s", w.Code, w.Body)
	}
	if b, _ := os.ReadFile(a.PassPath); strings.Contains(string(b), "correct horse") {
		t.Fatal("password stored in plain text")
	}
	w := do(h, "POST", "/api/auth/password/login", body("correct horse"))
	if w.Code != 200 || len(w.Result().Cookies()) != 1 {
		t.Fatalf("login: %d %s", w.Code, w.Body)
	}
	// lockout after pwMaxFails wrong attempts (skip the 1 s pause in the test)
	now := time.Now()
	for i := 0; i < pwMaxFails; i++ {
		if ok, locked := a.checkPassword("wrong", now); ok || locked > 0 {
			t.Fatalf("attempt %d: ok=%v locked=%v", i, ok, locked)
		}
	}
	if ok, locked := a.checkPassword("correct horse", now); ok || locked <= 0 {
		t.Fatal("not locked after too many failures (even the right password must wait)")
	}
	if ok, _ := a.checkPassword("correct horse", now.Add(pwFailWindow+time.Second)); !ok {
		t.Fatal("still locked after the window")
	}
	// remove
	if w := do(h, "DELETE", "/api/auth/password", withCk); w.Code != 200 {
		t.Fatalf("remove: %d", w.Code)
	}
	if w := do(h, "POST", "/api/auth/password/login", body("correct horse")); w.Code != 404 {
		t.Fatalf("login after removal: %d", w.Code)
	}
}

// An approved login nobody collects (the browser closed right after the button
// press) must not block new login requests forever (it once did, until a restart).
func TestApprovedLoginNotCollectedFreesSlot(t *testing.T) {
	a, h, _ := newTestAuth(t)
	w := do(h, "POST", "/api/auth/request", func(r *http.Request) { r.Header.Set("Origin", "http://10.0.0.219") })
	if w.Code != 200 {
		t.Fatalf("request: %d %s", w.Code, w.Body)
	}
	a.mu.Lock()
	p := a.pend
	a.mu.Unlock()
	if p == nil {
		t.Fatal("no pending login")
	}
	a.mu.Lock()
	p.approved = true
	p.created = time.Now().Add(-2 * pendingTTL) // approved long ago, never collected
	a.mu.Unlock()
	// the request that expires the stuck pending takes the "pause" itself (429), so one more
	w = do(h, "POST", "/api/auth/request", func(r *http.Request) { r.Header.Set("Origin", "http://10.0.0.219") })
	a.mu.Lock()
	a.lastDone = time.Now().Add(-time.Minute)
	a.mu.Unlock()
	w = do(h, "POST", "/api/auth/request", func(r *http.Request) { r.Header.Set("Origin", "http://10.0.0.219") })
	if w.Code != 200 {
		t.Fatalf("stuck approved login blocks new requests: %d %s", w.Code, w.Body)
	}
}

// The reported case: the page reloaded during the code wait, the button then approved
// the old request nobody collects. The same browser can start over at once; another
// device still can't take the slot, and the old approval hands out no session.
func TestReloadedLoginPageStartsOver(t *testing.T) {
	a, h, _ := newTestAuth(t)
	ua := func(r *http.Request) { r.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux) Firefox/140.0") }
	w := do(h, "POST", "/api/auth/request", ua)
	var first struct{ ID string }
	json.Unmarshal(w.Body.Bytes(), &first)
	if !a.Approve("button", "") {
		t.Fatal("approve")
	}
	if w := do(h, "POST", "/api/auth/request", func(r *http.Request) { r.RemoteAddr = "192.0.2.99:1234"; ua(r) }); w.Code != 409 {
		t.Fatalf("another device took an approved slot: %d", w.Code)
	}
	w = do(h, "POST", "/api/auth/request", ua)
	var second struct{ ID string }
	json.Unmarshal(w.Body.Bytes(), &second)
	if w.Code != 200 || !a.Pending() {
		t.Fatalf("same browser after reload: %d %s", w.Code, w.Body)
	}
	if w := do(h, "GET", "/api/auth/poll?id="+first.ID, nil); len(w.Result().Cookies()) != 0 || !strings.Contains(w.Body.String(), "expired") {
		t.Fatalf("old approval still collectable: %s", w.Body)
	}
	if w := do(h, "GET", "/api/auth/poll?id="+second.ID, nil); !strings.Contains(w.Body.String(), "pending") {
		t.Fatalf("new request should wait for the button: %s", w.Body)
	}
}

func TestFirstRunFlag(t *testing.T) {
	a, h, _ := newTestAuth(t)
	a.PassPath = filepath.Join(t.TempDir(), "password.json")
	if w := do(h, "GET", "/api/auth/methods", nil); !strings.Contains(w.Body.String(), `"first":true`) {
		t.Fatalf("new clock should be first run: %s", w.Body)
	}
	loginWith(t, a, h, func(string) bool { return a.Approve("button", "") })
	if w := do(h, "GET", "/api/auth/methods", nil); !strings.Contains(w.Body.String(), `"first":false`) {
		t.Fatalf("after the first approval: %s", w.Body)
	}
}

// One address can't keep the single button-login slot (and the face) busy:
// after requestMax unapproved requests it waits; other addresses don't.
func TestLoginRequestLimitPerAddress(t *testing.T) {
	a, h, _ := newTestAuth(t)
	from := func(ip string) func(*http.Request) {
		return func(r *http.Request) { r.RemoteAddr = ip + ":1234" }
	}
	free := func() { // as if the request expired and the pause passed
		a.mu.Lock()
		a.pend = nil
		a.lastDone = time.Time{}
		a.mu.Unlock()
	}
	for i := 0; i < requestMax; i++ {
		if w := do(h, "POST", "/api/auth/request", from("192.0.2.7")); w.Code != 200 {
			t.Fatalf("request %d: %d", i, w.Code)
		}
		free()
	}
	if w := do(h, "POST", "/api/auth/request", from("192.0.2.7")); w.Code != 429 {
		t.Fatalf("over the limit: %d", w.Code)
	}
	w := do(h, "POST", "/api/auth/request", from("192.0.2.8"))
	if w.Code != 200 {
		t.Fatalf("another address: %d", w.Code)
	}
	// an approved login clears its address
	var req struct{ ID string }
	json.Unmarshal(w.Body.Bytes(), &req)
	a.mu.Lock()
	a.reqs["192.0.2.8"] = append(a.reqs["192.0.2.8"], time.Now(), time.Now(), time.Now(), time.Now(), time.Now())
	a.mu.Unlock()
	a.Approve("button", "")
	if w := do(h, "GET", "/api/auth/poll?id="+req.ID, from("192.0.2.8")); !strings.Contains(w.Body.String(), "approved") {
		t.Fatalf("poll: %s", w.Body.String())
	}
	free()
	if w := do(h, "POST", "/api/auth/request", from("192.0.2.8")); w.Code != 200 {
		t.Fatalf("after an approved login: %d", w.Code)
	}
}

// Parallel wrong passwords get no more tries than the lockout allows.
func TestPasswordLockoutHoldsInParallel(t *testing.T) {
	a, _, _ := newTestAuth(t)
	a.PassPath = filepath.Join(t.TempDir(), "password.json")
	if err := a.SetPassword("correct horse"); err != nil {
		t.Fatal(err)
	}
	var tried atomic.Int32
	done := make(chan struct{})
	for i := 0; i < 20; i++ {
		go func() {
			if _, locked := a.checkPassword("wrong", time.Now()); locked == 0 {
				tried.Add(1)
			}
			done <- struct{}{}
		}()
	}
	for i := 0; i < 20; i++ {
		<-done
	}
	if n := tried.Load(); n != pwMaxFails {
		t.Fatalf("%d attempts were checked, lockout is %d", n, pwMaxFails)
	}
}
