package main

import (
	"encoding/binary"
	"math"
	"os"
)

// writeWAVFloat writes interleaved float32 samples as a 16-bit PCM WAV file.
func writeWAVFloat(path string, s []float32, rate, ch int) error {
	data := make([]byte, 44+len(s)*2)
	copy(data, "RIFF")
	binary.LittleEndian.PutUint32(data[4:], uint32(36+len(s)*2))
	copy(data[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(data[16:], 16)
	binary.LittleEndian.PutUint16(data[20:], 1)
	binary.LittleEndian.PutUint16(data[22:], uint16(ch))
	binary.LittleEndian.PutUint32(data[24:], uint32(rate))
	binary.LittleEndian.PutUint32(data[28:], uint32(rate*ch*2))
	binary.LittleEndian.PutUint16(data[32:], uint16(ch*2))
	binary.LittleEndian.PutUint16(data[34:], 16)
	copy(data[36:], "data")
	binary.LittleEndian.PutUint32(data[40:], uint32(len(s)*2))
	for i, v := range s {
		binary.LittleEndian.PutUint16(data[44+i*2:], uint16(int16(math.Round(float64(max(-1, min(1, v)))*32767))))
	}
	return os.WriteFile(path, data, 0o644)
}
