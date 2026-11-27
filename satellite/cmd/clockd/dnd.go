package main

import (
	"context"
	"sync"
	"time"

	esphome "github.com/ygelfand/go-esphome-device"

	"lenovo-satellite/internal/config"
)

// Do not disturb: silences what the clock sounds on its own: the hourly
// chime, the wake sound, Home Assistant's announcements (reported to HA as
// finished, so it doesn't wait) and the startup tune. Never alarms, timers,
// answers to a turn the user started, or anything the user plays or presses.
// On by a schedule (Config.DND: none, night mode, own times) or by hand (the
// HA switch, the web UI); a hand setting holds until the schedule next
// changes, as night mode's does.
type dndCtl struct {
	d  *daemon
	sw *esphome.Switch

	mu           sync.Mutex
	active       bool
	override     *bool
	overrideBase bool // the scheduled state when the override was set
}

func newDNDCtl(d *daemon) *dndCtl { return &dndCtl{d: d} }

func (c *dndCtl) entity() *esphome.Switch {
	c.sw = &esphome.Switch{Base: esphome.Base{ObjectID: "do_not_disturb", Name: "Do not disturb", Icon: "mdi:bell-sleep"}}
	c.sw.OnCommand = func(v bool) { c.set(v) }
	return c.sw
}

// scheduled reports whether the schedule wants do not disturb at t.
func (c *dndCtl) scheduled(cfg config.DND, t time.Time) bool {
	switch cfg.Schedule {
	case "night":
		if c.d.night == nil {
			return false
		}
		on, _ := c.d.night.State()
		return on
	case "times":
		s, err1 := parseClock(cfg.Start)
		e, err2 := parseClock(cfg.End)
		return err1 == nil && err2 == nil && inNight(t.Hour()*60+t.Minute(), s, e)
	}
	return false
}

// set turns do not disturb on or off by hand (until the schedule next changes).
func (c *dndCtl) set(v bool) {
	sched := c.scheduled(c.d.cfg.Get().DND, time.Now())
	c.mu.Lock()
	c.override, c.overrideBase = &v, sched
	c.mu.Unlock()
	c.d.log.Info("do not disturb set by hand", "on", v)
	c.On()
}

// On reports whether do not disturb is on now (and keeps the HA switch and
// the log up to date).
func (c *dndCtl) On() bool {
	if c == nil {
		return false
	}
	sched := c.scheduled(c.d.cfg.Get().DND, time.Now())
	c.mu.Lock()
	if c.override != nil && sched != c.overrideBase {
		c.override = nil // the schedule moved on: back to it
	}
	on := sched
	if c.override != nil {
		on = *c.override
	}
	changed := on != c.active
	c.active = on
	byHand := c.override != nil
	c.mu.Unlock()
	if changed {
		c.d.log.Info("do not disturb", "on", on, "by_hand", byHand)
	}
	if c.sw != nil && c.sw.Get() != on {
		c.sw.Set(on)
	}
	return on
}

// status is the web UI's view: on, and whether by hand.
func (c *dndCtl) status() (on, byHand bool) {
	on = c.On()
	c.mu.Lock()
	defer c.mu.Unlock()
	return on, c.override != nil
}

func (c *dndCtl) run(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		c.On()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
