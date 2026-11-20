package main

import (
	"testing"
	"time"

	"lenovo-satellite/internal/hw"
)

func TestRescueHold(t *testing.T) {
	t0 := time.Unix(1000, 0)
	var h rescueHold
	h.key(hw.KeyEvent{Code: hw.KeyAlarm, Value: 1}, t0)
	if h.held(t0.Add(time.Second)) != 0 {
		t.Fatal("alarm alone started the hold")
	}
	h.key(hw.KeyEvent{Code: hw.KeyVolumeDown, Value: 1}, t0.Add(time.Second))
	if got := h.held(t0.Add(4 * time.Second)); got < 3*time.Second || got > 3*time.Second+time.Millisecond {
		t.Fatalf("held %v, want 3s", got)
	}
	// autorepeat and other keys change nothing
	h.key(hw.KeyEvent{Code: hw.KeyAlarm, Value: 2}, t0.Add(5*time.Second))
	h.key(hw.KeyEvent{Code: hw.KeyPlay, Value: 1}, t0.Add(5*time.Second))
	if h.at != t0.Add(time.Second) {
		t.Fatal("hold restarted by autorepeat or another key")
	}
	h.key(hw.KeyEvent{Code: hw.KeyVolumeDown, Value: 0}, t0.Add(6*time.Second))
	if h.held(t0.Add(7*time.Second)) != 0 {
		t.Fatal("letting go of volume down didn't cancel")
	}
	h.key(hw.KeyEvent{Code: hw.KeyVolumeDown, Value: 1}, t0.Add(8*time.Second))
	if h.at != t0.Add(8*time.Second) {
		t.Fatal("pressing again didn't restart the hold from zero")
	}
}

func TestRescueView(t *testing.T) {
	if s, c := rescueView(0); s != "Err" || c[0].R == 0 {
		t.Errorf("idle: %q %v", s, c)
	}
	for _, tc := range []struct {
		held time.Duration
		text string
		lit  int
	}{
		{time.Nanosecond, "12", 1},
		{2900 * time.Millisecond, "10", 1},
		{3 * time.Second, "9", 2},
		{9 * time.Second, "3", 4},
		{11900 * time.Millisecond, "1", 4},
	} {
		s, c := rescueView(tc.held)
		lit := 0
		for _, x := range c {
			if x != (hw.RGB{}) {
				lit++
			}
		}
		if s != tc.text || lit != tc.lit {
			t.Errorf("%v: %q with %d LEDs, want %q with %d", tc.held, s, lit, tc.text, tc.lit)
		}
	}
}
