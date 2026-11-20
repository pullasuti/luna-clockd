package main

import (
	"testing"
	"time"
)

func TestMicGain(t *testing.T) {
	g := &micGain{}
	s := []int16{1000, -1000, 20000, -20000}
	g.apply(s, 6.0206) // x2
	want := []int16{2000, -2000, 32767, -32768}
	for i := range s {
		if s[i] != want[i] {
			t.Fatalf("sample %d: %d, want %d", i, s[i], want[i])
		}
	}
	if g.clipped != 2 {
		t.Fatalf("clipped %d, want 2", g.clipped)
	}

	s = []int16{1000, -1000}
	g.apply(s, 0)
	if s[0] != 1000 || s[1] != -1000 {
		t.Fatalf("0 dB changed the samples: %v", s)
	}
}

func TestMicMeter(t *testing.T) {
	g := &micGain{}
	sine := make([]int16, 160) // full-scale/2 square wave: peak = rms = -6 dBFS
	for i := range sine {
		sine[i] = 16384
		if i%2 == 1 {
			sine[i] = -16384
		}
	}
	deadline := time.Now().Add(time.Second)
	for g.level().At.IsZero() && time.Now().Before(deadline) {
		buf := append([]int16(nil), sine...)
		g.apply(buf, 0)
		time.Sleep(5 * time.Millisecond)
	}
	l := g.level()
	if l.PeakDB != -6 || l.RMSDB != -6 || l.Clipped != 0 {
		t.Fatalf("level %+v, want -6/-6 dBFS", l)
	}
}
