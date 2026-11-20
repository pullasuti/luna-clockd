package main

import (
	"net"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// netWatch restarts the clock when the Wi-Fi chip itself has died — the state
// clock #2 hit on 2026-11-04: the dhd driver logs "failed backplane access
// over SDIO, halting operation" / "chip crash", wlan0 loses its address and
// wpa_supplicant reports INTERFACE_DISABLED, and only a reboot brings the
// radio back (the driver is a module, but reloading it hung the kernel on
// other Amlogic boards, so the clock restarts instead).
//
// A network outage alone never triggers it: the restart requires BOTH no IPv4
// address on wlan0 for over 5 minutes AND a fresh (< 10 min) dhd crash line in
// the kernel log, so an unplugged router or a DHCP problem is left alone.
type netWatch struct {
	d          *daemon
	downSince  time.Time
	lastCheck  time.Time
	lastAction time.Time
}

func newNetWatch(d *daemon) *netWatch {
	return &netWatch{d: d}
}

func (n *netWatch) run(stop <-chan struct{}) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			n.tick()
		}
	}
}

func (n *netWatch) tick() {
	if n.d.setup != nil && n.d.setup.Active() { // the setup hotspot owns the radio
		n.downSince = time.Time{}
		return
	}
	if hasWLANAddress() {
		n.downSince = time.Time{}
		return
	}
	if n.downSince.IsZero() {
		n.downSince = time.Now()
		return
	}
	if time.Since(n.downSince) < 5*time.Minute || time.Since(n.lastAction) < 15*time.Minute {
		return
	}
	if !recentDHDCrash(time.Now()) {
		return
	}
	n.lastAction = time.Now()
	n.d.log.Error("Wi-Fi chip appears dead (no address and a fresh driver crash): restarting the clock",
		"down_for", time.Since(n.downSince).Round(time.Second).String())
	n.d.face.showText("network lost restarting", 3)
	go func() {
		time.Sleep(20 * time.Second) // let the line above and the face be seen
		syscall.Sync()
		if err := syscall.Reboot(syscall.LINUX_REBOOT_CMD_RESTART); err != nil {
			n.d.log.Error("restart after Wi-Fi death failed", "err", err)
		}
	}()
}

func hasWLANAddress() bool {
	ifc, err := net.InterfaceByName("wlan0")
	if err != nil || ifc.Flags&net.FlagUp == 0 {
		return false
	}
	addrs, err := ifc.Addrs()
	if err != nil {
		return false
	}
	for _, a := range addrs {
		if ip := a.(*net.IPNet).IP; ip.To4() != nil && !ip.IsLoopback() {
			return true
		}
	}
	return false
}

// recentDHDCrash looks for a dhd/Broadcom driver crash line in the kernel log
// no older than maxAge (dmesg timestamps are seconds since boot).
func recentDHDCrash(now time.Time) bool {
	out, err := exec.Command("/bin/dmesg").Output()
	if err != nil {
		if out2, err2 := exec.Command("/sbin/busybox", "dmesg").Output(); err2 == nil {
			out, err = out2, nil
		}
	}
	if err != nil {
		return false
	}
	uptime := uptimeSeconds(now)
	for _, l := range strings.Split(string(out), "\n") {
		if !strings.Contains(l, "halting operation") && !strings.Contains(l, "chip crash") {
			continue
		}
		// "[ 15303.979245@0] dhd_prot_ioctl : bus is down..."
		t := strings.TrimSpace(strings.TrimPrefix(l[:min(len(l), 32)], "["))
		if i := strings.IndexByte(t, '.'); i > 0 {
			if sec, err := strconv.ParseFloat(t[:i], 64); err == nil {
				if uptime-sec < 600 {
					return true
				}
			}
		}
	}
	return false
}

func uptimeSeconds(_ time.Time) float64 {
	b, err := exec.Command("/sbin/busybox", "cat", "/proc/uptime").Output()
	if err != nil {
		return 0
	}
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return 0
	}
	v, _ := strconv.ParseFloat(f[0], 64)
	return v
}
