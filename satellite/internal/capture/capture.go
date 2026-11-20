// Package capture reads microphone audio from iot_audio_capture's Cast capture
// service (abstract unix socket @/tmp/capture-service).
//
// Wire format on this firmware (see notes): every message is a uint16 BE size
// followed by the body. The client sends a 14-byte handshake; the server then
// streams PCM bodies of type(1) stream(1) format(1) channels(1) rate(u16 LE)
// timestamp(i64 LE) followed by samples.
package capture

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"time"
)

// Stream types offered by iot_audio_capture.
const (
	StreamMicRaw           = 0 // 2 ch, unprocessed
	StreamEchoCancelled    = 1 // mono, AEC + NS + gain (needs the loopback reference)
	StreamEchoRescaled     = 4
	socketPath             = "@/tmp/capture-service"
	handshakeBodyBytes     = 14
	pcmHeaderBytes         = 14
	sampleRateWanted       = 16000
	framesPerBufferRequest = 160
)

// Client streams 16 kHz mono int16 audio to a callback, reconnecting on errors.
type Client struct {
	Stream int
	Log    *slog.Logger
}

// Run blocks until ctx is done. fn receives mono 16-bit samples (channel 0)
// at the stream's native rate (16 kHz on this device). fn must not retain
// the slice.
func (c *Client) Run(ctx context.Context, fn func(samples []int16)) error {
	log := c.Log
	if log == nil {
		log = slog.Default()
	}
	backoff := 200 * time.Millisecond
	for ctx.Err() == nil {
		err := c.session(ctx, fn)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		log.Warn("capture session ended", "err", err, "retry_in", backoff)
		select {
		case <-ctx.Done():
		case <-time.After(backoff):
		}
		if backoff < 5*time.Second {
			backoff *= 2
		}
	}
	return ctx.Err()
}

func (c *Client) session(ctx context.Context, fn func([]int16)) error {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", socketPath)
	if err != nil {
		return err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()

	hs := make([]byte, 2+handshakeBodyBytes)
	binary.BigEndian.PutUint16(hs[0:], handshakeBodyBytes)
	hs[2] = 0 // kHandshake
	hs[3] = byte(c.Stream)
	hs[4] = 0 // PCM
	hs[5] = 0 // INTERLEAVED_INT16 (the server picks its own format anyway)
	hs[6] = 1 // channels
	binary.LittleEndian.PutUint16(hs[8:], framesPerBufferRequest)
	binary.LittleEndian.PutUint32(hs[12:], sampleRateWanted)
	if _, err := conn.Write(hs); err != nil {
		return err
	}

	hdr := make([]byte, 2)
	body := make([]byte, 65536)
	var out []int16
	for {
		conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		if _, err := io.ReadFull(conn, hdr); err != nil {
			return err
		}
		n := int(binary.BigEndian.Uint16(hdr))
		if _, err := io.ReadFull(conn, body[:n]); err != nil {
			return err
		}
		b := body[:n]
		if n < pcmHeaderBytes || b[0] != 1 {
			continue // handshake echo or unknown message
		}
		format, ch := int(b[2]), int(b[3])
		if ch < 1 {
			return fmt.Errorf("bad channel count %d", ch)
		}
		out = channel0(out[:0], b[pcmHeaderBytes:], format, ch)
		fn(out)
	}
}

// channel0 extracts channel 0 of one packet as int16.
func channel0(dst []int16, b []byte, format, ch int) []int16 {
	planar := format >= 3
	var size int
	switch format {
	case 0, 3:
		size = 2
	default:
		size = 4
	}
	frames := len(b) / size / ch
	for f := 0; f < frames; f++ {
		idx := f * ch
		if planar {
			idx = f // channel 0 block comes first
		}
		var v float64
		switch format {
		case 0, 3:
			dst = append(dst, int16(binary.LittleEndian.Uint16(b[idx*2:])))
			continue
		case 1, 4:
			v = float64(int32(binary.LittleEndian.Uint32(b[idx*4:]))) / 2147483648
		default:
			v = float64(math.Float32frombits(binary.LittleEndian.Uint32(b[idx*4:])))
		}
		v *= 32767
		if v > 32767 {
			v = 32767
		} else if v < -32768 {
			v = -32768
		}
		dst = append(dst, int16(v))
	}
	return dst
}
