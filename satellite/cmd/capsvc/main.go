// capsvc: client for the Cast "capture service" served by iot_audio_capture
// on the abstract unix socket @/tmp/capture-service.
//
// Wire format (chromecast/media/audio/capture_service, Chromium ~M89):
//
//	every message: uint16 big-endian body size, then body
//	handshake body (14 B): type=0, stream_type, codec, sample_format,
//	                       num_channels, pad, uint16 frames (LE), pad16, uint32 rate (LE)
//	pcm body: type=1, stream_type, int64 timestamp_us (LE), then audio
//
//	capsvc <stream_type> <seconds> <out.wav> [gain dB]
//	stream types: 0 mic raw, 1 sw AEC, 2 sw AEC linear, 3 hw AEC, 4 sw echo rescaled, 5 hw rescaled
package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"strconv"
	"time"
)

func die(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...); os.Exit(1) }

var formatNames = []string{"S16 interleaved", "S32 interleaved", "float interleaved", "S16 planar", "S32 planar", "float planar"}

func main() {
	if len(os.Args) < 4 {
		die("usage: capsvc <stream_type 0-5> <seconds> <out.wav>")
	}
	st, _ := strconv.Atoi(os.Args[1])
	sec, _ := strconv.Atoi(os.Args[2])
	if len(os.Args) > 4 { // optional gain in dB
		db, _ := strconv.ParseFloat(os.Args[4], 64)
		gain = math.Pow(10, db/20)
	}

	c, err := net.Dial("unix", "@/tmp/capture-service")
	if err != nil {
		die("dial: %v", err)
	}
	defer c.Close()

	// request: PCM, S16 interleaved, 1 ch, 160 frames, 16 kHz
	hs := make([]byte, 16)
	binary.BigEndian.PutUint16(hs[0:], 14)
	hs[2] = 0        // kHandshake
	hs[3] = byte(st) // stream type
	hs[4] = 0        // kPcm
	hs[5] = 0        // INTERLEAVED_INT16
	hs[6] = 1        // channels
	binary.LittleEndian.PutUint16(hs[8:], 160)
	binary.LittleEndian.PutUint32(hs[12:], 16000)
	if _, err := c.Write(hs); err != nil {
		die("write handshake: %v", err)
	}

	var (
		ch, rate, fmtID = 1, 16000, 0
		pcm             []byte
		packets         int
		deadline        = time.Now().Add(time.Duration(sec)*time.Second + 3*time.Second)
	)
	c.SetReadDeadline(deadline)
	hdr := make([]byte, 2)
	for {
		if _, err := io.ReadFull(c, hdr); err != nil {
			fmt.Printf("read stopped: %v\n", err)
			break
		}
		body := make([]byte, binary.BigEndian.Uint16(hdr))
		if _, err := io.ReadFull(c, body); err != nil {
			fmt.Printf("read body: %v\n", err)
			break
		}
		if os.Getenv("DUMP") != "" && packets < 3 {
			m := len(body)
			if m > 32 {
				m = 32
			}
			fmt.Printf("body len=%d: % x\n", len(body), body[:m])
		}
		switch body[0] {
		case 0:
			if len(body) != 14 {
				die("handshake reply len %d", len(body))
			}
			fmtID, ch = int(body[3]), int(body[4])
			rate = int(binary.LittleEndian.Uint32(body[10:]))
			fmt.Printf("server handshake: stream=%d codec=%d format=%d(%s) ch=%d frames=%d rate=%d\n",
				body[1], body[2], fmtID, formatNames[fmtID%6], ch, binary.LittleEndian.Uint16(body[6:]), rate)
		case 1:
			// this firmware: type, stream, format, channels, uint16 rate (LE), int64 ts, audio
			packets++
			fmtID, ch = int(body[2]), int(body[3])
			rate = int(binary.LittleEndian.Uint16(body[4:]))
			if packets == 1 {
				fmt.Printf("pcm: format=%d(%s) ch=%d rate=%d bytes/packet=%d\n", fmtID, formatNames[fmtID%6], ch, rate, len(body)-14)
			}
			pcm = append(pcm, toS16(body[14:], fmtID, ch)...)
		default:
			fmt.Printf("message type %d, %d bytes (ignored)\n", body[0], len(body))
		}
		if len(pcm) >= sec*rate*ch*2 {
			break
		}
	}
	fmt.Printf("packets=%d bytes=%d\n", packets, len(pcm))
	if len(pcm) == 0 {
		return
	}
	// per-channel level (skip first 0.5 s)
	n := len(pcm) / 2 / ch
	for c := 0; c < ch; c++ {
		var sum float64
		cnt := 0
		for i := rate / 2; i < n; i++ {
			v := float64(int16(binary.LittleEndian.Uint16(pcm[(i*ch+c)*2:])))
			sum += v * v
			cnt++
		}
		if cnt > 0 {
			fmt.Printf("ch%d rms %.1f dBFS\n", c, 20*math.Log10(math.Sqrt(sum/float64(cnt))/32768+1e-12))
		}
	}
	writeWav(os.Args[3], pcm, ch, rate)
	fmt.Printf("wrote %s\n", os.Args[3])
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
	binary.LittleEndian.PutUint32(h[28:], uint32(rate*ch*2))
	binary.LittleEndian.PutUint16(h[32:], uint16(ch*2))
	binary.LittleEndian.PutUint16(h[34:], 16)
	copy(h[36:], "data")
	binary.LittleEndian.PutUint32(h[40:], uint32(len(data)))
	f.Write(h)
	f.Write(data)
}

// toS16 converts one packet to interleaved int16.
// gain (linear) is applied before the 16-bit conversion (the raw mic stream is
// around -87 dBFS: without gain it would be mostly quantisation).
var gain = 1.0

func toS16(b []byte, f, ch int) []byte {
	var frames int
	sample := func(i int) float64 { return 0 }
	switch f {
	case 0, 3: // int16
		frames = len(b) / 2 / ch
		sample = func(i int) float64 { return float64(int16(binary.LittleEndian.Uint16(b[i*2:]))) / 32768 }
	case 1, 4: // int32
		frames = len(b) / 4 / ch
		sample = func(i int) float64 { return float64(int32(binary.LittleEndian.Uint32(b[i*4:]))) / 2147483648 }
	default: // float
		frames = len(b) / 4 / ch
		sample = func(i int) float64 { return float64(math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))) }
	}
	planar := f >= 3
	out := make([]byte, frames*ch*2)
	for fr := 0; fr < frames; fr++ {
		for c := 0; c < ch; c++ {
			idx := fr*ch + c
			if planar {
				idx = c*frames + fr
			}
			v := sample(idx) * 32767 * gain
			if v > 32767 {
				v = 32767
			} else if v < -32768 {
				v = -32768
			}
			binary.LittleEndian.PutUint16(out[(fr*ch+c)*2:], uint16(int16(v)))
		}
	}
	return out
}
