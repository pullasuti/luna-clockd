package output

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The fixtures are ffmpeg-encoded tones (44.1 kHz stereo 16-bit, 48 kHz mono
// 16-bit, 48 kHz stereo 24-bit), committed as small FLAC files. Decoding must
// be exact: the golden hashes pin the whole resampled output, so any decoder
// or resampler change that alters a sample fails here.
func decodeFile(t *testing.T, path string) ([]float32, error) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b := NewBuffer(30)
	err = DecodeFLAC(f, b)
	var out []float32
	for {
		tmp := make([]float32, 48000)
		n, ok := b.Read(tmp)
		out = append(out, tmp[:n]...)
		if !ok {
			break
		}
	}
	return out, err
}

func TestDecodeFLAC(t *testing.T) {
	for _, tc := range []struct {
		name           string
		frames         int
		sha            [32]byte
		firstL, firstR float32
	}{
		{"tone44s2.flac", 48000, sha("fec9afb531a8e036eba1d81651896e1b2c1f78b0234dbadc8e7549563f09407b"), 0, 0},
		{"tone48m1.flac", 24000, sha("ea0787f65f73b0013d03b359490e3125211b28ad5c1502ffb1544c0ded4192f5"), -0.2427051, -0.2427051},
		{"tone48s2_24.flac", 19200, sha("0c5cc90b079d0d9c1ded1376357d23a9782a704a83e01731f50ccd162e246492"), 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := decodeFile(t, filepath.Join("testdata", tc.name))
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got := len(out) / 2; got != tc.frames {
				t.Fatalf("got %d frames, want %d", got, tc.frames)
			}
			raw := make([]byte, len(out)*4)
			for i, v := range out {
				binary.LittleEndian.PutUint32(raw[i*4:], uint32(v))
			}
			if sum := sha256.Sum256(raw); sum != tc.sha {
				t.Errorf("output hash %x, want %x", sum, tc.sha)
			}
			if out[0] != tc.firstL || out[1] != tc.firstR {
				t.Errorf("first samples [%g %g], want [%g %g]", out[0], out[1], tc.firstL, tc.firstR)
			}
			// mono must reach both channels identically
			if strings.Contains(tc.name, "m1") && out[0] != out[1] {
				t.Errorf("mono not duplicated: %g != %g", out[0], out[1])
			}
		})
	}
}

func sha(hexStr string) [32]byte {
	raw, err := hex.DecodeString(hexStr)
	if err != nil || len(raw) != 32 {
		panic("bad golden hash")
	}
	var b [32]byte
	copy(b[:], raw)
	return b
}

func TestDecodeFLACAbort(t *testing.T) {
	b := NewBuffer(0.05)
	f, err := os.Open(filepath.Join("testdata", "tone44s2.flac"))
	if err != nil {
		t.Fatal(err)
	}
	b.Abort()
	if err := DecodeFLAC(f, b); err != nil {
		t.Errorf("aborted decode returned %v, want nil", err)
	}
}

func TestDecodeFLACBadInput(t *testing.T) {
	b := NewBuffer(1)
	err := DecodeFLAC(strings.NewReader("not a flac stream at all"), b)
	if err == nil {
		t.Error("garbage input: want an error")
	}
	if _, ok := b.Read(make([]float32, 8)); ok {
		t.Error("buffer still live after a failed decode")
	}
}

func TestDecodeSniffsFormat(t *testing.T) {
	// a WAV header followed by nothing: must fail inside the WAV parser
	// (unexpected EOF), proving the sniff routed it there, not to FLAC.
	wav := "RIFF\x18\x00\x00\x00WAVEfmt "
	b := NewBuffer(1)
	err := Decode(strings.NewReader(wav), b)
	if err == nil || !strings.Contains(err.Error(), "unexpected EOF") {
		t.Errorf("WAV sniff: got %v, want unexpected EOF", err)
	}
	f, err := os.Open(filepath.Join("testdata", "tone48m1.flac"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	b2 := NewBuffer(30)
	if err := Decode(f, b2); err != nil {
		t.Errorf("FLAC sniff: %v", err)
	}
}

// splitFLAC divides a file into its codec header ("fLaC" + metadata blocks)
// and the frame bytes that follow. Metadata blocks are 4-byte heads (last
// flag, type, 24-bit length) + payload.
func splitFLAC(t *testing.T, file string) (header, frames []byte) {
	t.Helper()
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw[:4]) != "fLaC" {
		t.Fatal("not a flac file")
	}
	off := 4
	for {
		var blk [4]byte
		copy(blk[:], raw[off:off+4])
		size := int(blk[1])<<16 | int(blk[2])<<8 | int(blk[3])
		off += 4 + size
		if blk[0]&0x80 != 0 { // last-metadata-block flag
			break
		}
	}
	return raw[:off], raw[off:]
}

// The Sendspin shape: header once, frames in chunks of arbitrary sizes (a
// frame may straddle a chunk boundary). At the stream's own rate (48 k) the
// chunk decoder must produce exactly what the file decoder produces.
func TestFLACChunkDecoder(t *testing.T) {
	header, frames := splitFLAC(t, filepath.Join("testdata", "tone48m1.flac"))
	d, err := NewFLACChunkDecoder(header)
	if err != nil {
		t.Fatalf("header: %v", err)
	}
	if d.Rate() != 48000 {
		t.Fatalf("rate %d, want 48000", d.Rate())
	}
	for _, split := range []int{len(frames), 960 * 3, 4100, 53, 1} {
		var out []float32
		for off := 0; off < len(frames); off += split {
			end := min(off+split, len(frames))
			s, err := d.Decode(frames[off:end])
			if err != nil {
				t.Fatalf("split %d: %v", split, err)
			}
			out = append(out, s...)
		}
		if got := len(out) / 2; got != 24000 {
			t.Errorf("split %d: %d frames, want 24000", split, got)
		}
		if split != 1 { // 1-byte feeds exercise leftovers; hash the rest
			raw := make([]byte, len(out)*4)
			for i, v := range out {
				binary.LittleEndian.PutUint32(raw[i*4:], uint32(v))
			}
			if sum := sha256.Sum256(raw); sum != sha("ea0787f65f73b0013d03b359490e3125211b28ad5c1502ffb1544c0ded4192f5") {
				t.Errorf("split %d: output hash mismatch", split)
			}
		}
	}
	// a corrupted byte inside a frame must fail the frame's CRC, not decode
	// silently: flip a byte near the end of the last full frame
	bad := append([]byte(nil), frames...)
	bad[len(bad)-3] ^= 0xff
	if _, err := d.Decode(bad); err == nil {
		t.Error("corrupted stream decoded without error")
	}
	if _, err := d.Decode(frames[:20]); err != nil {
		t.Errorf("partial frame: %v", err)
	}
	d2, err := NewFLACChunkDecoder([]byte("not a header"))
	if err == nil || d2 != nil {
		t.Error("bad header accepted")
	}
}
