package main

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"sync/atomic"
	"time"

	"lenovo-satellite/internal/localmic"
)

// Test captures: GET /api/capture streams what the local microphone path sees
// as a 16 kHz 16-bit WAV with five channels, so a wake word test run can be
// replayed and scored offline (tools/aec/apmtool, cmd/wwfile) with any
// canceller setting, without playing anything in the room again:
//
//	0 mic    mono mic exactly as the canceller gets it (after its pre-gain:
//	         replay with apmtool --pre 0)
//	1 left   mic 1 (same gain, no DC filter)
//	2 right  mic 2
//	3 ref    the speaker reference the canceller saw for that block
//	4 out    what went on to the wake word models
//
// Nothing is stored on the clock. Only with the WebRTC source
// (the Original one gives no raw microphones); one stream at a time.

const captureChannels = 5

var captureBusy atomic.Bool

// errNoCapture: the microphone path has no tap (Original source).
var errNoCapture = errors.New("captures need the WebRTC microphone source")

func (ms *micSource) tapTarget() *localmic.Mic {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	if ms.active == "vendor" || ms.active == "" {
		return nil
	}
	return ms.local
}

// capture streams up to max of capture to w. It ends when ctx ends (the client
// went away), max is reached, or no audio comes for 3 s (source switched).
func (ms *micSource) capture(ctx context.Context, max time.Duration, w io.Writer, flush func()) error {
	l := ms.tapTarget()
	if l == nil {
		return errNoCapture
	}
	if !captureBusy.CompareAndSwap(false, true) {
		return errors.New("a capture is already running")
	}
	defer captureBusy.Store(false)

	chunks := make(chan []byte, 256) // ~4 s at 16 ms blocks
	var dropped atomic.Int64
	l.SetTap(func(mono, left, right, ref, out []int16) {
		b := make([]byte, 2*captureChannels*len(mono))
		for i := range mono {
			o := 2 * captureChannels * i
			for c, v := range [captureChannels]int16{mono[i], left[i], right[i], ref[i], out[i]} {
				binary.LittleEndian.PutUint16(b[o+2*c:], uint16(v))
			}
		}
		select {
		case chunks <- b:
		default:
			dropped.Add(1)
		}
	})
	defer l.SetTap(nil)

	// length unknown: readers take the data to the end; run.py fixes it on saving
	if _, err := w.Write(wavHeader(localmic.Rate, captureChannels, 0xFFFFFFFF-36)); err != nil {
		return err
	}
	flush()
	ms.d.log.Info("test capture started", "max", max)
	start := time.Now()
	end := time.NewTimer(max)
	defer end.Stop()
	var bytes int64
	defer func() {
		ms.d.log.Info("test capture ended", "seconds", float64(bytes)/(2*captureChannels*localmic.Rate),
			"dropped_blocks", dropped.Load(), "took", time.Since(start).Round(time.Second))
	}()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-end.C:
			return nil
		case <-time.After(3 * time.Second):
			return errors.New("no audio for 3 s (microphone source changed?)")
		case b := <-chunks:
			if _, err := w.Write(b); err != nil {
				return nil // the client went away
			}
			bytes += int64(len(b))
			if len(chunks) == 0 {
				flush()
			}
		}
	}
}
