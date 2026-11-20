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
	src := &rampSource{src: constSource{}, total: 100}
	buf := make([]float32, 200)
	src.Read(buf)
	if buf[0] != 0.25 || buf[99] < 0.99 || buf[150] != 1 {
		t.Errorf("ramp %v %v %v", buf[0], buf[99], buf[150])
	}
}

type constSource struct{}

func (constSource) Read(dst []float32) (int, bool) {
	for i := range dst {
		dst[i] = 1
	}
	return len(dst), true
}
