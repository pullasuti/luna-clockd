package main

// Measurements for the RTL2832U/R820T fact list (docs/radio-provenance.md,
// section 7): what each register write does on a real stick. All silent: I/Q
// captures and register reads only.
//
//	rtlfm facts-powerup out.json          registers before any setup write
//	rtlfm facts-ablate MHz secs dir pu.json   one capture per setup write put back
//	                                      to its power-up value (and a baseline)
//	rtlfm facts-fir MHz cutHz secs out.iq load a channel FIR, read it back, capture
//	rtlfm facts-if MHz ifHz secs out.iq   tuner and demodulator on another IF
//	rtlfm facts-demodif MHz ifHz secs out.iq  only the demodulator's IF changed
//	rtlfm facts-rate Hz secs              sample rate actually delivered
//	rtlfm facts-pll fromMHz toMHz stepMHz VCO lock range per mixer divider
//	rtlfm facts-i2cw                      longest tuner write the bridge takes

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"lenovo-satellite/internal/fmradio"
	"lenovo-satellite/internal/rtlsdr"
)

type regVal struct {
	Step  int    `json:"step"`
	Demod bool   `json:"demod"`
	Block uint16 `json:"block"`
	Addr  uint16 `json:"addr"`
	N     int    `json:"n"`
	Ours  uint16 `json:"ours"`
	Power int    `json:"power"` // -1: unreadable
}

func facts(args []string, gain []int) {
	node, _, err := rtlsdr.Find()
	if err != nil {
		die("%v", err)
	}
	atoi := func(s string) int { v, _ := strconv.Atoi(s); return v }
	atof := func(s string) float64 { v, _ := strconv.ParseFloat(s, 64); return v }
	switch args[0] {
	case "facts-powerup":
		s, err := rtlsdr.OpenRaw(node)
		if err != nil {
			die("%v", err)
		}
		var out []regVal
		init := rtlsdr.BasebandInit()
		// pass 1: everything as found; pass 2 (demodulator only): after the two
		// power writes (block 2), since a powered-down demodulator does not answer
		for pass := 1; pass <= 2; pass++ {
			if pass == 2 {
				for _, w := range init {
					if !w.Demod && w.Block == 2 {
						fmt.Printf("   powering: %s %d:%04x = %02x\n", kind(w), w.Block, w.Addr, w.Val)
						if err := s.Write(w); err != nil {
							die("power: %v", err)
						}
					}
				}
			}
			for i, w := range init {
				if pass == 2 && !w.Demod {
					continue
				}
				v, err := s.Read(w)
				p := int(v)
				if err != nil {
					p = -1
				}
				if pass == 1 || !w.Demod {
					out = append(out, regVal{i, w.Demod, w.Block, w.Addr, w.N, w.Val, p})
				} else if p >= 0 && out[i].Power < 0 {
					out[i].Power = p
				}
				fmt.Printf("p%d %2d %s %d:%04x n=%d ours=%04x read=%04x err=%v\n", pass, i, kind(w), w.Block, w.Addr, w.N, w.Val, v, err)
			}
		}
		// registers the later setup writes: IF (1:19-1b), ratio (1:9f-a2), FIR (1:1c-2f)
		for _, a := range []uint16{0x19, 0x1a, 0x1b, 0x9f, 0xa0, 0xa1, 0xa2} {
			v, err := s.Read(rtlsdr.RegWrite{Demod: true, Block: 1, Addr: a, N: 1})
			fmt.Printf("   demod 1:%02x power-up=%02x err=%v\n", a, v, err)
		}
		fmt.Print("   FIR 1:1c-2f power-up:")
		for a := uint16(0x1c); a < 0x30; a++ {
			v, _ := s.Read(rtlsdr.RegWrite{Demod: true, Block: 1, Addr: a, N: 1})
			fmt.Printf(" %02x", v)
		}
		fmt.Println()
		s.CloseRaw()
		b, _ := json.MarshalIndent(out, "", " ")
		os.WriteFile(args[1], b, 0o644)

	case "facts-ablate":
		hz, secs, dir := mhz(args[1]), atof(args[2]), args[3]
		var pu []regVal
		b, err := os.ReadFile(args[4])
		if err != nil || json.Unmarshal(b, &pu) != nil {
			die("power-up file: %v", err)
		}
		os.MkdirAll(dir, 0o755)
		for step := -1; step < len(pu); step++ {
			if step >= 0 && (pu[step].Power < 0 || uint16(pu[step].Power) == pu[step].Ours) {
				fmt.Printf("step %2d: power-up value equals ours or unreadable (%d), skipped\n", step, pu[step].Power)
				continue
			}
			ov := func(i int, w rtlsdr.RegWrite) rtlsdr.RegWrite {
				if i == step {
					w.Val = uint16(pu[step].Power)
				}
				return w
			}
			name := filepath.Join(dir, fmt.Sprintf("step%02d.iq", step+1)) // step00 = baseline
			res, err := captureWith(node, ov, gain, hz, secs, name, nil)
			fmt.Printf("step %2d -> %s: %s %v\n", step, name, res, err)
		}

	case "facts-fir":
		hz, cut, secs := mhz(args[1]), atof(args[2]), atof(args[3])
		res, err := captureWith(node, nil, gain, hz, secs, args[4], func(s *rtlsdr.Stick) {
			t := rtlsdr.LowpassFIR(cut)
			if err := s.SetFIR(t); err != nil {
				die("fir: %v", err)
			}
			fmt.Println("taps", t)
			fmt.Print("read back 1:1c-2f:")
			for a := uint16(0x1c); a < 0x30; a++ {
				v, _ := s.Read(rtlsdr.RegWrite{Demod: true, Block: 1, Addr: a, N: 1})
				fmt.Printf(" %02x", v)
			}
			fmt.Println()
		})
		fmt.Println(res, err)

	case "facts-if":
		hz, ifhz, secs := mhz(args[1]), atoi(args[2]), atof(args[3])
		res, err := captureWith(node, nil, gain, hz, secs, args[4], func(s *rtlsdr.Stick) {
			if err := s.SetTunerIF(ifhz); err != nil {
				die("if: %v", err)
			}
		})
		fmt.Println(res, err)

	case "facts-demodif":
		hz, ifhz, secs := mhz(args[1]), atoi(args[2]), atof(args[3])
		res, err := captureWith(node, nil, gain, hz, secs, args[4], nil, func(s *rtlsdr.Stick) {
			if err := s.SetDemodIF(ifhz); err != nil {
				die("if: %v", err)
			}
		})
		fmt.Println(res, err)

	case "facts-proto":
		// 1. the I2C repeater: tuner reads with it off and on
		s, err := rtlsdr.Open(node, fmradio.WideRate)
		if err != nil {
			die("%v", err)
		}
		s.Repeater(false)
		_, e1 := s.RawTunerReadNoRepeater(1)
		s.Repeater(true)
		b, e2 := s.RawTunerReadNoRepeater(1)
		s.Repeater(false)
		fmt.Printf("tuner read, repeater off: %s; on: %s (% x)\n", errStr(e1), errStr(e2), b)
		s.Close()
		// 2. registers back to their power-up values (with the sync read), then the
		// setup without the read after each demodulator write, then read back
		var pu []regVal
		if raw, err := os.ReadFile(args[4]); err != nil || json.Unmarshal(raw, &pu) != nil {
			die("power-up file: %v", err)
		}
		s, err = rtlsdr.Open(node, fmradio.WideRate)
		if err != nil {
			die("%v", err)
		}
		for i, w := range rtlsdr.BasebandInit() {
			if w.Demod && pu[i].Power >= 0 && i != 5 {
				w.Val = uint16(pu[i].Power)
				s.Write(w)
			}
		}
		s.CloseRaw()
		rtlsdr.SetDemodSync(false)
		res, err := captureWith(node, nil, gain, mhz(args[1]), atof(args[2]), args[3], nil, func(s *rtlsdr.Stick) {
			bad := 0
			for i, w := range rtlsdr.BasebandInit() {
				if !w.Demod || i == 5 || i == 7 || (w.Addr == 0x15 && w.Val == 0) {
					continue // the reset pulse and the overwritten 0x15
				}
				v, err := s.Read(w)
				if err != nil || v != w.Val {
					bad++
					fmt.Printf("  step %d %d:%02x wrote %02x read %02x %v\n", i, w.Block, w.Addr, w.Val, v, err)
				}
			}
			fmt.Printf("without the sync read: %d setup registers differ\n", bad)
		})
		rtlsdr.SetDemodSync(true)
		fmt.Println(res, err)

	case "facts-rate":
		rate, secs := atoi(args[1]), atof(args[2])
		s, err := rtlsdr.Open(node, rate)
		if err != nil {
			die("%v", err)
		}
		defer s.Close()
		s.Tune(100_000_000)
		n, skip := 0, 8
		var t0 time.Time
		s.Stream(nil, 16384, func(iq []byte) bool {
			if skip > 0 { // let the FIFO settle before timing
				skip--
				if skip == 0 {
					t0 = time.Now()
				}
				return true
			}
			n += len(iq)
			return time.Since(t0).Seconds() < secs
		})
		el := time.Since(t0).Seconds()
		fmt.Printf("asked %d, driver says %d, delivered %.0f S/s over %.1f s\n", rate, s.Rate(), float64(n/2)/el, el)

	case "facts-pll":
		from, to, step := atof(args[1]), atof(args[2]), atof(args[3])
		s, err := rtlsdr.Open(node, fmradio.WideRate)
		if err != nil {
			die("%v", err)
		}
		defer s.Close()
		for f := from; f <= to+1e-9; f += step {
			ok, err := s.PLLLock(int64(f*1e6), 0)
			fmt.Printf("VCO %7.1f MHz lock=%v %v\n", f, ok, errStr(err))
		}

	case "facts-i2cw":
		s, err := rtlsdr.Open(node, fmradio.WideRate)
		if err != nil {
			die("%v", err)
		}
		defer s.Close()
		for n := 1; n <= 26; n++ {
			fmt.Printf("write of %2d registers (+1 address byte): %v\n", n, errStr(s.TunerWriteLen(n)))
		}
		// the tuner must still answer after the probing
		_, err = s.TunerRegs()
		fmt.Println("tuner read afterwards:", errStr(err))

	default:
		die("unknown %s", args[0])
	}
}

func kind(w rtlsdr.RegWrite) string {
	if w.Demod {
		return "demod"
	}
	return "block"
}

func errStr(err error) string {
	if err == nil {
		return "ok"
	}
	return err.Error()
}

// captureWith opens the stick (setup writes changed by ov), runs pre, tunes,
// and writes secs of u8 I/Q at fmradio.WideRate to name.
func captureWith(node string, ov func(int, rtlsdr.RegWrite) rtlsdr.RegWrite, gain []int, hz int, secs float64, name string, pre func(*rtlsdr.Stick), post ...func(*rtlsdr.Stick)) (string, error) {
	s, err := rtlsdr.OpenWith(node, fmradio.WideRate, ov)
	if err != nil {
		return "", err
	}
	defer s.Close()
	if pre != nil {
		pre(s)
	}
	s.SetGain(gain[0], gain[1], gain[2])
	got, err := s.Tune(hz)
	if err != nil {
		return "", err
	}
	for _, p := range post {
		p(s)
	}
	f, err := os.Create(name)
	if err != nil {
		return "", err
	}
	defer f.Close()
	want := int(secs * fmradio.WideRate * 2)
	n, skip := 0, 4
	t0 := time.Now()
	err = s.Stream(nil, 65536, func(iq []byte) bool {
		if skip > 0 {
			skip--
			return true
		}
		f.Write(iq)
		n += len(iq)
		return n < want && time.Since(t0) < time.Duration(secs*4)*time.Second
	})
	return fmt.Sprintf("tuned %d, %d bytes, %.0f S/s", got, n, float64(n/2)/time.Since(t0).Seconds()), err
}
