// wwfile: run microWakeWord on 16 kHz mono 16-bit WAV files and list where each
// model scored high (echo canceller comparisons).
//
//	wwfile <models dir> <file.wav>...
//
// WWFLOOR=0.1 lists bursts from that score up instead of 0.5 (how far below a
// threshold the misses were, and how high sound without the word gets).
package main

import (
	"encoding/binary"
	"fmt"
	"os"
	"strconv"

	"lenovo-satellite/internal/wake"
)

func readWAV(path string) ([]int16, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	// find the data chunk
	for i := 12; i+8 <= len(b); {
		id, n := string(b[i:i+4]), int(binary.LittleEndian.Uint32(b[i+4:]))
		if id == "data" {
			d := b[i+8 : min(len(b), i+8+n)]
			s := make([]int16, len(d)/2)
			for k := range s {
				s[k] = int16(binary.LittleEndian.Uint16(d[2*k:]))
			}
			return s, nil
		}
		i += 8 + n + n%2
	}
	return nil, fmt.Errorf("%s: no data chunk", path)
}

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: wwfile <models dir> <file.wav>...")
		os.Exit(2)
	}
	models, err := wake.LoadDir(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	floor := 0.5
	if v, err := strconv.ParseFloat(os.Getenv("WWFLOOR"), 64); err == nil && v > 0 {
		floor = v
	}
	for _, path := range os.Args[2:] {
		pcm, err := readWAV(path)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Printf("== %s (%.1f s)\n", path, float64(len(pcm))/16000)
		for _, m := range models {
			det, err := wake.New(m, m.Cutoff)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			// report each burst of scores >= floor as one event with its peak
			var events []string
			peak, at, in := 0.0, 0.0, false
			for i := 0; i+160 <= len(pcm); i += 160 {
				det.Process(pcm[i : i+160])
				sc := det.Score()
				if sc >= floor {
					if !in || sc > peak {
						peak, at = sc, float64(i)/16000
					}
					in = true
				} else if in {
					events = append(events, fmt.Sprintf("%.1fs:%.2f", at, peak))
					peak, in = 0, false
				}
			}
			if in {
				events = append(events, fmt.Sprintf("%.1fs:%.2f", at, peak))
			}
			fmt.Printf("  %-10s cutoff %.2f  %v\n", m.ID, m.Cutoff, events)
		}
	}
}
