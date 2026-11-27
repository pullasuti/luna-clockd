package rtlsdr

// The stick's API above the chip setup (rtl2832.go): tuning and gain through
// the tuner, and the sample stream.

import (
	"errors"
	"fmt"
	"time"
)

const epSamples = 0x81 // bulk endpoint of the u8 I/Q samples

// Rate is the real sample rate (the ratio register's rounding).
func (s *Stick) Rate() int { return s.rate }

// Tune sets the receive frequency; it returns the PLL's actual frequency.
func (s *Stick) Tune(hz int) (int, error) {
	if err := s.i2cRepeater(true); err != nil {
		return 0, err
	}
	defer s.i2cRepeater(false)
	return s.tuner.tune(hz)
}

// SetGain sets the tuner's manual gain stages (each 0..15); lna < 0 = auto.
func (s *Stick) SetGain(lna, mixer, vga int) error {
	if err := s.i2cRepeater(true); err != nil {
		return err
	}
	defer s.i2cRepeater(false)
	return s.tuner.setGain(lna, mixer, vga)
}

// TunerRegs reads the tuner's 32 registers (diagnostics).
func (s *Stick) TunerRegs() ([]byte, error) {
	if err := s.i2cRepeater(true); err != nil {
		return nil, err
	}
	defer s.i2cRepeater(false)
	return s.tuner.readAll()
}

// RawTunerRead reads n bytes from the tuner as they come (diagnostics).
func (s *Stick) RawTunerRead(n int) ([]byte, error) {
	if err := s.i2cRepeater(true); err != nil {
		return nil, err
	}
	defer s.i2cRepeater(false)
	return s.i2cRead(r820tAddr, n)
}

// ErrStopped: Stream ended because stop was closed.
var ErrStopped = errors.New("stopped")

// Stream resets the sample FIFO and calls fn with blocks of interleaved
// unsigned 8-bit I/Q until fn returns false, stop is closed or the stick
// fails. The block slice is reused.
func (s *Stick) Stream(stop <-chan struct{}, block int, fn func(iq []byte) bool) error {
	if err := s.fifoReset(); err != nil {
		return err
	}
	// a reader goroutine keeps a read pending at all times: the stick's FIFO
	// is small, and reading only between blocks of processing lost ~20 % of
	// the samples at 240 kS/s
	const pool = 8
	free := make(chan []byte, pool)
	for i := 0; i < pool; i++ {
		free <- make([]byte, block)
	}
	full := make(chan []byte, pool)
	quit := make(chan struct{})
	errc := make(chan error, 1)
	go func() {
		defer close(full)
		for {
			var buf []byte
			select {
			case buf = <-free:
			case <-quit:
				return
			}
			n, err := s.dev.bulkIn(epSamples, buf, 1000)
			if err != nil {
				errc <- fmt.Errorf("samples: %w", err)
				return
			}
			if n == 0 {
				free <- buf
				time.Sleep(time.Millisecond)
				continue
			}
			full <- buf[:n]
		}
	}()
	defer func() {
		close(quit)
		for range full { // let the reader finish its last read
		}
	}()
	for {
		select {
		case <-stop:
			return ErrStopped
		case buf, ok := <-full:
			if !ok {
				return <-errc
			}
			more := fn(buf)
			free <- buf[:cap(buf)]
			if !more {
				return nil
			}
		}
	}
}
