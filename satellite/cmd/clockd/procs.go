package main

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Process list for the web UI's System tab: a simple, friendly "top".

// procInfo explains the processes this clock normally runs.
var procInfo = map[string]string{
	"clockd":             "This project: voice, clock face, lights, web UI",
	"apmd":               "Microphone processing: WebRTC echo canceller, noise suppression",
	"vqed":               "Microphone processing: LocalVQE echo canceller",
	"iot_audio_capture":  "Vendor microphone processing (Chromecast audio)",
	"logwrapper":         "Logs a vendor service's output",
	"wpa_supplicant":     "Wi-Fi connection",
	"dhcpcd":             "Vendor network address client (DHCP; stopped at boot unless use-dhcpcd is set)",
	"busybox":            "Our busybox: udhcpc keeps the network address (DHCP)",
	"wpa_cli":            "Vendor Wi-Fi command tool (only used when use-wpa_cli is set)",
	"dropbearmulti":      "ssh (dropbear, keys only)",
	"net_mgr":            "Vendor network manager (checks internet connectivity; stopped at boot unless use-net_mgr is set)",
	"adbd":               "Android debug bridge (adb, port 5555)",
	"init":               "System start-up and services",
	"ueventd":            "Device manager (hardware events)",
	"logd":               "System log",
	"servicemanager":     "Vendor service registry",
	"bluetoothtbd":       "Bluetooth (vendor, idle)",
	"watchdog":           "Hardware watchdog: restarts a hung system",
	"start.sh":           "Starts clockd and restarts it if it stops",
	"wifi_display.sh":    "Vendor Wi-Fi status script",
	"mount_usb_drive.sh": "Vendor USB drive automount",
	"sleep":              "Short wait inside a script",
	"sh":                 "Shell",
	"console":            "Serial console shell (ttyS0, started by init)",
	"health_check":       "Vendor system health check (runs now and then)",
}

type procSample struct {
	ticks uint64 // utime + stime
}

type procSampler struct {
	mu        sync.Mutex
	prev      map[int]procSample
	prevTotal uint64
	prevAt    time.Time
}

// cpuTotal is the sum of all jiffies in /proc/stat's "cpu" line, and the core count.
func cpuTotal() (uint64, int) {
	b, _ := os.ReadFile("/proc/stat")
	var total uint64
	cores := 0
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.Fields(l)
		if len(f) == 0 {
			continue
		}
		if f[0] == "cpu" {
			for _, v := range f[1:] {
				n, _ := strconv.ParseUint(v, 10, 64)
				total += n
			}
		} else if strings.HasPrefix(f[0], "cpu") {
			cores++
		}
	}
	return total, max(cores, 1)
}

type procRow struct {
	PID     int     `json:"pid"`
	Name    string  `json:"name"`
	Desc    string  `json:"desc,omitempty"`
	CPU     float64 `json:"cpu"`    // % of the whole CPU (all cores)
	MemMB   float64 `json:"mem_mb"` // resident memory
	Threads int     `json:"threads"`
	Kernel  bool    `json:"kernel,omitempty"`
	Helper  bool    `json:"helper,omitempty"` // short-lived helper (sleep in a script loop)
	Cmd     string  `json:"cmd,omitempty"`
	UpS     int64   `json:"up_s"`
	ticks   uint64
}

func readProcs() []procRow {
	ents, _ := os.ReadDir("/proc")
	var btime int64
	if b, err := os.ReadFile("/proc/uptime"); err == nil {
		if f := strings.Fields(string(b)); len(f) > 0 {
			up, _ := strconv.ParseFloat(f[0], 64)
			btime = time.Now().Unix() - int64(up)
		}
	}
	const hz = 100 // USER_HZ on this kernel
	var rows []procRow
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		dir := filepath.Join("/proc", e.Name())
		st, err := os.ReadFile(filepath.Join(dir, "stat"))
		if err != nil {
			continue
		}
		s := string(st)
		lp, rp := strings.IndexByte(s, '('), strings.LastIndexByte(s, ')')
		if lp < 0 || rp < lp {
			continue
		}
		comm := s[lp+1 : rp]
		f := strings.Fields(s[rp+1:]) // f[0] = state, f[1] = ppid ...
		if len(f) < 22 {
			continue
		}
		ppid, _ := strconv.Atoi(f[1])
		ut, _ := strconv.ParseUint(f[11], 10, 64)
		stt, _ := strconv.ParseUint(f[12], 10, 64)
		threads, _ := strconv.Atoi(f[17])
		start, _ := strconv.ParseInt(f[19], 10, 64)
		rss, _ := strconv.ParseInt(f[21], 10, 64)
		cl, _ := os.ReadFile(filepath.Join(dir, "cmdline"))
		cmd := strings.TrimSpace(strings.ReplaceAll(string(cl), "\x00", " "))
		r := procRow{PID: pid, Name: comm, Threads: threads, MemMB: float64(rss*4096) / (1 << 20),
			Kernel: cmd == "" && (ppid == 2 || pid == 2), Cmd: cmd, ticks: ut + stt}
		if btime > 0 {
			r.UpS = time.Now().Unix() - (btime + start/hz)
		}
		// scripts run by a shell: name them after the script
		if f := strings.Fields(cmd); len(f) > 1 && (comm == "busybox" || comm == "sh" || strings.HasSuffix(f[0], "sh")) {
			for _, a := range f[1:] {
				if strings.HasSuffix(a, ".sh") {
					r.Name = filepath.Base(a)
					break
				}
			}
		}
		if cmd == "/sbin/busybox sh" && ppid == 1 {
			r.Name = "console" // init's "console" service
		}
		r.Helper = r.Name == "sleep" // short waits inside scripts
		if len(r.Cmd) > 160 {
			r.Cmd = r.Cmd[:160] + "…"
		}
		r.Desc = procInfo[r.Name]
		rows = append(rows, r)
	}
	return rows
}

// Processes returns the process list with CPU use since the previous call
// (or over half a second when there is no recent one).
func (p *procSampler) Processes() map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.prev == nil || time.Since(p.prevAt) > 15*time.Second {
		p.snapshot(readProcs())
		time.Sleep(500 * time.Millisecond)
	}
	rows := readProcs()
	total, cores := cpuTotal()
	dTotal := float64(total - p.prevTotal)
	interval := time.Since(p.prevAt).Seconds()
	var busy float64
	for i := range rows {
		if prev, ok := p.prev[rows[i].PID]; ok && dTotal > 0 && rows[i].ticks >= prev.ticks {
			rows[i].CPU = float64(rows[i].ticks-prev.ticks) / dTotal * 100
		}
		busy += rows[i].CPU
	}
	p.snapshot(rows)
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].CPU != rows[j].CPU {
			return rows[i].CPU > rows[j].CPU
		}
		return rows[i].MemMB > rows[j].MemMB
	})
	for i := range rows {
		rows[i].CPU = float64(int(rows[i].CPU*10+0.5)) / 10
		rows[i].MemMB = float64(int(rows[i].MemMB*10+0.5)) / 10
	}
	mi := meminfo()
	return map[string]any{
		"cores":        cores,
		"cpu_busy":     float64(int(min(busy, 100)*10+0.5)) / 10,
		"mem_total_mb": mi["MemTotal"] / 1024,
		"mem_avail_mb": mi["MemAvailable"] / 1024,
		"interval_s":   float64(int(interval*10+0.5)) / 10,
		"procs":        rows,
	}
}

func (p *procSampler) snapshot(rows []procRow) {
	p.prev = make(map[int]procSample, len(rows))
	for _, r := range rows {
		p.prev[r.PID] = procSample{ticks: r.ticks}
	}
	p.prevTotal, _ = cpuTotal()
	p.prevAt = time.Now()
}
