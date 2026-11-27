package rtlsdr

// Diagnostics for measuring the chip (docs/radio-provenance.md): raw register
// access on a stick that has not been set up.

// OpenRaw opens the stick without touching any register.
func OpenRaw(node string) (*Stick, error) {
	d, err := openUSB(node)
	if err != nil {
		return nil, err
	}
	return &Stick{dev: d}, nil
}

// CloseRaw releases a stick opened with OpenRaw (no power-down writes).
func (s *Stick) CloseRaw() { s.dev.close() }

// Read reads a register of the write's kind (n bytes, big-endian).
func (s *Stick) Read(w RegWrite) (uint16, error) {
	b := make([]byte, w.N)
	var err error
	if w.Demod {
		_, err = s.dev.control(ctrlIn, w.Addr<<8|0x20, w.Block, b)
	} else {
		_, err = s.dev.control(ctrlIn, w.Addr, w.Block<<8, b)
	}
	if err != nil {
		return 0, err
	}
	v := uint16(b[0])
	if w.N == 2 {
		v = v<<8 | uint16(b[1])
	}
	return v, nil
}

// Write performs one register write.
func (s *Stick) Write(w RegWrite) error { return s.apply(w) }

// Reset resets the stick on the USB bus (USBDEVFS_RESET).
func (s *Stick) Reset() error { return s.dev.reset() }

// SetTunerIF moves the IF between tuner and demodulator (both sides).
func (s *Stick) SetTunerIF(hz int) error {
	s.tuner.ifHz = hz
	return s.setIF(hz)
}

// PLLLock programs the tuner's VCO directly and reports PLL lock.
func (s *Stick) PLLLock(vcoHz int64, divCode byte) (bool, error) {
	if err := s.i2cRepeater(true); err != nil {
		return false, err
	}
	defer s.i2cRepeater(false)
	return s.tuner.pllLock(vcoHz, divCode)
}

// TunerWriteLen sends one I2C write of n registers from R5 (their current
// shadow values, so nothing changes) and reports whether the bridge took it.
func (s *Stick) TunerWriteLen(n int) error {
	if err := s.i2cRepeater(true); err != nil {
		return err
	}
	defer s.i2cRepeater(false)
	msg := append([]byte{5}, s.tuner.regs[5:5+n]...)
	return s.i2cWrite(r820tAddr, msg)
}

// SetDemodIF changes only the demodulator's IF (the tuner keeps its LO):
// the station moves in the baseband by the difference.
func (s *Stick) SetDemodIF(hz int) error { return s.setIF(hz) }

// noDemodSync drops the read after each demodulator write (diagnostics).
var noDemodSync bool

// SetDemodSync switches the read after each demodulator write.
func SetDemodSync(on bool) { noDemodSync = !on }

// Repeater switches the demodulator's I2C repeater.
func (s *Stick) Repeater(on bool) error { return s.i2cRepeater(on) }

// RawTunerReadNoRepeater reads the tuner without touching the repeater.
func (s *Stick) RawTunerReadNoRepeater(n int) ([]byte, error) { return s.i2cRead(r820tAddr, n) }
