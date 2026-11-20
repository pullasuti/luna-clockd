package output

import (
	"bytes"
	"encoding/binary"
	"log/slog"
	"math"
	"testing"
	"time"
)

func makeWAV(rate, ch int, frames int, freq float64) []byte {
	var data bytes.Buffer
	for i := 0; i < frames; i++ {
		v := int16(16000 * math.Sin(2*math.Pi*freq*float64(i)/float64(rate)))
		for c := 0; c < ch; c++ {
			binary.Write(&data, binary.LittleEndian, v)
		}
	}
	h := make([]byte, 44)
	copy(h, "RIFF")
	binary.LittleEndian.PutUint32(h[4:], uint32(36+data.Len()))
	copy(h[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(h[16:], 16)
	binary.LittleEndian.PutUint16(h[20:], 1)
	binary.LittleEndian.PutUint16(h[22:], uint16(ch))
	binary.LittleEndian.PutUint32(h[24:], uint32(rate))
	binary.LittleEndian.PutUint32(h[28:], uint32(rate*ch*2))
	binary.LittleEndian.PutUint16(h[32:], uint16(ch*2))
	binary.LittleEndian.PutUint16(h[34:], 16)
	copy(h[36:], "data")
	binary.LittleEndian.PutUint32(h[40:], 0xFFFFFFFF) // streaming length
	return append(h, data.Bytes()...)
}

func decodeAll(t *testing.T, wav []byte) []float32 {
	b := NewBuffer(600)
	if err := DecodeWAV(bytes.NewReader(wav), b); err != nil {
		t.Fatal(err)
	}
	var out []float32
	tmp := make([]float32, 960)
	for {
		n, ok := b.Read(tmp)
		out = append(out, tmp[:n]...)
		if !ok {
			return out
		}
	}
}

func TestDecodeRates(t *testing.T) {
	for _, tc := range []struct{ rate, ch int }{{48000, 2}, {16000, 1}, {22050, 1}, {44100, 2}} {
		out := decodeAll(t, makeWAV(tc.rate, tc.ch, tc.rate, 440)) // 1 s
		frames := len(out) / 2
		if frames < 47900 || frames > 48100 {
			t.Errorf("%d Hz %d ch: got %d output frames, want ~48000", tc.rate, tc.ch, frames)
		}
		// zero crossings of the left channel ~ 2*440
		zc := 0
		for i := 1; i < frames; i++ {
			if (out[2*i-2] < 0) != (out[2*i] < 0) {
				zc++
			}
		}
		if zc < 860 || zc > 900 {
			t.Errorf("%d Hz: %d zero crossings, want ~880", tc.rate, zc)
		}
		if out[2*1000] != out[2*1000+1] {
			t.Errorf("%d Hz: channels differ", tc.rate)
		}
	}
}

func TestSamplesLoop(t *testing.T) {
	s := NewSamples([]float32{1, 1, 2, 2}, 1)
	buf := make([]float32, 16)
	n, ok := s.Read(buf)
	if n != 8 || ok {
		t.Fatalf("n=%d ok=%v, want 8 false", n, ok)
	}
}

// constSource plays a fixed amplitude for a number of periods.
type constSource struct {
	amp  float32
	left int
}

func (c *constSource) Read(dst []float32) (int, bool) {
	if c.left == 0 {
		return 0, false
	}
	c.left--
	for i := range dst {
		dst[i] = c.amp
	}
	return len(dst), true
}

func TestLevel(t *testing.T) {
	o := New(slog.New(slog.DiscardHandler))
	o.SetVolume(0) // the level is measured before the volume
	o.Play(&constSource{amp: 0.5, left: 3}, 1, "voice")
	o.Play(&constSource{amp: 0.2, left: 3}, 1, "media")
	mix, tmp := make([]float32, period*Channels), make([]float32, period*Channels)
	t0 := time.Now()
	for i := 0; i < 5; i++ {
		o.mixPeriod(mix, tmp, t0.Add(time.Duration(i)*10*time.Millisecond))
	}
	if l := o.Level("voice", t0, t0.Add(20*time.Millisecond)); l < 0.49 || l > 0.51 {
		t.Errorf("voice level %v, want 0.5", l)
	}
	if l := o.Level("media", t0, t0.Add(20*time.Millisecond)); l < 0.19 || l > 0.21 {
		t.Errorf("media level %v, want 0.2", l)
	}
	if l := o.Level("voice", t0.Add(35*time.Millisecond), t0.Add(time.Second)); l != 0 {
		t.Errorf("voice level after it ended: %v", l)
	}
	if l := o.Level("alert", t0, t0.Add(time.Second)); l != 0 {
		t.Errorf("level of a group that never played: %v", l)
	}
}

func TestBufferRing(t *testing.T) {
	b := NewBuffer(0.001) // 96 samples
	go func() {
		s := make([]float32, 7)
		for i := 0; i < 1000; i += 7 {
			for k := range s {
				s[k] = float32(i + k)
			}
			b.Write(s)
		}
		b.Close(nil)
	}()
	dst := make([]float32, 13)
	next := 0
	for {
		n, ok := b.Read(dst)
		for _, v := range dst[:n] {
			if v != float32(next) {
				t.Fatalf("got %v, want %d", v, next)
			}
			next++
		}
		if !ok {
			break
		}
		if n == 0 {
			time.Sleep(time.Millisecond)
		}
	}
	if next != 1001 {
		t.Fatalf("read %d samples, want 1001", next)
	}
}
