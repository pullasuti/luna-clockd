package output

import (
	"bytes"
	"encoding/binary"
	"math"
	"os"
	"testing"
	"time"
)

// Fixtures (ffmpeg/LAME): tone44s2.mp3 1 kHz 3 s 44.1 kHz stereo CBR 128k
// with ID3v2.3 (UTF-16 title/artist) and a Xing/Info header; vbr22m1.mp3
// 440 Hz 2 s 22.05 kHz mono VBR with ID3v2.4; cbr48v1.mp3 1 kHz 3 s 48 kHz
// stereo 96k, no Xing, only an ID3v1 tag.

func decodeMP3File(t *testing.T, name string, skip time.Duration) []float32 {
	t.Helper()
	f := mustOpen(t, "testdata/"+name)
	b := NewBuffer(30)
	var err error
	if skip > 0 {
		err = DecodeFrom(f, skip, b)
	} else {
		err = Decode(f, b)
	}
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return drain(b)
}

// goertzel gives the relative power of frequency hz in the left channel.
func toneRatio(s []float32, hz float64) float64 {
	var s1, s2, total float64
	k := 2 * math.Cos(2*math.Pi*hz/Rate)
	n := len(s) / 2
	for i := range n {
		x := float64(s[2*i])
		total += x * x
		s0 := x + k*s1 - s2
		s2, s1 = s1, s0
	}
	p := s1*s1 + s2*s2 - k*s1*s2
	return p / (total * float64(n) / 2)
}

func TestDecodeMP3(t *testing.T) {
	for _, tc := range []struct {
		name string
		hz   float64
		sec  float64
	}{
		{"tone44s2.mp3", 1000, 3},
		{"vbr22m1.mp3", 440, 2},
		{"cbr48v1.mp3", 1000, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := decodeMP3File(t, tc.name, 0)
			// encoder delay/padding adds up to a few frames
			if sec := float64(len(out)/2) / Rate; sec < tc.sec-0.01 || sec > tc.sec+0.15 {
				t.Errorf("%.3f s, want ~%.1f", sec, tc.sec)
			}
			mid := out[len(out)/4&^1 : len(out)*3/4&^1]
			if r := toneRatio(mid, tc.hz); r < 0.9 {
				t.Errorf("%.0f Hz holds %.2f of the power, want > 0.9", tc.hz, r)
			}
			var peak float64
			for i, v := range mid {
				peak = max(peak, math.Abs(float64(v)))
				if i%2 == 1 && v != mid[i-1] {
					t.Fatalf("L != R at %d (mono source / identical stereo)", i)
				}
			}
			if peak < 0.05 || peak > 1 {
				t.Errorf("peak %.3f", peak)
			}
		})
	}
}

func TestMP3Meta(t *testing.T) {
	for _, tc := range []struct {
		name, tag string
		rate      int
		sec       float64
	}{
		{"tone44s2.mp3", "Clöck - Tëst Tone", 44100, 3},
		{"vbr22m1.mp3", "Mono VBR", 22050, 2},
		{"cbr48v1.mp3", "Old - V1Only", 48000, 3},
	} {
		info, err := TrackMeta(mustOpen(t, "testdata/"+tc.name))
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if info.Name != tc.tag || info.Rate != tc.rate {
			t.Errorf("%s: %q %d Hz, want %q %d", tc.name, info.Name, info.Rate, tc.tag, tc.rate)
		}
		if d := info.Duration.Seconds(); math.Abs(d-tc.sec) > 0.12 {
			t.Errorf("%s: %.3f s, want ~%.0f", tc.name, d, tc.sec)
		}
	}
}

// A tag with big cover art (beyond the first 64 KB) must still give the
// title and the length.
func TestMP3MetaBigTag(t *testing.T) {
	audio, err := os.ReadFile("testdata/cbr48v1.mp3")
	if err != nil {
		t.Fatal(err)
	}
	audio = audio[:len(audio)-128] // drop its ID3v1
	frame := func(id string, data []byte) []byte {
		h := make([]byte, 10)
		copy(h, id)
		binary.BigEndian.PutUint32(h[4:], uint32(len(data)))
		return append(h, data...)
	}
	body := append(frame("TIT2", []byte("\x03Big Art")), frame("APIC", make([]byte, 100_000))...)
	n := len(body)
	tag := []byte{'I', 'D', '3', 3, 0, 0, byte(n >> 21 & 0x7F), byte(n >> 14 & 0x7F), byte(n >> 7 & 0x7F), byte(n & 0x7F)}
	file := append(append(tag, body...), audio...)
	info, err := TrackMeta(bytes.NewReader(file))
	if err != nil {
		t.Fatal(err)
	}
	if info.Name != "Big Art" || info.Rate != 48000 || math.Abs(info.Duration.Seconds()-3) > 0.12 {
		t.Errorf("got %q %d Hz %.3f s", info.Name, info.Rate, info.Duration.Seconds())
	}
	b := NewBuffer(30)
	if err := Decode(bytes.NewReader(file), b); err != nil {
		t.Fatal(err)
	}
	if sec := float64(len(drain(b))/2) / Rate; sec < 2.9 {
		t.Errorf("decoded %.2f s after a big tag", sec)
	}
}

func TestDecodeFromMP3(t *testing.T) {
	full := decodeMP3File(t, "tone44s2.mp3", 0)
	tail := decodeMP3File(t, "tone44s2.mp3", time.Second)
	want := len(full)/2 - Rate
	if n := len(tail) / 2; n < want-1200 || n > want+1200 {
		t.Errorf("resumed: %d frames, want ~%d", n, want)
	}
	if r := toneRatio(tail[len(tail)/4:len(tail)*3/4], 1000); r < 0.9 {
		t.Errorf("resumed tone ratio %.2f", r)
	}
	// past the end: plays nothing, no error
	if out := decodeMP3File(t, "tone44s2.mp3", 10*time.Second); len(out) != 0 {
		t.Errorf("skip past the end gave %d samples", len(out))
	}
}

func TestMP3Garbage(t *testing.T) {
	for _, in := range [][]byte{
		[]byte("ID3\x03\x00\x00\x00\x00\x00\x10"),
		append([]byte{0xFF, 0xFB, 0x90, 0x64}, make([]byte, 50)...),
		[]byte{0xFF, 0xFB},
	} {
		b := NewBuffer(1)
		_ = Decode(bytes.NewReader(in), b) // must not panic or hang
		drain(b)
		_, _ = TrackMeta(bytes.NewReader(in))
	}
	if isMP3([]byte{0xFF, 0xF1, 0x50, 0x80}) { // ADTS AAC
		t.Error("ADTS taken for MP3")
	}
}

// An internet radio stream joined mid-frame: junk before the first frame.
func TestDecodeMP3MidStream(t *testing.T) {
	audio, err := os.ReadFile("testdata/cbr48v1.mp3")
	if err != nil {
		t.Fatal(err)
	}
	cut := audio[777:] // somewhere inside a frame
	b := NewBuffer(30)
	if err := Decode(bytes.NewReader(cut), b); err != nil {
		t.Fatal(err)
	}
	out := drain(b)
	if sec := float64(len(out)/2) / Rate; sec < 2.5 {
		t.Errorf("decoded %.2f s", sec)
	}
	if r := toneRatio(out[len(out)/4&^1:len(out)*3/4&^1], 1000); r < 0.9 {
		t.Errorf("tone ratio %.2f", r)
	}
}
