package output

import (
	"math"
	"testing"
)

// Every sleep sound comes out at about the same level, finite and in range,
// and fades in from silence.
func TestNoiseLevels(t *testing.T) {
	for _, s := range NoiseSounds {
		z := NewNoise(s.ID, 42)
		buf := make([]float32, 2*Rate) // 1 s stereo
		var first float64
		var sum float64
		var n int
		for sec := range 30 {
			z.Read(buf)
			for i := 0; i < len(buf); i += 2 {
				v := float64(buf[i])
				if math.IsNaN(v) || v > 1 || v < -1 || buf[i] != buf[i+1] {
					t.Fatalf("%s: bad sample %v / %v", s.ID, buf[i], buf[i+1])
				}
				if sec == 0 && i < 2*Rate/100 {
					first = max(first, math.Abs(v))
				}
				if sec >= 5 {
					sum += v * v
					n++
				}
			}
		}
		rms := math.Sqrt(sum / float64(n))
		db := 20 * math.Log10(rms/noiseRMS)
		t.Logf("%-6s rms %.4f (%+.1f dB), exact gain %.4g", s.ID, rms, db, noiseGains[s.ID]*noiseRMS/rms)
		if math.Abs(db) > 3 {
			t.Errorf("%s: level %+.1f dB off the target", s.ID, db)
		}
		if first > 0.01 {
			t.Errorf("%s: no fade-in (first 10 ms peak %.3f)", s.ID, first)
		}
	}
}

// One second of each sound: the mixer pulls this in real time on an A53.
func BenchmarkNoiseSecond(b *testing.B) {
	for _, s := range NoiseSounds {
		b.Run(s.ID, func(b *testing.B) {
			z := NewNoise(s.ID, 1)
			buf := make([]float32, 2*Rate)
			for b.Loop() {
				z.Read(buf)
			}
		})
	}
}
