package main

import (
	"testing"

	"lenovo-satellite/internal/wake"
)

func TestWakeThreshold(t *testing.T) {
	m := wake.Model{Cutoff: 0.97}
	for s, want := range map[string]float64{"slight": 0.97, "": 0.97, "moderate": 0.90, "very": 0.80, "bogus": 0.97} {
		if got := wakeThreshold(m, s); got != want {
			t.Errorf("%q: %v, want %v", s, got, want)
		}
	}
	// a model that is already more sensitive keeps its own cutoff
	if got := wakeThreshold(wake.Model{Cutoff: 0.6}, "very"); got != 0.6 {
		t.Errorf("got %v", got)
	}
}

func TestNearMiss(t *testing.T) {
	var n nearMiss
	var got, lvl float64
	for i, sc := range []float64{0, 0.3, 0.7, 0.93, 0.6, 0.1} {
		if p, l, ok := n.track(sc, wakeNearMissFloor, float64(-40+i)); ok {
			got, lvl = p, l
		}
	}
	if got != 0.93 || lvl != -36 {
		t.Fatalf("near miss peak %v level %v, want 0.93 / -36", got, lvl)
	}
	// small bumps are not near misses
	for _, sc := range []float64{0.3, 0.45, 0.1} {
		if p, _, ok := n.track(sc, wakeNearMissFloor, -40); ok {
			t.Fatalf("reported %v for a small bump", p)
		}
	}
	// the stop word's lower floor
	for _, sc := range []float64{0.05, 0.2, 0.25, 0.02} {
		if p, _, ok := n.track(sc, stopNearMissFloor(0.3), -30); ok {
			got = p
		}
	}
	if got != 0.25 {
		t.Fatalf("stop near miss %v, want 0.25", got)
	}
}

func TestWakeThresholdPlaying(t *testing.T) {
	m := wake.Model{Cutoff: 0.97}
	for sens, want := range map[string]float64{"slight": 0.90, "moderate": 0.80, "very": 0.75, "": 0.90} {
		if got := wakeThresholdPlaying(m, sens); got != want {
			t.Errorf("%q: %v, want %v", sens, got, want)
		}
		if wakeThresholdPlaying(m, sens) >= wakeThreshold(m, sens) {
			t.Errorf("%q: not easier while playing", sens)
		}
	}
	// a model with a low cutoff of its own keeps it
	if got := wakeThresholdPlaying(wake.Model{Cutoff: 0.6}, "very"); got != 0.6 {
		t.Errorf("low-cutoff model: %v", got)
	}
}
