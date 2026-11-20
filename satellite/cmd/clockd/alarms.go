package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	esphome "github.com/ygelfand/go-esphome-device"

	"lenovo-satellite/internal/alarms"
	"lenovo-satellite/internal/output"
)

// The clock's own alarms (internal/alarms has the list and the times). They
// work without Home Assistant once the clock knows the time: the clock has no
// battery clock (aml_vrtc starts at 2015 after a power cut), so nothing rings
// before the first time sync.
//
// While an alarm rings, the alarm button (or "stop") stops it and any other
// button snoozes it (Voice.SnoozeMinutes). Timers from Home Assistant ring the
// same way but have no snooze.
//
// Sunrise: an alarm with Sunrise set brings the night light up over the
// sunriseLead before it rings, when the room is dark. The light stays on after
// the alarm until a short alarm press (or sunriseKeep).

const (
	alarmLate   = 2 * time.Minute  // an alarm found later than this (clock off, time jump) is skipped
	alarmRamp   = 30 * time.Second // volume ramp at the start
	timeKnownY  = 2025
	sunriseLead = 10 * time.Minute
	sunriseKeep = 30 * time.Minute // the sunrise light turns itself off this long after the alarm
)

// snoozeFor is the snooze length setting.
func (d *daemon) snoozeFor() time.Duration {
	m := d.cfg.Get().Voice.SnoozeMinutes
	if m <= 0 {
		m = 10
	}
	return time.Duration(m) * time.Minute
}

type alarmCtl struct {
	d    *daemon
	path string

	mu       sync.Mutex
	list     alarms.List
	armed    time.Time // alarm times up to here have been handled
	snoozeAt time.Time
	snoozeOf alarms.Alarm
	lastNext string    // last next-alarm value published
	sunFor   time.Time // the alarm time a sunrise light is up for (zero = none)
}

func newAlarmCtl(d *daemon, path string) *alarmCtl {
	c := &alarmCtl{d: d, path: path}
	l, err := alarms.Load(path)
	if err != nil {
		d.log.Error("alarms file unreadable: starting without alarms (kept as .corrupt)", "err", err)
		_ = os.Rename(path, path+".corrupt")
		l = alarms.List{}
	}
	c.list = l
	return c
}

func timeKnown(t time.Time) bool { return t.Year() >= timeKnownY }

func alarmSoundNames() map[string]bool {
	m := map[string]bool{}
	for k := range alarmSounds {
		m[k] = true
	}
	return m
}

// List is a copy of the alarms.
func (c *alarmCtl) List() alarms.List {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append(alarms.List{}, c.list...)
}

// Set replaces the alarms (web UI, Home Assistant).
func (c *alarmCtl) Set(l alarms.List) error {
	l, err := l.Clean(alarmSoundNames())
	if err != nil {
		return err
	}
	if err := alarms.Save(c.path, l); err != nil {
		return err
	}
	c.mu.Lock()
	c.list = l
	if !c.snoozeAt.IsZero() && !c.has(c.snoozeOf.ID) {
		c.snoozeAt = time.Time{} // its alarm was deleted
	}
	c.mu.Unlock()
	c.d.log.Info("alarms saved", "count", len(l))
	c.publish(time.Now())
	return nil
}

func (c *alarmCtl) has(id string) bool {
	for _, a := range c.list {
		if a.ID == id {
			return true
		}
	}
	return false
}

// Next is the next ring (a snooze counts), for the face, web UI and HA.
func (c *alarmCtl) Next(now time.Time) (time.Time, alarms.Alarm, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	at, a, ok := c.list.Next(now)
	if !c.snoozeAt.IsZero() && (!ok || c.snoozeAt.Before(at)) {
		return c.snoozeAt, c.snoozeOf, true
	}
	return at, a, ok
}

func (c *alarmCtl) snoozed() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.snoozeAt
}

func (c *alarmCtl) run(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	c.publish(time.Now())
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			c.tick(now)
		}
	}
}

// tick rings what is due since the last tick.
func (c *alarmCtl) tick(now time.Time) {
	c.sunrise(now)
	if due := c.due(now); due != nil {
		c.d.log.Info("alarm ringing", "time", due.Time, "label", due.Label)
		c.d.ring(due.Sound, due.ID)
	}
	c.publish(now)
}

// due is the alarm (or snooze) to ring now, if any; it moves the handled
// mark on and turns off one-time alarms that have had their moment.
func (c *alarmCtl) due(now time.Time) *alarms.Alarm {
	if !timeKnown(now) {
		return nil
	}
	c.mu.Lock()
	if !timeKnown(c.armed) {
		c.armed = now
	}
	var due *alarms.Alarm
	changed := false
	for i, a := range c.list {
		n := a.Next(c.armed)
		if n.IsZero() || now.Before(n) {
			continue
		}
		if now.Sub(n) < alarmLate && due == nil {
			a := a
			due = &a
		} else {
			c.d.log.Warn("alarm skipped: the clock was off or its time jumped", "alarm", a.Time, "due", n.Format(time.DateTime))
		}
		if !a.Repeats() {
			c.list[i].On = false // a one-time alarm has had its moment
			changed = true
		}
	}
	if !c.snoozeAt.IsZero() && !now.Before(c.snoozeAt) {
		if due == nil && now.Sub(c.snoozeAt) < alarmLate {
			a := c.snoozeOf
			due = &a
		}
		c.snoozeAt = time.Time{}
	}
	if now.After(c.armed) {
		c.armed = now // never back: an hour repeated by DST does not ring twice
	}
	l := c.list
	c.mu.Unlock()
	if changed {
		if err := alarms.Save(c.path, l); err != nil {
			c.d.log.Error("alarms not saved", "err", err)
		}
	}
	return due
}

// snooze: the ringing alarm rings again in snoozeFor.
func (c *alarmCtl) snooze(a alarms.Alarm) {
	at := time.Now().Add(c.d.snoozeFor())
	c.mu.Lock()
	c.snoozeAt, c.snoozeOf = at, a
	c.mu.Unlock()
	c.d.log.Info("alarm snoozed", "until", at.Format("15:04"))
	c.publish(time.Now())
}

func (c *alarmCtl) clearSnooze() {
	c.mu.Lock()
	c.snoozeAt = time.Time{}
	c.mu.Unlock()
	c.publish(time.Now())
}

// publish updates the face icon and Home Assistant when the next alarm changes.
func (c *alarmCtl) publish(now time.Time) {
	at, _, ok := c.Next(now)
	v := ""
	if ok && timeKnown(now) {
		v = at.Format(time.RFC3339)
	}
	c.mu.Lock()
	same := v == c.lastNext
	c.lastNext = v
	anyOn := c.list.AnyOn() || !c.snoozeAt.IsZero()
	c.mu.Unlock()
	if c.d.face != nil {
		c.d.face.setAlarmSet(anyOn)
	}
	if !same && c.d.hw.nextAlarm != nil {
		c.d.hw.nextAlarm.Set(v)
	}
}

// byID finds an alarm (for snoozing the one that rings).
func (c *alarmCtl) byID(id string) (alarms.Alarm, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, a := range c.list {
		if a.ID == id {
			return a, true
		}
	}
	if c.snoozeOf.ID == id {
		return c.snoozeOf, true
	}
	return alarms.Alarm{}, false
}

// sunrise starts the night light ramp before an alarm with Sunrise set (room
// dark), and ends it sunriseKeep after the alarm or when its alarm is gone.
func (c *alarmCtl) sunrise(now time.Time) {
	n := c.d.night
	if n == nil || !timeKnown(now) {
		return
	}
	c.mu.Lock()
	var next time.Time
	for _, a := range c.list {
		if !a.Sunrise {
			continue
		}
		if t := a.Next(now); !t.IsZero() && (next.IsZero() || t.Before(next)) {
			next = t
		}
	}
	sun := c.sunFor
	c.mu.Unlock()
	if !sun.IsZero() {
		if !n.sunriseOn() { // the night light was changed by hand: that wins
			c.mu.Lock()
			c.sunFor = time.Time{}
			c.mu.Unlock()
			return
		}
		gone := now.Before(sun) && !next.Equal(sun) // its alarm was turned off or changed
		if gone || now.Sub(sun) > sunriseKeep {
			c.endSunrise(gone)
		}
		return
	}
	if next.IsZero() || next.Sub(now) > sunriseLead {
		return
	}
	if _, dark := n.State(); !dark {
		return
	}
	c.mu.Lock()
	c.sunFor = next
	c.mu.Unlock()
	c.d.log.Info("sunrise light", "alarm", next.Format("15:04"))
	n.sunrise(next.Sub(now))
}

// endSunrise turns the sunrise light off (back to what the night schedule
// wants). Reports whether one was on.
func (c *alarmCtl) endSunrise(quick bool) bool {
	c.mu.Lock()
	on := !c.sunFor.IsZero()
	c.sunFor = time.Time{}
	c.mu.Unlock()
	if on && c.d.night != nil {
		c.d.log.Info("sunrise light off")
		dur := 3 * time.Second
		if quick {
			dur = time.Second
		}
		c.d.night.endSunrise(dur)
	}
	return on
}

// sunriseLit: a sunrise light is up and its alarm has rung (a short alarm
// press then dismisses the light).
func (c *alarmCtl) sunriseLit(now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.sunFor.IsZero() && !now.Before(c.sunFor)
}

// ---- ringing (timers and alarms) ----

// ring starts the alarm sound (5 minutes at most). alarmID is "" for a timer.
func (d *daemon) ring(sound, alarmID string) {
	if sound == "" {
		sound = d.cfg.Get().Voice.AlarmSound
	}
	d.mu.Lock()
	if d.alarm != nil {
		d.mu.Unlock()
		return // already ringing
	}
	pcm := alarmTone(sound)
	var src output.Source = output.NewSamples(pcm, alarmRepeats(pcm))
	if alarmID != "" {
		src = &rampSource{src: src, total: int(alarmRamp.Seconds() * output.Rate * output.Channels)}
	}
	d.alarm = d.out.Play(src, 1, "alert")
	d.alarmID = alarmID
	v := d.alarm
	d.mu.Unlock()
	d.face.setRinging(true)
	d.face.wake(5 * time.Minute)
	d.publishRinging(true)
	go func() {
		<-v.Done()
		d.mu.Lock()
		if d.alarm == v {
			d.alarm, d.alarmID = nil, ""
		}
		d.mu.Unlock()
		d.face.setRinging(false)
		d.publishRinging(false)
		d.updateLEDs()
	}()
	d.updateLEDs()
}

func (d *daemon) publishRinging(on bool) {
	if d.hw.alarmRinging != nil {
		d.hw.alarmRinging.Set(on)
	}
}

// ringingAlarm is the alarm ringing now ("" for none or a timer).
func (d *daemon) ringingAlarm() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.alarm == nil {
		return ""
	}
	return d.alarmID
}

// snoozeAlarm silences a ringing alarm for the snooze length.
func (d *daemon) snoozeAlarm() bool {
	id := d.ringingAlarm()
	if id == "" {
		return false
	}
	a, ok := d.alarms.byID(id)
	d.stopRinging()
	if ok {
		d.alarms.snooze(a)
		d.face.flash(fmt.Sprintf("%2d", int(d.snoozeFor().Minutes())), 2*time.Second)
	}
	return true
}

// rampSource raises the volume from a quarter to full over its first total samples.
type rampSource struct {
	src      output.Source
	n, total int
}

func (r *rampSource) Read(dst []float32) (int, bool) {
	n, ok := r.src.Read(dst)
	for i := 0; i < n && r.n < r.total; i++ {
		dst[i] *= 0.25 + 0.75*float32(r.n)/float32(r.total)
		r.n++
	}
	return n, ok
}

// ---- web UI ----

func (b *webBackend) Alarms() any {
	c := b.d.alarms
	now := time.Now()
	at, a, ok := c.Next(now)
	next := map[string]any{}
	if ok {
		next = map[string]any{"at": at.Format(time.RFC3339), "id": a.ID, "label": a.Label}
	}
	sn := ""
	if s := c.snoozed(); !s.IsZero() {
		sn = s.Format(time.RFC3339)
	}
	return map[string]any{
		"alarms": c.List(), "next": next, "snoozed_until": sn, "ringing": b.d.ringingAlarm() != "",
		"time_known": timeKnown(now), "snooze_minutes": int(b.d.snoozeFor().Minutes()), "max": alarms.Max,
	}
}

func (b *webBackend) SetAlarms(raw json.RawMessage) error {
	var l alarms.List
	if err := json.Unmarshal(raw, &l); err != nil {
		return errors.New("alarms: " + err.Error())
	}
	return b.d.alarms.Set(l)
}

// ---- Home Assistant actions ----
//
// esphome.<clock>_set_alarm, _delete_alarm, _switch_alarm and _list_alarms.
// HA has no alarm ids, so an alarm is named by its label, or by its time when
// it has none. HA asks for every argument; "" picks the default.

var dayNames = map[string]int{"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6}

// parseDays reads "once", "daily", "weekdays", "weekends" or a list of day
// names ("mon,wed,fri"; the first three letters count).
func parseDays(s string) ([]int, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	switch s {
	case "", "once":
		return nil, nil
	case "daily", "every day", "everyday", "all":
		return []int{0, 1, 2, 3, 4, 5, 6}, nil
	case "weekdays":
		return []int{1, 2, 3, 4, 5}, nil
	case "weekends":
		return []int{0, 6}, nil
	}
	var out []int
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' }) {
		d, ok := dayNames[f[:min(3, len(f))]]
		if !ok {
			return nil, fmt.Errorf("days %q: use once, daily, weekdays, weekends or day names like mon,wed", s)
		}
		out = append(out, d)
	}
	return out, nil
}

// findAlarm: the alarm with this label (any case) or, failing that, this time.
func findAlarm(l alarms.List, name string) int {
	name = strings.TrimSpace(name)
	for i, a := range l {
		if a.Label != "" && strings.EqualFold(a.Label, name) {
			return i
		}
	}
	if h, m, err := (alarms.Alarm{Time: name}).HM(); err == nil {
		want := fmt.Sprintf("%02d:%02d", h, m)
		for i, a := range l {
			if a.Time == want {
				return i
			}
		}
	}
	return -1
}

// setAlarm adds an alarm, or changes the one with the same label (or, with no
// label, the same time). A changed alarm keeps its sound and sunrise setting.
func setAlarm(l alarms.List, tm, days, label string) (alarms.List, error) {
	ds, err := parseDays(days)
	if err != nil {
		return nil, err
	}
	a := alarms.Alarm{Time: strings.TrimSpace(tm), Days: ds, On: true, Label: strings.TrimSpace(label)}
	if _, _, err := a.HM(); err != nil {
		return nil, err
	}
	key := a.Label
	if key == "" {
		key = a.Time
	}
	l = append(alarms.List{}, l...)
	if i := findAlarm(l, key); i >= 0 && (a.Label != "" || l[i].Label == "") {
		a.ID, a.Sound, a.Sunrise = l[i].ID, l[i].Sound, l[i].Sunrise
		l[i] = a
		return l, nil
	}
	return append(l, a), nil
}

func (d *daemon) alarmActions() []*esphome.Action {
	c := d.alarms
	str := func(n string) esphome.Arg { return esphome.Arg{Name: n, Type: esphome.ArgString} }
	return []*esphome.Action{{
		Name: "set_alarm",
		Args: []esphome.Arg{str("time"), str("days"), str("label")},
		Run: func(call esphome.Call) (any, error) {
			l, err := setAlarm(c.List(), call.String("time"), call.String("days"), call.String("label"))
			if err == nil {
				err = c.Set(l)
			}
			if err != nil {
				d.log.Warn("home assistant: alarm not set", "err", err)
				return nil, err
			}
			d.log.Info("home assistant: alarm set", "time", call.String("time"), "days", call.String("days"))
			return nil, nil
		},
	}, {
		Name: "delete_alarm",
		Args: []esphome.Arg{str("alarm")},
		Run: func(call esphome.Call) (any, error) {
			l := c.List()
			i := findAlarm(l, call.String("alarm"))
			if i < 0 {
				return nil, fmt.Errorf("no alarm %q (give its label or HH:MM)", call.String("alarm"))
			}
			d.log.Info("home assistant: alarm deleted", "time", l[i].Time)
			return nil, c.Set(append(l[:i], l[i+1:]...))
		},
	}, {
		Name: "switch_alarm",
		Args: []esphome.Arg{str("alarm"), {Name: "on", Type: esphome.ArgBool}},
		Run: func(call esphome.Call) (any, error) {
			l := c.List()
			i := findAlarm(l, call.String("alarm"))
			if i < 0 {
				return nil, fmt.Errorf("no alarm %q (give its label or HH:MM)", call.String("alarm"))
			}
			l[i].On = call.Bool("on")
			d.log.Info("home assistant: alarm switched", "time", l[i].Time, "on", l[i].On)
			return nil, c.Set(l)
		},
	}, {
		Name:    "list_alarms",
		Answers: true,
		Run: func(esphome.Call) (any, error) {
			at, a, ok := c.Next(time.Now())
			next := map[string]any{}
			if ok {
				next = map[string]any{"at": at.Format(time.RFC3339), "label": a.Label, "time": a.Time}
			}
			return map[string]any{"alarms": c.List(), "next": next}, nil
		},
	}}
}
