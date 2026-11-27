package main

import (
	"testing"

	"lenovo-satellite/internal/config"
)

func TestRadioOption(t *testing.T) {
	if o := radioOption(config.RadioStation{Hz: 91_200_000, Name: "POOKI"}); o != "91.2 MHz · POOKI" {
		t.Errorf("option %q", o)
	}
	for o, want := range map[string]int{"91.2 MHz · POOKI": 91_200_000, "106.8 MHz": 106_800_000, "87.5MHz": 87_500_000, "76.0 MHz · X": 76_000_000} {
		if hz, ok := radioOptionHz(o); !ok || hz != want {
			t.Errorf("%q: %d %v, want %d", o, hz, ok, want)
		}
	}
	for _, o := range []string{radioOff, "", "MHz", "200 MHz"} {
		if _, ok := radioOptionHz(o); ok {
			t.Errorf("%q taken as a station", o)
		}
	}
}
