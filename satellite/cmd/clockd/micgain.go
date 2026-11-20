package main

import (
	"math"
	"sync"
	"time"
)

// micGain applies the configured digital microphone gain (the PDM input has
// no hardware gain control) and measures the level after it, for the web UI
// meter.
type micGain struct {
	mu      sync.Mutex
	db      float64
	factor  float32
	peak    float64 // max |sample| in the current window, 0..1
	sumSq   float64
	n       int
	clipped int // samples clipped in the current window
	last    micLevel
	start   time.Time
}

type micLevel struct {
	PeakDB  float64 `json:"peak_db"` // dBFS
	RMSDB   float64 `json:"rms_db"`  // dBFS
	Clipped int     `json:"clipped"` // samples clipped in the window
	GainDB  float64 `json:"gain_db"`
	At      time.Time
}

const meterWindow = 150 * time.Millisecond

func dbfs(x float64) float64 {
	if x < 1e-5 {
		return -100
	}
	return math.Round(20*math.Log10(x)*10) / 10
}

// apply scales s in place (saturating) and updates the meter.
func (g *micGain) apply(s []int16, gainDB float64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if gainDB != g.db || g.factor == 0 {
		g.db = gainDB
		g.factor = float32(math.Pow(10, gainDB/20))
	}
	f := g.factor
	for i, v := range s {
		x := float32(v)
		if f != 1 {
			x *= f
			if x > 32767 {
				x = 32767
				g.clipped++
			} else if x < -32768 {
				x = -32768
				g.clipped++
			}
			s[i] = int16(x)
		} else if v == 32767 || v == -32768 {
			g.clipped++
		}
		a := math.Abs(float64(x)) / 32768
		if a > g.peak {
			g.peak = a
		}
		g.sumSq += a * a
	}
	g.n += len(s)
	now := time.Now()
	if g.start.IsZero() {
		g.start = now
	}
	if now.Sub(g.start) >= meterWindow && g.n > 0 {
		g.last = micLevel{PeakDB: dbfs(g.peak), RMSDB: dbfs(math.Sqrt(g.sumSq / float64(g.n))),
			Clipped: g.clipped, GainDB: g.db, At: now}
		g.peak, g.sumSq, g.n, g.clipped, g.start = 0, 0, 0, 0, now
	}
}

// level is the last full window (zero/stale when the mic is muted).
func (g *micGain) level() micLevel {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.last
}
