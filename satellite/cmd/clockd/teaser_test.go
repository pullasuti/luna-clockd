package main

import (
	"math/rand/v2"
	"testing"
	"time"

	"lenovo-satellite/internal/display"
)

func TestTeaserFrame(t *testing.T) {
	var normal, luna display.Frame
	normal.Big("2147")
	normal.Set("colon", true)
	normal.Set("snow", true)
	normal.Weekday(time.Thursday)
	luna.Big("luna")
	const seed = 42

	for _, tm := range []teaserTiming{teaserSlow, teaserFirst} {
		if fr, ok := teaserFrame(normal, tm.length, seed, tm); ok || fr != normal {
			t.Errorf("%+v: after the end: ok %v", tm, ok)
		}
		a, _ := teaserFrame(normal, 2*time.Second, seed, tm)
		b, _ := teaserFrame(normal, 2*time.Second+10*time.Millisecond, seed, tm) // same 80 ms step
		if a != b {
			t.Error("same seed and step gave different frames")
		}
		// never a segment that doesn't exist
		for ms := time.Duration(0); ms < tm.length; ms += tm.step {
			fr, ok := teaserFrame(normal, ms, seed, tm)
			if !ok {
				t.Fatalf("over early at %v", ms)
			}
			for i := range fr.A {
				if fr.A[i]&^teaserMask.A[i] != 0 || fr.B[i]&^teaserMask.B[i] != 0 {
					t.Fatalf("%v: segment outside the face: %x %x", ms, fr.A, fr.B)
				}
			}
		}
		// the hold shows "luna" (at most one segment flickering) and nothing else
		for ms := tm.form; ms < tm.hold; ms += tm.step {
			fr, _ := teaserFrame(normal, ms, seed, tm)
			diff := 0
			for i := range fr.A {
				for x := fr.A[i] ^ luna.A[i]; x != 0; x &= x - 1 {
					diff++
				}
			}
			if diff > 1 || fr.B != luna.B {
				t.Fatalf("%v: not luna: %x %x", ms, fr.A, fr.B)
			}
		}
		// the suspense: a dark face, at most one spark (the first version has none)
		if tm.fade > 0 {
			for ms := tm.fade; ms < tm.dark; ms += tm.step {
				fr, _ := teaserFrame(normal, ms, seed, tm)
				lit := 0
				for i := range fr.A {
					for x := fr.A[i] | fr.B[i]; x != 0; x &= x - 1 {
						lit++
					}
				}
				if lit > 1 {
					t.Fatalf("%v: %d segments lit in the dark", ms, lit)
				}
			}
		}
		// the chaos really hides the time at its peak
		changed := 0
		for s := uint64(0); s < 20; s++ {
			fr, _ := teaserFrame(normal, tm.chaos-tm.step, s, tm)
			if fr.A != normal.A {
				changed++
			}
		}
		if changed < 19 {
			t.Errorf("peak noise left the time as it was in %d of 20 runs", 20-changed)
		}
	}

	// the first variant matches the original v0.107.0 teaser frame for frame
	for ms := time.Duration(0); ms < 8600*time.Millisecond; ms += 80 * time.Millisecond {
		fr, ok := teaserFrame(normal, ms, seed, teaserFirst)
		if old := v107Frame(normal, ms, seed); !ok || fr != old {
			t.Fatalf("%v: teaserFirst differs from the v0.107.0 teaser", ms)
		}
	}
}

// v107Frame is the v0.107.0 teaserFrame, kept here to prove that teaserFirst
// plays the original animation frame for frame.
func v107Frame(normal display.Frame, t time.Duration, seed uint64) display.Frame {
	const (
		calm, chaos = 1200 * time.Millisecond, 3400 * time.Millisecond
		form, hold  = 4800 * time.Millisecond, 7800 * time.Millisecond
		length      = 8600 * time.Millisecond
		step        = 80 * time.Millisecond
	)
	if t < 0 || t >= length {
		return normal
	}
	rng := rand.New(rand.NewPCG(seed, uint64(t/step)))
	var luna display.Frame
	luna.Big("luna")
	ramp := func(a, b time.Duration) float64 { return float64(t-a) / float64(b-a) }
	switch {
	case t < calm:
		return glitch(normal, rng, 0.012)
	case t < chaos:
		p := ramp(calm, chaos)
		out := glitch(normal, rng, 0.02+0.45*p*p)
		if rng.Float64() < 0.3*p {
			i := rng.IntN(4)
			out.A[i] = 0x7f * byte(rng.IntN(2))
		}
		return out
	case t < form:
		return blend(segNoise(rng, 0.45), luna, rng, ease(ramp(chaos, form)))
	case t < hold:
		out := luna
		if rng.Float64() < 0.12 {
			out.A[rng.IntN(4)] ^= 1 << rng.IntN(7)
		}
		return out
	default:
		return blend(glitch(luna, rng, 0.08), normal, rng, ease(ramp(hold, length)))
	}
}
