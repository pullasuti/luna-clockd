package alarms

import (
	"path/filepath"
	"testing"
	"time"
)

func TestNext(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Helsinki")
	if err != nil {
		t.Skip("no zoneinfo:", err)
	}
	wed := time.Date(2026, 11, 11, 6, 30, 0, 0, loc) // a Wednesday
	for _, c := range []struct {
		a    Alarm
		want time.Time
	}{
		{Alarm{Time: "07:00", On: true}, time.Date(2026, 11, 11, 7, 0, 0, 0, loc)},
		{Alarm{Time: "06:30", On: true}, time.Date(2026, 11, 12, 6, 30, 0, 0, loc)}, // not "now" again
		{Alarm{Time: "07:00", On: false}, time.Time{}},
		{Alarm{Time: "07:00", On: true, Days: []int{0, 6}}, time.Date(2026, 11, 14, 7, 0, 0, 0, loc)}, // weekend
		{Alarm{Time: "05:00", On: true, Days: []int{3}}, time.Date(2026, 11, 18, 5, 0, 0, 0, loc)},    // next Wednesday
		{Alarm{Time: "7:5", On: true}, time.Time{}},
	} {
		if got := c.a.Next(wed); !got.Equal(c.want) {
			t.Errorf("%+v: next %v, want %v", c.a, got, c.want)
		}
	}
	// spring forward: 03:30 does not exist on 2027-03-28 in Helsinki; it rings once, an hour on
	pre := time.Date(2027, 3, 28, 1, 0, 0, 0, loc)
	n := Alarm{Time: "03:30", On: true}.Next(pre)
	if n.IsZero() || n.Day() != 28 {
		t.Errorf("DST night: %v", n)
	}
}

func TestListNextAndClean(t *testing.T) {
	now := time.Date(2026, 11, 11, 8, 0, 0, 0, time.UTC)
	l, err := List{
		{Time: "7:00", On: true, Days: []int{5, 1, 1}},
		{Time: "09:15", On: true},
		{Time: "08:30", On: false},
	}.Clean(map[string]bool{"gentle": true})
	if err != nil {
		t.Fatal(err)
	}
	if l[0].Time != "07:00" || len(l[0].Days) != 2 || l[0].Days[0] != 1 || l[0].ID == "" || l[0].ID == l[1].ID {
		t.Errorf("clean: %+v", l)
	}
	at, a, ok := l.Next(now)
	if !ok || a.Time != "09:15" || at.Hour() != 9 {
		t.Errorf("next: %v %+v %v", at, a, ok)
	}
	for _, bad := range []List{
		{{Time: "24:00"}}, {{Time: "07:00", Days: []int{7}}}, {{Time: "07:00", Sound: "siren"}},
		make(List, Max+1),
	} {
		if _, err := bad.Clean(map[string]bool{"gentle": true}); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

func TestSaveLoad(t *testing.T) {
	p := filepath.Join(t.TempDir(), "alarms.json")
	if l, err := Load(p); err != nil || len(l) != 0 {
		t.Fatalf("missing file: %v %v", l, err)
	}
	in := List{{ID: "a", Time: "06:45", Days: []int{1, 2, 3, 4, 5}, On: true, Sunrise: true}}
	if err := Save(p, in); err != nil {
		t.Fatal(err)
	}
	out, err := Load(p)
	if err != nil || len(out) != 1 || out[0].Time != "06:45" || !out[0].Sunrise || len(out[0].Days) != 5 {
		t.Errorf("round trip: %+v %v", out, err)
	}
}

// skip the next: that one day is left out, the one after rings; a weekly
// alarm then rings in two weeks; Clean checks the date
func TestSkipNext(t *testing.T) {
	loc := time.FixedZone("EET", 2*3600)
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, loc) // a Monday
	wk := Alarm{Time: "06:45", Days: []int{1, 2, 3, 4, 5}, On: true}.SkipNext(now, true)
	if wk.Skip != "2026-11-24" || !wk.Skipping(now) {
		t.Fatalf("skip %q", wk.Skip)
	}
	if n := wk.Next(now); n.Format(time.DateTime) != "2026-11-25 06:45:00" {
		t.Errorf("next after the skipped Tuesday: %v", n)
	}
	if wk.Skipping(time.Date(2026, 11, 25, 7, 0, 0, 0, loc)) {
		t.Error("still skipping after the day")
	}
	mon := Alarm{Time: "08:00", Days: []int{1}, On: true}.SkipNext(now, true)
	if n := mon.Next(now); n.Format(time.DateOnly) != "2026-12-07" {
		t.Errorf("weekly with its next one skipped: %v", n)
	}
	if wk.SkipNext(now, false).Skip != "" {
		t.Error("unskip")
	}
	if _, err := (List{{Time: "07:00", Skip: "tomorrow"}}).Clean(nil); err == nil {
		t.Error("a bad skip date was accepted")
	}
	if l, err := (List{{Time: "07:00", Skip: "2026-11-24"}}).Clean(nil); err != nil || l[0].Skip != "2026-11-24" {
		t.Errorf("clean: %v %v", l, err)
	}
}
