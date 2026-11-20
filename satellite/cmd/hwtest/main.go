// hwtest: minimal static ALSA probe for the Lenovo Smart Clock.
//
//	hwtest list
//	hwtest rec  <card> <dev> <channels> <rate> <seconds> <out.wav>
//	hwtest tone <card> <dev> <freq> <dBFS> <seconds>
package main

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"runtime"
	"strconv"

	"github.com/yobert/alsa"
)

func die(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...); os.Exit(1) }

func atoi(s string) int {
	v, err := strconv.Atoi(s)
	if err != nil {
		die("bad number %q", s)
	}
	return v
}

func find(card, dev int, play bool) (*alsa.Device, []*alsa.Card) {
	cards, err := alsa.OpenCards()
	if err != nil {
		die("open cards: %v", err)
	}
	for _, c := range cards {
		if c.Number != card {
			continue
		}
		devs, err := c.Devices()
		if err != nil {
			die("devices: %v", err)
		}
		for _, d := range devs {
			if d.Type == alsa.PCM && d.Number == dev && d.Play == play && d.Record == !play {
				return d, cards
			}
		}
	}
	die("hw:%d,%d (play=%v) not found", card, dev, play)
	return nil, nil
}

func setup(d *alsa.Device, ch, rate int, fmts ...alsa.FormatType) alsa.FormatType {
	if err := d.Open(); err != nil {
		die("open %s: %v", d.Path, err)
	}
	if _, err := d.NegotiateChannels(ch); err != nil {
		die("channels: %v", err)
	}
	if _, err := d.NegotiateRate(rate); err != nil {
		die("rate: %v", err)
	}
	f, err := d.NegotiateFormat(fmts...)
	if err != nil {
		die("format: %v", err)
	}
	cands := []int{256, 512, 1024, 128, 2048}
	if e := os.Getenv("PERIOD"); e != "" {
		cands = []int{atoi(e)}
	}
	p, err := d.NegotiatePeriodSize(cands...)
	if err != nil {
		die("period: %v", err)
	}
	b, err := d.NegotiateBufferSize(p*4, p*8, p*2)
	if err != nil {
		die("buffer: %v", err)
	}
	if err := d.Prepare(); err != nil {
		die("prepare: %v", err)
	}
	// yobert/alsa sets start_threshold = buffer size; a capture read must
	// request >= that many frames or the stream never starts (EIO after timeout).
	periodFrames = b
	fmt.Printf("negotiated %s: ch=%d rate=%d fmt=%v period=%d buffer=%d\n", d.Path, ch, rate, f, p, b)
	return f
}

var periodFrames int

func main() {
	if len(os.Args) < 2 {
		die("usage: hwtest list | rec card dev ch rate sec out.wav | tone card dev freq dBFS sec")
	}
	fmt.Printf("hwtest %s/%s\n", runtime.GOOS, runtime.GOARCH)
	switch os.Args[1] {
	case "list":
		cards, err := alsa.OpenCards()
		if err != nil {
			die("%v", err)
		}
		for _, c := range cards {
			devs, _ := c.Devices()
			for _, d := range devs {
				fmt.Printf("card %d dev %d play=%v rec=%v %s (%s)\n", c.Number, d.Number, d.Play, d.Record, d.Title, d.Path)
			}
		}
	case "rec":
		card, dev, ch, rate, sec := atoi(os.Args[2]), atoi(os.Args[3]), atoi(os.Args[4]), atoi(os.Args[5]), atoi(os.Args[6])
		d, cards := find(card, dev, false)
		defer alsa.CloseCards(cards)
		setup(d, ch, rate, alsa.S32_LE)
		period := periodFrames
		buf := make([]byte, period*ch*4)
		var data []byte
		for got := 0; got < sec*rate; got += period {
			if err := d.Read(buf); err != nil {
				die("read: %v", err)
			}
			data = append(data, buf...)
		}
		d.Close()
		// per-channel RMS (skip first 0.5 s of filter settle)
		n := len(data) / 4 / ch
		for c := 0; c < ch; c++ {
			var sum, mean float64
			cnt := 0
			for i := rate / 2; i < n; i++ {
				mean += float64(int32(binary.LittleEndian.Uint32(data[(i*ch+c)*4:])))
				cnt++
			}
			mean /= float64(cnt)
			for i := rate / 2; i < n; i++ {
				v := float64(int32(binary.LittleEndian.Uint32(data[(i*ch+c)*4:]))) - mean
				sum += v * v
			}
			rms := math.Sqrt(sum/float64(cnt)) / 2147483648.0
			fmt.Printf("ch%d rms %.1f dBFS\n", c, 20*math.Log10(rms+1e-12))
		}
		writeWav(os.Args[7], data, ch, rate)
		fmt.Printf("wrote %s (%d frames)\n", os.Args[7], n)
	case "tone":
		card, dev, freq, db, sec := atoi(os.Args[2]), atoi(os.Args[3]), atoi(os.Args[4]), atoi(os.Args[5]), atoi(os.Args[6])
		if db > -6 {
			die("refusing amplitude above -6 dBFS")
		}
		d, cards := find(card, dev, true)
		defer alsa.CloseCards(cards)
		f := setup(d, 2, 48000, alsa.S32_LE, alsa.S16_LE)
		amp := math.Pow(10, float64(db)/20)
		period := periodFrames
		bpf := d.BytesPerFrame()
		buf := make([]byte, period*bpf)
		t := 0
		for t < sec*48000 {
			for j := 0; j < period; j++ {
				s := amp * math.Sin(2*math.Pi*float64(freq)*float64(t)/48000)
				t++
				for c := 0; c < 2; c++ {
					off := j*bpf + c*bpf/2
					if f == alsa.S32_LE {
						binary.LittleEndian.PutUint32(buf[off:], uint32(int32(s*2147483647)))
					} else {
						binary.LittleEndian.PutUint16(buf[off:], uint16(int16(s*32767)))
					}
				}
			}
			if err := d.Write(buf, period); err != nil {
				die("write: %v", err)
			}
		}
		d.Close()
		fmt.Printf("played %d Hz at %d dBFS for %d s (format %v)\n", freq, db, sec, f)
	default:
		die("unknown command")
	}
}

func writeWav(path string, data []byte, ch, rate int) {
	f, err := os.Create(path)
	if err != nil {
		die("%v", err)
	}
	defer f.Close()
	h := make([]byte, 44)
	copy(h[0:], "RIFF")
	binary.LittleEndian.PutUint32(h[4:], uint32(36+len(data)))
	copy(h[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(h[16:], 16)
	binary.LittleEndian.PutUint16(h[20:], 1)
	binary.LittleEndian.PutUint16(h[22:], uint16(ch))
	binary.LittleEndian.PutUint32(h[24:], uint32(rate))
	binary.LittleEndian.PutUint32(h[28:], uint32(rate*ch*4))
	binary.LittleEndian.PutUint16(h[32:], uint16(ch*4))
	binary.LittleEndian.PutUint16(h[34:], 32)
	copy(h[36:], "data")
	binary.LittleEndian.PutUint32(h[40:], uint32(len(data)))
	f.Write(h)
	f.Write(data)
}
