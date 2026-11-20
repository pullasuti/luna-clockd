package timesync

import (
	"testing"
	"time"
)

func TestLoadZonePOSIX(t *testing.T) {
	loc, err := LoadZone("EET-2EEST,M3.5.0/3,M10.5.0/4")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		when string
		name string
		off  int
	}{
		{"2026-01-15T12:00:00Z", "EET", 2 * 3600},
		{"2026-07-15T12:00:00Z", "EEST", 3 * 3600},
		{"2026-10-25T00:59:00Z", "EEST", 3 * 3600}, // just before the switch back (04:00 local EEST = 01:00Z)
		{"2026-10-25T01:00:00Z", "EET", 2 * 3600},
	} {
		tt, _ := time.Parse(time.RFC3339, tc.when)
		name, off := tt.In(loc).Zone()
		if name != tc.name || off != tc.off {
			t.Errorf("%s: got %s %d, want %s %d", tc.when, name, off, tc.name, tc.off)
		}
	}
}

func TestLoadZoneIANA(t *testing.T) {
	if _, err := LoadZone("Europe/Helsinki"); err != nil {
		t.Skip("no zoneinfo on this host:", err)
	}
}

func TestLoadZoneBad(t *testing.T) {
	if _, err := LoadZone("Not/AZone"); err == nil {
		t.Error("bogus zone accepted")
	}
}
