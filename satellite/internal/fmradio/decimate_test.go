package fmradio

import (
	"math"
	"math/rand"
	"testing"
)

// complexTone is u8 I/Q of a tone at f (Hz, may be negative) at WideRate,
// dithered (a plain 8-bit tone has quantisation spurs near -55 dB that land
// anywhere, the channel included).
func complexTone(f float64, n int, amp float64) []byte {
	iq := make([]byte, 2*n)
	rng := rand.New(rand.NewSource(1))
	for k := 0; k < n; k++ {
		a := 2 * math.Pi * f * float64(k) / WideRate
		iq[2*k] = byte(math.Round(127.5 + amp*math.Cos(a) + rng.Float64() - rng.Float64()))
		iq[2*k+1] = byte(math.Round(127.5 + amp*math.Sin(a) + rng.Float64() - rng.Float64()))
	}
	return iq
}

// level of a complex tone at f in I/Q at rate, dB relative to amplitude amp
func complexLevel(i, q []float32, f, rate, amp float64) float64 {
	var re, im float64
	for k := range i {
		a := -2 * math.Pi * f * float64(k) / rate
		c, s := math.Cos(a), math.Sin(a)
		re += float64(i[k])*c - float64(q[k])*s
		im += float64(i[k])*s + float64(q[k])*c
	}
	return 20 * math.Log10(math.Hypot(re, im)/float64(len(i))/amp+1e-12)
}

// the channel passes flat; everything that would fold into it is gone
func TestDecimator8(t *testing.T) {
	const n = WideRate / 4 // 0.25 s
	for _, c := range []struct {
		f, at    float64 // input tone, where it lands at InRate
		min, max float64 // dB
	}{
		{20e3, 20e3, -0.3, 0.3}, {-80e3, -80e3, -0.3, 0.3}, {100e3, 100e3, -0.6, 0.3},
		{160e3, -80e3, -200, -60}, {-150e3, 90e3, -200, -60}, // the last stage's stopband
		{400e3, -80e3, -200, -60}, {600e3, 120e3, -200, -60}, {-900e3, 60e3, -200, -60},
	} {
		d := newDecimator8()
		var i, q []float32
		iq := complexTone(c.f, n, 100)
		for k := 0; k < len(iq); k += 8190 { // blocks that split samples' I and Q too
			s := d.process(iq[k:min(k+8190, len(iq))])
			for j := 0; j+1 < len(s); j += 2 {
				i, q = append(i, float32(s[j])/64), append(q, float32(s[j+1])/64)
			}
		}
		if len(i) != n/8 {
			t.Fatalf("%d samples out, want %d", len(i), n/8)
		}
		if lv := complexLevel(i[200:], q[200:], c.at, InRate, 100); lv < c.min || lv > c.max {
			t.Errorf("%+.0f kHz (at %+.0f kHz): %.1f dB, want %.1f..%.1f", c.f/1e3, c.at/1e3, lv, c.min, c.max)
		}
	}
}

// a station with a stronger one 600 kHz away: through the wide path it plays
// as if alone (at 240 kS/s straight from the chip the neighbour would fold in)
func TestDemodWide(t *testing.T) {
	const secs = 1.5
	n := int(secs * WideRate)
	tone := func(t float64) float64 { return 0.5 * math.Sin(2*math.Pi*1000*t) }
	mpx := stereoMPX(tone, silent, nil)
	iq := make([]byte, 2*n)
	var ph, ph2 float64
	for k := 0; k < n; k++ {
		tt := float64(k) / WideRate
		ph += 2 * math.Pi * maxDev * mpx(tt) / WideRate
		ph2 += 2 * math.Pi * (600e3 + maxDev*math.Sin(2*math.Pi*3000*tt)) / WideRate
		iq[2*k] = byte(127.5 + 35*math.Cos(ph) + 60*math.Cos(ph2))
		iq[2*k+1] = byte(127.5 + 35*math.Sin(ph) + 60*math.Sin(ph2))
	}
	d := New(50e-6)
	var out []float32
	for k := 0; k < len(iq); k += 65536 {
		out = d.ProcessWide(iq[k:min(k+65536, len(iq))], out)
	}
	if len(out) < 2*OutRate*14/10 {
		t.Fatalf("%d values for %.1f s", len(out), secs)
	}
	if !d.Stereo() {
		t.Fatal("not in stereo")
	}
	out = out[OutRate:]
	l, r := toneDB(out, 0, 1000), toneDB(out, 1, 1000)
	if l < -9 || l > -5 || l-r < 25 {
		t.Errorf("L %.1f dB, R %.1f dB: want L about -7 and 25 dB of separation", l, r)
	}
	if x := toneDB(out, 0, 3000); x > -40 {
		t.Errorf("the neighbour's 3 kHz tone at %.1f dB", x)
	}
}
