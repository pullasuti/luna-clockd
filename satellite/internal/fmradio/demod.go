// Package fmradio turns 8-bit I/Q samples from an RTL-SDR stick into FM
// broadcast audio at 48 kHz, stereo, with the RDS station name.
//
// At 240 kS/s (the RTL2832U's low range, 5 x 48 kHz) the whole multiplex
// signal fits: L+R (0-15 kHz), the 19 kHz pilot, L-R on 38 kHz (23-53 kHz)
// and RDS on 57 kHz. Chain per sample: polar discriminator -> pilot PLL ->
// pilot subtracted -> L+R and L-R (x -2sin 2θ) through the same 151-tap
// low-pass FIR, decimated by 5 -> stereo blend by pilot strength ->
// de-emphasis (50 us Europe, 75 us Americas). RDS: rds.go.
package fmradio

import (
	"math"
)

const (
	InRate  = 240_000
	OutRate = 48_000
	decim   = InRate / OutRate
	maxDev  = 75_000 // Hz, FM broadcast deviation
	pilotHz = 19_000
)

// Demod is an FM stereo demodulator. Not safe for concurrent use.
type Demod struct {
	prevI, prevQ float64

	// the low-pass FIR is symmetric: half its taps, and each history kept
	// twice over (ring of n written at i and i+n) so a window is contiguous
	half       []float32 // taps 0..n/2 (the middle one last)
	n          int
	histM      []float32 // L+R
	histS      []float32 // L-R
	pos, phase int

	deL, deR, alpha float64

	// pilot PLL: theta is the pilot's phase, freq its offset from 19 kHz
	// (rad/sample), amp its amplitude (low-passed)
	theta, freq, amp float64
	perr             float64 // the phase detector's output, low-passed
	inv              float64 // 1 / (amp/2)
	blend            float64 // 0 mono .. 1 full stereo

	// DC of the multiplex signal: a stick whose crystal is off by a few
	// kHz shifts the whole station, which the discriminator turns into DC
	// (10 kHz = 13 % of full scale: a thump at tune and less headroom).
	// Tracked and removed per sample; OffsetHz reports it.
	dc     float64
	dcSeen int

	pilot, noise goertzel // for PilotDB (the station search)
	measure      bool     // run them (MeasurePilot)

	rds *rdsDecoder
	dec *decimator8 // ProcessWide
}

// New makes a demodulator with de-emphasis tau (50e-6 Europe, 75e-6 Americas).
func New(tau float64) *Demod {
	taps := lowpass(firCut, InRate, firTaps)
	d := &Demod{n: firTaps}
	for i := 0; i <= firTaps/2; i++ {
		d.half = append(d.half, float32(taps[i]))
	}
	d.histM = make([]float32, 2*firTaps)
	d.histS = make([]float32, 2*firTaps)
	d.alpha = 1 - math.Exp(-1/(OutRate*tau))
	d.pilot = newGoertzel(pilotHz, InRate)
	d.noise = newGoertzel(17_500, InRate)
	d.rds = newRDS()
	return d
}

const (
	// low-pass for L+R and L-R before decimating to 48 kHz: flat to ~15 kHz,
	// > 70 dB down from ~24 kHz (everything that would fold back into the
	// audio, 33 kHz and up, is gone). The pilot is subtracted before both
	// mixers, so the filter needn't reach 19 kHz: 151 taps instead of 255,
	// the biggest cost on the clock's A53.
	firTaps = 151
	firCut  = 19_500

	pllStep = 2 * math.Pi * pilotHz / InRate
	// second-order loop, ~20 Hz bandwidth, damping 0.7
	pllKp = 7.4e-4
	pllKi = 2.7e-7
	// pilot amplitude for full stereo / mono, in units of full deviation
	// (a broadcast pilot is ~0.09)
	blendFull = 0.06
	blendNone = 0.03

	dcFast = 1.0 / (0.004 * InRate)
	dcSlow = 1.0 / (0.5 * InRate)
)

// sideGain undoes the discriminator's droop on L-R. The discriminator
// averages the frequency over a sample (1/InRate): a sinc low-pass, nothing
// at 15 kHz (-0.06 dB) but -0.36 dB on the subcarrier at 38 kHz, which left
// L-R 4 % weak and the crosstalk at -34 dB (measured 2026-11-23).
var sideGain = func() float64 {
	x := math.Pi * 2 * pilotHz / InRate
	return x / math.Sin(x)
}()

// Process demodulates interleaved u8 I/Q at InRate and appends interleaved
// stereo samples (L, R; -1..1) at 48 kHz to out.
func (d *Demod) Process(iq []byte, out []float32) []float32 {
	for k := 0; k+1 < len(iq); k += 2 {
		out = d.sample(float64(iq[k])-127.5, float64(iq[k+1])-127.5, out)
	}
	return out
}

// ProcessWide is Process for u8 I/Q at WideRate (decimate.go).
func (d *Demod) ProcessWide(iq []byte, out []float32) []float32 {
	if d.dec == nil {
		d.dec = newDecimator8()
	}
	s := d.dec.process(iq)
	for k := 0; k+1 < len(s); k += 2 {
		out = d.sample(float64(s[k]), float64(s[k+1]), out)
	}
	return out
}

// sample takes one I/Q sample at InRate.
func (d *Demod) sample(i, q float64, out []float32) []float32 {
	const scale = float64(InRate) / (2 * math.Pi * maxDev)
	re := i*d.prevI + q*d.prevQ
	im := q*d.prevI - i*d.prevQ
	d.prevI, d.prevQ = i, q
	v := fastAtan2(im, re) * scale // the multiplex signal, 1 = 75 kHz

	// DC block: a fast start (~4 ms) so a tune doesn't thump, then ~0.5 s,
	// far below the audio (a 0.3 Hz high-pass)
	a := dcSlow
	if d.dcSeen < InRate/10 {
		d.dcSeen++
		a = dcFast
	}
	d.dc += a * (v - d.dc)
	v -= d.dc

	if d.measure {
		d.pilot.add(v)
		d.noise.add(v)
	}

	// pilot PLL: phase error from v x -sin θ, low-passed (the audio and
	// the subcarriers in v are many times the pilot: clipping them biased
	// the lock by ~30°), normalised by the pilot's amplitude
	// (per-sample hot path: no division, plain comparisons instead of
	// min/max, whose NaN handling cost more than the filter)
	s, c := sincos(d.theta)
	d.amp += 0.0005 * (2*v*c - d.amp)
	d.perr += 0.05 * (-v*s - d.perr)
	if d.phase == 0 { // the amplitude moves slowly: its reciprocal every 5 samples
		a := d.amp / 2
		if a < 0.005 {
			a = 0.005
		}
		d.inv = 1 / a
	}
	e := d.perr * d.inv
	if e > 4 {
		e = 4
	} else if e < -4 {
		e = -4
	}
	d.freq += pllKi * e
	if d.freq > 0.002 { // +-76 Hz
		d.freq = 0.002
	} else if d.freq < -0.002 {
		d.freq = -0.002
	}
	d.theta += pllStep + d.freq + pllKp*e
	wrapped := false
	if d.theta >= 2*math.Pi {
		d.theta -= 2 * math.Pi
		wrapped = true
	} else if d.theta < 0 {
		d.theta += 2 * math.Pi
	}

	// L+R without the pilot; L-R from the 38 kHz subcarrier. The standard
	// sends the pilot as sin ωt and L-R on sin 2ωt; the loop locks cos θ
	// to the pilot (θ = ωt - π/2), so sin 2ωt = -sin 2θ
	vp := v - d.amp*c                        // without the pilot
	m, sd := float32(vp), float32(-4*vp*s*c) // x -2 sin 2θ
	d.histM[d.pos], d.histM[d.pos+d.n] = m, m
	d.histS[d.pos], d.histS[d.pos+d.n] = sd, sd

	// RDS on 57 kHz: x cos 3θ and x sin 3θ, summed per pilot cycle
	c3 := c * (4*c*c - 3)
	s3 := s * (3 - 4*s*s)
	d.rds.add(v*c3, v*s3, wrapped)

	d.pos++
	if d.pos == d.n {
		d.pos = 0
	}
	d.phase++
	if d.phase < decim {
		return out
	}
	d.phase = 0
	// the oldest sample is at pos: the window is hist[pos : pos+n]
	wm, ws := d.histM[d.pos:d.pos+d.n], d.histS[d.pos:d.pos+d.n]
	last := d.n - 1
	var fm, fs float32
	for j, t := range d.half[:len(d.half)-1] {
		fm += t * (wm[j] + wm[last-j])
		fs += t * (ws[j] + ws[last-j])
	}
	mid := d.n / 2
	fm += d.half[mid] * wm[mid]
	fs += d.half[mid] * ws[mid]
	am, as := float64(fm), float64(fs)*sideGain
	// stereo only with a clear pilot, faded in and out over ~0.2 s
	want := (d.amp - blendNone) / (blendFull - blendNone)
	if want < 0 {
		want = 0
	} else if want > 1 {
		want = 1
	}
	d.blend += 0.0001 * (want - d.blend)
	as *= d.blend
	d.deL += d.alpha * (am + as - d.deL)
	d.deR += d.alpha * (am - as - d.deR)
	out = append(out, float32(d.deL), float32(d.deR))
	return out
}

// Stereo reports whether the station plays in stereo (more than half blended in).
func (d *Demod) Stereo() bool { return d.blend > 0.5 }

// StationName is the RDS programme service name ("" until received twice
// alike) and the programme identification (0 = none yet).
func (d *Demod) StationName() (name string, pi uint16) { return d.rds.name, d.rds.pi }

// OffsetHz is how far the station sits from the tuned frequency (the
// stick's crystal error plus the tuner's rounding), from the removed DC.
func (d *Demod) OffsetHz() float64 { return d.dc * maxDev }

// MeasurePilot switches on the pilot measurement for PilotDB (the station
// search; playing doesn't need it).
func (d *Demod) MeasurePilot() { d.measure = true }

// PilotDB is the 19 kHz stereo pilot over the band next to it (17.5 kHz) since
// the last call, in dB: a stereo station shows 20-60 dB, noise about 0.
func (d *Demod) PilotDB() float64 {
	p, n := d.pilot.power(), d.noise.power()
	d.pilot.reset()
	d.noise.reset()
	return 10 * math.Log10((p+1e-12)/(n+1e-12))
}

// sincos from a table (1024 steps a turn, linear in between: ~5e-6 off).
func sincos(th float64) (s, c float64) {
	x := th * (sinN / (2 * math.Pi))
	i := int(x)
	f := x - float64(i)
	i &= sinN - 1
	s = sinTab[i] + f*(sinTab[i+1]-sinTab[i])
	j := (i + sinN/4) & (sinN - 1)
	c = sinTab[j] + f*(sinTab[j+1]-sinTab[j])
	return
}

const sinN = 1024

var sinTab = func() (t [sinN + 1]float64) {
	for i := range t {
		t[i] = math.Sin(2 * math.Pi * float64(i) / sinN)
	}
	return
}()

// fastAtan2 is atan2 within ~0.0015 rad (the 8-bit samples resolve less).
func fastAtan2(y, x float64) float64 {
	ax, ay := math.Abs(x), math.Abs(y)
	if ax == 0 && ay == 0 {
		return 0
	}
	var a float64
	if ax >= ay {
		r := ay / ax
		a = r * (math.Pi/4 + (1-r)*(0.2447+0.0663*r))
	} else {
		r := ax / ay
		a = math.Pi/2 - r*(math.Pi/4+(1-r)*(0.2447+0.0663*r))
	}
	if x < 0 {
		a = math.Pi - a
	}
	if y < 0 {
		a = -a
	}
	return a
}

// lowpass is a windowed-sinc (Blackman) FIR, unity gain at DC.
func lowpass(cut, rate float64, n int) []float64 {
	t := make([]float64, n)
	m := float64(n - 1)
	var sum float64
	for i := range t {
		x := float64(i) - m/2
		s := 2 * cut / rate
		if x != 0 {
			s = math.Sin(2*math.Pi*cut/rate*x) / (math.Pi * x)
		}
		w := 0.42 - 0.5*math.Cos(2*math.Pi*float64(i)/m) + 0.08*math.Cos(4*math.Pi*float64(i)/m)
		t[i] = s * w
		sum += t[i]
	}
	for i := range t {
		t[i] /= sum
	}
	return t
}

type goertzel struct {
	c, s1, s2 float64
	n         int
}

func newGoertzel(f, rate float64) goertzel { return goertzel{c: 2 * math.Cos(2*math.Pi*f/rate)} }

func (g *goertzel) add(x float64) {
	s := x + g.c*g.s1 - g.s2
	g.s2, g.s1 = g.s1, s
	g.n++
}

func (g *goertzel) power() float64 {
	if g.n == 0 {
		return 0
	}
	return (g.s1*g.s1 + g.s2*g.s2 - g.c*g.s1*g.s2) / float64(g.n*g.n)
}

func (g *goertzel) reset() { g.s1, g.s2, g.n = 0, 0, 0 }
