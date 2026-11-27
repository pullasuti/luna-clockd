package main

import (
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"lenovo-satellite/internal/alarms"
)

func testAlarmCtl(t *testing.T, l alarms.List) *alarmCtl {
	d := &daemon{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	return &alarmCtl{d: d, path: filepath.Join(t.TempDir(), "alarms.json"), list: l}
}

func TestAlarmDue(t *testing.T) {
	at := func(h, m, s int) time.Time { return time.Date(2026, 11, 11, h, m, s, 0, time.Local) }
	c := testAlarmCtl(t, alarms.List{
		{ID: "d", Time: "07:00", Days: []int{0, 1, 2, 3, 4, 5, 6}, On: true},
		{ID: "o", Time: "07:30", On: true},
	})
	if c.due(time.Date(2015, 1, 1, 7, 0, 0, 0, time.Local)) != nil {
		t.Fatal("rang with the time unknown")
	}
	if a := c.due(at(6, 59, 59)); a != nil {
		t.Fatalf("early: %+v", a)
	}
	if a := c.due(at(7, 0, 0)); a == nil || a.ID != "d" {
		t.Fatalf("7:00: %+v", a)
	}
	if a := c.due(at(7, 0, 1)); a != nil {
		t.Fatalf("rang twice: %+v", a)
	}
	// clockd busy or the clock suspended for 10 minutes: the one-time 7:30 alarm is skipped and turned off
	c.due(at(7, 20, 0))
	if a := c.due(at(7, 40, 0)); a != nil {
		t.Fatalf("late alarm rang: %+v", a)
	}
	if l := c.List(); l[1].On {
		t.Error("missed one-time alarm still on")
	}
	// snooze
	c.snoozeAt, c.snoozeOf = at(7, 50, 0), alarms.Alarm{ID: "d", Time: "07:00"}
	if a := c.due(at(7, 50, 0)); a == nil || a.ID != "d" || !c.snoozed().IsZero() {
		t.Fatalf("snooze: %+v", a)
	}
	// a time jump back (DST) does not ring the same alarm again
	c2 := testAlarmCtl(t, alarms.List{{ID: "d", Time: "07:00", Days: []int{0, 1, 2, 3, 4, 5, 6}, On: true}})
	c2.due(at(7, 0, 0))
	if a := c2.due(at(6, 59, 0)); a != nil {
		t.Fatal("prepared to ring again after the jump back")
	}
	if a := c2.due(at(7, 0, 0)); a != nil {
		t.Fatalf("rang again after a jump back: %+v", a)
	}
}

func TestRampSource(t *testing.T) {
	src := &rampSource{src: constSource{}, total: 100, from: 0.25}
	buf := make([]float32, 200)
	src.Read(buf)
	if buf[0] != 0.25 || buf[99] < 0.99 || buf[150] != 1 {
		t.Errorf("ramp %v %v %v", buf[0], buf[99], buf[150])
	}
	// fade-in: from near silence, a quarter of the way still quiet
	f := newRamp(constSource{}, 2)
	if f.total != 2*60*48000*2 {
		t.Fatalf("fade total %d", f.total)
	}
	buf = make([]float32, f.total+10)
	f.Read(buf)
	if q := buf[f.total/4]; buf[0] > 0.03 || q > 0.1 || buf[f.total-1] < 0.99 || buf[f.total+5] != 1 {
		t.Errorf("fade %v %v %v", buf[0], q, buf[f.total-1])
	}
}

type constSource struct{}

func (constSource) Read(dst []float32) (int, bool) {
	for i := range dst {
		dst[i] = 1
	}
	return len(dst), true
}

// pressing again right after a snooze steps its length 10 -> 15 -> 20 -> 5,
// counted from the snooze; later presses don't
func TestStepSnooze(t *testing.T) {
	c := testAlarmCtl(t, alarms.List{{ID: "a", Time: "07:00", Days: []int{1}, On: true}})
	t0 := time.Date(2026, 11, 23, 7, 0, 0, 0, time.Local)
	c.snoozeAt, c.snoozeOf = t0.Add(10*time.Minute), c.list[0]
	c.snoozeFrom, c.snoozeTouch, c.snoozeLen = t0, t0, 10*time.Minute
	want := []int{15, 20, 5, 10}
	for i, m := range want {
		now := t0.Add(time.Duration(i+1) * time.Second)
		n, ok := c.stepSnooze(now)
		if !ok || int(n.Minutes()) != m || !c.snoozeAt.Equal(t0.Add(n)) {
			t.Fatalf("step %d: %v %v, rings %v; want %d min from the snooze", i+1, n, ok, c.snoozeAt, m)
		}
	}
	if _, ok := c.stepSnooze(t0.Add(4*time.Second + snoozeStepWindow + time.Millisecond)); ok {
		t.Error("stepped after the window")
	}
	c.snoozeAt = time.Time{}
	if _, ok := c.stepSnooze(t0); ok {
		t.Error("stepped without a snooze")
	}
}

// skip: the next ring of the alarm is left out, and back
func TestAlarmSkip(t *testing.T) {
	c := testAlarmCtl(t, alarms.List{{ID: "w", Time: "06:45", Days: []int{1, 2, 3, 4, 5}, On: true}})
	if err := c.skip("w", true); err != nil {
		t.Fatal(err)
	}
	if !c.List()[0].Skipping(time.Now()) {
		t.Error("not skipping")
	}
	if err := c.skip("w", false); err != nil || c.List()[0].Skip != "" {
		t.Errorf("unskip: %v %q", err, c.List()[0].Skip)
	}
	if c.skip("nope", true) == nil {
		t.Error("unknown alarm skipped")
	}
}
