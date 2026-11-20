package main

import (
	"context"
	"math"
	"sync"
	"time"

	esphome "github.com/ygelfand/go-esphome-device"

	"lenovo-satellite/internal/hw"
)

// ambient reads the light sensor once a second and keeps a smoothed value.
// It feeds the "Ambient light" sensor in Home Assistant and auto brightness.
type ambient struct {
	d      *daemon
	sensor *esphome.Sensor // 0-100 %, log scale (uncalibrated, behind the window)

	mu     sync.Mutex
	raw    float64 // smoothed raw count
	prox   float32 // last raw proximity
	proxOK bool
	ok     bool
	level  float64 // 0..1 (hw.LightLevel of raw)
}

func (a *ambient) entity() *esphome.Sensor {
	a.sensor = &esphome.Sensor{
		Base:       esphome.Base{ObjectID: "ambient_light", Name: "Ambient light", Icon: "mdi:brightness-5"},
		Unit:       "%",
		StateClass: esphome.StateClassMeasurement,
	}
	return a.sensor
}

// Proximity reports the last raw proximity reading (uncalibrated).
func (a *ambient) Proximity() (float32, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.prox, a.proxOK
}

// Level returns the smoothed light level 0..1 and whether the sensor works.
func (a *ambient) Level() (float64, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.level, a.ok
}

// Raw returns the smoothed raw count.
func (a *ambient) Raw() float64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.raw
}

func (a *ambient) run(ctx context.Context) {
	als, err := hw.OpenAmbientLight()
	if err != nil {
		a.d.log.Warn("light sensor", "err", err)
		return
	}
	t := time.NewTicker(250 * time.Millisecond)
	defer t.Stop()
	published := -1.0
	base := -1.0 // proximity idle level
	for n := 0; ; n++ {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		// proximity 4x a second: idles ~5, a hand a few cm away ~80-100,
		// covering it 300+. Wake on a rise of 20 over the slowly tracked idle level.
		if p, err := als.Proximity(); err == nil {
			fp := float64(p)
			a.mu.Lock()
			a.prox, a.proxOK = float32(p), true
			a.mu.Unlock()
			if base < 0 {
				base = fp
			}
			if fp > base+20 {
				if a.d.cfg.Get().Night.ProximityWake {
					a.d.face.wake(10 * time.Second)
				}
			} else {
				base += 0.05 * (fp - base) // follow drift only while nothing is close
			}
			a.d.shushTick(float32(fp), float32(base))
		}
		if n%4 != 0 {
			continue
		}
		v, err := als.Raw()
		if err != nil {
			continue
		}
		a.mu.Lock()
		if !a.ok {
			a.raw = float64(v)
		} else {
			// fast when it gets much brighter/darker (lights switched), slow otherwise
			alpha := 0.15
			if f := (float64(v) + 1) / (a.raw + 1); f > 2 || f < 0.5 {
				alpha = 0.6
			}
			a.raw += alpha * (float64(v) - a.raw)
		}
		a.ok = true
		a.level = hw.LightLevel(a.raw)
		pct := math.Round(a.level * 100)
		a.mu.Unlock()
		if a.sensor != nil && pct != published {
			a.sensor.Set(float32(pct))
			published = pct
		}
	}
}

// autoBrightness maps the light level to the display range [lo, hi] (1..16).
func autoBrightness(level float64, lo, hi int) float64 {
	if hi < lo {
		lo, hi = hi, lo
	}
	return float64(lo) + level*float64(hi-lo)
}
