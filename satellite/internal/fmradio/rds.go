package fmradio

import "strings"

// RDS (IEC 62106): 1187.5 bit/s, biphase-coded, differentially encoded BPSK
// on 57 kHz = 3 x the pilot, and the bit clock is the pilot / 16. So with the
// pilot PLL locked, the 57 kHz carrier is cos/sin 3θ and every bit is 16
// pilot cycles: two halves of 8 with opposite sign. Per pilot cycle the
// mixed-down signal is summed; a bit is (first half - second half), at the
// one of the 16 cycle offsets (and the carrier axis, cos or sin) where that
// difference is largest. Then differential decoding, and 26-bit blocks
// (16 data + 10 check) found by their checkwords (offset words A, B, C, C',
// D). Only the station name (group 0, the programme service name) and PI are
// used; no error correction (a damaged block is just skipped).

const (
	rdsPoly = 0x5B9 // x^10 + x^8 + x^7 + x^5 + x^4 + x^3 + 1

	offA  = 0x0FC
	offB  = 0x198
	offC  = 0x168
	offCp = 0x350
	offD  = 0x1B4
)

type rdsDecoder struct {
	accI, accQ float64     // the current pilot cycle
	ringI      [16]float64 // the last 16 cycles
	ringQ      [16]float64
	cycles     int
	energy     [2][16]float64 // per axis and cycle offset: how clear the bits are
	axis, slot int

	prev uint32    // last raw bit (differential decoding)
	blk  [4]uint32 // the last 104 bits as four 26-bit blocks

	ps     [8]byte
	got    [4]bool
	seen   map[string]int // complete names and how often (some stations scroll text through it)
	name   string
	pi     uint16
	groups int
}

func newRDS() *rdsDecoder { return &rdsDecoder{seen: map[string]int{}} }

// add takes one sample mixed with cos 3θ (i) and sin 3θ (q); wrapped marks
// the first sample of a new pilot cycle.
func (r *rdsDecoder) add(i, q float64, wrapped bool) {
	if wrapped {
		r.cycle()
	}
	r.accI += i
	r.accQ += q
}

func (r *rdsDecoder) cycle() {
	k := r.cycles % 16
	r.ringI[k], r.ringQ[k] = r.accI, r.accQ
	r.accI, r.accQ = 0, 0
	r.cycles++
	if r.cycles < 16 {
		return
	}
	// the bit that would end with this cycle: halves = cycles k-15..k-8, k-7..k
	var d [2]float64
	for j := 0; j < 16; j++ {
		idx := (r.cycles - 16 + j) % 16
		sign := 1.0
		if j >= 8 {
			sign = -1
		}
		d[0] += sign * r.ringI[idx]
		d[1] += sign * r.ringQ[idx]
	}
	for a := 0; a < 2; a++ {
		r.energy[a][k] = 0.98*r.energy[a][k] + 0.02*abs(d[a])
	}
	if k == r.slot {
		bit := uint32(0)
		if d[r.axis] > 0 {
			bit = 1
		}
		r.bit(bit ^ r.prev)
		r.prev = bit
	}
	if k == 15 { // once per bit: follow the clearest offset (with hysteresis)
		ba, bs := r.axis, r.slot
		for a := 0; a < 2; a++ {
			for s := 0; s < 16; s++ {
				if r.energy[a][s] > 1.3*r.energy[ba][bs] {
					ba, bs = a, s
				}
			}
		}
		r.axis, r.slot = ba, bs
	}
}

func (r *rdsDecoder) bit(b uint32) {
	const mask = 1<<26 - 1
	for i := 0; i < 3; i++ {
		r.blk[i] = (r.blk[i]<<1 | r.blk[i+1]>>25) & mask
	}
	r.blk[3] = (r.blk[3]<<1 | b) & mask
	a, bOK := rdsCheck(r.blk[0], offA), rdsCheck(r.blk[1], offB)
	c := rdsCheck(r.blk[2], offC) || rdsCheck(r.blk[2], offCp)
	dOK := rdsCheck(r.blk[3], offD)
	// B and D carry the name; A or C must agree too, so a block boundary
	// is not taken from two lucky checkwords
	if !bOK || !dOK || !(a || c) {
		return
	}
	r.groups++
	if a {
		r.pi = uint16(r.blk[0] >> 10)
	}
	B, D := r.blk[1]>>10, r.blk[3]>>10
	if B>>12 != 0 { // group type 0 (A or B): the programme service name
		return
	}
	seg := B & 3
	r.ps[2*seg], r.ps[2*seg+1] = byte(D>>8), byte(D)
	r.got[seg] = true
	if r.got[0] && r.got[1] && r.got[2] && r.got[3] {
		// the name is the most frequent complete one seen at least twice
		n := rdsText(r.ps[:])
		_, known := r.seen[n]
		if n != "" && (known || len(r.seen) < 64) { // bounded: scrolling text never ends
			r.seen[n]++
			if r.seen[n] >= 2 && r.seen[n] >= r.seen[r.name] {
				r.name = n
			}
		}
		r.got = [4]bool{}
	}
}

// rdsCheck reports whether a 26-bit block's checkword matches the offset word.
func rdsCheck(blk, off uint32) bool {
	reg := blk >> 10 << 10
	for bit := 25; bit >= 10; bit-- {
		if reg&(1<<bit) != 0 {
			reg ^= rdsPoly << (bit - 10)
		}
	}
	return reg&0x3ff^blk&0x3ff == off
}

// rdsText converts the RDS character set (mostly ASCII; the Nordic and
// common accented letters mapped) and trims the padding.
func rdsText(b []byte) string {
	var sb strings.Builder
	for _, c := range b {
		switch {
		case c >= 0x20 && c < 0x7f:
			sb.WriteByte(c)
		default:
			if r, ok := rdsChars[c]; ok {
				sb.WriteRune(r)
			} else {
				sb.WriteByte(' ')
			}
		}
	}
	return strings.TrimSpace(sb.String())
}

var rdsChars = map[byte]rune{
	0x80: 'á', 0x81: 'à', 0x82: 'é', 0x83: 'è', 0x84: 'í', 0x85: 'ì', 0x86: 'ó', 0x87: 'ò',
	0x88: 'ú', 0x89: 'ù', 0x8a: 'Ñ', 0x8b: 'Ç', 0x8d: 'ß',
	0x90: 'â', 0x91: 'ä', 0x92: 'ê', 0x93: 'ë', 0x94: 'î', 0x95: 'ï', 0x96: 'ô', 0x97: 'ö',
	0x98: 'û', 0x99: 'ü', 0x9a: 'ñ', 0x9b: 'ç',
	0xc0: 'Á', 0xc1: 'À', 0xc2: 'É', 0xc3: 'È', 0xc4: 'Í', 0xc5: 'Ì', 0xc6: 'Ó', 0xc7: 'Ò',
	0xc8: 'Ú', 0xc9: 'Ù', 0xcc: 'Š', 0xcd: 'Ž',
	0xd0: 'Â', 0xd1: 'Ä', 0xd2: 'Ê', 0xd3: 'Ë', 0xd4: 'Î', 0xd5: 'Ï', 0xd6: 'Ô', 0xd7: 'Ö',
	0xd8: 'Û', 0xd9: 'Ü', 0xdc: 'š', 0xdd: 'ž',
	0xe1: 'Å', 0xe2: 'Æ', 0xe7: 'Ø', 0xf1: 'å', 0xf2: 'æ', 0xf7: 'ø',
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}
