package main

import (
	"testing"

	"lenovo-satellite/internal/alarms"
)

func TestHAAlarmActions(t *testing.T) {
	for in, want := range map[string]string{"once": "[]", "": "[]", "Weekdays": "[1 2 3 4 5]", "weekends": "[0 6]",
		"daily": "[0 1 2 3 4 5 6]", "mon,wed, friday": "[1 3 5]"} {
		ds, err := parseDays(in)
		if err != nil || fmtInts(ds) != want {
			t.Errorf("parseDays(%q) = %v, %v; want %s", in, ds, err, want)
		}
	}
	if _, err := parseDays("someday"); err == nil {
		t.Error("parseDays accepted a bad day")
	}
	l := alarms.List{{ID: "w", Time: "06:45", Label: "Work", Sound: "bells", Sunrise: true, On: false}, {ID: "x", Time: "09:00", On: true}}
	l2, err := setAlarm(l, "7:15", "weekdays", "work")
	if err != nil || len(l2) != 2 || l2[0].Time != "7:15" || l2[0].ID != "w" || l2[0].Sound != "bells" || !l2[0].Sunrise || !l2[0].On || l2[0].Label != "work" {
		t.Errorf("relabel by label: %+v %v", l2, err)
	}
	if l[0].Time != "06:45" {
		t.Error("setAlarm changed its input")
	}
	l3, _ := setAlarm(l, "09:00", "daily", "")
	if len(l3) != 2 || len(l3[1].Days) != 7 {
		t.Errorf("same time, no label: %+v", l3)
	}
	l4, _ := setAlarm(l, "06:45", "once", "")
	if len(l4) != 3 {
		t.Errorf("a labelled alarm was replaced by an unlabelled one at its time: %+v", l4)
	}
	if _, err := setAlarm(l, "25:00", "once", ""); err == nil {
		t.Error("bad time accepted")
	}
	if findAlarm(l, "WORK") != 0 || findAlarm(l, "9:00") != 1 || findAlarm(l, "10:00") != -1 {
		t.Error("findAlarm")
	}
}

func fmtInts(v []int) string {
	s := "["
	for i, x := range v {
		if i > 0 {
			s += " "
		}
		s += string(rune('0' + x))
	}
	return s + "]"
}
