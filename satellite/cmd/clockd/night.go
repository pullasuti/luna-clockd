package main

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	esphome "github.com/ygelfand/go-esphome-device"

	"lenovo-satellite/internal/config"
	"lenovo-satellite/internal/hw"
)

// Night mode: a daily period (start..end, local time) during which the
// display is dimmed or switched off and the night light can come on.
//
// The night light is only driven on changes of what the schedule wants
// (night starts/ends, the room gets dark/light), so a manual change from the
// web UI or Home Assistant sticks until the next such change. The "Night mode"
// switch in Home Assistant overrides the schedule until the next start/end.
type nightCtl struct {
	d   *daemon
	sw  *esphome.Switch
	sun *sunInfo

	mu           sync.Mutex
	active       bool
	dark         bool
	override     *bool // forced on/off from HA / web
	overrideBase bool  // the scheduled state when the override was set
	lastDesired  int   // last night-light level we applied (-1 = none yet)
	fadeStop     chan struct{}
	sunUp        bool // a sunrise light (alarms.go) owns the night light
}

func newNightCtl(d *daemon) *nightCtl { return &nightCtl{d: d, lastDesired: -1} }

func (n *nightCtl) entity() *esphome.Switch {
	n.sw = &esphome.Switch{Base: esphome.Base{ObjectID: "night_mode", Name: "Night mode", Icon: "mdi:weather-night"}}
	n.sw.OnCommand = func(v bool) { n.setOverride(v) }
	return n.sw
}

// ParseClock parses "HH:MM" into minutes after midnight.
func parseClock(s string) (int, error) {
	h, m, ok := strings.Cut(strings.TrimSpace(s), ":")
	if !ok {
		return 0, fmt.Errorf("time %q: use HH:MM", s)
	}
	hh, err1 := strconv.Atoi(h)
	mm, err2 := strconv.Atoi(m)
	if err1 != nil || err2 != nil || hh < 0 || hh > 23 || mm < 0 || mm > 59 {
		return 0, fmt.Errorf("time %q: use HH:MM", s)
	}
	return hh*60 + mm, nil
}

// inNight reports whether minute-of-day now falls in [start, end), wrapping
// over midnight when start > end. start == end means never.
func inNight(now, start, end int) bool {
	if start == end {
		return false
	}
	if start < end {
		return now >= start && now < end
	}
	return now >= start || now < end
}

func (n *nightCtl) scheduled(c config.Night) bool {
	if !c.Enabled {
		return false
	}
	if c.Schedule == "sun" && n.sun != nil {
		if set, rise, ok := n.sun.times(); ok {
			return sunNight(time.Now(), set, rise,
				time.Duration(c.SunsetOffset)*time.Minute, time.Duration(c.SunriseOffset)*time.Minute)
		}
		// no sun data from Home Assistant yet: fixed times below
	}
	s, err1 := parseClock(c.Start)
	e, err2 := parseClock(c.End)
	if err1 != nil || err2 != nil {
		return false
	}
	t := n.d.face.tk.Now()
	return inNight(t.Hour()*60+t.Minute(), s, e)
}

func (n *nightCtl) setOverride(v bool) {
	sched := n.scheduled(n.d.cfg.Get().Night)
	n.mu.Lock()
	n.override, n.overrideBase = &v, sched
	n.mu.Unlock()
	n.d.log.Info("night mode set by hand", "on", v)
	n.tick() // apply at once
}

// State reports whether night mode is active and whether the room is dark.
func (n *nightCtl) State() (active, dark bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.active, n.dark
}

func (n *nightCtl) run(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		n.tick()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (n *nightCtl) tick() {
	c := n.d.cfg.Get().Night
	sched := n.scheduled(c)

	level, haveLight := 0.0, false
	if n.d.amb != nil {
		level, haveLight = n.d.amb.Level()
	}
	pct := level * 100

	n.mu.Lock()
	if n.override != nil && sched != n.overrideBase {
		n.override = nil // the schedule moved on (next start/end): back to the schedule
	}
	active := sched
	if n.override != nil {
		active = *n.override
	}
	boundary := active != n.active
	n.active = active
	// darkness with hysteresis: dark below DarkLevel %, light again above DarkLevel+5 %
	switch {
	case !haveLight:
		n.dark = false
	case pct < float64(c.DarkLevel):
		n.dark = true
	case pct > float64(c.DarkLevel+5):
		n.dark = false
	}
	dark := n.dark

	// desired: the level the schedule wants, -1 = leave the light alone. A
	// night that controls the light takes it over and remembers the level it
	// had (the day level, e.g. bright by hand); when the night ends the light
	// goes back to that level, not off.
	desired := -1
	st := config.LoadState(n.d.statePath)
	takes := active && (c.NightLight == "on" || c.NightLight == "dark")
	switch {
	case takes:
		desired = 0
		if c.NightLight == "on" || dark {
			desired = c.NightLightLevel
		}
	case st.NightLightHeld:
		desired = st.NightLightDay // the night is over: give the light back
	}
	changed := desired >= 0 && desired != n.lastDesired && !n.sunUp // the sunrise light ends by itself
	first := n.lastDesired < 0
	if changed {
		n.lastDesired = desired
	}
	n.mu.Unlock()
	if changed && takes && !st.NightLightHeld {
		n.hold(st.NightLight)
	}
	if changed && !takes && st.NightLightHeld {
		n.release()
	}

	if boundary {
		n.d.log.Info("night mode", "active", active)
		if n.sw != nil {
			n.sw.Set(active)
		}
	}
	if changed {
		dur := 2 * time.Second // the room got dark / light
		switch {
		case first:
			dur = 0
		case boundary:
			dur = time.Duration(c.FadeMinutes) * time.Minute
		}
		n.fadeNightLight(desired, dur)
	}
}

// hold records the day level when the night takes the light over.
func (n *nightCtl) hold(day int) {
	if err := config.UpdateState(n.d.statePath, func(s *config.State) { s.NightLightDay, s.NightLightHeld = day, true }); err != nil {
		n.d.log.Warn("save night light", "err", err)
	}
	n.d.log.Info("night light: night takes it over", "day_level", day)
}

// release: the night gave the light back.
func (n *nightCtl) release() {
	if err := config.UpdateState(n.d.statePath, func(s *config.State) { s.NightLightDay, s.NightLightHeld = 0, false }); err != nil {
		n.d.log.Warn("save night light", "err", err)
	}
	n.d.log.Info("night light: back to the day level")
}

// fadeNightLight moves the night light to target over dur (perceptually even).
func (n *nightCtl) fadeNightLight(target int, dur time.Duration) {
	n.fade(target, dur, true)
}

// sunrise brings the night light up to full over dur (before an alarm). It
// isn't saved: a restart meanwhile comes back to the scheduled level.
func (n *nightCtl) sunrise(dur time.Duration) {
	n.mu.Lock()
	n.sunUp = true
	n.mu.Unlock()
	n.fade(255, dur, false)
}

func (n *nightCtl) sunriseOn() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.sunUp
}

// endSunrise hands the night light back to the schedule.
func (n *nightCtl) endSunrise(dur time.Duration) {
	n.mu.Lock()
	n.sunUp = false // the next tick applies a schedule change made meanwhile
	n.mu.Unlock()
	lvl := config.LoadState(n.d.statePath).NightLight // what was there before
	n.fade(lvl, dur, false)
}

func (n *nightCtl) fade(target int, dur time.Duration, save bool) {
	n.mu.Lock()
	if n.fadeStop != nil {
		close(n.fadeStop)
	}
	stop := make(chan struct{})
	n.fadeStop = stop
	n.mu.Unlock()

	if save {
		n.d.saveNightLight(target)
	}
	from := n.d.nightLevel()
	if dur <= 0 || from == target {
		n.d.setNightLight(target)
		return
	}
	go func() {
		const step = 100 * time.Millisecond
		steps := int(dur / step)
		a, b := math.Sqrt(float64(from)), math.Sqrt(float64(target))
		t := time.NewTicker(step)
		defer t.Stop()
		for i := 1; i <= steps; i++ {
			select {
			case <-stop:
				return
			case <-t.C:
			}
			x := a + (b-a)*float64(i)/float64(steps)
			n.d.setNightLight(int(math.Round(x * x)))
		}
	}()
}

// cancelFade stops a running fade (a manual change wins).
func (n *nightCtl) cancelFade() {
	n.mu.Lock()
	if n.fadeStop != nil {
		close(n.fadeStop)
		n.fadeStop = nil
	}
	n.sunUp = false // a change by hand also ends a sunrise light
	n.mu.Unlock()
}

// ---- night light (shared by HA, web UI and the schedule) ----

func (d *daemon) nightLevel() int {
	if l := d.hw.nightLight; l != nil {
		if s := l.Get(); s.On {
			return max(1, int(s.Brightness*255+0.5))
		}
	}
	return 0
}

func (d *daemon) setNightLight(level int) {
	level = max(0, min(level, 255))
	if err := hw.SetNightLight(level); err != nil {
		d.log.Warn("night light", "err", err)
	}
	if l := d.hw.nightLight; l != nil {
		l.Set(esphome.LightState{On: level > 0, Brightness: max(float32(level), 1) / 255, ColorMode: esphome.ColorModeBrightness})
	}
}

// setNightLightByHand is a user change (web UI / HA): it stops any fade and
// sticks until the schedule next changes its mind.
func (d *daemon) setNightLightByHand(level int) {
	if d.night != nil {
		d.night.cancelFade()
	}
	d.setNightLight(level)
	d.saveNightLight(level)
}

// saveNightLight remembers the intended level across restarts.
func (d *daemon) saveNightLight(level int) {
	level = max(0, min(level, 255))
	if err := config.UpdateState(d.statePath, func(s *config.State) { s.NightLight = level }); err != nil {
		d.log.Warn("save night light", "err", err)
	}
}
