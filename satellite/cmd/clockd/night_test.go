package main

import (
	"testing"
	"time"
)

func TestInNight(t *testing.T) {
	at := func(s string) int {
		m, err := parseClock(s)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	cases := []struct {
		now, start, end string
		want            bool
	}{
		{"23:00", "22:00", "07:00", true},
		{"03:00", "22:00", "07:00", true},
		{"07:00", "22:00", "07:00", false}, // end is exclusive
		{"21:59", "22:00", "07:00", false},
		{"22:00", "22:00", "07:00", true},
		{"13:00", "12:00", "14:00", true}, // same-day window
		{"15:00", "12:00", "14:00", false},
		{"12:00", "12:00", "12:00", false}, // empty window
	}
	for _, c := range cases {
		if got := inNight(at(c.now), at(c.start), at(c.end)); got != c.want {
			t.Errorf("%s in %s-%s = %v, want %v", c.now, c.start, c.end, got, c.want)
		}
	}
	for _, bad := range []string{"24:00", "7", "ab:cd", "12:60"} {
		if _, err := parseClock(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestSunNight(t *testing.T) {
	loc := time.FixedZone("EET", 2*3600)
	at := func(s string) time.Time {
		v, err := time.ParseInLocation("2006-01-02 15:04", s, loc)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	min := time.Minute
	// HA at 12:00 on the 30th: next sunset 30th 16:20, next sunrise 31st 08:10
	daySet, dayRise := at("2026-10-30 16:20"), at("2026-10-31 08:10")
	// HA at 23:00 on the 30th: next sunset 31st 16:18, next sunrise 31st 08:10
	nightSet, nightRise := at("2026-10-31 16:18"), at("2026-10-31 08:10")
	cases := []struct {
		name       string
		now        time.Time
		set, rise  time.Time
		sOff, eOff time.Duration
		want       bool
	}{
		{"noon", at("2026-10-30 12:00"), daySet, dayRise, 0, 0, false},
		{"just after sunset", at("2026-10-30 16:25"), daySet, dayRise, 0, 0, true},
		{"30 min before sunset, offset -30", at("2026-10-30 15:55"), daySet, dayRise, -30 * min, 0, true},
		{"after sunset, offset +60 not yet", at("2026-10-30 17:00"), daySet, dayRise, 60 * min, 0, false},
		{"midnight", at("2026-10-31 00:30"), nightSet, nightRise, 0, 0, true},
		{"before sunrise", at("2026-10-31 08:00"), nightSet, nightRise, 0, 0, true},
		// after sunrise HA already reports tomorrow's sunrise; the +30 end offset still counts
		{"after sunrise, end +30", at("2026-10-31 08:20"), at("2026-10-31 16:18"), at("2026-11-01 08:12"), 0, 30 * min, true},
		{"after sunrise, no offset", at("2026-10-31 08:20"), at("2026-10-31 16:18"), at("2026-11-01 08:12"), 0, 0, false},
	}
	for _, c := range cases {
		if got := sunNight(c.now, c.set, c.rise, c.sOff, c.eOff); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
