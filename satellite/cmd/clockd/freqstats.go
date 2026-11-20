package main

import (
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// freqStats samples cpufreq's time_in_state once a minute and reports the
// share of time at each CPU speed over a rolling window (not since boot:
// since-boot numbers carry the pre-governor "performance" era for a long
// time after the change).
type freqStats struct {
	mu   sync.Mutex
	hist []freqSample // one per minute, newest last, ~10 min
}

type freqSample struct {
	at  time.Time
	jif []uint64 // per frequency step, aligned with the file's order
}

func newFreqStats() *freqStats {
	fs := &freqStats{}
	go func() {
		for t := time.NewTicker(time.Minute); ; {
			<-t.C
			if s, ok := readTimeInState(); ok {
				fs.mu.Lock()
				fs.hist = append(fs.hist, freqSample{time.Now(), s})
				if len(fs.hist) > 11 {
					fs.hist = fs.hist[len(fs.hist)-11:]
				}
				fs.mu.Unlock()
			}
		}
	}()
	return fs
}

func readTimeInState() ([]uint64, bool) {
	b, err := os.ReadFile("/sys/devices/system/cpu/cpu0/cpufreq/stats/time_in_state")
	if err != nil {
		return nil, false
	}
	var out []uint64
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.Fields(l)
		if len(f) != 2 {
			continue
		}
		j, err := strconv.ParseUint(f[1], 10, 64)
		if err != nil {
			return nil, false
		}
		out = append(out, j)
	}
	return out, len(out) > 0
}

// shares is the % time at each speed over the rolling window (shorter while
// it fills), newest sample vs the oldest one at least half the window back.
// The frequencies themselves come from the file's first pass at start.
func (fs *freqStats) shares() []map[string]any {
	fs.mu.Lock()
	h := fs.hist
	fs.mu.Unlock()
	if len(h) < 2 {
		return nil
	}
	// pick the oldest sample within ~1.1x of the window target
	lo := h[0]
	for _, s := range h {
		if h[len(h)-1].at.Sub(s.at) >= 9*time.Minute {
			lo = s
			break
		}
	}
	hi := h[len(h)-1]
	if len(lo.jif) != len(hi.jif) || (&lo == &hi) {
		return nil
	}
	var total uint64
	d := make([]uint64, len(hi.jif))
	for i := range hi.jif {
		if hi.jif[i] > lo.jif[i] {
			d[i] = hi.jif[i] - lo.jif[i]
		}
		total += d[i]
	}
	if total == 0 {
		return nil
	}
	freqs, _ := readTimeInStateFreqs()
	out := make([]map[string]any, 0, len(d))
	for i, j := range d {
		m := map[string]any{"pct": int((100*j + total/2) / total)}
		if i < len(freqs) {
			m["mhz"] = freqs[i]
		}
		out = append(out, m)
	}
	return out
}

func readTimeInStateFreqs() ([]int, bool) {
	b, err := os.ReadFile("/sys/devices/system/cpu/cpu0/cpufreq/stats/time_in_state")
	if err != nil {
		return nil, false
	}
	var out []int
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.Fields(l)
		if len(f) != 2 {
			continue
		}
		khz, err := strconv.Atoi(f[0])
		if err != nil {
			return nil, false
		}
		out = append(out, khz/1000)
	}
	return out, len(out) > 0
}
