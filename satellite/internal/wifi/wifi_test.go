package wifi

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPSKVector(t *testing.T) {
	// IEEE 802.11i test vector
	if got := PSK("password", "IEEE"); got != "f42c6fc52df0ebef9ebb4b90b38a5f902e83fe1b135a70e23aed762e9710a12e" {
		t.Fatalf("PSK = %s", got)
	}
}

func TestValidate(t *testing.T) {
	for _, c := range []struct {
		ssid, pw string
		ok       bool
	}{
		{"home", "", true}, {"home", "12345678", true}, {"", "12345678", false},
		{"home", "short", false}, {"home", "pässword123", false}, {strings.Repeat("x", 33), "", false},
	} {
		if err := Validate(c.ssid, c.pw); (err == nil) != c.ok {
			t.Errorf("Validate(%q, %q) = %v", c.ssid, c.pw, err)
		}
	}
}

func TestUnescape(t *testing.T) {
	if got := unescape(`a\tb\nc\rd\ee\q`); got != "a\tb\nc\rd\x1be\\q" {
		t.Fatalf("control escapes: %q", got)
	}
	if got := unescape(`caf\xc3\xa9 \"x\" \\`); got != `café "x" \` {
		t.Fatalf("%q", got)
	}
}

func TestParseEvent(t *testing.T) {
	e := parseEvent("<3>CTRL-EVENT-CONNECTED - Connection to 02:00:00:00:00:01 completed [id=1 id_str=]")
	if e.Level != 3 || e.Name != "CTRL-EVENT-CONNECTED" || e.Arg("id") != "1" || e.Arg("id_str") != "" {
		t.Fatalf("%+v id=%q", e, e.Arg("id"))
	}
	e = parseEvent(`IFNAME=wlan0 <3>CTRL-EVENT-SSID-TEMP-DISABLED id=2 ssid="cabin" auth_failures=1 duration=10 reason=WRONG_KEY`)
	if e.Name != "CTRL-EVENT-SSID-TEMP-DISABLED" || e.Arg("id") != "2" || e.Arg("ssid") != "cabin" || e.Arg("reason") != "WRONG_KEY" {
		t.Fatalf("%+v", e)
	}
}

// fakeWPA is a stand-in wpa_supplicant on a unix datagram socket: text
// commands, ATTACH, events. Network 0 "home" is current and has the strongest
// signal. A selected network connects if it is open or its psk is right
// (after `delay`); like the real one, select_network disables the other
// networks, and a scan moves to the enabled network with the highest
// priority, the strongest ("home", then by id) among equals.
type fakeWPA struct {
	t    *testing.T
	path string
	conn *net.UnixConn

	mu       sync.Mutex
	nets     map[int]map[string]string
	next     int
	current  int
	goodPSK  string
	saved    bool
	commands []string
	attached map[string]*net.UnixAddr
	mute     bool          // deliver no events (the client must poll)
	absent   bool          // the selected network is out of range
	delay    time.Duration // association time
	ip       string
}

func newFake(t *testing.T, goodPSK string) (*fakeWPA, *Manager) {
	t.Helper()
	dir, err := os.MkdirTemp("", "wpa") // short: unix socket paths are limited to ~100 bytes
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	f := &fakeWPA{t: t, path: filepath.Join(dir, "srv"), nets: map[int]map[string]string{0: {"ssid": "686f6d65"}}, next: 1,
		goodPSK: goodPSK, attached: map[string]*net.UnixAddr{}, delay: 30 * time.Millisecond, ip: "192.0.2.58"}
	f.listen()
	t.Cleanup(func() { f.conn.Close() })
	c := &Ctrl{Server: f.path, Local: filepath.Join(dir, "cli"), Timeout: time.Second}
	t.Cleanup(c.Close)
	return f, &Manager{Ctrl: c}
}

func (f *fakeWPA) listen() {
	os.Remove(f.path)
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: f.path, Net: "unixgram"})
	if err != nil {
		f.t.Fatal(err)
	}
	f.conn = conn
	go func() {
		buf := make([]byte, 4096)
		for {
			n, from, err := conn.ReadFromUnix(buf)
			if err != nil {
				return
			}
			conn.WriteToUnix([]byte(f.handle(string(buf[:n]), from)), from)
		}
	}()
}

func (f *fakeWPA) event(s string) {
	if f.mute {
		return
	}
	for _, a := range f.attached {
		f.conn.WriteToUnix([]byte("<3>"+s), a)
	}
}

func (f *fakeWPA) ssidOf(id int) string {
	b := f.nets[id]["ssid"]
	out := make([]byte, len(b)/2)
	for i := range out {
		v, _ := strconv.ParseUint(b[2*i:2*i+2], 16, 8)
		out[i] = byte(v)
	}
	return string(out)
}

func (f *fakeWPA) connectTo(id int) {
	if f.current == id {
		return
	}
	if f.current >= 0 {
		f.event("CTRL-EVENT-DISCONNECTED bssid=02:00:00:00:00:0" + itoa(f.current) + " reason=3 locally_generated=1")
	}
	f.current = -1
	go func() {
		time.Sleep(f.delay)
		f.mu.Lock()
		defer f.mu.Unlock()
		n, ok := f.nets[id]
		switch {
		case !ok || n["disabled"] == "1":
		case f.absent && id != 0:
			f.event("CTRL-EVENT-NETWORK-NOT-FOUND")
			f.event("CTRL-EVENT-NETWORK-NOT-FOUND")
			f.event("CTRL-EVENT-NETWORK-NOT-FOUND")
		case id == 0 || n["key_mgmt"] == "NONE" || n["psk"] == f.goodPSK:
			f.current = id
			f.event("CTRL-EVENT-CONNECTED - Connection to 02:00:00:00:00:0" + itoa(id) + " completed [id=" + itoa(id) + " id_str=]")
		default:
			f.event("WPA: 4-Way Handshake failed - pre-shared key may be incorrect")
			f.event(`CTRL-EVENT-SSID-TEMP-DISABLED id=` + itoa(id) + ` ssid="` + f.ssidOf(id) + `" auth_failures=1 duration=10 reason=WRONG_KEY`)
		}
	}()
}

// best is the network a scan result makes wpa_supplicant choose.
func (f *fakeWPA) best() int {
	best, bestP := -1, -1
	for id := 0; id < f.next; id++ {
		n, ok := f.nets[id]
		if !ok || n["disabled"] == "1" {
			continue
		}
		if p := atoi(n["priority"]); p > bestP {
			best, bestP = id, p
		}
	}
	return best
}

func (f *fakeWPA) handle(line string, from *net.UnixAddr) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	args := strings.Fields(line)
	args[0] = strings.ToLower(args[0])
	f.commands = append(f.commands, strings.Join(args, " "))
	switch args[0] {
	case "attach":
		f.attached[from.Name] = from
	case "detach":
		delete(f.attached, from.Name)
	case "ping":
		return "PONG"
	case "list_networks":
		s := "network id / ssid / bssid / flags"
		for id := 0; id < f.next; id++ {
			if _, ok := f.nets[id]; ok {
				flag := ""
				if id == f.current {
					flag = "[CURRENT]"
				}
				s += "\n" + itoa(id) + "\t" + f.ssidOf(id) + "\tany\t" + flag
			}
		}
		return s
	case "add_network":
		f.nets[f.next] = map[string]string{"disabled": "1"}
		f.next++
		return itoa(f.next - 1)
	case "set_network":
		if _, ok := f.nets[atoi(args[1])]; !ok {
			return "FAIL"
		}
		f.nets[atoi(args[1])][args[2]] = args[3]
	case "get_network":
		n, ok := f.nets[atoi(args[1])]
		if !ok {
			return "FAIL"
		}
		if v := n[args[2]]; v != "" {
			return v
		}
		return "0"
	case "select_network":
		id := atoi(args[1])
		if _, ok := f.nets[id]; !ok {
			return "FAIL"
		}
		for n := range f.nets {
			f.nets[n]["disabled"] = "1"
		}
		f.nets[id]["disabled"] = "0"
		f.connectTo(id)
	case "enable_network":
		for n := range f.nets {
			f.nets[n]["disabled"] = "0"
		}
	case "scan":
		f.event("CTRL-EVENT-SCAN-RESULTS")
		if b := f.best(); b >= 0 {
			f.connectTo(b)
		}
	case "remove_network":
		delete(f.nets, atoi(args[1]))
	case "save_config":
		f.saved = true
	case "status":
		if f.current < 0 {
			return "wpa_state=SCANNING"
		}
		s := "bssid=02:00:00:00:00:0" + itoa(f.current) + "\nssid=" + f.ssidOf(f.current) + "\nid=" + itoa(f.current) + "\nwpa_state=COMPLETED\nfreq=5180"
		if f.ip != "" {
			s += "\nip_address=" + f.ip
		}
		return s
	case "signal_poll":
		return "RSSI=-60"
	}
	return "OK"
}

func (f *fakeWPA) get(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn()
}

func (f *fakeWPA) wasSaved() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.saved
}

func (f *fakeWPA) cur() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.current
}

func itoa(i int) string { return strconv.Itoa(i) }
func atoi(s string) int { n, _ := strconv.Atoi(s); return n }

func waitJob(t *testing.T, m *Manager) Job {
	t.Helper()
	for i := 0; i < 400; i++ {
		if j := m.Job(); j.State != "connecting" {
			return j
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("job did not finish")
	return Job{}
}

// scanSettles runs a scan on the fake and waits for whatever it reconnects to.
func scanSettles(t *testing.T, f *fakeWPA, m *Manager) int {
	t.Helper()
	if _, err := m.cmd("scan"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(f.delay + 100*time.Millisecond)
	return f.cur()
}

func TestConnectSuccessSaves(t *testing.T) {
	f, m := newFake(t, PSK("correct horse", "cabin"))
	if err := m.Connect("cabin", "correct horse", false, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	if j := waitJob(t, m); j.State != "connected" {
		t.Fatalf("job %+v", j)
	}
	f.get(func() {
		if !f.saved {
			t.Fatal("not saved")
		}
		if _, ok := f.nets[0]; !ok {
			t.Fatal("old network removed; it should stay as a fallback")
		}
		if f.nets[0]["disabled"] == "1" {
			t.Fatal("old network left disabled; it should stay as a fallback")
		}
		for _, c := range f.commands {
			if strings.Contains(c, "correct horse") {
				t.Fatal("plain password sent to wpa_supplicant")
			}
		}
	})
}

// The 2026-11-09 report: the join worked, and the next scan took the clock
// back to the stronger old network because both had priority 0.
func TestJoinedNetworkSurvivesAScan(t *testing.T) {
	f, m := newFake(t, PSK("correct horse", "cabin"))
	m.Connect("cabin", "correct horse", false, 2*time.Second)
	if j := waitJob(t, m); j.State != "connected" {
		t.Fatalf("job %+v", j)
	}
	if got := scanSettles(t, f, m); got != 1 {
		t.Fatalf("after a scan the clock is on network %d, want the joined one (1)", got)
	}
	// back to "home" by choice: that one must stick too, and "cabin" stays saved
	if err := m.Switch(0, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	if j := waitJob(t, m); j.State != "connected" {
		t.Fatalf("switch job %+v", j)
	}
	if got := scanSettles(t, f, m); got != 0 {
		t.Fatalf("after switching back and a scan: network %d, want 0", got)
	}
	f.get(func() {
		if atoi(f.nets[0]["priority"]) <= atoi(f.nets[1]["priority"]) {
			t.Fatalf("priorities: %v", f.nets)
		}
	})
}

func TestConnectWrongPasswordRollsBack(t *testing.T) {
	f, m := newFake(t, PSK("right password", "cabin"))
	start := time.Now()
	if err := m.Connect("cabin", "wrong password", false, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	j := waitJob(t, m)
	if j.State != "failed" || !strings.Contains(j.Message, "wrong password") {
		t.Fatalf("job %+v", j)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("a wrong password should fail on the event, not wait for the timeout")
	}
	time.Sleep(f.delay + 100*time.Millisecond) // the rollback reconnects
	f.get(func() {
		if f.saved {
			t.Fatal("saved a network that didn't work")
		}
		if f.current != 0 || len(f.nets) != 1 || f.nets[0]["disabled"] == "1" {
			t.Fatalf("not rolled back: current=%d nets=%v", f.current, f.nets)
		}
	})
}

func TestConnectOutOfRange(t *testing.T) {
	f, m := newFake(t, "")
	f.get(func() { f.absent = true })
	m.Connect("cabin", "", false, 5*time.Second)
	if j := waitJob(t, m); j.State != "failed" || !strings.Contains(j.Message, "not found") {
		t.Fatalf("job %+v", j)
	}
	time.Sleep(f.delay + 100*time.Millisecond)
	if f.cur() != 0 {
		t.Fatalf("not back on the old network: %d", f.cur())
	}
}

// No events at all (a wpa_supplicant that doesn't deliver them): the status
// poll still sees the connection; a network that never answers is a timeout.
func TestConnectWithoutEvents(t *testing.T) {
	f, m := newFake(t, PSK("right password", "cabin"))
	f.get(func() { f.mute = true })
	m.Connect("cabin", "right password", false, 5*time.Second)
	if j := waitJob(t, m); j.State != "connected" {
		t.Fatalf("job %+v", j)
	}
	m.Connect("other", "wrong password", false, 2500*time.Millisecond)
	if j := waitJob(t, m); j.State != "failed" || !strings.Contains(j.Message, "no answer") {
		t.Fatalf("job %+v", j)
	}
	time.Sleep(f.delay + 100*time.Millisecond)
	if f.cur() != 1 {
		t.Fatalf("not back on the previous network: %d", f.cur())
	}
}

// Both networks hand out the same address (same subnet): the join counts when
// the link is on the new network and WaitIP has an answer; without an answer
// it is rolled back even though the old address is still on the interface.
func TestConnectNeedsAnAddressFromTheNewNetwork(t *testing.T) {
	f, m := newFake(t, "")
	asked := 0
	m.WaitIP = func(ctx context.Context) (string, error) {
		asked++
		if f.cur() != 1 {
			t.Error("WaitIP called before the link was on the new network")
		}
		return "192.0.2.58", nil
	}
	m.Connect("cabin", "", false, 2*time.Second)
	if j := waitJob(t, m); j.State != "connected" || asked != 1 {
		t.Fatalf("job %+v asked=%d", j, asked)
	}
	m.WaitIP = func(ctx context.Context) (string, error) { <-ctx.Done(); return "", errors.New("no lease") }
	m.Connect("garden", "", false, time.Second)
	if j := waitJob(t, m); j.State != "failed" || !strings.Contains(j.Message, "no address") {
		t.Fatalf("job %+v", j)
	}
	time.Sleep(f.delay + 100*time.Millisecond)
	f.get(func() {
		if f.current != 1 || len(f.nets) != 2 {
			t.Fatalf("not rolled back: current=%d nets=%v", f.current, f.nets)
		}
	})
}

// The way back (use-wpa_cli): commands go through Exec, the control socket is
// never opened, and without events the status poll finds the connection.
func TestExecRouteWithoutTheSocket(t *testing.T) {
	f, side := newFake(t, PSK("right password", "cabin"))
	m := &Manager{Ctrl: &Ctrl{Server: f.path + "-none", Timeout: 100 * time.Millisecond}}
	m.Exec = func(args ...string) (string, error) {
		return side.Ctrl.Request(strings.ToUpper(args[0]) + strings.Join(append([]string{""}, args[1:]...), " "))
	}
	f.get(func() { f.mute = true })
	m.Connect("cabin", "right password", false, 5*time.Second)
	if j := waitJob(t, m); j.State != "connected" {
		t.Fatalf("job %+v", j)
	}
	if got := scanSettles(t, f, m); got != 1 {
		t.Fatalf("after a scan: network %d, want 1", got)
	}
	if m.Ctrl.conn != nil {
		t.Fatal("the control socket was opened")
	}
}

func TestSwitchToSaved(t *testing.T) {
	f, m := newFake(t, "")
	f.get(func() {
		f.nets[1] = map[string]string{"ssid": "636162696e", "key_mgmt": "NONE"} // "cabin", reachable
		f.nets[2] = map[string]string{"ssid": "6f6c64", "psk": "stale"}         // "old", wrong psk
		f.next = 3
	})
	if err := m.Switch(2, 3*time.Second); err != nil {
		t.Fatal(err)
	}
	j := waitJob(t, m)
	time.Sleep(f.delay + 100*time.Millisecond)
	if j.State != "failed" || f.cur() != 0 {
		t.Fatalf("unreachable saved network: job %+v current %d", j, f.cur())
	}
	if f.wasSaved() {
		t.Fatal("a failed switch must not rewrite the config")
	}
	if err := m.Switch(1, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	if j := waitJob(t, m); j.State != "connected" || f.cur() != 1 {
		t.Fatalf("switch: job %+v current %d", j, f.cur())
	}
	if !f.wasSaved() {
		t.Fatal("the new order of preference was not saved")
	}
}

func TestForget(t *testing.T) {
	f, m := newFake(t, "")
	f.get(func() {
		f.nets[1] = map[string]string{"ssid": "636162696e"}
		f.next = 2
	})
	if err := m.Forget(0); err == nil {
		t.Fatal("forgot the current network")
	}
	if err := m.Forget(1); err != nil {
		t.Fatal(err)
	}
	f.get(func() {
		if _, ok := f.nets[1]; ok || !f.saved {
			t.Fatalf("not forgotten/saved: %v saved=%v", f.nets, f.saved)
		}
	})
}

// wpa_supplicant restarted: the next request dials and attaches again, and
// subscribers are told (they may have missed events).
func TestCtrlReattaches(t *testing.T) {
	f, m := newFake(t, "")
	ev, done := m.Ctrl.Subscribe()
	defer done()
	if out, err := m.Ctrl.Request("PING"); err != nil || out != "PONG" {
		t.Fatal(out, err)
	}
	f.conn.Close()
	if _, err := m.Ctrl.Request("PING"); err == nil {
		t.Fatal("no error from a dead wpa_supplicant")
	}
	f.get(func() {
		f.attached = map[string]*net.UnixAddr{}
		f.listen()
	})
	if out, err := m.Ctrl.Request("PING"); err != nil || out != "PONG" {
		t.Fatal(out, err)
	}
	m.cmd("scan")
	var got []string
	for len(got) < 3 {
		select {
		case e := <-ev:
			got = append(got, e.Name)
		case <-time.After(time.Second):
			t.Fatalf("events: %v", got)
		}
	}
	if fmt.Sprint(got) != "["+Attached+" "+Attached+" CTRL-EVENT-SCAN-RESULTS]" {
		t.Fatalf("events: %v", got)
	}
}
