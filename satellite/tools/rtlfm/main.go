// rtlfm: try the pure-Go RTL-SDR driver on the clock.
//
//	rtlfm regs                      tuner registers after init
//	rtlfm scan [from to step]       signal power across the FM band (MHz)
//	rtlfm iq   MHz seconds out.iq   raw u8 I/Q at 240 kS/s
//	rtlfm fm   MHz seconds out.wav  demodulated mono, 48 kHz 16-bit
//	rtlfm gain lna mixer vga ...    with any command first: manual gains (lna -1 = auto)
package main

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"lenovo-satellite/internal/fmradio"
	"lenovo-satellite/internal/rtlsdr"
)

func die(f string, a ...any) {
	fmt.Fprintf(os.Stderr, f+"\n", a...)
	os.Exit(1)
}

func mhz(s string) int {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		die("bad frequency %q", s)
	}
	return int(math.Round(v * 1e6))
}

func main() {
	args := os.Args[1:]
	gain := []int{10, 8, 8}
	rate, fir := fmradio.InRate, -1.0
	for len(args) >= 2 {
		switch {
		case args[0] == "gain" && len(args) >= 4:
			for i := range gain {
				gain[i], _ = strconv.Atoi(args[1+i])
			}
			args = args[4:]
			continue
		case args[0] == "rate":
			rate, _ = strconv.Atoi(args[1])
		case args[0] == "fir": // cut-off in Hz at 28.8 MHz; 0 = leave the chip's
			fir, _ = strconv.ParseFloat(args[1], 64)
		default:
			goto run
		}
		args = args[2:]
	}
run:
	if len(args) == 0 {
		die("usage: rtlfm [gain l m v] regs|scan|iq|fm ...")
	}
	if strings.HasPrefix(args[0], "facts-") {
		facts(args, gain)
		return
	}
	node, prod, err := rtlsdr.Find()
	if err != nil {
		die("%v", err)
	}
	t0 := time.Now()
	s, err := rtlsdr.Open(node, rate)
	if err != nil {
		die("open %s: %v", node, err)
	}
	defer s.Close()
	if fir > 0 {
		t := rtlsdr.LowpassFIR(fir)
		fmt.Println("FIR", t)
		if err := s.SetFIR(t); err != nil {
			die("fir: %v", err)
		}
	}
	fmt.Printf("%s at %s, %d S/s, opened in %v\n", prod, node, s.Rate(), time.Since(t0).Round(time.Millisecond))
	if err := s.SetGain(gain[0], gain[1], gain[2]); err != nil {
		die("gain: %v", err)
	}

	switch args[0] {
	case "probe":
		probeReadLen(s)
	case "regs":
		r, err := s.TunerRegs()
		if err != nil {
			die("%v", err)
		}
		for i, v := range r {
			fmt.Printf("R%-2d %02x  %08b\n", i, v, v)
		}

	case "scan":
		from, to, step := 87.5, 108.0, 0.1
		if len(args) == 4 {
			from, _ = strconv.ParseFloat(args[1], 64)
			to, _ = strconv.ParseFloat(args[2], 64)
			step, _ = strconv.ParseFloat(args[3], 64)
		}
		type hit struct {
			f       float64
			db, pil float64
		}
		var hits []hit
		for f := from; f <= to+1e-9; f += step {
			if _, err := s.Tune(int(math.Round(f * 1e6))); err != nil {
				fmt.Printf("%.1f: %v\n", f, err)
				continue
			}
			var sum float64
			var n int
			d := fmradio.New(50e-6)
			d.MeasurePilot()
			skip := 2 // let the PLL and the FIFO settle
			s.Stream(nil, 16384, func(iq []byte) bool {
				if skip > 0 {
					skip--
					return true
				}
				for _, b := range iq {
					x := float64(b) - 127.5
					sum += x * x
				}
				n += len(iq)
				d.Process(iq, nil)
				return n < 2*fmradio.InRate/5 // 200 ms
			})
			db := 10 * math.Log10(sum/float64(n)/(127.5*127.5))
			h := hit{f, db, d.PilotDB()}
			hits = append(hits, h)
			fmt.Printf("%6.1f MHz  %6.1f dBFS  pilot %5.1f dB\n", f, db, h.pil)
		}
		sort.Slice(hits, func(i, j int) bool { return hits[i].pil > hits[j].pil })
		fmt.Println("strongest pilots:")
		for _, h := range hits[:min(12, len(hits))] {
			fmt.Printf("  %6.1f MHz  %6.1f dBFS  pilot %5.1f dB\n", h.f, h.db, h.pil)
		}

	case "iq", "fm":
		if len(args) != 4 {
			die("%s MHz seconds file", args[0])
		}
		got, err := s.Tune(mhz(args[1]))
		if err != nil {
			fmt.Println("tune:", err)
		}
		secs, _ := strconv.ParseFloat(args[2], 64)
		want := int(secs * float64(s.Rate()) * 2)
		f, err := os.Create(args[3])
		if err != nil {
			die("%v", err)
		}
		defer f.Close()
		d := fmradio.New(50e-6)
		d.MeasurePilot()
		var pcm []float32
		n, blocks := 0, 0
		start := time.Now()
		err = s.Stream(nil, 16384, func(iq []byte) bool {
			blocks++
			if args[0] == "iq" {
				f.Write(iq)
			} else {
				if s.Rate() == fmradio.WideRate {
					pcm = d.ProcessWide(iq, pcm)
				} else {
					pcm = d.Process(iq, pcm)
				}
			}
			n += len(iq)
			return n < want
		})
		el := time.Since(start)
		fmt.Printf("tuned %d Hz; %d bytes in %d blocks, %v = %.0f S/s (want %d)\n", got, n, blocks,
			el.Round(time.Millisecond), float64(n/2)/el.Seconds(), s.Rate())
		if err != nil {
			fmt.Println("stream:", err)
		}
		if args[0] == "fm" {
			name, pi := d.StationName()
			fmt.Printf("pilot %.1f dB, stereo %v, RDS name %q PI %04x\n", d.PilotDB(), d.Stereo(), name, pi)
			writeWAV(f, pcm)
		}
	default:
		die("unknown command %q", args[0])
	}
}

func writeWAV(f *os.File, pcm []float32) {
	h := make([]byte, 44)
	copy(h, "RIFF")
	binary.LittleEndian.PutUint32(h[4:], uint32(36+2*len(pcm)))
	copy(h[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(h[16:], 16)
	binary.LittleEndian.PutUint16(h[20:], 1)
	binary.LittleEndian.PutUint16(h[22:], 2) // stereo
	binary.LittleEndian.PutUint32(h[24:], fmradio.OutRate)
	binary.LittleEndian.PutUint32(h[28:], fmradio.OutRate*4)
	binary.LittleEndian.PutUint16(h[32:], 4)
	binary.LittleEndian.PutUint16(h[34:], 16)
	copy(h[36:], "data")
	binary.LittleEndian.PutUint32(h[40:], uint32(2*len(pcm)))
	f.Write(h)
	b := make([]byte, 2*len(pcm))
	for i, v := range pcm {
		binary.LittleEndian.PutUint16(b[2*i:], uint16(int16(max(-1, min(1, v*0.8))*32767)))
	}
	f.Write(b)
}
