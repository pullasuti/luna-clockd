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
	"lenovo-satellite/internal/rtlsdr"
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
	// the snooze length can be stepped by pressing again right after
	// snoozing (stepSnooze): when it started, how long, the last press
	snoozeFrom, snoozeTouch time.Time
	snoozeLen               time.Duration
	lastNext                string    // last next-alarm value published
	sunFor                  time.Time // the alarm time a sunrise light is up for (zero = none)
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
		c.d.ring(due.Sound, due.ID, due.Fade)
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
	now := time.Now()
	n := c.d.snoozeFor()
	c.mu.Lock()
	c.snoozeAt, c.snoozeOf = now.Add(n), a
	c.snoozeFrom, c.snoozeTouch, c.snoozeLen = now, now, n
	c.mu.Unlock()
	c.d.log.Info("alarm snoozed", "until", now.Add(n).Format("15:04"))
	c.publish(now)
}

// snoozeStepWindow: a press this soon after snoozing (or the last step)
// steps the snooze length instead of doing what the button does.
const snoozeStepWindow = 3 * time.Second

// stepSnooze: the next snooze length (5 more minutes, after 20 back to 5),
// counted from when it was snoozed. Only within snoozeStepWindow of the last
// press; reports the new length.
func (c *alarmCtl) stepSnooze(now time.Time) (time.Duration, bool) {
	c.mu.Lock()
	if c.snoozeAt.IsZero() || now.Sub(c.snoozeTouch) > snoozeStepWindow {
		c.mu.Unlock()
		return 0, false
	}
	n := c.snoozeLen.Round(5*time.Minute) + 5*time.Minute
	if n > 20*time.Minute {
		n = 5 * time.Minute
	}
	c.snoozeLen, c.snoozeTouch = n, now
	c.snoozeAt = c.snoozeFrom.Add(n)
	at := c.snoozeAt
	c.mu.Unlock()
	c.d.log.Info("snooze length stepped", "minutes", int(n.Minutes()), "until", at.Format("15:04"))
	c.publish(now)
	return n, true
}

// skip: skip (or no longer skip) the next ring of an alarm.
func (c *alarmCtl) skip(id string, on bool) error {
	l := c.List()
	now := time.Now()
	for i := range l {
		if l[i].ID == id {
			l[i] = l[i].SkipNext(now, on)
			c.d.log.Info("alarm skip", "time", l[i].Time, "skip", l[i].Skip)
			return c.Set(l)
		}
	}
	return fmt.Errorf("no alarm %q", id)
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
// An FM sound ("fm:91.2") plays the radio; without a working radio the alarm
// rings with the timer sound. fade: minutes an alarm fades in (0 = the quick
// ramp).
func (d *daemon) ring(sound, alarmID string, fade int) {
	if hz, radio := alarms.RadioHz(sound); radio {
		if d.ringRadio(hz, alarmID, fade) {
			return
		}
		sound = ""
	}
	if path, usb := strings.CutPrefix(sound, "usb:"); usb {
		if d.ringUSB(path, alarmID, fade) {
			return
		}
		sound = ""
	}
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
		src = newRamp(src, fade)
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

// ringRadio rings with an FM station: the radio as an alarm voice (volume
// ramp, buttons, stop word and the 5-minute limit as for any alarm sound).
// False when the radio can't (no stick, a search runs); a stick that fails
// while ringing hands over to the timer sound. The station takes the tuner
// from whatever played; network listeners hear the alarm station.
func (d *daemon) ringRadio(hz int, alarmID string, fade int) bool {
	node, _, err := rtlsdr.Find()
	if err != nil {
		d.log.Warn("alarm: no radio stick, ringing the timer sound", "mhz", float64(hz)/1e6)
		return false
	}
	d.mu.Lock()
	ringing := d.alarm != nil
	d.mu.Unlock()
	if ringing {
		return true
	}
	r := &d.radio
	r.mu.Lock()
	if r.searching {
		r.mu.Unlock()
		d.log.Warn("alarm: a station search runs, ringing the timer sound")
		return false
	}
	speaker := r.speaker
	r.haltLocked()
	q := make(chan struct{})
	r.quit, r.want, r.speaker = q, hz, true
	r.mu.Unlock()
	if speaker {
		d.stopMedia() // the radio played as media: the alarm takes the tuner
	}
	buf := output.NewBuffer(5)
	src := newRamp(buf, fade)
	d.mu.Lock()
	d.alarm = d.out.Play(src, 1, "alert")
	d.alarmID = alarmID
	v := d.alarm
	d.mu.Unlock()
	d.log.Info("alarm: radio", "mhz", float64(hz)/1e6)
	runErr := make(chan error, 1)
	go func() { runErr <- d.radioRun(node, hz, buf, q) }()
	limit := time.AfterFunc(alarmMs*time.Millisecond, func() { d.out.Stop(v) })
	d.face.setRinging(true)
	d.face.wake(5 * time.Minute)
	d.publishRinging(true)
	go func() {
		<-v.Done()
		limit.Stop()
		buf.Abort() // the run lets go of the stick (or plays on for listeners)
		d.mu.Lock()
		current := d.alarm == v // not stopped or snoozed: ended by itself
		if current {
			d.alarm, d.alarmID = nil, ""
		}
		d.mu.Unlock()
		var err error
		select {
		case err = <-runErr:
		case <-time.After(2 * time.Second):
		}
		if current && err != nil {
			d.log.Warn("alarm: the radio failed, ringing the timer sound", "err", err)
			d.ring("", alarmID, fade)
			return
		}
		d.face.setRinging(false)
		d.publishRinging(false)
		d.updateLEDs()
	}()
	d.updateLEDs()
	return true
}

// ringUSB rings with a music file from the USB drive, repeated until the
// alarm stops (the 5-minute limit as for any alarm sound). False when the
// drive or the file isn't there or gives no audio within usbAlarmWait: then
// the timer sound rings, so a missing or slow drive never costs the alarm. A
// drive that fails while ringing hands over to the timer sound too.
func (d *daemon) ringUSB(path, alarmID string, fade int) bool {
	wb := d.fw.wb.Load()
	if wb == nil || wb.usb == nil {
		return false
	}
	d.mu.Lock()
	ringing := d.alarm != nil
	d.mu.Unlock()
	if ringing {
		return true
	}
	open := func() (*os.File, error) { f, _, err := wb.usb.OpenAudio(path); return f, err }
	f, err := open()
	if err != nil {
		d.log.Warn("alarm: USB file not available, ringing the timer sound", "file", path, "err", err)
		return false
	}
	src := newLoopSource(f, open)
	if !src.started(usbAlarmWait) {
		src.stop()
		d.log.Warn("alarm: no audio from the USB file, ringing the timer sound", "file", path, "err", src.failure())
		return false
	}
	d.mu.Lock()
	if d.alarm != nil {
		d.mu.Unlock()
		src.stop()
		return true
	}
	d.alarm = d.out.Play(newRamp(src, fade), 1, "alert")
	d.alarmID = alarmID
	v := d.alarm
	d.mu.Unlock()
	d.log.Info("alarm: USB music", "file", path)
	limit := time.AfterFunc(alarmMs*time.Millisecond, func() { d.out.Stop(v) })
	d.face.setRinging(true)
	d.face.wake(5 * time.Minute)
	d.publishRinging(true)
	go func() {
		<-v.Done()
		limit.Stop()
		src.stop()
		d.mu.Lock()
		current := d.alarm == v // not stopped or snoozed: ended by itself
		if current {
			d.alarm, d.alarmID = nil, ""
		}
		d.mu.Unlock()
		if err := src.failure(); current && err != nil {
			d.log.Warn("alarm: the USB file failed, ringing the timer sound", "file", path, "err", err)
			d.ring("", alarmID, fade)
			return
		}
		d.face.setRinging(false)
		d.publishRinging(false)
		d.updateLEDs()
	}()
	d.updateLEDs()
	return true
}

// usbAlarmWait: how long a USB alarm file may take to give its first audio
// (a drive spinning up) before the timer sound rings instead.
const usbAlarmWait = 3 * time.Second

// loopSource plays a file over and over: a producer decodes each pass into
// its own Buffer (the next one fills while the current one plays, so the
// repeat has no gap); Read moves on to the next buffer when one ends.
type loopSource struct {
	next  chan *output.Buffer
	quit  chan struct{}
	first chan struct{} // closed at the first audio or the first failure
	cur   *output.Buffer

	mu    sync.Mutex
	bufs  []*output.Buffer
	err   error
	done  bool // the producer gave up (err says why)
	audio bool // the first pass gave audio
	once  sync.Once
}

func newLoopSource(f *os.File, open func() (*os.File, error)) *loopSource {
	l := &loopSource{next: make(chan *output.Buffer, 1), quit: make(chan struct{}), first: make(chan struct{})}
	go l.produce(f, open)
	return l
}

func (l *loopSource) produce(f *os.File, open func() (*os.File, error)) {
	firstOnce := sync.OnceFunc(func() { close(l.first) })
	defer firstOnce()
	fail := func(err error) {
		l.mu.Lock()
		l.err, l.done = err, true
		l.mu.Unlock()
	}
	for pass := 0; ; pass++ {
		if pass > 0 {
			var err error
			if f, err = open(); err != nil {
				fail(err)
				return
			}
		}
		b := output.NewBuffer(5)
		l.mu.Lock()
		l.bufs = append(l.bufs, b)
		l.mu.Unlock()
		select {
		case l.next <- b:
		case <-l.quit:
			f.Close()
			return
		}
		go func() { // the first audio, while Decode fills the buffer
			for i := 0; i < 100; i++ {
				if w, _ := b.Progress(); w > 0 {
					l.mu.Lock()
					l.audio = true
					l.mu.Unlock()
					firstOnce()
					return
				}
				time.Sleep(30 * time.Millisecond)
			}
		}()
		err := output.Decode(f, b)
		f.Close()
		if w, _ := b.Progress(); w == 0 { // nothing decoded: don't spin on a bad file
			if err == nil {
				err = fmt.Errorf("no audio in the file")
			}
			fail(err)
			return
		}
		select {
		case <-l.quit:
			return
		default:
		}
	}
}

// started waits for the first audio; false after a failure or the timeout.
func (l *loopSource) started(timeout time.Duration) bool {
	select {
	case <-l.first:
	case <-time.After(timeout):
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.audio
}

func (l *loopSource) Read(dst []float32) (int, bool) {
	for {
		if l.cur == nil {
			select {
			case l.cur = <-l.next:
			default:
				l.mu.Lock()
				over := l.done
				l.mu.Unlock()
				return 0, !over // the next pass is still opening: a short gap
			}
		}
		n, ok := l.cur.Read(dst)
		if ok || n > 0 {
			return n, true
		}
		l.cur = nil // this pass ended: on to the next
	}
}

func (l *loopSource) stop() {
	l.once.Do(func() {
		close(l.quit)
		l.mu.Lock()
		for _, b := range l.bufs {
			b.Abort()
		}
		l.mu.Unlock()
	})
}

// failure: why the file stopped playing (nil while it plays or after stop).
func (l *loopSource) failure() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.err
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

// snoozeAlarm silences a ringing alarm for the snooze length; right after a
// snooze it steps the length instead (Home Assistant's Snooze pressed again).
func (d *daemon) snoozeAlarm() bool {
	id := d.ringingAlarm()
	if id == "" {
		return d.stepSnooze()
	}
	a, ok := d.alarms.byID(id)
	d.stopRinging()
	if ok {
		d.alarms.snooze(a)
		d.face.flash(fmt.Sprintf("%2d", int(d.snoozeFor().Minutes())), snoozeStepWindow)
	}
	return true
}

// stepSnooze: a button pressed again while the snooze minutes show steps the
// length (10 -> 15 -> 20 -> 5 ...); reports whether it did.
func (d *daemon) stepSnooze() bool {
	n, ok := d.alarms.stepSnooze(time.Now())
	if !ok {
		return false
	}
	d.out.Play(output.NewSamples(d.chimeTick, 0), 1, "alert")
	d.face.flash(fmt.Sprintf("%2d", int(n.Minutes())), snoozeStepWindow)
	return true
}

// rampSource raises the volume from `from` to full over its first total
// samples.
type rampSource struct {
	src      output.Source
	n, total int
	from     float32
}

// newRamp is the alarm's start: from a quarter to full in 30 s, or with
// fade-in minutes from near silence (squared, so it creeps up as ears hear).
func newRamp(src output.Source, fade int) *rampSource {
	if fade <= 0 {
		return &rampSource{src: src, total: int(alarmRamp.Seconds() * output.Rate * output.Channels), from: 0.25}
	}
	return &rampSource{src: src, total: fade * 60 * output.Rate * output.Channels, from: 0.02}
}

func (r *rampSource) Read(dst []float32) (int, bool) {
	n, ok := r.src.Read(dst)
	for i := 0; i < n && r.n < r.total; i++ {
		x := float32(r.n) / float32(r.total)
		if r.from < 0.25 {
			x *= x
		}
		dst[i] *= r.from + (1-r.from)*x
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
		"today":  now.Format(time.DateOnly), // skip dates on or after it are still to come
		"alarms": c.List(), "next": next, "snoozed_until": sn, "ringing": b.d.ringingAlarm() != "",
		"time_known": timeKnown(now), "snooze_minutes": int(b.d.snoozeFor().Minutes()), "max": alarms.Max,
	}
}

func (b *webBackend) SkipAlarm(id string, skip bool) error { return b.d.alarms.skip(id, skip) }

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
