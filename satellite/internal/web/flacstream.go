package web

import (
	"io"
	"math/bits"
)

// A lean FLAC encoder for the radio's stream link: 16-bit stereo, fixed
// blocks, fixed prediction (orders 0-4), the cheapest of left/right, left/
// side, side/right and mid/side per frame, constant subframes (a mono
// station's side channel costs nothing), Rice coding with the parameter
// estimated from the mean residual instead of searched. The library encoder
// (mewkiz/flac) spent half its time searching Rice parameters: ~15 % of a core
// per listener on the clock. Written from the FLAC format specification
// (RFC 9639). The length is unknown, so STREAMINFO says 0 samples and no MD5,
// as live FLAC streams do.

const flacBlock = 4096 // samples per channel per frame (128 ms at 32 kHz)

type flacStream struct {
	w    io.Writer
	rate int
	num  uint64 // frame number
	l, r []int32
	mid  []int32
	side []int32
	res  []int32 // scratch: residuals
	bw   bitWriter
}

func newFLACStream(w io.Writer, rate int) (*flacStream, error) {
	f := &flacStream{w: w, rate: rate}
	b := &f.bw
	b.bytes([]byte("fLaC"))
	b.put(1, 1) // the last metadata block
	b.put(0, 7) // STREAMINFO
	b.put(34, 24)
	b.put(flacBlock, 16)
	b.put(flacBlock, 16)
	b.put(0, 24) // min/max frame size unknown
	b.put(0, 24)
	b.put(uint64(rate), 20)
	b.put(2-1, 3)  // channels - 1
	b.put(16-1, 5) // bits per sample - 1
	b.put(0, 36)   // total samples unknown
	b.bytes(make([]byte, 16))
	_, err := w.Write(b.flush())
	return f, err
}

// write takes 16-bit stereo samples and writes every complete frame.
func (f *flacStream) write(l, r []int16) error {
	for i := range l {
		f.l = append(f.l, int32(l[i]))
		f.r = append(f.r, int32(r[i]))
		if len(f.l) == flacBlock {
			if err := f.frame(); err != nil {
				return err
			}
		}
	}
	return nil
}

// flacRateCode is the frame header's sample rate code (0 = as in STREAMINFO).
func flacRateCode(rate int) uint64 {
	switch rate {
	case 32000:
		return 8
	case 44100:
		return 9
	case 48000:
		return 10
	}
	return 0
}

func (f *flacStream) frame() error {
	n := len(f.l)
	f.mid, f.side = f.mid[:0], f.side[:0]
	for i := range f.l {
		f.mid = append(f.mid, (f.l[i]+f.r[i])>>1)
		f.side = append(f.side, f.l[i]-f.r[i])
	}
	ol, cl := bestOrder(f.l)
	or, cr := bestOrder(f.r)
	om, cm := bestOrder(f.mid)
	os, cs := bestOrder(f.side)
	// channel assignment: 1 = left/right, 8 = left/side, 9 = side/right, 10 = mid/side
	type ch struct {
		x          []int32
		bps, order int
	}
	L, R, M, Sd := ch{f.l, 16, ol}, ch{f.r, 16, or}, ch{f.mid, 16, om}, ch{f.side, 17, os}
	assign, a, b := uint64(1), L, R
	best := cl + cr
	if c := cl + cs; c < best {
		best, assign, a, b = c, 8, L, Sd
	}
	if c := cs + cr; c < best {
		best, assign, a, b = c, 9, Sd, R
	}
	if c := cm + cs; c < best {
		assign, a, b = 10, M, Sd
	}

	w := &f.bw
	w.put(0xFFF8, 16) // sync, fixed block size
	if n == flacBlock {
		w.put(12, 4) // 256 * 2^(12-8) = 4096
	} else {
		w.put(7, 4) // 16-bit (blocksize-1) after the frame number
	}
	w.put(flacRateCode(f.rate), 4)
	w.put(assign, 4)
	w.put(4, 3) // 16 bits per sample
	w.put(0, 1)
	w.utf8(f.num)
	if n != flacBlock {
		w.put(uint64(n-1), 16)
	}
	w.bytes([]byte{crc8(w.buf)}) // w.buf holds just this header
	f.subframe(a.x, a.bps, a.order)
	f.subframe(b.x, b.bps, b.order)
	w.align()
	crc := crc16(w.buf)
	w.bytes([]byte{byte(crc >> 8), byte(crc)})
	f.num++
	f.l, f.r = f.l[:0], f.r[:0]
	_, err := f.w.Write(w.flush())
	return err
}

// bestOrder picks the fixed prediction order with the smallest residuals and
// estimates the bits (Rice-coded as one partition).
func bestOrder(x []int32) (order, bitsEst int) {
	var sum [5]uint64
	for i := 4; i < len(x); i++ {
		e0 := x[i]
		e1 := e0 - x[i-1]
		e2 := x[i] - 2*x[i-1] + x[i-2]
		e3 := x[i] - 3*x[i-1] + 3*x[i-2] - x[i-3]
		e4 := x[i] - 4*x[i-1] + 6*x[i-2] - 4*x[i-3] + x[i-4]
		sum[0] += abs32(e0)
		sum[1] += abs32(e1)
		sum[2] += abs32(e2)
		sum[3] += abs32(e3)
		sum[4] += abs32(e4)
	}
	for o := 1; o < 5; o++ {
		if sum[o] < sum[order] {
			order = o
		}
	}
	n := uint64(len(x))
	z := 2 * sum[order] // zigzag doubles the magnitudes
	k := riceParam(z, n)
	return order, int(n*uint64(k+1) + z>>k)
}

func (f *flacStream) subframe(x []int32, bps, order int) {
	w := &f.bw
	mask := uint64(1)<<bps - 1
	constant := true
	for _, v := range x[1:] {
		if v != x[0] {
			constant = false
			break
		}
	}
	if constant {
		w.put(0, 8) // pad bit, CONSTANT, no wasted bits
		w.put(uint64(x[0])&mask, bps)
		return
	}
	w.put(0, 1)
	w.put(uint64(8|order), 6) // FIXED, order
	w.put(0, 1)
	for i := 0; i < order; i++ {
		w.put(uint64(x[i])&mask, bps) // warm-up samples
	}
	res := f.res[:0]
	for i := order; i < len(x); i++ {
		var e int32
		switch order {
		case 0:
			e = x[i]
		case 1:
			e = x[i] - x[i-1]
		case 2:
			e = x[i] - 2*x[i-1] + x[i-2]
		case 3:
			e = x[i] - 3*x[i-1] + 3*x[i-2] - x[i-3]
		case 4:
			e = x[i] - 4*x[i-1] + 6*x[i-2] - 4*x[i-3] + x[i-4]
		}
		res = append(res, e)
	}
	f.res = res
	// Rice partitions: the partition order (0-6) with the smallest estimate.
	// Partitions split the block evenly; the first is short by the warm-up.
	n := len(x)
	bestP, bestBits := 0, -1
	for p := 0; p <= 6 && n%(1<<p) == 0 && n>>p > order; p++ {
		b := 0
		for i := 0; i < 1<<p; i++ {
			lo, hi := partition(i, p, n, order)
			s := zsum(res[lo:hi])
			k := riceParam(s, uint64(hi-lo))
			b += 4 + (hi-lo)*(k+1) + int(s>>k)
		}
		if bestBits < 0 || b < bestBits {
			bestP, bestBits = p, b
		}
	}
	w.put(0, 2) // Rice coding, 4-bit parameters
	w.put(uint64(bestP), 4)
	for i := 0; i < 1<<bestP; i++ {
		lo, hi := partition(i, bestP, n, order)
		k := riceParam(zsum(res[lo:hi]), uint64(hi-lo))
		w.put(uint64(k), 4)
		for _, e := range res[lo:hi] {
			u := zigzag(e)
			w.unary(u >> k)
			w.put(u&(1<<k-1), k)
		}
	}
}

// partition is partition i's range in the residuals (which start after the
// order warm-up samples).
func partition(i, p, n, order int) (lo, hi int) {
	size := n >> p
	lo, hi = i*size-order, (i+1)*size-order
	if i == 0 {
		lo = 0
	}
	return
}

func zsum(res []int32) uint64 {
	var s uint64
	for _, e := range res {
		s += zigzag(e)
	}
	return s
}

// riceParam is the Rice parameter for n values summing to sum (about log2 of
// their mean), 0-14 (15 would be the escape code).
func riceParam(sum, n uint64) int {
	if n == 0 || sum < n {
		return 0
	}
	return min(14, bits.Len64(sum/n)-1)
}

func zigzag(e int32) uint64 { return uint64(uint32(e<<1) ^ uint32(e>>31)) }

func abs32(e int32) uint64 {
	if e < 0 {
		return uint64(-int64(e))
	}
	return uint64(e)
}

// bitWriter packs bits MSB first.
type bitWriter struct {
	buf  []byte
	acc  uint64
	nacc int // bits in acc not yet in buf (< 8 between calls)
}

func (w *bitWriter) put(v uint64, n int) {
	for n > 0 {
		take := min(n, 56-w.nacc)
		w.acc = w.acc<<take | (v>>(n-take))&(1<<take-1)
		w.nacc += take
		n -= take
		for w.nacc >= 8 {
			w.nacc -= 8
			w.buf = append(w.buf, byte(w.acc>>w.nacc))
		}
	}
}

// unary writes q zeros and a one.
func (w *bitWriter) unary(q uint64) {
	for q >= 32 {
		w.put(0, 32)
		q -= 32
	}
	w.put(1, int(q)+1)
}

func (w *bitWriter) align() {
	if w.nacc > 0 {
		w.put(0, 8-w.nacc)
	}
}

func (w *bitWriter) bytes(b []byte) {
	w.align()
	w.buf = append(w.buf, b...)
}

// utf8 writes a frame number in FLAC's UTF-8-like coding (up to 36 bits).
func (w *bitWriter) utf8(v uint64) {
	if v < 0x80 {
		w.put(v, 8)
		return
	}
	n := 2 // bytes: an n-byte code holds 5n+1 bits
	for v >= 1<<(5*n+1) {
		n++
	}
	w.put(uint64(0xFF00>>n)&0xFF|v>>(6*(n-1)), 8)
	for i := n - 2; i >= 0; i-- {
		w.put(0x80|(v>>(6*i))&0x3F, 8)
	}
}

// flush returns the bytes written so far (aligned) and starts over.
func (w *bitWriter) flush() []byte {
	w.align()
	b := w.buf
	w.buf = nil
	return b
}

// FLAC's CRC-8 (poly 0x07) over a frame header and CRC-16 (poly 0x8005) over
// a frame, from byte tables.
var crc8Tab, crc16Tab = func() (t8 [256]byte, t16 [256]uint16) {
	for i := range 256 {
		c8, c16 := byte(i), uint16(i)<<8
		for range 8 {
			if c8&0x80 != 0 {
				c8 = c8<<1 ^ 0x07
			} else {
				c8 <<= 1
			}
			if c16&0x8000 != 0 {
				c16 = c16<<1 ^ 0x8005
			} else {
				c16 <<= 1
			}
		}
		t8[i], t16[i] = c8, c16
	}
	return
}()

func crc8(b []byte) byte {
	var c byte
	for _, x := range b {
		c = crc8Tab[c^x]
	}
	return c
}

func crc16(b []byte) uint16 {
	var c uint16
	for _, x := range b {
		c = c<<8 ^ crc16Tab[byte(c>>8)^x]
	}
	return c
}
