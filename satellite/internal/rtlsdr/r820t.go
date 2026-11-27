package rtlsdr

import (
	"fmt"
	"math"
	"math/bits"
	"time"
)

// R820T / R820T2 tuner, from Rafael Micro's "R820T2 Register Description"
// (VER1.3, 2014; published by Rafael Micro in 2016). What the document says,
// and how this driver uses it:
//
//   - I2C address 0x34 (write) / 0x35 (read). A write sends the first register
//     number, then data for consecutive registers. A read always starts at R0
//     and sends each byte LSB first (R0 reads as 0x69 for 0x96).
//   - R0-R4 are status (R2 bit 6: PLL locked), R5-R31 are settings.
//   - PLL: Nint = 4*Ni2c + Si2c + 13 (R20), Nfra = SDM_IN[16:1]/2^16 (R21,
//     R22), Ndiv = (Nint+Nfra)*2; the mixer gets the VCO divided by 2..64
//     (SEL_DIV, R16). REFDIV halves the reference.
//
// The RTL2832U's I2C bridge reads at most 16 bytes, so only R0-R15 can be
// read back. R5-R15 start from the chip's power-up values with the
// document's fields set; R16-R31 are written whole: the fixed bits from the
// document's register matrix (Table 1-2), "-" bits as 0, and the fields set
// below.

const (
	r820tAddr = 0x34
	r820tIF   = 3_570_000 // IF handed to the RTL2832U (LO above the station)

	vcoMinHz = 1_770_000_000
	vcoMaxHz = 3_540_000_000
)

type r820t struct {
	s    *Stick
	regs [32]byte // shadow of R0..R31 (R0-R4 as last read)
	fref int      // PLL reference, Hz
	ifHz int      // IF handed to the RTL2832U
}

func newR820T(s *Stick) (*r820t, error) {
	t := &r820t{s: s, fref: XtalHz, ifHz: r820tIF}
	r, err := t.readAll()
	if err != nil {
		return nil, err
	}
	if r[0] != 0x96 {
		return nil, fmt.Errorf("%w; no R820T at i2c 0x%02x (chip id 0x%02x)", ErrTuner, r820tAddr, r[0])
	}
	copy(t.regs[:], r)
	return t, nil
}

// readAll reads R0..R31 (bit order fixed).
func (t *r820t) readAll() ([]byte, error) {
	var b []byte
	var err error
	for _, n := range []int{32, 16, 8} { // the bridge may limit the read length
		if b, err = t.s.i2cRead(r820tAddr, n); err == nil {
			break
		}
	}
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(b))
	for i, v := range b {
		out[i] = bits.Reverse8(v)
	}
	return out, nil
}

// write sends registers first..last from the shadow (at most 7 per message).
func (t *r820t) write(first, last int) error {
	for r := first; r <= last; r += 7 {
		end := min(last, r+6)
		msg := append([]byte{byte(r)}, t.regs[r:end+1]...)
		if err := t.s.i2cWrite(r820tAddr, msg); err != nil {
			return fmt.Errorf("tuner R%d-R%d: %w", r, end, err)
		}
	}
	return nil
}

// set changes bits under mask in register r (shadow only).
func (t *r820t) set(r int, mask, val byte) {
	t.regs[r] = t.regs[r]&^mask | val&mask
}

// fixed bits of R5-R15 from the register matrix (mask, value)
var r820tFixed = map[int][2]byte{
	5:  {0x40, 0x00},
	6:  {0x18, 0x10},
	7:  {0x80, 0x00},
	10: {0x10, 0x10},
	11: {0x90, 0x00},
	12: {0xa0, 0xa0},
}

func (t *r820t) init() error {
	for r, mv := range r820tFixed {
		t.set(r, mv[0], mv[1])
	}
	// R16-R31: matrix fixed bits | fields
	t.regs[16] = 0x04 | 0x01 // SEL_DIV (set by tune), REF_DIV2 0, fixed 0 1, CAPX 01 (10 pF)
	t.regs[17] = 0x80 | 0x03 // PW_LDO_A 10 (2.0 V), fixed 0000 11
	t.regs[18] = 0x80        // fixed 1000, PW_SDM 0, fixed 000
	t.regs[19] = 0x00        // fixed
	t.regs[20] = 0x00        // S_I2C / N_I2C (set by tune)
	t.regs[21] = 0x00        // SDM_IN
	t.regs[22] = 0x00
	t.regs[23] = 0x30 | 0x04 // PW_LDO_D 00 (1.8 V, 8 mA), fixed 11, OPEN_D 0, fixed 100
	t.regs[24] = 0x40        // fixed 01, rest unused
	t.regs[25] = 0x80 | 0x0c // PWD_RFFILT on, fixed 00, SW_AGC 0, fixed 11
	t.regs[26] = 0x20        // RFMUX 00 (tracking filter on), fixed 10, PLL_AUTO_CLK 00 (128 kHz), RFFILT (tune)
	t.regs[27] = 0x00        // TF_NCH / TF_LP: highest corners
	t.regs[28] = 0x50 | 0x04 // PDET3_GAIN 5, fixed 0 1 - 0
	t.regs[29] = 0xc0 | 3<<3 | 4
	t.regs[30] = 0x40 | 0x0a // fixed 0 1, PDET_CLK
	t.regs[31] = 0xc0        // fixed 110000
	t.set(5, 0x80, 0x80)     // PWD_LT: loop-through off
	t.set(5, 0x20, 0x00)     // PWD_LNA1: LNA on
	t.set(6, 0x80, 0x00)     // PWD_PDET1: power detector 1 on
	t.set(6, 0x20, 0x20)     // FILT_3DB: +3 dB
	t.set(6, 0x07, 0x00)     // PW_LNA: max LNA current
	t.set(7, 0x60, 0x60)     // PWD_MIX on, PW0_MIX normal current
	t.set(8, 0x80, 0x80)     // PWD_AMP: mixer buffer on
	t.set(9, 0x80, 0x00)     // PWD_IFFILT: IF filter on
	t.set(10, 0x80, 0x80)    // PWD_FILT: channel filter on
	t.set(10, 0x60, 0x00)    // PW_FILT: highest power
	t.set(10, 0x0f, 0x00)    // FILT_CODE: widest
	t.set(11, 0x60, 0x00)    // FILT_BW: widest
	t.set(11, 0x0f, 0x0f)    // HPF: lowest corner
	t.set(12, 0x40, 0x40)    // PWD_VGA: on
	t.set(12, 0x10, 0x00)    // VGA_MODE: gain from VGA_CODE
	if err := t.write(5, 31); err != nil {
		return err
	}
	return t.setGain(10, 8, 8)
}

func (t *r820t) standby() {
	t.set(5, 0x20, 0x20)  // LNA off
	t.set(7, 0x40, 0x00)  // mixer off
	t.set(12, 0x40, 0x00) // VGA off
	t.write(5, 12)
}

// setGain sets LNA, mixer and VGA codes (0..15); lna < 0 lets the tuner's
// LNA and mixer AGC run.
func (t *r820t) setGain(lna, mixer, vga int) error {
	if lna < 0 {
		t.set(5, 0x10, 0x00) // LNA_GAIN_MODE auto
		t.set(7, 0x10, 0x10) // MIXGAIN_MODE auto
	} else {
		t.set(5, 0x1f, 0x10|byte(min(lna, 15)))
		t.set(7, 0x1f, byte(min(mixer, 15)))
	}
	t.set(12, 0x0f, byte(min(max(vga, 0), 15)))
	return t.write(5, 12)
}

// tune sets the PLL for a station at hz and returns the frequency actually
// received (the fractional divider's rounding).
func (t *r820t) tune(hz int) (int, error) {
	lo := int64(hz + t.ifHz)
	// RF filter band and tracking filter corners by frequency
	switch {
	case hz < 200_000_000:
		t.set(26, 0x03, 0x02) // RFFILT low band
	case hz < 450_000_000:
		t.set(26, 0x03, 0x01) // med band
	default:
		t.set(26, 0x03, 0x00) // highest band
	}
	div, code := 2, byte(0)
	for lo*int64(div) < vcoMinHz && div < 64 {
		div *= 2
		code++
	}
	vco := lo * int64(div)
	if vco < vcoMinHz || vco > vcoMaxHz {
		return 0, fmt.Errorf("%d Hz is out of the tuner's range", hz)
	}
	n := float64(vco) / float64(2*t.fref) // Nint + Nfra
	nint := int(n)
	sdm := int(math.Round((n - float64(nint)) * 65536))
	if sdm == 65536 {
		nint, sdm = nint+1, 0
	}
	if nint < 13 || nint > 4*63+3+13 {
		return 0, fmt.Errorf("PLL divider %d out of range", nint)
	}
	ni, si := (nint-13)/4, (nint-13)%4
	t.set(16, 0xe0, code<<5)
	t.regs[20] = byte(si)<<6 | byte(ni)
	t.regs[21] = byte(sdm)
	t.regs[22] = byte(sdm >> 8)
	if err := t.write(16, 16); err != nil {
		return 0, err
	}
	if err := t.write(20, 22); err != nil {
		return 0, err
	}
	if err := t.write(26, 26); err != nil {
		return 0, err
	}
	locked := false
	for i := 0; i < 10 && !locked; i++ {
		time.Sleep(2 * time.Millisecond)
		r, err := t.readAll()
		if err != nil {
			return 0, err
		}
		copy(t.regs[:5], r[:5])
		locked = r[2]&0x40 != 0
	}
	got := int((float64(nint)+float64(sdm)/65536)*float64(2*t.fref)/float64(div)) - t.ifHz
	if !locked {
		return got, fmt.Errorf("PLL not locked at %d Hz (VCO %d MHz, /%d)", hz, vco/1e6, div)
	}
	return got, nil
}

// pllLock programs the PLL for a VCO frequency with mixer divider code
// (0 = /2 .. 5 = /64), ignoring the range limits, and reports the lock bit
// (diagnostics: measuring the VCO range).
func (t *r820t) pllLock(vco int64, code byte) (bool, error) {
	n := float64(vco) / float64(2*t.fref)
	nint := int(n)
	sdm := int(math.Round((n - float64(nint)) * 65536))
	if sdm == 65536 {
		nint, sdm = nint+1, 0
	}
	if nint < 13 || nint > 4*63+3+13 {
		return false, fmt.Errorf("PLL divider %d out of range", nint)
	}
	ni, si := (nint-13)/4, (nint-13)%4
	t.set(16, 0xe0, code<<5)
	t.regs[20] = byte(si)<<6 | byte(ni)
	t.regs[21] = byte(sdm)
	t.regs[22] = byte(sdm >> 8)
	if err := t.write(16, 16); err != nil {
		return false, err
	}
	if err := t.write(20, 22); err != nil {
		return false, err
	}
	locked := false
	for i := 0; i < 10 && !locked; i++ {
		time.Sleep(2 * time.Millisecond)
		b, err := t.readAll()
		if err != nil {
			return false, err
		}
		locked = b[2]&0x40 != 0
	}
	return locked, nil
}
