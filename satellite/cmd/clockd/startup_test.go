package main

import (
	"math"
	"testing"

	"lenovo-satellite/internal/output"
)

func TestNoteFreq(t *testing.T) {
	for s, want := range map[string]float64{"A4": 440, "C4": 261.63, "G#4": 415.30, "Eb5": 622.25, "B3": 246.94, "C6": 1046.50} {
		if got := nf(s); math.Abs(got-want) > 0.01 {
			t.Errorf("%s: %.2f, want %.2f", s, got, want)
		}
	}
}

func TestTunes(t *testing.T) {
	seen := map[string]bool{}
	for _, tu := range tunes {
		if seen[tu.ID] {
			t.Errorf("duplicate id %s", tu.ID)
		}
		seen[tu.ID] = true
		pcm := tu.pcm()
		sec := float64(len(pcm)) / output.Channels / output.Rate
		if sec < 1.5 || sec > 6.5 {
			t.Errorf("%s lasts %.1f s", tu.ID, sec)
		}
		peak := 0.0
		for _, v := range pcm {
			peak = math.Max(peak, math.Abs(float64(v)))
		}
		if db := 20 * math.Log10(peak); math.Abs(db+12) > 0.1 {
			t.Errorf("%s peaks at %.1f dBFS", tu.ID, db)
		}
		t.Logf("%-12s %.1f s", tu.ID, sec)
	}
}
