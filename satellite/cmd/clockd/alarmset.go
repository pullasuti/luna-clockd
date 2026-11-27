package main

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"lenovo-satellite/internal/alarms"
	"lenovo-satellite/internal/hw"
	"lenovo-satellite/internal/output"
)

// Alarms on the clock itself, as the stock firmware does them:
//
//   - a short alarm press shows the next alarm; volume up/down step through
//     the others (time on the digits, days on the weekday marks, the alarm
//     icon lit when it is on); another short press, or 8 s idle, goes back
//   - in that view, a short alarm + volume down press together deletes the
//     alarm shown (held on, the pair is the 12 s back-to-stock countdown)
//   - in that view, the action button skips the next ring of the alarm shown
//     ("skip"; again: "on"), or switches a one-time alarm on and off
//   - holding the alarm button 2 s sets an alarm: the one shown in the view,
//     or a new one from the clock. The steps are repeat (once, every day,
//     weekdays, weekends or one day), hours, minutes and, with a 12 h clock,
//     AM/PM; volume up/down change the blinking part, a short alarm press
//     goes on and the last one saves. 8 s idle or another 2 s hold leaves
//     without saving.
//
// Label, sound and sunrise are kept as they were (set in the web UI).

const (
	alarmHoldFor   = 2 * time.Second
	alarmIdleFor   = 8 * time.Second
	alarmDeleteMax = 1500 * time.Millisecond // a pair press shorter than this deletes
)

const (
	uiOff = iota
	uiView
	uiEdit
)

const (
	stepDays = iota
	stepHour
	stepMinute
	stepAMPM
)

// repeatChoices are the repeat step's values, in order.
var repeatChoices = []struct {
	text string
	days []int
}{
	{"once", nil},
	{"all", []int{0, 1, 2, 3, 4, 5, 6}},
	{"1-5", []int{1, 2, 3, 4, 5}},
	{"6-7", []int{0, 6}},
	{"mon", []int{1}}, {"tue", []int{2}}, {"wed", []int{3}}, {"thu", []int{4}},
	{"fri", []int{5}}, {"sat", []int{6}}, {"sun", []int{0}},
}

type alarmUI struct {
	d *daemon

	mu      sync.Mutex
	mode    int
	list    alarms.List // view: the alarms by time
	idx     int
	ed      alarms.Alarm // edit: the alarm being set
	edNew   bool
	step    int
	rep     int // index in repeatChoices
	downAt  time.Time
	used    bool      // the alarm press was taken (hold, combo)
	volDnAt time.Time // last volume down press that stepped the view
	idle    *time.Timer
	gen     int // bumps on every change, for the idle timer and repeats
	repStop chan struct{}
}

func newAlarmUI(d *daemon) *alarmUI { return &alarmUI{d: d} }

// key handles the alarm button and, while alarms are shown, the volume
// buttons. Reports whether it used the event.
func (u *alarmUI) key(e hw.KeyEvent) bool {
	switch e.Code {
	case hw.KeyAlarm:
		switch e.Value {
		case 1:
			u.mu.Lock()
			at := time.Now()
			u.downAt, u.used = at, false
			u.mu.Unlock()
			time.AfterFunc(alarmHoldFor, func() { u.held(at) })
		case 0:
			u.mu.Lock()
			short := !u.downAt.IsZero() && !u.used
			u.downAt = time.Time{}
			u.mu.Unlock()
			if short {
				u.short()
			}
		}
		return true
	case hw.KeyPlay:
		u.mu.Lock()
		mode := u.mode
		u.mu.Unlock()
		if mode != uiView {
			return false
		}
		if e.Value == 1 {
			u.toggleSkip()
		}
		return true
	case hw.KeyVolumeUp, hw.KeyVolumeDown:
		u.mu.Lock()
		mode := u.mode
		u.mu.Unlock()
		if mode == uiOff {
			return false
		}
		dir := 1
		if e.Code == hw.KeyVolumeDown {
			dir = -1
		}
		switch e.Value {
		case 1:
			u.change(dir, e.Code == hw.KeyVolumeDown)
			u.startRepeat(dir)
		case 0:
			u.stopRepeat()
		}
		return true
	}
	return false
}

// held: the alarm button has been down for alarmHoldFor.
func (u *alarmUI) held(at time.Time) {
	u.mu.Lock()
	if !u.downAt.Equal(at) || u.used || u.d.stockHeld() > 0 {
		u.mu.Unlock()
		return
	}
	u.used = true
	mode := u.mode
	u.mu.Unlock()
	if mode == uiEdit {
		u.d.log.Info("alarm setting left without saving")
		u.close()
		u.d.face.flash("", 0)
		u.beep()
		return
	}
	u.startEdit()
}

func (u *alarmUI) short() {
	u.mu.Lock()
	mode := u.mode
	u.mu.Unlock()
	switch mode {
	case uiEdit:
		u.next()
	case uiView:
		u.close()
	default:
		if u.d.alarms.sunriseLit(time.Now()) {
			u.d.alarms.endSunrise(false)
			return
		}
		u.openView()
	}
}

func (u *alarmUI) sorted() alarms.List {
	l := u.d.alarms.List()
	sort.SliceStable(l, func(i, j int) bool { return l[i].Time < l[j].Time })
	return l
}

func (u *alarmUI) openView() {
	l := u.sorted()
	if len(l) == 0 {
		u.d.face.flash("off", 2*time.Second)
		return
	}
	idx := 0
	if _, next, ok := u.d.alarms.Next(time.Now()); ok {
		for i, a := range l {
			if a.ID == next.ID {
				idx = i
			}
		}
	}
	u.mu.Lock()
	u.mode, u.list, u.idx = uiView, l, idx
	u.mu.Unlock()
	u.show()
}

func (u *alarmUI) startEdit() {
	u.mu.Lock()
	var a alarms.Alarm
	isNew := true
	if u.mode == uiView && len(u.list) > 0 {
		a, isNew = u.list[u.idx], false
	}
	u.mu.Unlock()
	if isNew {
		if len(u.d.alarms.List()) >= alarms.Max {
			u.close()
			u.d.face.flash("full", 2*time.Second)
			return
		}
		a = alarms.Alarm{Time: "07:00", On: true}
	}
	rep := 0
	for i, c := range repeatChoices {
		if fmt.Sprint(c.days) == fmt.Sprint(a.Days) {
			rep = i
		}
	}
	if rep == 0 && len(a.Days) > 0 {
		rep = 1 // chosen days the clock can't show as one choice: start from every day
	}
	u.mu.Lock()
	u.mode, u.ed, u.edNew, u.step, u.rep = uiEdit, a, isNew, stepDays, rep
	u.mu.Unlock()
	u.d.log.Info("alarm setting on the clock", "new", isNew, "alarm", a.Time)
	u.beep()
	u.show()
}

// next goes to the next step; after the last one the alarm is saved.
func (u *alarmUI) next() {
	u.mu.Lock()
	last := stepMinute
	if !u.d.cfg.Get().Display.Clock24h {
		last = stepAMPM
	}
	if u.step < last {
		u.step++
		u.mu.Unlock()
		u.beep()
		u.show()
		return
	}
	a, isNew := u.ed, u.edNew
	a.Days = append([]int(nil), repeatChoices[u.rep].days...)
	a.On = true
	u.mu.Unlock()
	l := u.d.alarms.List()
	if isNew {
		l = append(l, a)
	} else {
		for i := range l {
			if l[i].ID == a.ID {
				l[i] = a
			}
		}
	}
	u.close()
	if err := u.d.alarms.Set(l); err != nil {
		u.d.log.Error("alarm not saved", "err", err)
		u.d.out.Play(output.NewSamples(u.d.chimeError, 0), 1, "alert")
		u.d.face.flash("err", 2*time.Second)
		return
	}
	u.d.log.Info("alarm set on the clock", "time", a.Time, "days", a.Days)
	u.d.out.Play(output.NewSamples(u.d.chimeWake, 0), 1, "alert")
	u.d.face.flash("set", 1500*time.Millisecond)
}

// change steps the view or the value being set.
func (u *alarmUI) change(dir int, volDn bool) {
	u.mu.Lock()
	switch u.mode {
	case uiView:
		if n := len(u.list); n > 0 {
			u.idx = (u.idx + dir + n) % n
		}
		if volDn {
			u.volDnAt = time.Now()
		}
	case uiEdit:
		h, m, _ := u.ed.HM()
		switch u.step {
		case stepDays:
			n := len(repeatChoices)
			u.rep = (u.rep + dir + n) % n
		case stepHour:
			if u.d.cfg.Get().Display.Clock24h {
				h = (h + dir + 24) % 24
			} else { // stay in the same half of the day: AM/PM is its own step
				h = h/12*12 + (h%12+dir+12)%12
			}
		case stepMinute:
			m = (m + dir + 60) % 60
		case stepAMPM:
			h = (h + 12) % 24
		}
		u.ed.Time = fmt.Sprintf("%02d:%02d", h, m)
	}
	u.mu.Unlock()
	u.show()
}

// startRepeat: hours and minutes keep stepping while the button is held.
func (u *alarmUI) startRepeat(dir int) {
	u.stopRepeat()
	u.mu.Lock()
	if u.mode != uiEdit || (u.step != stepHour && u.step != stepMinute) {
		u.mu.Unlock()
		return
	}
	stop := make(chan struct{})
	u.repStop = stop
	u.mu.Unlock()
	go func() {
		select {
		case <-stop:
			return
		case <-time.After(500 * time.Millisecond):
		}
		t := time.NewTicker(120 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				u.change(dir, false)
			}
		}
	}()
}

func (u *alarmUI) stopRepeat() {
	u.mu.Lock()
	if u.repStop != nil {
		close(u.repStop)
		u.repStop = nil
	}
	u.mu.Unlock()
}

// comboStarted: alarm + volume down went down together (stockCombo). The
// alarm press is no longer a short press or a hold; a volume down that
// stepped the view first is undone.
func (u *alarmUI) comboStarted(volDnFirst bool) (undone bool) {
	u.stopRepeat()
	u.mu.Lock()
	u.used = true
	if volDnFirst && u.mode == uiView && time.Since(u.volDnAt) < 2*time.Second {
		if n := len(u.list); n > 0 {
			u.idx = (u.idx + 1) % n
		}
		undone = true
	}
	u.mu.Unlock()
	if undone {
		u.show()
	}
	return undone
}

// comboReleased: the pair was let go after held. A short pair press in the
// view deletes the alarm shown. Reports whether it did.
func (u *alarmUI) comboReleased(held time.Duration) bool {
	u.mu.Lock()
	if u.mode != uiView || held >= alarmDeleteMax || len(u.list) == 0 {
		u.mu.Unlock()
		return false
	}
	gone := u.list[u.idx]
	u.mu.Unlock()
	var l alarms.List
	for _, a := range u.d.alarms.List() {
		if a.ID != gone.ID {
			l = append(l, a)
		}
	}
	if err := u.d.alarms.Set(l); err != nil {
		u.d.log.Error("alarm not deleted", "err", err)
		return true
	}
	u.d.log.Info("alarm deleted on the clock", "time", gone.Time)
	u.d.out.Play(output.NewSamples(u.d.chimeError, 0), 1, "alert")
	u.close()
	u.d.face.flash("del", 1500*time.Millisecond)
	return true
}

// toggleSkip: the action button in the view. A repeating alarm skips its next
// ring (or stops skipping it); a one-time alarm switches on or off.
func (u *alarmUI) toggleSkip() {
	u.mu.Lock()
	if len(u.list) == 0 {
		u.mu.Unlock()
		return
	}
	a := u.list[u.idx]
	u.mu.Unlock()
	now := time.Now()
	var err error
	word := ""
	if a.Repeats() {
		skip := !a.Skipping(now)
		err = u.d.alarms.skip(a.ID, skip)
		word = map[bool]string{true: "skip", false: "on"}[skip]
	} else {
		l := u.d.alarms.List()
		for i := range l {
			if l[i].ID == a.ID {
				l[i].On = !l[i].On
				word = map[bool]string{true: "on", false: "off"}[l[i].On]
			}
		}
		err = u.d.alarms.Set(l)
	}
	if err != nil {
		u.d.log.Error("alarm not changed", "err", err)
		u.d.out.Play(output.NewSamples(u.d.chimeError, 0), 1, "alert")
		return
	}
	// the view keeps the same alarm, with its new state
	l := u.sorted()
	u.mu.Lock()
	u.list = l
	for i, x := range l {
		if x.ID == a.ID {
			u.idx = i
		}
	}
	u.mu.Unlock()
	u.beep()
	u.show()
	u.d.face.flash(word, 1500*time.Millisecond)
}

func (u *alarmUI) close() {
	u.stopRepeat()
	u.mu.Lock()
	u.mode, u.list = uiOff, nil
	u.gen++
	u.mu.Unlock()
	u.d.face.setAlarmView(nil)
}

func (u *alarmUI) beep() { u.d.out.Play(output.NewSamples(u.d.chimeTick, 0), 1, "alert") }

// show draws the current state and restarts the idle timer.
func (u *alarmUI) show() {
	h24 := u.d.cfg.Get().Display.Clock24h
	u.mu.Lock()
	u.gen++
	gen := u.gen
	var v *alarmView
	switch u.mode {
	case uiView:
		if len(u.list) > 0 {
			a := u.list[u.idx]
			v = timeView(a.Time, h24)
			v.alarmIcon = a.On
			v.days = a.Days
			if len(a.Days) == 0 {
				if n := a.Next(time.Now()); !n.IsZero() {
					v.days = []int{int(n.Weekday())}
				}
			}
		}
	case uiEdit:
		v = timeView(u.ed.Time, h24)
		v.alarmIcon = true
		v.days = repeatChoices[u.rep].days
		switch u.step {
		case stepDays:
			v.big, v.colon = repeatChoices[u.rep].text, false
			v.am, v.pm = false, false
			v.daysBlink = true
		case stepHour:
			v.blink = [4]bool{true, true}
		case stepMinute:
			v.blink = [4]bool{2: true, 3: true}
		case stepAMPM:
			v.ampmBlink = true
		}
	}
	u.mu.Unlock()
	u.d.face.setAlarmView(v)
	u.d.face.wake(alarmIdleFor)
	time.AfterFunc(alarmIdleFor, func() {
		u.mu.Lock()
		stale := u.gen != gen || u.mode == uiOff
		u.mu.Unlock()
		if !stale {
			u.d.log.Info("alarm view closed (idle)")
			u.close()
		}
	})
}

// timeView: an "HH:MM" alarm time as the face shows it.
func timeView(t string, h24 bool) *alarmView {
	a := alarms.Alarm{Time: t}
	h, m, _ := a.HM()
	v := &alarmView{colon: true}
	if h24 {
		v.big = fmt.Sprintf("%02d%02d", h, m)
		return v
	}
	v.am, v.pm = h < 12, h >= 12
	if h = h % 12; h == 0 {
		h = 12
	}
	v.big = fmt.Sprintf("%2d%02d", h, m)
	return v
}
