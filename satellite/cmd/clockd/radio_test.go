package main

import (
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"testing"

	"lenovo-satellite/internal/config"
)

// the scan of clock #2 (2026-11-21): strong stations keep their entry, the
// leak 200 kHz next to them (20-30 dB weaker) and the neighbouring steps of
// one station go; two real stations 400 kHz apart both stay
func TestPickStations(t *testing.T) {
	hits := []radioHit{
		{89_200_000, 40}, {89_300_000, 60.1}, {89_400_000, 45}, {89_500_000, 30.2}, // 89.3 + its leak
		{90_300_000, 46.4}, {90_700_000, 40}, // two stations
		{91_200_000, 50.2}, {91_400_000, 31.4}, // leak
		{96_200_000, 44}, {96_900_000, 27.2}, {97_200_000, 43.5}, // 96.9: 300 kHz from 97.2, 16 dB weaker
		{100_000_000, 12}, // below the cutoff
	}
	got := pickStations(hits)
	want := []int{89_300_000, 90_300_000, 90_700_000, 91_200_000, 96_200_000, 97_200_000}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i, s := range got {
		if s.Hz != want[i] {
			t.Errorf("station %d: %d, want %d", i, s.Hz, want[i])
		}
	}
}

// favourites: kept in order without duplicates, checked, and they become HA's
// dropdown (names from the search); without them the listed stations
func TestRadioFavs(t *testing.T) {
	d := &daemon{log: slog.New(slog.NewTextHandler(io.Discard, nil)), statePath: filepath.Join(t.TempDir(), "state.json")}
	config.UpdateState(d.statePath, func(s *config.State) {
		s.RadioStations = []config.RadioStation{{Hz: 89_300_000, PilotDB: 50, Name: "POOKI"}, {Hz: 96_700_000, PilotDB: 30}, {Hz: 104_900_000, PilotDB: 46, Name: "NRJ"}}
	})
	if o := d.radioOptions(); !slices.Equal(o, []string{"Off", "89.3 MHz · POOKI", "104.9 MHz · NRJ"}) {
		t.Errorf("no favourites: %q", o)
	}
	if err := d.radioSetFavs([]int{104_900_000, 99_100_000, 104_900_000}); err != nil {
		t.Fatal(err)
	}
	if f := config.LoadState(d.statePath).RadioFavs; !slices.Equal(f, []int{104_900_000, 99_100_000}) {
		t.Errorf("saved %v", f)
	}
	if o := d.radioOptions(); !slices.Equal(o, []string{"Off", "104.9 MHz · NRJ", "99.1 MHz"}) {
		t.Errorf("favourites: %q", o)
	}
	for _, bad := range [][]int{{91_250_000}, {120_000_000}, {1, 2, 3, 4, 5, 6, 7}} {
		if d.radioSetFavs(bad) == nil {
			t.Errorf("%v accepted", bad)
		}
	}
	if err := d.radioSetFavs(nil); err != nil || len(d.radioOptions()) != 3 {
		t.Errorf("cleared: %v %q", err, d.radioOptions())
	}
}
