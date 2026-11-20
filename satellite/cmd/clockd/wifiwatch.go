package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"lenovo-satellite/internal/wifi"
)

// wifiWatch follows wpa_supplicant's events (internal/wifi Ctrl): it logs
// each connect and disconnect, has our DHCP client ask for the address again
// on every new association, and wakes the mDNS advertiser. It replaces the
// loop in bin/dhcp.sh that polled the vendor wpa_cli every 3 s.
type wifiWatch struct {
	log  *slog.Logger
	wm   *wifi.Manager
	dhcp *dhcpClient
	mdns chan<- bool // true: advertise again even if the addresses are the same

	lastNet  string // bssid + ssid of the association the last renew was for
	lastSSID string
}

func (w *wifiWatch) run(ctx context.Context) {
	if w.wm.Exec != nil {
		w.poll(ctx)
		return
	}
	ev, done := w.wm.Ctrl.Subscribe()
	defer done()
	go w.wm.Ctrl.Watch(ctx.Done(), 20*time.Second)
	w.dhcp.stopOldWatcher(w.log)
	attached := false
	for {
		select {
		case <-ctx.Done():
			return
		case e := <-ev:
			switch e.Name {
			case wifi.Attached:
				// events before this are unknown (clockd or wpa_supplicant just
				// started): look at where the clock is now
				if !attached {
					w.log.Info("Wi-Fi: following wpa_supplicant's events")
				} else {
					w.log.Info("Wi-Fi: wpa_supplicant's control socket reconnected")
				}
				attached = true
				w.associated(false)
			case "CTRL-EVENT-CONNECTED":
				w.associated(true)
			case "CTRL-EVENT-DISCONNECTED":
				w.log.Info("Wi-Fi disconnected", "bssid", e.Arg("bssid"), "reason", e.Arg("reason"),
					"by_us", e.Arg("locally_generated") == "1")
			case "CTRL-EVENT-SSID-TEMP-DISABLED":
				w.log.Warn("Wi-Fi: network refused for now", "ssid", e.Arg("ssid"), "reason", e.Arg("reason"),
					"failures", e.Arg("auth_failures"), "pause_s", e.Arg("duration"))
			}
		}
	}
}

// poll is the watcher on the old wpa_cli route (use-wpa_cli), which has no
// events: it asks for the status every 3 s, as dhcp.sh's loop did.
func (w *wifiWatch) poll(ctx context.Context) {
	w.dhcp.stopOldWatcher(w.log)
	w.log.Warn("Wi-Fi: using the vendor wpa_cli (use-wpa_cli is set), asking for the status every 3 s")
	first := true
	for {
		if st := w.wm.Status(); st.State == "COMPLETED" && st.BSSID+" "+st.SSID != w.lastNet {
			w.associated(!first)
		}
		first = false
		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Second):
		}
	}
}

// associated: wpa_supplicant is (or may be) on a network. A new association
// needs a fresh answer from that network's DHCP server: same network, a quick
// renew; another one, the renew goes unanswered and udhcpc starts over.
func (w *wifiWatch) associated(event bool) {
	st := w.wm.Status()
	if st.State != "COMPLETED" {
		return
	}
	key := st.BSSID + " " + st.SSID
	if !event && key == w.lastNet {
		return
	}
	if event {
		w.log.Info("Wi-Fi connected", "ssid", st.SSID, "bssid", st.BSSID, "freq", st.Freq, "id", st.ID)
	}
	newSSID := w.lastSSID != "" && st.SSID != w.lastSSID
	w.lastNet, w.lastSSID = key, st.SSID
	if w.dhcp.renew() {
		w.log.Info("Wi-Fi: asking for the network address again", "ssid", st.SSID)
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		w.dhcp.wait(ctx)
		select {
		case w.mdns <- newSSID: // another network has not heard our announcements
		default:
		}
	}()
}

// dhcpClient is our udhcpc (bin/dhcp.sh). With the vendor dhcpcd in use
// (use-dhcpcd) there is no pid file and everything here is a no-op: dhcpcd
// follows the link itself.
type dhcpClient struct {
	pidFile string // /tmp/udhcpc.pid
	stamp   string // /tmp/dhcp-lease: dhcp.sh rewrites it at every bound/renew
	watcher string // /tmp/dhcp-watch.pid: the poll loop of dhcp.sh before v0.83
	signal  func(pid int) error
	hasAddr func() bool

	mu     sync.Mutex
	sentAt time.Time
	before time.Time // the stamp's time when the renew was sent
}

func newDHCPClient() *dhcpClient {
	return &dhcpClient{pidFile: "/tmp/udhcpc.pid", stamp: "/tmp/dhcp-lease", watcher: "/tmp/dhcp-watch.pid",
		signal:  func(pid int) error { return syscall.Kill(pid, syscall.SIGUSR1) },
		hasAddr: hasWLANAddress}
}

func pidOf(file, want string) int {
	b, err := os.ReadFile(file)
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	if pid <= 1 {
		return 0
	}
	cmd, _ := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	if !strings.Contains(strings.ReplaceAll(string(cmd), "\x00", " "), want) {
		return 0
	}
	return pid
}

func (d *dhcpClient) stampTime() time.Time {
	if fi, err := os.Stat(d.stamp); err == nil {
		return fi.ModTime()
	}
	return time.Time{}
}

// renew has udhcpc ask again (SIGUSR1). Calls within 2 s of each other are
// one renew: the event watcher and a Wi-Fi change both react to the same
// CTRL-EVENT-CONNECTED. False: nothing sent (no udhcpc, or just done).
func (d *dhcpClient) renew() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if time.Since(d.sentAt) < 2*time.Second {
		return false
	}
	pid := pidOf(d.pidFile, "udhcpc")
	if pid == 0 {
		return false
	}
	before := d.stampTime()
	if d.signal(pid) != nil {
		return false
	}
	d.sentAt, d.before = time.Now(), before
	return true
}

// wait returns once the network has answered the last renew (dhcp.sh wrote
// the stamp again) and wlan0 has an address. This is what makes a change to
// another network on the same subnet count: the address never changes there,
// the answer does. Without our udhcpc: once wlan0 has an address.
func (d *dhcpClient) wait(ctx context.Context) (string, error) {
	for {
		d.mu.Lock()
		sentAt, before := d.sentAt, d.before
		d.mu.Unlock()
		answered := sentAt.IsZero() || !d.stampTime().Equal(before)
		// a dhcp.sh from before v0.83 writes no stamp: settle for the address after 4 s
		if !answered && before.IsZero() && d.stampTime().IsZero() && time.Since(sentAt) > 4*time.Second {
			answered = true
		}
		if answered && d.hasAddr() {
			return wlanIP(), nil
		}
		select {
		case <-ctx.Done():
			return "", errors.New("no address")
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// waitIP is wifi.Manager's WaitIP: called when the link is up on the network
// a Wi-Fi change selected.
func (d *dhcpClient) waitIP(ctx context.Context) (string, error) {
	d.renew()
	return d.wait(ctx)
}

// stopOldWatcher ends the wpa_cli poll loop an older dhcp.sh left running
// (it would only go away with the next power cycle).
func (d *dhcpClient) stopOldWatcher(log *slog.Logger) {
	if pid := pidOf(d.watcher, "dhcp.sh watch"); pid != 0 {
		// its own session (setsid): the group takes the sleep or wpa_cli child along
		if syscall.Kill(-pid, syscall.SIGTERM) != nil {
			syscall.Kill(pid, syscall.SIGTERM)
		}
		log.Info("Wi-Fi: stopped the old wpa_cli poll loop", "pid", pid)
	}
	os.Remove(d.watcher)
}

func wlanIP() string {
	ifc, err := net.InterfaceByName("wlan0")
	if err != nil {
		return ""
	}
	addrs, _ := ifc.Addrs()
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil {
			return ipn.IP.String()
		}
	}
	return ""
}
