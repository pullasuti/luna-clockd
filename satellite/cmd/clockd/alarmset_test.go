package main

import (
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"lenovo-satellite/internal/alarms"
	"lenovo-satellite/internal/config"
)

func testAlarmUI(t *testing.T, h24 bool) *alarmUI {
	cfg, err := config.Load(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Update(func(c *config.Config) { c.Display.Clock24h = h24 }); err != nil {
		t.Fatal(err)
	}
	d := &daemon{log: slog.New(slog.DiscardHandler), cfg: cfg, face: &face{}}
	return newAlarmUI(d)
}

func TestAlarmSetSteps(t *testing.T) {
	u := testAlarmUI(t, false)
	u.mode, u.ed = uiEdit, alarms.Alarm{Time: "00:30"}
	u.step = stepHour
	u.change(-1, false) // 12 AM -> 11 AM: the hours step stays in its half of the day
	if u.ed.Time != "11:30" {
		t.Errorf("12 AM - 1 = %s", u.ed.Time)
	}
	u.change(+1, false)
	u.change(+1, false) // 11 -> 12 AM -> 1 AM
	if u.ed.Time != "01:30" {
		t.Errorf("got %s", u.ed.Time)
	}
	u.step = stepAMPM
	u.change(+1, false)
	if u.ed.Time != "13:30" {
		t.Errorf("PM: %s", u.ed.Time)
	}
	u.step = stepMinute
	u.change(-1, false)
	u.change(-1, false)
	if u.ed.Time != "13:28" {
		t.Errorf("minutes: %s", u.ed.Time)
	}
	u.step = stepDays
	u.change(-1, false) // once -> the last choice (wraps)
	if repeatChoices[u.rep].text != "sun" {
		t.Errorf("repeat wrap: %s", repeatChoices[u.rep].text)
	}

	v := u.d.face.av
	if v == nil || !v.daysBlink || v.big != "sun" || len(v.days) != 1 || v.days[0] != 0 {
		t.Errorf("face: %+v", v)
	}
	u.close()
	if u.d.face.av != nil {
		t.Error("face still shows the alarm view")
	}

	u24 := testAlarmUI(t, true)
	u24.mode, u24.ed, u24.step = uiEdit, alarms.Alarm{Time: "23:00"}, stepHour
	u24.change(+1, false)
	if u24.ed.Time != "00:00" {
		t.Errorf("24 h wrap: %s", u24.ed.Time)
	}
}

func TestAlarmViewComboUndo(t *testing.T) {
	u := testAlarmUI(t, true)
	u.mode, u.list, u.idx = uiView, alarms.List{{ID: "a", Time: "06:00"}, {ID: "b", Time: "07:00"}, {ID: "c", Time: "08:00"}}, 1
	u.change(-1, true) // volume down first: steps to "a"
	if !u.comboStarted(true) || u.idx != 1 {
		t.Errorf("volume down step not undone: idx %d", u.idx)
	}
	if !u.used {
		t.Error("the alarm press still counts as a short press")
	}
	u.volDnAt = time.Now().Add(-time.Minute)
	if u.comboStarted(true) {
		t.Error("undid an old step")
	}
	u.close()
}

func TestTimeView(t *testing.T) {
	if v := timeView("07:05", true); v.big != "0705" || v.am || v.pm {
		t.Errorf("24 h: %+v", v)
	}
	if v := timeView("00:05", false); v.big != "1205" || !v.am {
		t.Errorf("12 AM: %+v", v)
	}
	if v := timeView("13:45", false); v.big != " 145" || !v.pm {
		t.Errorf("1 PM: %+v", v)
	}
}
