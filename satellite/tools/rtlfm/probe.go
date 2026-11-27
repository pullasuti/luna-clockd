package main

import (
	"fmt"

	"lenovo-satellite/internal/rtlsdr"
)

// probeReadLen: the longest I2C read the bridge does (diagnostics).
func probeReadLen(s *rtlsdr.Stick) {
	for _, n := range []int{8, 15, 16, 17, 20, 24, 31, 32} {
		_, err := s.RawTunerRead(n)
		fmt.Printf("read %2d bytes: %v\n", n, err)
	}
}
