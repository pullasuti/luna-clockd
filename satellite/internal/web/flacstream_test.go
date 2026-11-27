package web

import (
	"bytes"
	"encoding/binary"
	"io"
	"math"
	"math/rand"
	"testing"

	"github.com/mewkiz/flac"
)

// what goes in comes out of a reference FLAC decoder sample for sample:
// stereo tones, a mono passage (constant side), noise, full-scale edges
func TestFLACStream(t *testing.T) {
	const n = 6*flacBlock + 123
	rng := rand.New(rand.NewSource(1))
	l, r := make([]int16, n), make([]int16, n)
	for i := range l {
		a := 12000 * math.Sin(2*math.Pi*440*float64(i)/32000)
		b := 9000*math.Sin(2*math.Pi*660*float64(i)/32000) - float64(i%7)
		switch i / flacBlock {
		case 1: // mono
			b = a
		case 2: // noise
			a, b = rng.NormFloat64()*3000, rng.NormFloat64()*3000
		case 3: // edges
			a, b = 32767, -32768
			if i%2 == 0 {
				a, b = -32768, 32767
			}
		}
		l[i], r[i] = int16(a), int16(b)
	}
	var out bytes.Buffer
	f, err := newFLACStream(&out, 32000)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i += 1000 { // pieces that don't line up with frames
		if err := f.write(l[i:min(i+1000, n)], r[i:min(i+1000, n)]); err != nil {
			t.Fatal(err)
		}
	}
	st, err := flac.New(bytes.NewReader(out.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if st.Info.SampleRate != 32000 || st.Info.NChannels != 2 || st.Info.BitsPerSample != 16 {
		t.Fatalf("STREAMINFO %+v", st.Info)
	}
	k := 0
	for {
		fr, err := st.ParseNext() // checks both CRCs
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("frame after %d samples: %v", k, err)
		}
		for j := 0; j < fr.Subframes[0].NSamples; j++ {
			if int32(l[k]) != fr.Subframes[0].Samples[j] || int32(r[k]) != fr.Subframes[1].Samples[j] {
				t.Fatalf("sample %d: got %d/%d, want %d/%d", k, fr.Subframes[0].Samples[j], fr.Subframes[1].Samples[j], l[k], r[k])
			}
			k++
		}
	}
	if want := n / flacBlock * flacBlock; k != want { // the last partial frame waits for more
		t.Errorf("%d samples decoded, want %d", k, want)
	}
	t.Logf("FLAC %d bytes = %.0f %% of 16-bit PCM", out.Len(), 100*float64(out.Len())/float64(4*k))
}

// frame numbers in FLAC's UTF-8-like coding
func TestFLACFrameNumbers(t *testing.T) {
	for _, v := range []uint64{0, 127, 128, 2047, 2048, 65535, 65536, 1<<31 - 1, 1 << 35} {
		var w bitWriter
		w.utf8(v)
		b := w.flush()
		n, got := 1, uint64(b[0])
		if b[0]&0x80 != 0 {
			n = 0
			for c := b[0]; c&0x80 != 0; c <<= 1 {
				n++
			}
			got = uint64(b[0]) & (0xFF >> (n + 1))
		}
		for _, c := range b[1:] {
			got = got<<6 | uint64(c&0x3F)
		}
		if got != v || len(b) != n {
			t.Errorf("%d: % x", v, b)
		}
	}
}

func toneLevel(x []int16, f, rate float64) float64 {
	var re, im float64
	for i, v := range x {
		a := 2 * math.Pi * f * float64(i) / rate
		re += float64(v) * math.Cos(a)
		im += float64(v) * math.Sin(a)
	}
	return 20 * math.Log10(2*math.Hypot(re, im)/float64(len(x))/32768+1e-12)
}

// 48 -> 32 kHz: the count is right, the audio band passes, what would fold
// back from above 17 kHz is gone
func TestResampler32(t *testing.T) {
	tone := func(f float64) []byte {
		b := make([]byte, 4*48000)
		for i := 0; i < 48000; i++ {
			v := uint16(int16(16000 * math.Sin(2*math.Pi*f*float64(i)/48000)))
			binary.LittleEndian.PutUint16(b[4*i:], v)
			binary.LittleEndian.PutUint16(b[4*i+2:], v)
		}
		return b
	}
	in := 20 * math.Log10(16000.0/32768)
	for _, c := range []struct {
		in, out  float64 // Hz: the tone, where it lands at 32 kHz
		min, max float64 // dB relative to the input
	}{
		{1000, 1000, -0.5, 0.5}, {10000, 10000, -0.5, 0.5}, {14000, 14000, -1.5, 0.5},
		{18000, 14000, -200, -45}, {20000, 12000, -200, -45},
	} {
		rs := newResampler32()
		b := tone(c.in)
		var l, r []int16
		for i := 0; i < len(b); i += 3264 { // the radio's block size
			l, r = rs.process(b[i:min(i+3264, len(b))], l, r)
		}
		if math.Abs(float64(len(l))-32000) > 60 || len(r) != len(l) {
			t.Fatalf("%d/%d samples out of 1 s", len(l), len(r))
		}
		if lv := toneLevel(l[2000:], c.out, 32000) - in; lv < c.min || lv > c.max {
			t.Errorf("%.0f Hz -> %.0f Hz: %.1f dB, want %.1f..%.1f", c.in, c.out, lv, c.min, c.max)
		}
	}
}
