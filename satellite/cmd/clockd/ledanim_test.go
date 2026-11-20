package main

import (
	"math"
	"testing"
	"time"
)

// The web UI repeats noise(); these values come from its JavaScript version
// (Math.imul), so both sides animate alike.
func TestHash01Pinned(t *testing.T) {
	for i, want := range map[int64]float64{0: 0, 1: 0.363580, 12345: 0.224437, 8812345678: 0.286194} {
		if got := hash01(i); got < want-0.0001 || got > want+0.0001 {
			t.Errorf("hash01(%d) = %.4f, want %.4f", i, got, want)
		}
	}
}

func TestSpeak(t *testing.T) {
	var lo, hi = 1.0, 0.0
	paused := 0
	for ms := 0; ms < 60000; ms += 40 {
		l := speak(time.Duration(ms) * time.Millisecond)
		for i, v := range l {
			if v < 0 || v > 1 {
				t.Fatalf("level %d out of range: %v", i, v)
			}
		}
		if l[1] != l[2] || l[0] != l[3] {
			t.Fatalf("not symmetric: %v", l)
		}
		lo, hi = min(lo, l[1]), max(hi, l[1])
		if l[1] < 0.2 {
			paused++
		}
	}
	if lo > 0.2 || hi < 0.9 {
		t.Errorf("middle LEDs range %.2f..%.2f: too flat", lo, hi)
	}
	if f := float64(paused) / 1500; f < 0.15 || f > 0.6 {
		t.Errorf("quiet %.0f%% of the time", f*100)
	}
}

// A syllable pattern at two very different loudnesses gives the same LED
// movement (automatic gain), and silence between syllables dims the LEDs.
func TestSpeechEnvelope(t *testing.T) {
	run := func(amp float32) (lo, hi float64) {
		var e speechEnv
		lo, hi = 1, 0
		for i := 0; i < 200; i++ { // 8 s of 40 ms frames
			rms := amp * 0.02 // gap
			if (i/4)%2 == 0 {
				rms = amp // 160 ms syllable
			}
			l := e.frame(true, rms, [4]float64{})
			if i > 25 {
				lo, hi = min(lo, l[1]), max(hi, l[1])
			}
		}
		return lo, hi
	}
	for _, amp := range []float32{0.5, 0.02} {
		lo, hi := run(amp)
		if hi < 0.55 || lo > 0.3 {
			t.Errorf("amplitude %v: middle LEDs %.2f..%.2f, want a clear pulse", amp, lo, hi)
		}
	}
	var e speechEnv
	fb := [4]float64{0.1, 0.2, 0.3, 0.4}
	if l := e.frame(false, 0.5, fb); l != fb {
		t.Errorf("not playing: %v, want the fallback", l)
	}
}

// The outer LEDs stay well below the middle ones and only light on strong parts.
func TestSpeechEnvelopeSpreads(t *testing.T) {
	var e speechEnv
	both := 0
	for i := 0; i < 200; i++ {
		rms := float32(0.3 * (0.5 + 0.5*math.Sin(float64(i)*0.9)))
		l := e.frame(true, rms, [4]float64{})
		if l[0] > 0.45 && l[1] > 0.6 {
			both++
		}
		if l[0] > 0.56+1e-9 {
			t.Fatalf("outer LED at %.2f", l[0])
		}
	}
	if both > 40 {
		t.Errorf("all four near full in %d of 200 frames", both)
	}
}
