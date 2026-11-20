// Package wifi manages Wi-Fi through the running wpa_supplicant (vendor
// service, config /data/wifi/wpa_supplicant.conf with update_config=1) over
// its control socket (ctrl.go; the vendor wpa_cli only with the use-wpa_cli
// flag file). Network changes are transactional:
// a new network is only saved once the clock has actually connected and got an
// address; otherwise the previous network is restored, so a typo can't strand
// the device.
//
// The network chosen last gets the highest priority. Without that a change
// did not stick (2026-11-09, clock #2: joined "clock", was back on the old
// network 6 s later): select_network disables the other networks, we enable
// them again as fallbacks, and with every network at priority 0 the next scan
// result (the Wi-Fi page scans) made wpa_supplicant move to whichever saved
// network had the strongest signal.
package wifi

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Network is a scan result (one per SSID, strongest kept).
type Network struct {
	SSID     string `json:"ssid"`
	Signal   int    `json:"signal"` // dBm
	Freq     int    `json:"freq"`
	Security string `json:"security"` // "open", "WPA2", "WPA2/WPA3", "WPA3", "WEP", "enterprise"
	Saved    bool   `json:"saved"`
	Current  bool   `json:"current"`
}

// Saved is a configured network.
type Saved struct {
	ID      int    `json:"id"`
	SSID    string `json:"ssid"`
	Current bool   `json:"current"`
}

// Status is the current connection.
type Status struct {
	State  string `json:"state"` // wpa_state, e.g. COMPLETED
	SSID   string `json:"ssid"`
	BSSID  string `json:"bssid"`
	ID     int    `json:"id"` // network id, -1 when not on a saved network
	Freq   int    `json:"freq"`
	IP     string `json:"ip"`
	Signal int    `json:"signal"`
	Job    Job    `json:"job"`
}

// Job is the last connect attempt.
type Job struct {
	State   string    `json:"state"` // "", "connecting", "connected", "failed"
	SSID    string    `json:"ssid"`
	Message string    `json:"message"`
	At      time.Time `json:"at"`
}

type Manager struct {
	Ctrl *Ctrl
	// Exec, when set, runs each command instead of the control socket (WpaCli:
	// the old route through the vendor wpa_cli, flag file use-wpa_cli). There
	// are no events then: a change is followed by asking for the status, so a
	// wrong password or a missing network shows as a timeout.
	Exec func(args ...string) (string, error)
	Log  *slog.Logger // every change attempt ends in one result line here
	// WaitIP waits for the network to hand out an address after the Wi-Fi link
	// came up, and returns it. clockd sets it to "our DHCP client has an
	// answer from THIS network": an address alone proves nothing after a
	// change between two networks on the same subnet, where udhcpc keeps the
	// old one throughout. Nil: wait until wpa_supplicant reports an address.
	WaitIP func(ctx context.Context) (string, error)

	mu  sync.Mutex
	job Job
	op  sync.Mutex // one change at a time
}

func New() *Manager {
	return &Manager{Ctrl: &Ctrl{}}
}

func (m *Manager) log() *slog.Logger {
	if m.Log != nil {
		return m.Log
	}
	return slog.Default()
}

// WpaCli runs the vendor wpa_cli (a Manager's Exec). It drops to the wifi user
// and delivers no events on these clocks; kept as the way back only.
func WpaCli(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	full := append([]string{"-p", "/data/wifi", "-i", "wlan0"}, args...)
	out, err := exec.CommandContext(ctx, "/system/bin/wpa_cli", full...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("wpa_cli %s: %v (%s)", args[0], err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// cmd sends a command, wpa_cli style: cmd("set_network", "1", "priority", "2").
func (m *Manager) cmd(args ...string) (string, error) {
	var out string
	var err error
	if m.Exec != nil {
		out, err = m.Exec(args...)
	} else {
		out, err = m.Ctrl.Request(strings.ToUpper(args[0]) + strings.Join(append([]string{""}, args[1:]...), " "))
	}
	if err != nil {
		return out, err
	}
	if strings.HasPrefix(out, "FAIL") {
		return out, fmt.Errorf("wpa_supplicant %s: %s", args[0], out)
	}
	return out, nil
}

func kv(out string) map[string]string {
	m := map[string]string{}
	for _, l := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(l), "="); ok {
			m[k] = v
		}
	}
	return m
}

// unescape decodes wpa_supplicant's printf-style SSID escapes: all that its
// printf_encode writes (\xNN, \\, \", \n, \r, \t, \e).
func unescape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			switch s[i+1] {
			case 'x':
				if i+3 < len(s) {
					if v, err := strconv.ParseUint(s[i+2:i+4], 16, 8); err == nil {
						b.WriteByte(byte(v))
						i += 3
						continue
					}
				}
			case '\\', '"':
				b.WriteByte(s[i+1])
				i++
				continue
			case 'n', 'r', 't', 'e':
				b.WriteByte(map[byte]byte{'n': '\n', 'r': '\r', 't': '\t', 'e': 0x1b}[s[i+1]])
				i++
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func (m *Manager) Status() Status {
	st := Status{ID: -1}
	if out, err := m.cmd("status"); err == nil {
		v := kv(out)
		st.State, st.SSID, st.BSSID, st.IP = v["wpa_state"], unescape(v["ssid"]), v["bssid"], v["ip_address"]
		st.Freq, _ = strconv.Atoi(v["freq"])
		if id, err := strconv.Atoi(v["id"]); err == nil {
			st.ID = id
		}
	}
	if out, err := m.cmd("signal_poll"); err == nil {
		st.Signal, _ = strconv.Atoi(kv(out)["RSSI"])
	}
	m.mu.Lock()
	st.Job = m.job
	m.mu.Unlock()
	return st
}

// SavedNetworks lists configured networks.
func (m *Manager) SavedNetworks() ([]Saved, error) {
	out, err := m.cmd("list_networks")
	if err != nil {
		return nil, err
	}
	var list []Saved
	for i, l := range strings.Split(out, "\n") {
		if i == 0 || strings.TrimSpace(l) == "" { // header
			continue
		}
		f := strings.Split(l, "\t")
		if len(f) < 2 {
			continue
		}
		id, err := strconv.Atoi(f[0])
		if err != nil {
			continue
		}
		s := Saved{ID: id, SSID: unescape(f[1])}
		if len(f) >= 4 && strings.Contains(f[3], "CURRENT") {
			s.Current = true
		}
		list = append(list, s)
	}
	return list, nil
}

func security(flags string) string {
	switch {
	case strings.Contains(flags, "EAP"):
		return "enterprise"
	case strings.Contains(flags, "SAE") && strings.Contains(flags, "PSK-"), strings.Contains(flags, "SAE") && strings.Contains(flags, "WPA2-PSK"):
		return "WPA2/WPA3"
	case strings.Contains(flags, "SAE"):
		return "WPA3"
	case strings.Contains(flags, "WPA2") || strings.Contains(flags, "WPA-"):
		return "WPA2"
	case strings.Contains(flags, "WEP"):
		return "WEP"
	}
	return "open"
}

// Scan triggers a scan and returns the networks found, strongest first.
func (m *Manager) Scan(ctx context.Context) ([]Network, error) {
	scan := func() error {
		if _, err := m.cmd("scan"); err != nil && !strings.Contains(err.Error(), "FAIL") {
			return err // FAIL-BUSY: a scan is already running, results still come
		}
		return nil
	}
	if err := scan(); err != nil {
		return nil, err
	}
	// The first scan after the radio has been idle takes longer (or is refused
	// while it wakes), so a fixed wait returned an empty list: wait 3 s, then
	// poll until the list stops growing (the connected network is listed before
	// the scan is done), re-sending the scan once if still empty.
	var out string
	prev := -1
	wait := 3 * time.Second
	for t := 3; ; t, wait = t+1, time.Second {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
		var err error
		if out, err = m.cmd("scan_results"); err != nil {
			return nil, err
		}
		n := strings.Count(strings.TrimSpace(out), "\n") // lines after the header
		if (n > 0 && n == prev && t >= 5) || t >= 12 {
			break
		}
		prev = n
		if t == 6 && n == 0 {
			if err := scan(); err != nil {
				return nil, err
			}
		}
	}
	saved, _ := m.SavedNetworks()
	cur := m.Status().SSID
	best := map[string]Network{}
	for i, l := range strings.Split(out, "\n") {
		if i == 0 {
			continue
		}
		f := strings.Split(l, "\t")
		if len(f) < 5 || f[4] == "" {
			continue // hidden networks have no SSID in scan results
		}
		n := Network{SSID: unescape(f[4]), Security: security(f[3])}
		n.Freq, _ = strconv.Atoi(f[1])
		n.Signal, _ = strconv.Atoi(f[2])
		if old, ok := best[n.SSID]; ok && old.Signal >= n.Signal {
			continue
		}
		best[n.SSID] = n
	}
	list := make([]Network, 0, len(best))
	for _, n := range best {
		for _, s := range saved {
			if s.SSID == n.SSID {
				n.Saved = true
			}
		}
		n.Current = n.SSID == cur
		list = append(list, n)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Signal > list[j].Signal })
	return list, nil
}

// PSK derives the WPA pre-shared key (PBKDF2-HMAC-SHA1, 4096 rounds, 32 bytes)
// so the passphrase itself is never stored.
func PSK(passphrase, ssid string) string {
	return hex.EncodeToString(pbkdf2([]byte(passphrase), []byte(ssid), 4096, 32))
}

func pbkdf2(password, salt []byte, iter, keyLen int) []byte {
	prf := hmac.New(sha1.New, password)
	hashLen := prf.Size()
	var dk []byte
	for block := 1; len(dk) < keyLen; block++ {
		prf.Reset()
		prf.Write(salt)
		prf.Write([]byte{byte(block >> 24), byte(block >> 16), byte(block >> 8), byte(block)})
		u := prf.Sum(nil)
		t := append([]byte(nil), u...)
		for n := 1; n < iter; n++ {
			prf.Reset()
			prf.Write(u)
			u = prf.Sum(u[:0])
			for i := range t {
				t[i] ^= u[i]
			}
		}
		dk = append(dk, t[:hashLen]...)
	}
	return dk[:keyLen]
}

// Validate checks the SSID and password before anything is touched.
func Validate(ssid, password string) error {
	if ssid == "" || len(ssid) > 32 {
		return errors.New("the network name must be 1-32 bytes")
	}
	if password != "" {
		if len(password) < 8 || len(password) > 63 || !utf8.ValidString(password) {
			return errors.New("a WPA password is 8-63 characters")
		}
		for _, r := range password {
			if r < 32 || r > 126 {
				return errors.New("a WPA password can only contain printable ASCII characters")
			}
		}
	}
	return nil
}

// Job returns the last connect attempt.
func (m *Manager) Job() Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.job
}

func (m *Manager) setJob(state, ssid, msg string) {
	m.mu.Lock()
	m.job = Job{State: state, SSID: ssid, Message: msg, At: time.Now()}
	m.mu.Unlock()
}

// Connect starts switching to ssid in the background (empty password = open
// network). Progress is in Job(), the outcome also in the log.
func (m *Manager) Connect(ssid, password string, hidden bool, timeout time.Duration) error {
	if err := Validate(ssid, password); err != nil {
		return err
	}
	if !m.op.TryLock() {
		return errors.New("a Wi-Fi change is already in progress")
	}
	m.setJob("connecting", ssid, "connecting…")
	go func() {
		defer m.op.Unlock()
		m.finish("join", ssid, "connected and saved", time.Now(), m.connect(ssid, password, hidden, timeout))
	}()
	return nil
}

// failure is why a change did not happen. result is the log's word for it:
// "failed" (the network or wpa_supplicant said no) or "timeout".
type failure struct {
	result, reason string
}

func (f *failure) Error() string { return f.reason }

func failed(reason string) error { return &failure{"failed", reason} }

// finish records the outcome of a change: the job the pages show, and one log
// line per attempt.
func (m *Manager) finish(what, ssid, okMsg string, start time.Time, err error) {
	took := time.Since(start).Round(100 * time.Millisecond).String()
	if err == nil {
		st := m.Status()
		m.setJob("connected", ssid, okMsg)
		m.log().Info("Wi-Fi "+what+": connected", "ssid", ssid, "bssid", st.BSSID, "ip", st.IP, "took", took)
		return
	}
	result := "failed"
	var f *failure
	if errors.As(err, &f) {
		result = f.result
	}
	st := m.Status()
	m.setJob("failed", ssid, "could not connect: "+err.Error()+"; the previous network stays in use")
	m.log().Warn("Wi-Fi "+what+": not connected", "ssid", ssid, "result", result, "reason", err.Error(),
		"took", took, "now", st.State, "now_ssid", st.SSID)
}

// await waits until wpa_supplicant is connected to network id and the network
// has given an address. It follows the events (subscribed before the select
// command) and also asks for the status every 2 s, in case an event is lost.
func (m *Manager) await(ev <-chan Event, id int, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	sid := strconv.Itoa(id)
	poll := time.NewTicker(2 * time.Second)
	defer poll.Stop()
	last, notFound := "", 0 // last: what the events said went wrong so far
link:
	for {
		select {
		case e := <-ev:
			switch {
			case e.Name == "CTRL-EVENT-CONNECTED" && e.Arg("id") == sid:
				break link
			case e.Name == "CTRL-EVENT-SSID-TEMP-DISABLED" && e.Arg("id") == sid:
				if e.Arg("reason") == "WRONG_KEY" {
					return failed("wrong password")
				}
				last = "the access point refused the connection (" + e.Arg("reason") + ")"
			case strings.Contains(e.Text, "pre-shared key may be incorrect"):
				last = "wrong password"
			case e.Name == "CTRL-EVENT-ASSOC-REJECT" || e.Name == "CTRL-EVENT-AUTH-REJECT":
				last = "the access point refused the connection (status " + e.Arg("status_code") + ")"
			case e.Name == "CTRL-EVENT-NETWORK-NOT-FOUND":
				// one per scan without the network; three scans take ~25 s
				if notFound++; notFound >= 3 {
					return failed("network not found (out of range, or the name is wrong)")
				}
				last = "network not found"
			}
		case <-poll.C:
			if st := m.Status(); st.State == "COMPLETED" && st.ID == id {
				break link
			}
		case <-ctx.Done():
			if last == "" {
				last = "no answer from the network"
			}
			return &failure{"timeout", last + " within " + timeout.String()}
		}
	}
	wait := m.WaitIP
	if wait == nil {
		wait = func(ctx context.Context) (string, error) {
			for {
				if st := m.Status(); st.IP != "" {
					return st.IP, nil
				}
				select {
				case <-ctx.Done():
					return "", ctx.Err()
				case <-time.After(500 * time.Millisecond):
				}
			}
		}
	}
	if _, err := wait(ctx); err != nil {
		return &failure{"timeout", "joined the network, but got no address from it (DHCP) within " + timeout.String()}
	}
	return nil
}

// prefer gives network id a higher priority than every other saved network,
// so wpa_supplicant stays on it while it is in range (see the package comment)
// and the others remain as fallbacks, in the order they were last chosen.
func (m *Manager) prefer(id int, saved []Saved) error {
	prio := func(n int) int {
		out, _ := m.cmd("get_network", strconv.Itoa(n), "priority")
		p, _ := strconv.Atoi(out)
		return p
	}
	top := -1
	for _, s := range saved {
		if s.ID != id {
			top = max(top, prio(s.ID))
		}
	}
	if top >= 1000 { // start over rather than count up forever
		for _, s := range saved {
			if s.ID != id {
				m.cmd("set_network", strconv.Itoa(s.ID), "priority", "0")
			}
		}
		top = 0
	}
	if prio(id) > top {
		return nil
	}
	_, err := m.cmd("set_network", strconv.Itoa(id), "priority", strconv.Itoa(top+1))
	return err
}

func currentID(saved []Saved) int {
	for _, s := range saved {
		if s.Current {
			return s.ID
		}
	}
	return -1
}

func (m *Manager) connect(ssid, password string, hidden bool, timeout time.Duration) error {
	saved, err := m.SavedNetworks()
	if err != nil {
		return err
	}
	prev := currentID(saved)
	out, err := m.cmd("add_network")
	if err != nil {
		return err
	}
	id, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return fmt.Errorf("add_network: %q", out)
	}
	sid := strconv.Itoa(id)
	set := func(k, v string) error { _, err := m.cmd("set_network", sid, k, v); return err }

	steps := [][2]string{{"ssid", hex.EncodeToString([]byte(ssid))}}
	if password == "" {
		steps = append(steps, [2]string{"key_mgmt", "NONE"})
	} else {
		steps = append(steps, [2]string{"psk", PSK(password, ssid)}, [2]string{"key_mgmt", "WPA-PSK"})
	}
	if hidden {
		steps = append(steps, [2]string{"scan_ssid", "1"})
	}
	rollback := func(why error) error {
		m.cmd("remove_network", sid)
		if prev >= 0 {
			m.cmd("select_network", strconv.Itoa(prev))
		}
		m.cmd("enable_network", "all")
		return why
	}
	for _, s := range steps {
		if err := set(s[0], s[1]); err != nil {
			return rollback(err)
		}
	}
	ev, done := m.Ctrl.Subscribe()
	defer done()
	if _, err := m.cmd("select_network", sid); err != nil {
		return rollback(err)
	}
	if err := m.await(ev, id, timeout); err != nil {
		return rollback(err)
	}
	// keep the old networks as fallbacks, drop older copies of this one
	rest := []Saved{}
	for _, s := range saved {
		if s.SSID == ssid {
			m.cmd("remove_network", strconv.Itoa(s.ID))
		} else {
			rest = append(rest, s)
		}
	}
	if err := m.prefer(id, rest); err != nil {
		return rollback(err)
	}
	m.cmd("enable_network", "all") // select_network disabled the others
	if _, err := m.cmd("save_config"); err != nil {
		return fmt.Errorf("connected, but saving failed: %v", err)
	}
	return nil
}

// Switch starts moving to an already saved network (no password needed),
// with the same fallback to the previous network if it doesn't connect.
func (m *Manager) Switch(id int, timeout time.Duration) error {
	saved, err := m.SavedNetworks()
	if err != nil {
		return err
	}
	var target *Saved
	for i := range saved {
		if saved[i].ID == id {
			target = &saved[i]
		}
	}
	if target == nil {
		return errors.New("no such saved network")
	}
	if target.Current {
		return errors.New("already connected to it")
	}
	if !m.op.TryLock() {
		return errors.New("a Wi-Fi change is already in progress")
	}
	m.setJob("connecting", target.SSID, "connecting…")
	go func() {
		defer m.op.Unlock()
		m.finish("switch", target.SSID, "connected", time.Now(), m.switchTo(*target, saved, timeout))
	}()
	return nil
}

func (m *Manager) switchTo(target Saved, saved []Saved, timeout time.Duration) error {
	prev := currentID(saved)
	rollback := func(why error) error {
		if prev >= 0 {
			m.cmd("select_network", strconv.Itoa(prev))
		}
		m.cmd("enable_network", "all")
		return why
	}
	ev, done := m.Ctrl.Subscribe()
	defer done()
	if _, err := m.cmd("select_network", strconv.Itoa(target.ID)); err != nil {
		return err
	}
	if err := m.await(ev, target.ID, timeout); err != nil {
		return rollback(err)
	}
	if err := m.prefer(target.ID, saved); err != nil {
		return rollback(err)
	}
	m.cmd("enable_network", "all")                  // select_network disabled the others
	if _, err := m.cmd("save_config"); err != nil { // the new order of preference
		return fmt.Errorf("connected, but saving failed: %v", err)
	}
	return nil
}

// Forget removes a saved network (not the current one).
func (m *Manager) Forget(id int) error {
	if !m.op.TryLock() {
		return errors.New("a Wi-Fi change is in progress")
	}
	defer m.op.Unlock()
	saved, err := m.SavedNetworks()
	if err != nil {
		return err
	}
	for _, s := range saved {
		if s.ID == id {
			if s.Current {
				return errors.New("can't forget the network the clock is using")
			}
			if _, err := m.cmd("remove_network", strconv.Itoa(id)); err != nil {
				return err
			}
			_, err := m.cmd("save_config")
			m.log().Info("Wi-Fi: saved network forgotten", "ssid", s.SSID, "saved", err == nil)
			return err
		}
	}
	return errors.New("no such network")
}
