package web

import (
	"encoding/binary"
	"math"
)

// resampler32 turns 48 kHz stereo into 32 kHz for the radio's FLAC stream: FM
// audio stops at 15 kHz, so 32 kHz keeps all of it. Polyphase 2/3: a Kaiser
// windowed-sinc low-pass on the 96 kHz grid (flat to ~14.5 kHz, ~50 dB down
// from 17 kHz, so what folds back lands above 15 kHz), two phases of
// rsTaps taps each.
type resampler32 struct {
	phase [2][]float32 // phase p: taps h[p], h[p+2], ...
	l, r  []float32    // input history (the last rsTaps samples kept)
	pos   int          // next output's position on the 96 kHz grid, relative to l[0]*2
}

const rsTaps = 56

func newResampler32() *resampler32 {
	const n, fs, fc, beta = 2 * rsTaps, 96_000.0, 15_750.0, 4.55
	h := make([]float64, n)
	m := float64(n-1) / 2
	for i := range h {
		x := float64(i) - m
		s := 2 * fc / fs
		if x != 0 {
			s = math.Sin(2*math.Pi*fc/fs*x) / (math.Pi * x)
		}
		t := 2*float64(i)/float64(n-1) - 1
		h[i] = s * bessel0(beta*math.Sqrt(1-t*t)) / bessel0(beta)
	}
	r := &resampler32{}
	for p := 0; p < 2; p++ {
		var sum float64
		for j := p; j < n; j += 2 {
			sum += h[j]
		}
		for j := p; j < n; j += 2 {
			r.phase[p] = append(r.phase[p], float32(h[j]/sum)) // unity gain per phase
		}
	}
	r.l = make([]float32, rsTaps) // start from silence
	r.r = make([]float32, rsTaps)
	r.pos = 2 * rsTaps
	return r
}

// bessel0 is the modified Bessel function I0 (for the Kaiser window).
func bessel0(x float64) float64 {
	s, t := 1.0, 1.0
	for k := 1; k < 30; k++ {
		t *= (x / 2) / float64(k)
		s += t * t
	}
	return s
}

// process takes interleaved little-endian 16-bit stereo at 48 kHz and appends
// the 32 kHz samples to l and r.
func (rs *resampler32) process(pcm []byte, l, r []int16) ([]int16, []int16) {
	for i := 0; i+3 < len(pcm); i += 4 {
		rs.l = append(rs.l, float32(int16(binary.LittleEndian.Uint16(pcm[i:]))))
		rs.r = append(rs.r, float32(int16(binary.LittleEndian.Uint16(pcm[i+2:]))))
	}
	// output at 96 kHz grid position u uses input samples k with 2k <= u,
	// newest first: phase u&1, input index (u - phase)/2 down
	for {
		base := (rs.pos - rs.pos&1) / 2
		if base >= len(rs.l) {
			break
		}
		h := rs.phase[rs.pos&1]
		var sl, sr float32
		for m, c := range h {
			sl += c * rs.l[base-m]
			sr += c * rs.r[base-m]
		}
		l = append(l, clip16(sl))
		r = append(r, clip16(sr))
		rs.pos += 3
	}
	// keep the history the next outputs need
	if drop := (rs.pos-rs.pos&1)/2 - rsTaps; drop > 0 {
		drop = min(drop, len(rs.l))
		rs.l = append(rs.l[:0], rs.l[drop:]...)
		rs.r = append(rs.r[:0], rs.r[drop:]...)
		rs.pos -= 2 * drop
	}
	return l, r
}

func clip16(v float32) int16 {
	return int16(max(-32768, min(32767, math.Round(float64(v)))))
}
