package output

import (
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTrackMeta(t *testing.T) {
	info, err := TrackMeta(mustOpen(t, "testdata/tone44s2.flac"))
	if err != nil {
		t.Fatalf("flac meta: %v", err)
	}
	if info.Rate != 44100 {
		t.Errorf("flac rate %d, want 44100", info.Rate)
	}
	if d := info.Duration.Seconds(); d < 0.99 || d > 1.01 {
		t.Errorf("flac duration %v, want ~1s", info.Duration)
	}
	if info.Name != "" {
		t.Errorf("untagged flac gave name %q", info.Name)
	}
	// tagged fixture: TITLE + ARTIST -> "Artist - Title"
	info, err = TrackMeta(mustOpen(t, "testdata/tagged44s2.flac"))
	if err != nil {
		t.Fatalf("tagged flac meta: %v", err)
	}
	if info.Name != "Test Signal - Night Tone" {
		t.Errorf("tagged flac name %q, want \"Test Signal - Night Tone\"", info.Name)
	}
	if d := info.Duration.Seconds(); d < 0.29 || d > 0.31 {
		t.Errorf("tagged flac duration %v, want ~0.3s", info.Duration)
	}
	// a WAV with a known data size: 0x6200 bytes at 48000 Hz mono 16-bit
	// (96000 byte/s) = 0.261 s, and no tags
	wav := "RIFF" + "\x24\x00\x00\x00" + "WAVEfmt " +
		"\x10\x00\x00\x00" + "\x01\x00" + "\x01\x00" + "\x80\xbb\x00\x00" + "\x00\x77\x01\x00" + "\x02\x00" + "\x10\x00" +
		"data" + "\x00\x62\x00\x00"
	info, err = TrackMeta(newStrReader(wav))
	if err != nil {
		t.Fatalf("wav meta: %v", err)
	}
	if info.Rate != 48000 {
		t.Errorf("wav rate %d, want 48000", info.Rate)
	}
	if d := info.Duration.Seconds(); d < 0.26 || d > 0.27 {
		t.Errorf("wav duration %v, want ~0.261s", info.Duration)
	}
	if info.Name != "" {
		t.Errorf("wav gave name %q", info.Name)
	}
}

// Resuming must produce the tail of a full decode. FLAC seeks by frame, so a
// little before the position may replay (one block at most).
func TestDecodeFromFLAC(t *testing.T) {
	full, err := decodeFile(t, filepath.Join("testdata", "tone44s2.flac"))
	if err != nil {
		t.Fatalf("full decode: %v", err)
	}
	b := NewBuffer(30)
	f := mustOpen(t, "testdata/tone44s2.flac")
	if err := DecodeFrom(f, 500*time.Millisecond, b); err != nil {
		t.Fatalf("decode from 0.5s: %v", err)
	}
	tail := drain(b)
	// full: 48000 frames; from 0.5s: 24000, less up to one 4096-sample input
	// block (the seek lands on a frame start) plus resampler slack
	lo, hi := 24000-4600, 24000+1100
	if n := len(tail) / 2; n < lo || n > hi {
		t.Errorf("resumed decode has %d frames, want [%d, %d]", n, lo, hi)
	}
	// the tail must track the full decode's end; a resumed decode starts a
	// fresh resampler, so samples differ by interpolation-phase amounts
	bad := 0
	for i := 1; i <= 2000; i++ {
		if d := tail[len(tail)-i] - full[len(full)-i]; d > 0.02 || d < -0.02 {
			bad++
		}
	}
	if bad > 10 {
		t.Errorf("tail deviates from the full decode in %d of the last 2000 samples", bad)
	}
}

func TestDecodeFromWAVExact(t *testing.T) {
	// one second of 48 kHz stereo 16-bit silence, built here
	data := make([]byte, 48000*4)
	wav := "RIFF" + le32(uint32(36+len(data))) + "WAVEfmt " +
		le32(16) + "\x01\x00\x02\x00" + le32(48000) + le32(48000*4) + "\x04\x00\x10\x00" +
		"data" + le32(uint32(len(data))) + string(data)
	b := NewBuffer(30)
	if err := DecodeFrom(newStrReader(wav), 250*time.Millisecond, b); err != nil {
		t.Fatalf("decode from 0.25s: %v", err)
	}
	if n := len(drain(b)) / 2; n != 36000 {
		t.Errorf("resumed wav has %d frames, want 36000 exactly", n)
	}
}

func TestBufferProgress(t *testing.T) {
	b := NewBuffer(1)
	if ok := b.Write(make([]float32, 1000)); !ok {
		t.Fatal("write failed")
	}
	out := make([]float32, 400)
	b.Read(out)
	w, buffered := b.Progress()
	if w != 1000 || buffered != 600 {
		t.Errorf("progress %d written, %d buffered; want 1000, 600", w, buffered)
	}
}

func mustOpen(t *testing.T, p string) *os.File {
	t.Helper()
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

type strReader struct {
	s string
	i int
}

func newStrReader(s string) *strReader { return &strReader{s: s} }

func (r *strReader) Read(p []byte) (int, error) {
	if r.i >= len(r.s) {
		return 0, io.EOF
	}
	n := copy(p, r.s[r.i:])
	r.i += n
	return n, nil
}

func (r *strReader) Seek(off int64, whence int) (int64, error) {
	if whence != io.SeekStart {
		return 0, io.EOF
	}
	r.i = int(off)
	return off, nil
}

func drain(b *Buffer) []float32 {
	var out []float32
	for {
		tmp := make([]float32, 48000)
		n, ok := b.Read(tmp)
		out = append(out, tmp[:n]...)
		if !ok {
			return out
		}
	}
}

func le32(v uint32) string {
	return string([]byte{byte(v), byte(v >> 8), byte(v >> 16), byte(v >> 24)})
}
