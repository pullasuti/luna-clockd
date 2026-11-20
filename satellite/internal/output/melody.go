package output

import "math"

// Timbre shapes the notes of a Melody.
type Timbre int

const (
	// Bell: struck, decays freely and rings on under the next notes.
	Bell Timbre = iota
	// Flute: soft attack, mostly the fundamental, ends with the note.
	Flute
	// Brass: slower attack and bright harmonics, ends with the note.
	Brass
	// Legato: a flute whose notes flow into each other (no gaps).
	Legato
	// SoftBell: a small, gentle bell: rounder than Bell (fewer, quieter
	// upper partials, a softer strike), for chimes.
	SoftBell
	// Wood: a soft wooden knock, short and low.
	Wood
)

type timbreSpec struct {
	partials [][2]float64 // (frequency ratio, amplitude)
	attack   float64      // seconds
	decay    float64      // exponential time constant (0 = sustained)
	ring     float64      // extra seconds a note sounds after its slot
	release  float64      // seconds
}

var timbres = map[Timbre]timbreSpec{
	// a softened church-bell spectrum: hum-ish fundamental, octave, a slightly
	// inharmonic "tierce" and a quiet upper partial
	Bell:     {partials: [][2]float64{{1, 1}, {2, 0.45}, {2.4, 0.18}, {3, 0.22}, {4.2, 0.08}}, attack: 0.004, decay: 0.55, ring: 1.0, release: 0.08},
	Flute:    {partials: [][2]float64{{1, 1}, {2, 0.12}, {3, 0.04}}, attack: 0.04, release: 0.06},
	Legato:   {partials: [][2]float64{{1, 1}, {2, 0.12}, {3, 0.04}}, attack: 0.07, ring: 0.09, release: 0.12},
	SoftBell: {partials: [][2]float64{{1, 1}, {2, 0.22}, {3, 0.05}}, attack: 0.012, decay: 0.9, ring: 1.6, release: 0.15},
	Wood:     {partials: [][2]float64{{1, 1}, {2.45, 0.18}}, attack: 0.002, decay: 0.035, ring: 0.15, release: 0.02},
	Brass:    {partials: [][2]float64{{1, 1}, {2, 0.6}, {3, 0.42}, {4, 0.25}, {5, 0.14}, {6, 0.07}}, attack: 0.06, release: 0.09},
}

// Melody synthesises a tune: each note is (frequency Hz, duration ms), a
// frequency of 0 is a rest. The result is normalised so its peak is db dBFS.
func Melody(db float64, t Timbre, notes ...[2]float64) []float32 {
	sp := timbres[t]
	total := 0.0
	for _, n := range notes {
		total += n[1] / 1000
	}
	frames := int((total + sp.ring + sp.release) * Rate)
	mono := make([]float64, frames)
	start := 0.0
	for _, n := range notes {
		dur := n[1] / 1000
		if n[0] > 0 {
			length := dur + sp.ring
			s0 := int(start * Rate)
			for i := 0; i < int(length*Rate) && s0+i < frames; i++ {
				x := float64(i) / Rate
				env := 1.0
				if x < sp.attack {
					env = x / sp.attack
				}
				if sp.decay > 0 {
					env *= math.Exp(-x / sp.decay)
				}
				if left := length - x; left < sp.release {
					env *= left / sp.release
				}
				var v float64
				for k, p := range sp.partials {
					f := n[0] * p[0]
					if f >= Rate/2-1000 {
						continue
					}
					a := p[1]
					if sp.decay > 0 && k > 0 { // upper partials of a bell die away first
						a *= math.Exp(-x * float64(k) / (3 * sp.decay))
					}
					v += a * math.Sin(2*math.Pi*f*x)
				}
				mono[s0+i] += env * v
			}
		}
		start += dur
	}
	peak := 0.0
	for _, v := range mono {
		peak = math.Max(peak, math.Abs(v))
	}
	g := 0.0
	if peak > 0 {
		g = math.Pow(10, db/20) / peak
	}
	out := make([]float32, 2*frames)
	for i, v := range mono {
		out[2*i], out[2*i+1] = float32(v*g), float32(v*g)
	}
	return out
}
