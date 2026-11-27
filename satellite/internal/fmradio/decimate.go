package fmradio

import "math"

// WideRate is the stick's sample rate for the radio: 8 x InRate. At 240 kS/s
// the RTL2832U's resampler folds stations up to a few MHz away into the
// channel (ghost stations in the search, noise under real ones); at 1.92 MS/s
// neighbours up to +-960 kHz stay where they are, the chip's FIR (loaded by
// rtlsdr, cut-off 0.8 MHz) holds back what lies further out, and three
// half-band stages bring the channel down to 240 kS/s here:
// 1.92 -> 0.96 -> 0.48 -> 0.24 MS/s, flat to +-100 kHz and > 60 dB down from
// 140 kHz (all that would fold into the channel).
//
// The stages work on interleaved I/Q in one pass (the first one straight
// from the stick's bytes through a table), over windows the compiler
// bounds-checks once: the first version (separate I and Q, per-sample
// append) cost 46 % of an A53 core.
const WideRate = 8 * InRate

// halfbandTaps designs a half-band filter of n taps (n = 4k+3, Kaiser window,
// unity gain): the centre tap and the odd taps h[c+1], h[c+3], ... (every
// other tap is zero).
func halfbandTaps(n int, beta float64) (centre float32, odd []float32) {
	c := (n - 1) / 2
	full := make([]float64, n)
	var sum float64
	for i := range full {
		x := float64(i - c)
		s := 0.5
		if x != 0 {
			s = math.Sin(math.Pi/2*x) / (math.Pi * x)
		}
		t := 2*float64(i)/float64(n-1) - 1
		full[i] = s * bessel0(beta*math.Sqrt(1-t*t)) / bessel0(beta)
		sum += full[i]
	}
	for k := 1; k <= c; k += 2 {
		odd = append(odd, float32(full[c+k]/sum))
	}
	return float32(full[c] / sum), odd
}

func bessel0(x float64) float64 {
	s, t := 1.0, 1.0
	for k := 1; k < 30; k++ {
		t *= (x / 2) / float64(k)
		s += t * t
	}
	return s
}

// The stages run in integers: the samples are 8-bit anyway, and on the
// in-order A53 a chain of float adds costs ~4 cycles a step, integer ones 1.
// Samples travel scaled by 64 (13 bits and sign, more than 8-bit input plus
// the decimation gain needs); FM keeps only the phase, so the scale is free.
const (
	hbShift1 = 14 - 6 // stage 1 taps x 2^14 on bytes -> samples x 64
	hbShift  = 12     // later stages: taps x 2^12
)

// hb15 is the first stage: 15 taps on interleaved u8 I/Q.
type hb15 struct {
	c0  int32    // centre tap x 2^14
	t   [4]int32 // odd taps x 2^14
	off int32    // 127.5 x the gain x 2^14: the stick's mid-scale
	buf []byte   // unused input (the history first)
}

func (h *hb15) process(iq []byte, out []int32) []int32 {
	h.buf = append(h.buf, iq...)
	b := h.buf
	c0, t0, t1, t2, t3, off := h.c0, h.t[0], h.t[1], h.t[2], h.t[3], h.off
	i := 0
	for ; i+30 <= len(b); i += 4 { // 15 complex samples in, one out per two
		w := (*[30]byte)(b[i : i+30])
		sI := c0*int32(w[14]) + t0*(int32(w[12])+int32(w[16])) + t1*(int32(w[8])+int32(w[20])) +
			t2*(int32(w[4])+int32(w[24])) + t3*(int32(w[0])+int32(w[28]))
		sQ := c0*int32(w[15]) + t0*(int32(w[13])+int32(w[17])) + t1*(int32(w[9])+int32(w[21])) +
			t2*(int32(w[5])+int32(w[25])) + t3*(int32(w[1])+int32(w[29]))
		out = append(out, (sI-off)>>hbShift1, (sQ-off)>>hbShift1)
	}
	h.buf = append(h.buf[:0], b[i:]...)
	return out
}

// hbc is a later half-band stage on interleaved I/Q.
type hbc struct {
	n   int // taps
	c0  int32
	t   []int32 // odd taps x 2^12 (an even count: hbc pairs them)
	buf []int32
}

func newHBC(n int, beta float64) *hbc {
	h := &hbc{n: n, buf: make([]int32, 2*(n-1))}
	c0, t := halfbandTaps(n, beta)
	h.c0 = int32(math.Round(float64(c0) * (1 << hbShift)))
	for _, x := range t {
		h.t = append(h.t, int32(math.Round(float64(x)*(1<<hbShift))))
	}
	return h
}

func (h *hbc) process(in, out []int32) []int32 {
	h.buf = append(h.buf, in...)
	b := h.buf
	n2, c2 := 2*h.n, h.n-1 // window length and centre, in values
	t := h.t
	i := 0
	for ; i+n2 <= len(b); i += 4 {
		w := b[i : i+n2 : i+n2]
		sI, sQ := h.c0*w[c2], h.c0*w[c2+1]
		lo, hi := c2-2, c2+2 // the odd taps' I positions, moving outwards by 4 values
		for j := 0; j+1 < len(t); j += 2 {
			a, b := t[j], t[j+1]
			sI += a*(w[lo]+w[hi]) + b*(w[lo-4]+w[hi+4])
			sQ += a*(w[lo+1]+w[hi+1]) + b*(w[lo-3]+w[hi+5])
			lo -= 8
			hi += 8
		}
		out = append(out, sI>>hbShift, sQ>>hbShift)
	}
	h.buf = append(h.buf[:0], b[i:]...)
	return out
}

// decimator8 takes u8 I/Q at WideRate to interleaved I/Q at InRate (x 64).
type decimator8 struct {
	s1     hb15
	s2, s3 *hbc
	a, b   []int32
	out    []int32
}

func newDecimator8() *decimator8 {
	// pass edges 140, 140 and 100 kHz; stop edges 820, 340, 140 kHz; 6 and
	// 14 odd taps in the later stages
	d := &decimator8{s2: newHBC(23, 7), s3: newHBC(55, 6.5)}
	c0, t := halfbandTaps(15, 7)
	d.s1.c0 = int32(math.Round(float64(c0) * (1 << 14)))
	gain := d.s1.c0
	for j := range d.s1.t {
		d.s1.t[j] = int32(math.Round(float64(t[j]) * (1 << 14)))
		gain += 2 * d.s1.t[j]
	}
	d.s1.off = int32(math.Round(127.5 * float64(gain)))
	d.s1.buf = make([]byte, 28)
	for i := range d.s1.buf {
		d.s1.buf[i] = 128 // silence before the first block
	}
	return d
}

// process returns interleaved I/Q at InRate, x 64 (valid until the next call).
func (d *decimator8) process(iq []byte) []int32 {
	d.a = d.s1.process(iq, d.a[:0])
	d.b = d.s2.process(d.a, d.b[:0])
	d.out = d.s3.process(d.b, d.out[:0])
	return d.out
}
