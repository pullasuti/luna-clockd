// The RTL2832U setup in this file (the baseband init sequence, demodulator
// register writes, sample rate, IF and channel-filter loading, power-down)
// follows librtlsdr (osmocom rtl-sdr, librtlsdr.c; Copyright (C) 2012-2014
// Steve Markgraf, Copyright (C) 2012 Dimitri Stolnikov and contributors;
// GPL-2.0-or-later), used here under the GNU GPL version 3 or later. See
// docs/radio-provenance.md.
package rtlsdr

import (
	"errors"
	"fmt"
	"math"
)

// The RTL2832U's register blocks, addressed through vendor control transfers:
// a block number in the high byte of wIndex, the register address in wValue.
const (
	blkDemod = 0
	blkUSB   = 1
	blkSys   = 2
	blkI2C   = 6

	regUSBSysctl = 0x2000
	regUSBEpaCtl = 0x2148
	regUSBEpaMax = 0x2158
	regDemodCtl  = 0x3000
	regDemodCtl1 = 0x300b

	XtalHz = 28_800_000 // the stick's crystal, shared by the RTL2832U and the tuner

)

const (
	ctrlIn  = 0xC0 // vendor, device-to-host
	ctrlOut = 0x40 // vendor, host-to-device
)

// Stick is an opened RTL2832U + R820T stick.
type Stick struct {
	dev   *usbDev
	tuner *r820t
	rate  int // sample rate, Hz
	ifHz  int // the tuner's IF the demodulator mixes down

	override func(i int, w RegWrite) RegWrite // diagnostics (OpenWith)
}

func (s *Stick) readArr(block, addr uint16, n int) ([]byte, error) {
	b := make([]byte, n)
	_, err := s.dev.control(ctrlIn, addr, block<<8, b)
	return b, err
}

func (s *Stick) writeArr(block, addr uint16, data []byte) error {
	_, err := s.dev.control(ctrlOut, addr, block<<8|0x10, data)
	return err
}

func (s *Stick) writeReg(block, addr uint16, val uint16, n int) error {
	d := []byte{byte(val >> 8), byte(val)}
	return s.writeArr(block, addr, d[2-n:])
}

// demodWrite writes a demodulator register (page 0..4); a read after it
// makes the write take effect before the next one.
func (s *Stick) demodWrite(page, addr uint16, val uint16, n int) error {
	d := []byte{byte(val >> 8), byte(val)}
	if _, err := s.dev.control(ctrlOut, addr<<8|0x20, 0x10|page, d[2-n:]); err != nil {
		return fmt.Errorf("demod %d:%02x: %w", page, addr, err)
	}
	if noDemodSync {
		return nil
	}
	b := make([]byte, 1)
	_, err := s.dev.control(ctrlIn, 0x01<<8|0x20, 0x0a, b)
	return err
}

func (s *Stick) demodWriteArr(page, addr uint16, data []byte) error {
	if _, err := s.dev.control(ctrlOut, addr<<8|0x20, 0x10|page, data); err != nil {
		return fmt.Errorf("demod %d:%02x: %w", page, addr, err)
	}
	b := make([]byte, 1)
	_, err := s.dev.control(ctrlIn, 0x01<<8|0x20, 0x0a, b)
	return err
}

// i2c passes tuner traffic through the demodulator's I2C repeater.
func (s *Stick) i2cRepeater(on bool) error {
	v := uint16(0x10)
	if on {
		v = 0x18
	}
	return s.demodWrite(1, 0x01, v, 1)
}

func (s *Stick) i2cWrite(addr uint16, data []byte) error {
	return s.writeArr(blkI2C, addr, data)
}

func (s *Stick) i2cRead(addr uint16, n int) ([]byte, error) {
	return s.readArr(blkI2C, addr, n)
}

// Open opens the stick at a usbfs node (Find), powers up the baseband and the
// tuner, and sets the sample rate (225-300 kHz or 0.9-3.2 MHz).
func Open(node string, rate int) (*Stick, error) {
	return OpenWith(node, rate, nil)
}

// OpenWith is Open with override changing setup write i (diagnostics).
func OpenWith(node string, rate int, override func(i int, w RegWrite) RegWrite) (*Stick, error) {
	d, err := openUSB(node)
	if err != nil {
		return nil, err
	}
	s := &Stick{dev: d, override: override}
	if err := s.initBaseband(); err != nil {
		d.close()
		return nil, fmt.Errorf("baseband: %w", err)
	}
	if err := s.i2cRepeater(true); err != nil {
		d.close()
		return nil, err
	}
	t, err := newR820T(s)
	if err == nil {
		err = t.init()
	}
	s.i2cRepeater(false)
	if err != nil {
		d.close()
		if errors.Is(err, ErrTuner) {
			return nil, err
		}
		return nil, fmt.Errorf("tuner: %w", err)
	}
	s.tuner = t
	// the R820T hands over a low IF; the demodulator mixes it to zero
	if err := s.setIF(t.ifHz); err != nil {
		d.close()
		return nil, err
	}
	if err := s.SetRate(rate); err != nil {
		d.close()
		return nil, err
	}
	return s, nil
}

func (s *Stick) Close() {
	s.writeReg(blkUSB, regUSBEpaCtl, 0x1002, 2) // stall the sample endpoint
	s.i2cRepeater(true)
	s.tuner.standby()
	s.i2cRepeater(false)
	s.writeReg(blkSys, regDemodCtl, 0x20, 1) // power the ADCs down
	s.dev.close()
}

// RegWrite is one register write of the baseband setup: a demodulator
// register (Demod: page Block) or a block register.
type RegWrite struct {
	Demod bool
	Block uint16 // block, or demodulator page
	Addr  uint16
	Val   uint16
	N     int // bytes
}

var basebandInit = []RegWrite{
	{false, blkUSB, regUSBSysctl, 0x09, 1},
	{false, blkUSB, regUSBEpaMax, 0x0002, 2},
	{false, blkUSB, regUSBEpaCtl, 0x1002, 2},
	{false, blkSys, regDemodCtl1, 0x22, 1},
	{false, blkSys, regDemodCtl, 0xe8, 1},
	{true, 1, 0x01, 0x14, 1},
	{true, 1, 0x01, 0x10, 1},
	{true, 1, 0x15, 0x00, 1},
	{true, 1, 0x16, 0x0000, 2},
	{true, 0, 0x19, 0x05, 1},
	{true, 1, 0x93, 0xf0, 1},
	{true, 1, 0x94, 0x0f, 1},
	{true, 1, 0x11, 0x00, 1},
	{true, 1, 0x04, 0x00, 1},
	{true, 0, 0x61, 0x60, 1},
	{true, 0, 0x06, 0x80, 1},
	{true, 1, 0xb1, 0x1a, 1},
	{true, 0, 0x08, 0x4d, 1},
	{true, 1, 0x15, 0x01, 1},
}

// BasebandInit lists the setup writes (diagnostics: provenance measurements).
func BasebandInit() []RegWrite { return append([]RegWrite(nil), basebandInit...) }

func (s *Stick) apply(w RegWrite) error {
	if w.Demod {
		return s.demodWrite(w.Block, w.Addr, w.Val, w.N)
	}
	return s.writeReg(w.Block, w.Addr, w.Val, w.N)
}

func (s *Stick) initBaseband() error {
	for i, w := range basebandInit {
		if s.override != nil {
			w = s.override(i, w)
		}
		if err := s.apply(w); err != nil {
			return fmt.Errorf("step %d: %w", i, err)
		}
	}
	return nil
}

// setIF tells the demodulator which IF to mix down to zero.
func (s *Stick) setIF(hz int) error {
	v := -int64(math.Round(float64(hz) * (1 << 22) / XtalHz))
	u := uint32(v) & 0x3fffff
	for i, a := range []uint16{0x19, 0x1a, 0x1b} {
		if err := s.demodWrite(1, a, uint16(u>>(16-8*i))&0xff, 1); err != nil {
			return err
		}
	}
	s.ifHz = hz
	return nil
}

// SetRate sets the sample rate (225001-300000 or 900001-3200000 Hz).
func (s *Stick) SetRate(hz int) error {
	if !(hz > 225000 && hz <= 300000) && !(hz > 900000 && hz <= 3200000) {
		return fmt.Errorf("sample rate %d Hz is outside 225-300 kHz and 0.9-3.2 MHz", hz)
	}
	// the resampler ratio register holds 28 bits; the chip repeats bit 27 as
	// bit 28, which the real ratio includes
	reg := uint32(uint64(XtalHz)<<22/uint64(hz)) & 0x0ffffffc
	ratio := reg | (reg&0x08000000)<<1
	if err := s.demodWrite(1, 0x9f, uint16(reg>>16), 2); err != nil {
		return err
	}
	if err := s.demodWrite(1, 0xa1, uint16(reg), 2); err != nil {
		return err
	}
	s.rate = int(uint64(XtalHz) << 22 / uint64(ratio))
	// the channel FIR: 0.8 MHz up to 2 MS/s (FM's 1.92 MS/s: flat to 140 kHz,
	// -21..-31 dB where signals would fold into the channel, -47 dB at
	// 2.9 MHz), 1.2 MHz above
	cut := 0.8e6
	if hz > 2_000_000 {
		cut = 1.2e6
	}
	if err := s.SetFIR(LowpassFIR(cut)); err != nil {
		return err
	}
	if err := s.demodWrite(1, 0x01, 0x14, 1); err != nil {
		return err
	}
	return s.demodWrite(1, 0x01, 0x10, 1)
}

// SetFIR loads the demodulator's channel filter, a symmetric 32-tap low-pass
// on the 28.8 MHz ADC stream before the resampler: taps 0-15 (the outer one
// first; taps 16-31 mirror them), taps 0-7 8-bit and 8-15 12-bit signed, all
// 32 summing to about 4096 for unity gain. The registers (page 1 from 0x1c,
// 20 bytes) hold taps 0-7 a byte each, then taps 8-15 as pairs of 12 bits in
// three bytes. Left unloaded the chip keeps its power-on values, which let
// strong stations several MHz away fold into a low sample rate.
func (s *Stick) SetFIR(t [16]int) error {
	var b [20]byte
	for i := 0; i < 8; i++ {
		if t[i] < -128 || t[i] > 127 {
			return fmt.Errorf("FIR tap %d = %d: outside 8 bits", i, t[i])
		}
		b[i] = byte(int8(t[i]))
	}
	for i := 0; i < 8; i += 2 {
		x, y := t[8+i], t[9+i]
		if x < -2048 || x > 2047 || y < -2048 || y > 2047 {
			return fmt.Errorf("FIR taps %d/%d = %d/%d: outside 12 bits", 8+i, 9+i, x, y)
		}
		u, v := uint16(x)&0xfff, uint16(y)&0xfff
		k := 8 + i/2*3
		b[k], b[k+1], b[k+2] = byte(u>>4), byte(u<<4)|byte(v>>8), byte(v)
	}
	for i := range b { // one register at a time, as the others are written
		if err := s.demodWrite(1, 0x1c+uint16(i), uint16(b[i]), 1); err != nil {
			return err
		}
	}
	return nil
}

// fifoReset stalls and clears the sample endpoint's FIFO before streaming.
func (s *Stick) fifoReset() error {
	if err := s.writeReg(blkUSB, regUSBEpaCtl, 0x1002, 2); err != nil {
		return err
	}
	return s.writeReg(blkUSB, regUSBEpaCtl, 0x0000, 2)
}
