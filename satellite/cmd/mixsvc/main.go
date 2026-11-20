// mixsvc: minimal stand-in for the Cast mixer service (@/tmp/mixer-service).
//
// It plays audio on the speaker itself and sends the same samples to every
// client that asked for loopback (iot_audio_capture uses this as the AEC
// reference). Everything a client sends is logged, because this firmware's
// protocol may differ from the Chromium M89 sources it was written from.
//
// Wire format (chromecast/media/audio/mixer_service, Chromium ~M89):
//
//	message  = uint16 BE size, body
//	metadata = int16 type 0, int32 BE padding count, protobuf Generic, padding
//	audio    = int16 type 1 (native LE), int64 timestamp_us (LE), int32 pad, samples
//
//	mixsvc tone <freq> <dBFS> <seconds>     wait for a loopback client, then play
//	mixsvc listen <seconds>                 only log clients
package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/yobert/alsa"
	"golang.org/x/sys/unix"
)

const (
	outRate     = 48000
	outChannels = 2
)

func die(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...); os.Exit(1) }
func logf(f string, a ...any) {
	fmt.Printf("%s "+f+"\n", append([]any{time.Now().Format("15:04:05.000")}, a...)...)
}

func monoUs() int64 {
	var ts unix.Timespec
	unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts)
	return ts.Sec*1e6 + ts.Nsec/1e3
}

// ---- minimal protobuf helpers ----

func pbVarint(b []byte, v uint64) []byte {
	for v >= 0x80 {
		b = append(b, byte(v)|0x80)
		v >>= 7
	}
	return append(b, byte(v))
}
func pbField(b []byte, field int, v uint64) []byte {
	return pbVarint(pbVarint(b, uint64(field<<3)), v)
}
func pbBytes(b []byte, field int, data []byte) []byte {
	b = pbVarint(b, uint64(field<<3|2))
	return append(pbVarint(b, uint64(len(data))), data...)
}

// pbTopFields returns the top-level field numbers present in a message.
func pbTopFields(b []byte) []int {
	var out []int
	for i := 0; i < len(b); {
		key, n := binary.Uvarint(b[i:])
		if n <= 0 {
			return append(out, -1)
		}
		i += n
		out = append(out, int(key>>3))
		switch key & 7 {
		case 0:
			_, n = binary.Uvarint(b[i:])
			i += n
		case 1:
			i += 8
		case 2:
			l, n := binary.Uvarint(b[i:])
			i += n + int(l)
		case 5:
			i += 4
		default:
			return append(out, -1)
		}
	}
	return out
}

// ---- connections ----

type client struct {
	c        net.Conn
	mu       sync.Mutex
	loopback bool
}

var (
	clientsMu sync.Mutex
	clients   []*client
)

func (cl *client) send(b []byte) error {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	cl.c.SetWriteDeadline(time.Now().Add(200 * time.Millisecond))
	_, err := cl.c.Write(b)
	return err
}

func metadataMsg(proto []byte) []byte {
	pad := (4 - len(proto)%4) % 4
	body := make([]byte, 0, 6+len(proto)+pad)
	body = binary.BigEndian.AppendUint16(body, 0) // kMetadata
	body = binary.BigEndian.AppendUint32(body, uint32(pad))
	body = append(body, proto...)
	body = append(body, make([]byte, pad)...)
	return append(binary.BigEndian.AppendUint16(nil, uint16(len(body))), body...)
}

func audioMsg(ts int64, samples []byte) []byte {
	body := make([]byte, 14, 14+len(samples))
	binary.LittleEndian.PutUint16(body[0:], 1) // kAudio, native order
	binary.LittleEndian.PutUint64(body[2:], uint64(ts))
	body = append(body, samples...)
	return append(binary.BigEndian.AppendUint16(nil, uint16(len(body))), body...)
}

func streamConfig() []byte {
	var sc []byte
	sc = pbField(sc, 1, 2) // SAMPLE_FORMAT_FLOAT_I
	sc = pbField(sc, 2, outRate)
	sc = pbField(sc, 3, outChannels)
	return pbBytes(nil, 9, sc) // Generic.stream_config
}

func handle(cl *client, id int) {
	defer cl.c.Close()
	hdr := make([]byte, 2)
	for {
		if _, err := io.ReadFull(cl.c, hdr); err != nil {
			logf("client %d closed: %v", id, err)
			clientsMu.Lock()
			for i, c := range clients {
				if c == cl {
					clients = append(clients[:i], clients[i+1:]...)
					break
				}
			}
			clientsMu.Unlock()
			return
		}
		body := make([]byte, binary.BigEndian.Uint16(hdr))
		if _, err := io.ReadFull(cl.c, body); err != nil {
			logf("client %d body: %v", id, err)
			return
		}
		m := len(body)
		if m > 48 {
			m = 48
		}
		logf("client %d sent %d bytes: % x", id, len(body), body[:m])
		if len(body) < 6 {
			continue
		}
		typ := binary.LittleEndian.Uint16(body[0:]) // 0 is 0 in either byte order
		if typ != 0 {
			continue
		}
		pad := int(binary.BigEndian.Uint32(body[2:]))
		if pad > 3 || 6+pad > len(body) {
			logf("client %d: odd padding %d", id, pad)
			continue
		}
		proto := body[6 : len(body)-pad]
		fields := pbTopFields(proto)
		logf("client %d metadata fields %v", id, fields)
		for _, f := range fields {
			if f == 10 { // loopback_request
				cl.mu.Lock()
				cl.loopback = true
				cl.mu.Unlock()
				if err := cl.send(metadataMsg(streamConfig())); err != nil {
					logf("client %d: send config: %v", id, err)
				} else {
					logf("client %d: loopback subscribed, sent stream_config float/%d/%d", id, outRate, outChannels)
				}
			}
		}
	}
}

func loopbackClients() []*client {
	clientsMu.Lock()
	defer clientsMu.Unlock()
	var out []*client
	for _, c := range clients {
		c.mu.Lock()
		if c.loopback {
			out = append(out, c)
		}
		c.mu.Unlock()
	}
	return out
}

func main() {
	if len(os.Args) < 3 {
		die("usage: mixsvc tone <freq> <dBFS> <sec> | mixsvc listen <sec>")
	}
	ln, err := net.Listen("unix", "@/tmp/mixer-service")
	if err != nil {
		die("listen: %v", err)
	}
	logf("listening on @/tmp/mixer-service")
	go func() {
		id := 0
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			id++
			cl := &client{c: c}
			clientsMu.Lock()
			clients = append(clients, cl)
			clientsMu.Unlock()
			logf("client %d connected", id)
			go handle(cl, id)
		}
	}()

	switch os.Args[1] {
	case "listen":
		sec, _ := strconv.Atoi(os.Args[2])
		time.Sleep(time.Duration(sec) * time.Second)
	case "tone":
		freq, _ := strconv.Atoi(os.Args[2])
		db, _ := strconv.Atoi(os.Args[3])
		sec, _ := strconv.Atoi(os.Args[4])
		if db > -6 {
			die("refusing amplitude above -6 dBFS")
		}
		for i := 0; i < 150 && len(loopbackClients()) == 0; i++ {
			time.Sleep(100 * time.Millisecond)
		}
		logf("loopback clients: %d", len(loopbackClients()))
		playTone(freq, db, sec)
	}
	logf("done")
}

func playTone(freq, db, sec int) {
	cards, err := alsa.OpenCards()
	if err != nil {
		die("cards: %v", err)
	}
	defer alsa.CloseCards(cards)
	var dev *alsa.Device
	for _, c := range cards {
		if c.Number != 0 {
			continue
		}
		ds, _ := c.Devices()
		for _, d := range ds {
			if d.Number == 2 && d.Play {
				dev = d
			}
		}
	}
	if dev == nil {
		die("hw:0,2 not found")
	}
	if err := dev.Open(); err != nil {
		die("open: %v", err)
	}
	defer dev.Close()
	dev.NegotiateChannels(outChannels)
	dev.NegotiateRate(outRate)
	if _, err := dev.NegotiateFormat(alsa.S32_LE); err != nil {
		die("format: %v", err)
	}
	period, err := dev.NegotiatePeriodSize(480, 512, 256)
	if err != nil {
		die("period: %v", err)
	}
	bufFrames, err := dev.NegotiateBufferSize(period*4, period*8)
	if err != nil {
		die("buffer: %v", err)
	}
	if err := dev.Prepare(); err != nil {
		die("prepare: %v", err)
	}
	logf("playing on hw:0,2 period=%d buffer=%d", period, bufFrames)

	amp := math.Pow(10, float64(db)/20)
	out := make([]byte, period*outChannels*4)
	lb := make([]byte, period*outChannels*4)
	latencyUs := int64(bufFrames) * 1e6 / outRate
	type lbItem struct {
		at  time.Time
		msg []byte
	}
	lbq := make(chan lbItem, 256)
	pacerDone := make(chan struct{})
	go func() {
		defer close(pacerDone)
		for it := range lbq {
			time.Sleep(time.Until(it.at))
			for _, cl := range loopbackClients() {
				if err := cl.send(it.msg); err != nil {
					logf("loopback send: %v", err)
				}
			}
		}
	}()
	t := 0
	sent := 0
	noLB := os.Getenv("NOLB") != ""
	rng := uint32(12345)
	for t < sec*outRate {
		for j := 0; j < period; j++ {
			var s float64
			if freq == 0 { // white noise
				rng = rng*1664525 + 1013904223
				s = amp * math.Sqrt(3) * (float64(rng)/2147483648.0 - 1)
			} else {
				s = amp * math.Sin(2*math.Pi*float64(freq)*float64(t)/outRate)
			}
			t++
			for c := 0; c < outChannels; c++ {
				off := (j*outChannels + c) * 4
				binary.LittleEndian.PutUint32(out[off:], uint32(int32(s*2147483647)))
				binary.LittleEndian.PutUint32(lb[off:], math.Float32bits(float32(s)))
			}
		}
		if err := dev.Write(out, period); err != nil {
			die("write: %v", err)
		}
		// Timestamp = when these samples reach the speaker (roughly one buffer later).
		msg := audioMsg(monoUs()+latencyUs, lb) // audioMsg copies lb
		if noLB {
			continue
		}
		// Hand the packet to the pacer: it is sent when the samples actually
		// reach the speaker, so AEC3 sees render audio at a steady real-time rate.
		lbq <- lbItem{at: time.Now().Add(time.Duration(latencyUs) * time.Microsecond), msg: msg}
		sent++
	}
	close(lbq)
	<-pacerDone
	logf("played %d Hz at %d dBFS for %d s, sent %d loopback packets", freq, db, sec, sent)
}
