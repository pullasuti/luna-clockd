package output

import (
	"math"
	"math/rand/v2"
)

// Sleep sounds: generated noise, no recordings. Every sound is made sample by
// sample from a seeded random source, so it never loops audibly and costs a
// few multiplies per sample. The speaker is mono (left + right summed), so the
// same signal goes to both channels.

// NoiseSound describes one generated sound for the UI and Home Assistant.
type NoiseSound struct {
	ID, Name string
}

// NoiseSounds lists the sleep sounds in display order.
var NoiseSounds = []NoiseSound{
	{"rain", "Rain"},
	{"rain_tin", "Rain on a tin roof"},
	{"rain_felt", "Rain on a felt roof"},
	{"waves", "Waves"},
	{"pink", "Pink noise"},
	{"brown", "Brown noise"},
	{"white", "White noise"},
}

// NoiseKnown reports whether id is a sleep sound.
func NoiseKnown(id string) bool {
	for _, s := range NoiseSounds {
		if s.ID == id {
			return true
		}
	}
	return false
}

// noiseRMS is the level every sound is calibrated to (about -20 dBFS): the
// user's volume does the rest, as for music.
const noiseRMS = 0.1

// onePole is a one-pole low-pass filter.
type onePole struct{ a, y float64 }

func lowPass(hz float64) onePole { return onePole{a: 1 - math.Exp(-2*math.Pi*hz/Rate)} }

func (f *onePole) step(x float64) float64 { f.y += f.a * (x - f.y); return f.y }

// pinkFilter turns white noise into pink (-3 dB/octave); Paul Kellet's
// refined filter, accurate to ±0.05 dB above 9 Hz.
type pinkFilter struct{ b0, b1, b2, b3, b4, b5, b6 float64 }

func (p *pinkFilter) step(w float64) float64 {
	p.b0 = 0.99886*p.b0 + w*0.0555179
	p.b1 = 0.99332*p.b1 + w*0.0750759
	p.b2 = 0.96900*p.b2 + w*0.1538520
	p.b3 = 0.86650*p.b3 + w*0.3104856
	p.b4 = 0.55000*p.b4 + w*0.5329522
	p.b5 = -0.7616*p.b5 - w*0.0168980
	out := p.b0 + p.b1 + p.b2 + p.b3 + p.b4 + p.b5 + p.b6 + w*0.5362
	p.b6 = w * 0.115926
	return out
}

// noiseGen makes one sound, sample by sample, before level calibration.
type noiseGen struct {
	kind string
	rng  *rand.Rand
	pink pinkFilter
	brn  float64 // brown: leaky integrator state
	hp   onePole // rain: low-pass whose output is subtracted = high-pass

	// rain: the current drop and the slow level drift
	drop, dropDecay float64
	drift, driftTo  float64
	driftN          int

	wv waveState // waves
	rf roofState // rain on a roof
}

func newNoiseGen(kind string, seed uint64) *noiseGen {
	g := &noiseGen{kind: kind, rng: rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))}
	g.hp = lowPass(500)
	g.drift, g.driftTo = 1, 1
	return g
}

func (g *noiseGen) white() float64 { return g.rng.NormFloat64() }

func (g *noiseGen) next() float64 {
	switch g.kind {
	case "white":
		return g.white()
	case "pink":
		return g.pink.step(g.white())
	case "brown":
		g.brn = (g.brn + 0.02*g.white()) / 1.02
		return g.brn
	case "rain":
		return g.rain()
	case "waves":
		return g.waves()
	case "rain_tin", "rain_felt":
		return g.roof()
	}
	return 0
}

// rain: a steady high-passed hiss (the far-off rain) with drops on top, and a
// slow random level drift so it never sounds like a machine.
func (g *noiseGen) rain() float64 {
	w := g.white()
	p := g.pink.step(w)
	hiss := p - g.hp.step(p) // high-pass ~500 Hz
	if g.driftN--; g.driftN <= 0 {
		g.driftTo = 0.75 + 0.5*g.rng.Float64()
		g.driftN = Rate * (2 + g.rng.IntN(4))
	}
	g.drift += (g.driftTo - g.drift) * 2e-5
	// drops: on average 35 a second, each a short burst of noise decaying in
	// 2-10 ms, sizes from an exponential spread (many small, few big)
	if g.rng.Float64() < 35.0/Rate {
		size := g.rng.ExpFloat64() * 0.9
		g.drop = max(g.drop, size)
		g.dropDecay = math.Exp(-1 / (Rate * (0.002 + 0.008*g.rng.Float64())))
	}
	d := g.drop * w
	g.drop *= g.dropDecay
	return g.drift * (hiss*0.8 + d*0.6)
}

// waves: surf in layers whose levels follow each wave; the filters stay put
// (a cutoff sweeping with the swell sounded like a synth "whoosh").
//   - rumble: distant surf, brown noise, always there
//   - body:   the swell, low-passed pink noise rising to the crest
//   - break:  the wave spilling over at the crest, a soft swell of mid-band
//     noise (300 Hz-2.5 kHz) rising in ~0.3 s and dying away over a second
//   - wash:   the water running back, a quiet steady mid-band hiss fading over
//     a few seconds
//
// Everything here moves smoothly. Fast amplitude changes on bright noise (a
// "turbulent" crash, clicky bubbles) sounded like crumpling paper, the
// opposite of calming: keep it that way.
//
// Waves differ in length (5-13 s), size and where they break; now and then a
// small one comes between the big ones.
func (g *noiseGen) waves() float64 {
	w := &g.wv
	if w.pos >= w.len {
		w.newWave(g.rng)
	}
	if w.pos&63 == 0 { // envelopes move slowly: update every 64 samples
		w.envelopes()
	}
	w.pos++
	u1 := g.rng.Float64()*2 - 1 // independent noises for the layers
	u2 := g.rng.Float64()*2 - 1
	n := g.white()

	g.brn = (g.brn + 0.02*n) / 1.02
	rumble := 3 * g.brn

	p := g.pink.step(n)
	body := w.bodyLP2.step(w.bodyLP.step(p))

	// break: noise band-passed to 300 Hz-2.5 kHz
	bh := u1 - w.brkHP.step(u1)
	brk := w.brkLP2.step(w.brkLP.step(bh))

	// wash: noise band-passed to 500 Hz-2 kHz
	wb := u2 - w.washHP.step(u2)
	wash := w.washLP2.step(w.washLP.step(wb))

	// each layer scaled to unit RMS (layerNorm), then mixed by its envelope
	w.layer = [4]float64{rumble * layerNorm[0], body * layerNorm[1], brk * layerNorm[2], wash * layerNorm[3]}
	return w.eRumble*w.layer[0] + w.eBody*w.layer[1] + w.eBreak*w.layer[2] + w.eWash*w.layer[3]
}

// layerNorm brings the waves' layers (rumble, body, break, wash) to unit RMS,
// so the envelopes alone set the mix (measured, see noise_test.go).
var layerNorm = [4]float64{3.34, 0.443, 6.85, 8.67}

// waveState is the waves generator's own state.
type waveState struct {
	pos, len int     // samples into the wave, its length
	crest    float64 // seconds from the start to the crest (where it breaks)
	size     float64 // 0-1: how big this wave is
	floor    float64 // the body's level between waves

	eRumble, eBody, eBreak, eWash float64 // current layer levels

	bodyLP, bodyLP2 onePole
	brkHP           onePole
	brkLP, brkLP2   onePole
	washHP          onePole
	washLP, washLP2 onePole
	layer           [4]float64 // the last sample of each layer (tests)
	inited          bool
}

func (w *waveState) newWave(rng *rand.Rand) {
	if !w.inited {
		w.bodyLP, w.bodyLP2 = lowPass(550), lowPass(550)
		w.brkHP = lowPass(300)
		w.brkLP, w.brkLP2 = lowPass(2500), lowPass(2500)
		w.washHP = lowPass(500)
		w.washLP, w.washLP2 = lowPass(2000), lowPass(2000)
		w.inited = true
	}
	small := rng.Float64() < 0.25
	if small {
		w.size = 0.3 + 0.2*rng.Float64()
		w.len = Rate * (5 + rng.IntN(3))
	} else {
		w.size = 0.65 + 0.35*rng.Float64()
		w.len = Rate * (8 + rng.IntN(6))
	}
	w.crest = float64(w.len) / Rate * (0.28 + 0.12*rng.Float64())
	w.floor = 0.12 + 0.1*rng.Float64()
	w.pos = 0
}

func (w *waveState) envelopes() {
	t := float64(w.pos) / Rate // seconds into the wave
	end := float64(w.len) / Rate
	var body float64
	if t < w.crest {
		body = w.floor + (w.size-w.floor)*math.Pow(math.Sin(t/w.crest*math.Pi/2), 2)
	} else {
		fall := (end - w.crest) / 3
		body = w.floor + (w.size-w.floor)*math.Exp(-(t-w.crest)/fall)
	}
	w.eBody = body
	w.eRumble = 0.35 + 0.15*body
	w.eBreak, w.eWash = 0, 0
	if dt := t - w.crest + 0.3; dt > 0 { // the break swells in around the crest
		att := smooth(dt / 0.3)
		w.eBreak = 0.8 * w.size * w.size * att * math.Exp(-dt/1.0)
	}
	if dt := t - w.crest - 0.3; dt > 0 { // then the water runs back
		att := smooth(dt / 0.8)
		w.eWash = 0.3 * w.size * att * math.Exp(-dt/2.2)
	}
}

// resonator is a two-pole band-pass that rings at hz and dies away with time
// constant tau (seconds): a struck surface is a few of these.
type resonator struct{ c1, c2, g, y1, y2 float64 }

func newResonator(hz, tau float64) resonator {
	r := math.Exp(-1 / (tau * Rate))
	return resonator{c1: 2 * r * math.Cos(2*math.Pi*hz/Rate), c2: -r * r, g: 1 - r}
}

func (q *resonator) step(x float64) float64 {
	y := q.c1*q.y1 + q.c2*q.y2 + q.g*x
	q.y2, q.y1 = q.y1, y
	return y
}

// Rain on a roof, heard from inside the house: every drop strikes the roof,
// which rings in its own way, and the ceiling takes off the highs.
//   - tin:  corrugated metal sheets; each sheet a few inharmonic modes
//     (150-1500 Hz) ringing 25-110 ms, so drops patter with a metallic drum
//     tone, over the dense roar of the rain on the metal.
//     Settled on this first version after six other tries; the dead ends are
//     in "Engineering Notes.md" (2026-11-14, rain on a tin roof).
//   - felt: bitumen felt on boards; drops are soft dull taps of the boards
//     (100-450 Hz, 10-25 ms) over a low murmur, everything below ~1 kHz
//
// The rain comes and goes in slow gusts (15-45 s), drop rate and loudness
// together. Nothing bright changes fast (see waves).
type roofState struct {
	tin           bool
	modes         []resonator // all panels' modes, panel after panel
	perPanel      int
	rate          float64 // drops a second at gust level 1
	gust, gustTo  float64
	gustN         int
	bedHP         onePole
	bedLP, bedLP2 onePole
	outLP, outLP2 onePole
	exc           []float64 // this sample's strike per mode
	layer         [2]float64
	inited        bool
}

// roofNorm brings the roof layers (drops, bed) to unit RMS, per roof.
var roofNorm = map[string][2]float64{"rain_tin": {45, 0.772}, "rain_felt": {10.9, 0.711}}

func (g *noiseGen) roof() float64 {
	r := &g.rf
	if !r.inited {
		r.inited = true
		r.tin = g.kind == "rain_tin"
		r.gust, r.gustTo = 1, 1
		if r.tin {
			// three sheets, each with its own pitch; plate-like mode ratios
			ratios := []float64{1, 2.32, 3.91, 5.6}
			for range 3 {
				f0 := 160 + 120*g.rng.Float64()
				for k, q := range ratios {
					f := f0 * q * (0.97 + 0.06*g.rng.Float64())
					tau := 0.11 / (1 + 0.6*float64(k))
					r.modes = append(r.modes, newResonator(f, tau))
				}
			}
			r.perPanel = len(ratios)
			r.rate = 220
			r.bedHP, r.bedLP, r.bedLP2 = lowPass(250), lowPass(2200), lowPass(2200)
			r.outLP, r.outLP2 = lowPass(3200), lowPass(3200)
		} else {
			ratios := []float64{1, 2.7}
			for range 4 {
				f0 := 100 + 60*g.rng.Float64()
				for k, q := range ratios {
					r.modes = append(r.modes, newResonator(f0*q*(0.95+0.1*g.rng.Float64()), 0.025/(1+float64(k))))
				}
			}
			r.perPanel = len(ratios)
			r.rate = 380
			r.bedHP, r.bedLP, r.bedLP2 = lowPass(60), lowPass(700), lowPass(700)
			r.outLP, r.outLP2 = lowPass(1000), lowPass(1000)
		}
		r.exc = make([]float64, len(r.modes))
	}
	// gusts: a new target every 15-45 s, approached smoothly
	if r.gustN--; r.gustN <= 0 {
		r.gustTo = 0.55 + 0.7*g.rng.Float64()
		r.gustN = Rate * (15 + g.rng.IntN(31))
	}
	r.gust += (r.gustTo - r.gust) * 1e-5

	// a drop: strikes one panel, each of its modes by a random amount
	if g.rng.Float64() < r.rate*r.gust/Rate {
		size := min(g.rng.ExpFloat64(), 4)
		p := g.rng.IntN(len(r.modes) / r.perPanel)
		for k := range r.perPanel {
			r.exc[p*r.perPanel+k] = size * (0.3 + g.rng.Float64())
		}
	}
	var drops float64
	for i := range r.modes {
		drops += r.modes[i].step(r.exc[i])
		r.exc[i] = 0
	}
	drops = r.outLP2.step(r.outLP.step(drops))

	n := g.pink.step(g.white())
	bed := r.bedLP2.step(r.bedLP.step(n - r.bedHP.step(n)))

	nm := roofNorm[g.kind]
	r.layer = [2]float64{drops * nm[0], bed * nm[1]}
	lvl := 0.6 + 0.4*r.gust // louder when it pours
	if r.tin {
		return lvl * (0.85*r.layer[0] + 0.55*r.layer[1])
	}
	return lvl * (0.6*r.layer[0] + 0.8*r.layer[1])
}

// smooth is a 0-1 S-curve over x in 0-1 (no corner where an envelope starts).
func smooth(x float64) float64 {
	x = min(max(x, 0), 1)
	return x * x * (3 - 2*x)
}

// Noise is a running sleep sound.
type Noise struct {
	gen  *noiseGen
	gain float64
	n    int // samples made so far (for the fade-in)
}

// NewNoise starts sound id (one of NoiseSounds) with the given random seed.
func NewNoise(id string, seed uint64) *Noise {
	return &Noise{gen: newNoiseGen(id, seed), gain: noiseGains[id]}
}

// noiseGains bring every sound to noiseRMS. Measured from the generators
// (noise_test.go checks them with another seed): run TestNoiseLevels -v after
// changing a generator and update the gain here.
var noiseGains = map[string]float64{
	"rain":      0.05843,
	"waves":     0.13,
	"rain_tin":  0.102,
	"rain_felt": 0.104,
	"pink":      0.03262,
	"brown":     0.9999,
	"white":     0.1,
}

// softClip leaves the signal alone up to ±0.6 and bends the rare peaks above
// that smoothly towards ±1 (rare noise peaks), instead of
// clipping them hard.
func softClip(v float64) float64 {
	a := math.Abs(v)
	if a <= 0.6 {
		return v
	}
	return math.Copysign(0.6+0.4*math.Tanh((a-0.6)/0.4), v)
}

// fadeIn is how long a sound takes to come up from silence.
const fadeIn = 3 * Rate

// Read fills dst with interleaved stereo; a sleep sound never ends.
func (z *Noise) Read(dst []float32) (int, bool) {
	for i := 0; i+1 < len(dst); i += 2 {
		v := z.gen.next() * z.gain
		if z.n < fadeIn {
			f := float64(z.n) / fadeIn
			v *= f * f
			z.n++
		}
		s := float32(softClip(v))
		dst[i], dst[i+1] = s, s
	}
	return len(dst) &^ 1, true
}
