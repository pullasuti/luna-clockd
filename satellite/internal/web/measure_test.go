package web

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"testing"
)

// TestMeasureResample: the 48 -> 32 kHz resampler's passband and what folds
// back (run: MEASURE=1 go test -run MeasureResample -v; docs/notes.md).
func TestMeasureResample(t *testing.T) {
	if os.Getenv("MEASURE") == "" {
		t.Skip("MEASURE=1")
	}
	amp := func(x []int16, f, fs float64) float64 {
		var ss, cc, sc, xs, xc float64
		for i, v := range x {
			s, c := math.Sincos(2 * math.Pi * f * float64(i) / fs)
			ss, cc, sc, xs, xc = ss+s*s, cc+c*c, sc+s*c, xs+float64(v)*s, xc+float64(v)*c
		}
		det := ss*cc - sc*sc
		return math.Hypot((xs*cc-xc*sc)/det, (xc*ss-xs*sc)/det)
	}
	run := func(f float64) (in float64, out []int16) {
		const n = 48000 * 2
		pcm := make([]byte, 4*n)
		in = 16000
		for i := range n {
			v := int16(math.Round(in * math.Sin(2*math.Pi*f*float64(i)/48000)))
			binary.LittleEndian.PutUint16(pcm[4*i:], uint16(v))
			binary.LittleEndian.PutUint16(pcm[4*i+2:], uint16(v))
		}
		rs := newResampler32()
		l, _ := rs.process(pcm, nil, nil)
		return in, l[3200:]
	}
	for _, f := range []float64{1000, 10000, 13000, 14000, 14500, 15000, 15500, 16000, 17000, 19000, 20000} {
		in, out := run(f)
		pass := amp(out, f, 32000) / in
		alias := 32000 - f
		s := fmt.Sprintf("%6.0f Hz: %+7.2f dB", f, 20*math.Log10(pass))
		if f > 16000 {
			s = fmt.Sprintf("%6.0f Hz: folds to %5.0f Hz at %+6.1f dB", f, alias, 20*math.Log10(amp(out, alias, 32000)/in))
		}
		fmt.Println(s)
	}
}
