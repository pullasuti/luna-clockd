package main

import (
	"math/rand/v2"
	"time"

	"lenovo-satellite/internal/display"
)

// The luna teaser (web UI: Display > Clock face; action face_teaser): the
// normal face starts to glitch (segments flicker, symbols blink), the noise
// grows until nothing of the time is left, dies away into a dark face where
// a lone segment sparks now and then (the suspense), then the segments settle
// into "luna", which holds for a few seconds before the clock comes back.
// With Display.StartupAnimation on, the original first version of the teaser
// (faster, without the suspense) plays once when clockd starts.

// teaserTiming is one variant of the teaser: the phase boundaries, counted
// from its start. Zero fade and dark skip those phases (the first version).
type teaserTiming struct {
	step      time.Duration // a new noise pattern every step
	calm      time.Duration // the normal face with a few stray flickers
	chaos     time.Duration // the noise grows until the face is gone
	fade      time.Duration // the noise dies away
	dark      time.Duration // dark, a lone segment sparks now and then
	form      time.Duration // the segments settle into "luna"
	hold      time.Duration // "luna" holds, now and then a segment flickers
	length    time.Duration // dissolves back into the clock
	formStep  time.Duration // noise cadence while "luna" forms (0: step)
	formNoise float64       // segment noise as "luna" starts to form
	formFades bool          // the noise thins out as "luna" forms
}

var (
	// teaserSlow: the teaser as it plays on demand, since v0.107.2.
	teaserSlow = teaserTiming{
		step:      80 * time.Millisecond,
		calm:      1200 * time.Millisecond,
		chaos:     3400 * time.Millisecond,
		fade:      3800 * time.Millisecond,
		dark:      5600 * time.Millisecond,
		form:      7800 * time.Millisecond,
		hold:      10800 * time.Millisecond,
		length:    11600 * time.Millisecond,
		formStep:  160 * time.Millisecond,
		formNoise: 0.25,
		formFades: true,
	}
	// teaserFirst: the original v0.107.0 teaser, faster and without the dark
	// suspense. It plays once at startup when Display.StartupAnimation is on.
	teaserFirst = teaserTiming{
		step:      80 * time.Millisecond,
		calm:      1200 * time.Millisecond,
		chaos:     3400 * time.Millisecond,
		form:      4800 * time.Millisecond,
		hold:      7800 * time.Millisecond,
		length:    8600 * time.Millisecond,
		formNoise: 0.45,
	}
)

// teaserMask: the segments and symbols that exist on the face (the four big
// digits; small digits, status, temperature row, weather, weekdays).
var teaserMask = display.Frame{
	A: [6]byte{0x7f, 0x7f, 0x7f, 0x7f, 0, 0},
	B: [6]byte{0x7f, 0x7f, 0x3f, 0x7f, 0x3f, 0x7f},
}

// teaser plays the slow teaser once, over whatever the face shows.
func (f *face) teaser() { f.playTeaser(teaserSlow) }

// playTeaser starts a teaser variant, over whatever the face shows.
func (f *face) playTeaser(tm teaserTiming) {
	f.mu.Lock()
	f.teaserStart, f.teaserSeed, f.teaserV = time.Now(), rand.Uint64(), tm
	f.mu.Unlock()
}

// teaserOn: a teaser is playing (the web UI's face picture follows it closely).
func (f *face) teaserOn() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return !f.teaserStart.IsZero()
}

// teaserFrame is what the face shows t into the teaser, given the normal
// frame; ok is false when the teaser is over. The same seed and t give the
// same frame.
func teaserFrame(normal display.Frame, t time.Duration, seed uint64, tm teaserTiming) (display.Frame, bool) {
	if t < 0 || t >= tm.length {
		return normal, false
	}
	rng := rand.New(rand.NewPCG(seed, uint64(t/tm.step)))
	var luna display.Frame
	luna.Big("luna")
	ramp := func(a, b time.Duration) float64 { return float64(t-a) / float64(b-a) }
	switch {
	case t < tm.calm:
		return glitch(normal, rng, 0.012), true
	case t < tm.chaos:
		p := ramp(tm.calm, tm.chaos)
		out := glitch(normal, rng, 0.02+0.45*p*p)
		if rng.Float64() < 0.3*p { // a whole digit drops out or lights fully
			i := rng.IntN(4)
			out.A[i] = 0x7f * byte(rng.IntN(2))
		}
		return out, true
	case t < tm.fade:
		return segNoise(rng, 0.45*(1-ease(ramp(tm.chaos, tm.fade)))), true
	case t < tm.dark:
		var out display.Frame
		if rng.Float64() < 0.07 { // one spark on the big digits
			out.A[rng.IntN(4)] = 1 << rng.IntN(7)
		}
		return out, true
	case t < tm.form:
		fs := tm.formStep
		if fs == 0 {
			fs = tm.step
		}
		rng := rand.New(rand.NewPCG(seed, uint64(t/fs)))
		from := tm.dark
		if tm.dark == 0 { // the first version forms straight out of the chaos
			from = tm.chaos
		}
		q := ease(ramp(from, tm.form))
		p := tm.formNoise
		if tm.formFades {
			p *= 1 - q
		}
		return blend(segNoise(rng, p), luna, rng, q), true
	case t < tm.hold:
		out := luna
		if rng.Float64() < 0.12 {
			out.A[rng.IntN(4)] ^= 1 << rng.IntN(7)
		}
		return out, true
	default:
		return blend(glitch(luna, rng, 0.08), normal, rng, ease(ramp(tm.hold, tm.length))), true
	}
}

// glitch flips each segment of f with probability p.
func glitch(f display.Frame, rng *rand.Rand, p float64) display.Frame {
	for i := range f.A {
		f.A[i] ^= randBits(rng, teaserMask.A[i], p)
		f.B[i] ^= randBits(rng, teaserMask.B[i], p)
	}
	return f
}

// segNoise lights each segment with probability p.
func segNoise(rng *rand.Rand, p float64) display.Frame {
	var f display.Frame
	for i := range f.A {
		f.A[i] = randBits(rng, teaserMask.A[i], p)
		f.B[i] = randBits(rng, teaserMask.B[i], p)
	}
	return f
}

// blend shows each segment as in to with probability q, else as in from.
func blend(from, to display.Frame, rng *rand.Rand, q float64) display.Frame {
	var f display.Frame
	for i := range f.A {
		m := randBits(rng, 0xff, q)
		f.A[i] = from.A[i]&^m | to.A[i]&m
		m = randBits(rng, 0xff, q)
		f.B[i] = from.B[i]&^m | to.B[i]&m
	}
	return f
}

// randBits sets each bit of mask with probability p.
func randBits(rng *rand.Rand, mask byte, p float64) byte {
	var b byte
	for k := 0; k < 8; k++ {
		if mask&(1<<k) != 0 && rng.Float64() < p {
			b |= 1 << k
		}
	}
	return b
}

func ease(x float64) float64 { return x * x * (3 - 2*x) }
