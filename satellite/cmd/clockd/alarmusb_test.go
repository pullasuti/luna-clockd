package main

import (
	"encoding/binary"
	"errors"
	"math"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"lenovo-satellite/internal/output"
)

// a short WAV repeats without end (until stopped); a file that isn't audio
// fails at once, so the alarm can fall back to the timer sound
func TestLoopSource(t *testing.T) {
	dir := t.TempDir()
	wav := filepath.Join(dir, "a.wav")
	if err := os.WriteFile(wav, testWAV(0.2), 0o644); err != nil {
		t.Fatal(err)
	}
	open := func() (*os.File, error) { return os.Open(wav) }
	f, _ := open()
	l := newLoopSource(f, open)
	if !l.started(time.Second) {
		t.Fatal("no audio")
	}
	buf := make([]float32, 960)
	got := 0
	for deadline := time.Now().Add(3 * time.Second); got < 3*int(0.2*output.Rate*output.Channels) && time.Now().Before(deadline); {
		n, ok := l.Read(buf)
		if !ok {
			t.Fatal("ended")
		}
		got += n
		if n == 0 {
			time.Sleep(5 * time.Millisecond)
		}
	}
	if got < 3*int(0.2*output.Rate*output.Channels) {
		t.Fatalf("only %d samples: it didn't repeat", got)
	}
	l.stop()
	if l.failure() != nil {
		t.Error(l.failure())
	}

	bad := filepath.Join(dir, "b.mp3")
	os.WriteFile(bad, []byte("not audio at all, not audio at all"), 0o644)
	f, _ = os.Open(bad)
	l = newLoopSource(f, func() (*os.File, error) { return nil, errors.New("gone") })
	if l.started(time.Second) {
		t.Error("a bad file started")
	}
	l.stop()

	// the drive goes away after the first pass: the source ends with an error
	var gone atomic.Bool
	f, _ = open()
	l = newLoopSource(f, func() (*os.File, error) {
		if gone.Load() {
			return nil, errors.New("no drive")
		}
		return open()
	})
	if !l.started(time.Second) {
		t.Fatal("no audio")
	}
	gone.Store(true)
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
		if _, ok := l.Read(buf); !ok {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if l.failure() == nil {
		t.Error("no failure after the drive went away")
	}
	l.stop()
}

// testWAV: secs of a 440 Hz tone, 16-bit stereo at the output rate
func testWAV(secs float64) []byte {
	frames := int(secs * output.Rate)
	b := make([]byte, 44+frames*4)
	copy(b, "RIFF")
	binary.LittleEndian.PutUint32(b[4:], uint32(36+frames*4))
	copy(b[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(b[16:], 16)
	binary.LittleEndian.PutUint16(b[20:], 1)
	binary.LittleEndian.PutUint16(b[22:], 2)
	binary.LittleEndian.PutUint32(b[24:], output.Rate)
	binary.LittleEndian.PutUint32(b[28:], output.Rate*4)
	binary.LittleEndian.PutUint16(b[32:], 4)
	binary.LittleEndian.PutUint16(b[34:], 16)
	copy(b[36:], "data")
	binary.LittleEndian.PutUint32(b[40:], uint32(frames*4))
	for i := 0; i < frames; i++ {
		v := int16(8000 * math.Sin(2*math.Pi*440*float64(i)/output.Rate))
		binary.LittleEndian.PutUint16(b[44+4*i:], uint16(v))
		binary.LittleEndian.PutUint16(b[46+4*i:], uint16(v))
	}
	return b
}
