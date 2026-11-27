package fmradio

// The audio chain measured on synthetic stations (run: MEASURE=1 go test
// -run Measure -v): the frequency response with and without de-emphasis, the
// stereo crosstalk, the pilot left in the audio, and the 8x decimator's
// passband. Results: docs/notes.md (FM radio audio, for nitpickers).

import (
	"fmt"
	"math"
	"os"
	"testing"
)

// fmIQ: u8 I/Q at WideRate carrying the multiplex signal mpx(t) (1 = 75 kHz)
func fmIQwide(secs float64, mpx func(t float64) float64) []byte {
	n := int(secs * WideRate)
	b := make([]byte, 2*n)
	ph := 0.0
	for k := 0; k < n; k++ {
		t := float64(k) / WideRate
		ph += 2 * math.Pi * maxDev * mpx(t) / WideRate
		b[2*k] = byte(math.Round(127.5 + 100*math.Cos(ph)))
		b[2*k+1] = byte(math.Round(127.5 + 100*math.Sin(ph)))
	}
	return b
}

// amplitude of a sine at f in x (rate fs), least squares
func ampAt(x []float64, f, fs float64) float64 {
	var ss, cc, sc, xs, xc float64
	for i, v := range x {
		s, c := math.Sincos(2 * math.Pi * f * float64(i) / fs)
		ss += s * s
		cc += c * c
		sc += s * c
		xs += v * s
		xc += v * c
	}
	det := ss*cc - sc*sc
	a := (xs*cc - xc*sc) / det
	bb := (xc*ss - xs*sc) / det
	return math.Hypot(a, bb)
}

func deemph(f float64) float64 { // the digital 1-pole de-emphasis as Demod runs it
	alpha := 1 - math.Exp(-1/(OutRate*50e-6))
	z := complex(math.Cos(2*math.Pi*f/OutRate), -math.Sin(2*math.Pi*f/OutRate))
	h := complex(alpha, 0) / (1 - complex(1-alpha, 0)*z)
	return math.Hypot(real(h), imag(h))
}

func TestMeasureResponse(t *testing.T) {
	if os.Getenv("MEASURE") == "" {
		t.Skip("MEASURE=1")
	}
	freqs := []float64{100, 1000, 5000, 10000, 12000, 13000, 14000, 14500, 15000, 15500, 16000}
	const settle, secs = 1.5, 2.5
	run := func(stereo bool, f float64) (l, r []float64, d *Demod) {
		d = New(50e-6)
		iq := fmIQwide(secs, func(t float64) float64 {
			a := 0.3 * math.Sin(2*math.Pi*f*t)
			if !stereo {
				return a
			}
			// L only: L+R = a, L-R = a; pilot 9 %
			w := 2 * math.Pi * pilotHz * t
			return 0.45*a + 0.45*a*math.Sin(2*w) + 0.09*math.Sin(w)
		})
		out := d.ProcessWide(iq, nil)
		for i := int(settle * OutRate * 2); i+1 < len(out); i += 2 {
			l, r = append(l, float64(out[i])), append(r, float64(out[i+1]))
		}
		return
	}
	var ref, refS float64
	fmt.Println("f Hz | mono dB (de-emph removed) | stereo L dB | R (crosstalk) dB | with de-emph dB")
	for _, f := range freqs {
		l, _, _ := run(false, f)
		m := ampAt(l, f, OutRate) / deemph(f)
		ls, rs, d := run(true, f)
		sl, sr := ampAt(ls, f, OutRate)/deemph(f), ampAt(rs, f, OutRate)/deemph(f)
		if f == 1000 {
			ref, refS = m, sl
		}
		if ref == 0 {
			ref, refS = m, sl
		}
		fmt.Printf("%6.0f | %+6.2f | %+6.2f | %+6.1f | %+6.2f\n", f, 20*math.Log10(m/ref), 20*math.Log10(sl/refS), 20*math.Log10(sr/refS), 20*math.Log10(ampAt(l, f, OutRate)/ref))
		_ = d
	}
	// the pilot left in the audio (stereo, silence + pilot)
	d := New(50e-6)
	out := d.ProcessWide(fmIQwide(secs, func(t float64) float64 { return 0.09 * math.Sin(2*math.Pi*pilotHz*t) }), nil)
	var l []float64
	for i := int(settle * OutRate * 2); i+1 < len(out); i += 2 {
		l = append(l, float64(out[i]))
	}
	full := ampAt(func() (x []float64) { // a full-scale 1 kHz L+R reference: 0.9 of deviation
		ls, _, _ := run(false, 1000)
		return ls
	}(), 1000, OutRate) / 0.3 * 0.9
	fmt.Printf("pilot residue at 19 kHz: %.1f dB below full modulation (de-emph included)\n", 20*math.Log10(ampAt(l, pilotHz, OutRate)/full))
}

func TestMeasureDecimator(t *testing.T) {
	if os.Getenv("MEASURE") == "" {
		t.Skip("MEASURE=1")
	}
	var ref float64
	for _, f := range []float64{0, 10e3, 20e3, 30e3, 40e3, 50e3, 60e3, 70e3, 80e3, 90e3, 100e3, 110e3} {
		const n = WideRate / 4
		iq := make([]byte, 2*n)
		for k := range n {
			s, c := math.Sincos(2 * math.Pi * f * float64(k) / WideRate)
			iq[2*k], iq[2*k+1] = byte(math.Round(127.5+100*c)), byte(math.Round(127.5+100*s))
		}
		out := newDecimator8().process(iq)
		var re, im, cnt float64
		var ph []float64
		for k := len(out) / 4; k+1 < len(out); k += 2 {
			x := complex(float64(out[k]), float64(out[k+1]))
			s, c := math.Sincos(-2 * math.Pi * f * float64(k/2) / InRate)
			y := x * complex(c, s)
			re, im, cnt = re+real(y), im+imag(y), cnt+1
			_ = ph
		}
		a := math.Hypot(re, im) / cnt
		if f == 0 {
			ref = a
		}
		fmt.Printf("%6.0f kHz: %+7.3f dB\n", f/1e3, 20*math.Log10(a/ref))
	}
}
