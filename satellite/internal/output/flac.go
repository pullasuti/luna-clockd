package output

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"

	"github.com/mewkiz/flac"
	"github.com/mewkiz/flac/frame"
)

// Decode sniffs the container (RIFF WAV, native FLAC or MP3) and streams the audio
// into b as 48 kHz stereo. It always closes b.
func Decode(r io.Reader, b *Buffer) (err error) {
	var magic [4]byte
	if _, err := io.ReadFull(r, magic[:]); err != nil {
		b.Close(err)
		return err
	}
	rest := io.MultiReader(bytes.NewReader(magic[:]), r)
	switch string(magic[:]) {
	case "fLaC":
		return DecodeFLAC(rest, b)
	case "RIFF":
		return DecodeWAV(rest, b)
	default:
		if isMP3(magic[:]) {
			return DecodeMP3(rest, b)
		}
		// a live MP3 stream (internet radio) joins mid-frame: look for two
		// consecutive frame headers in the first 16 KB
		br := bufio.NewReaderSize(rest, 16<<10)
		head, _ := br.Peek(16 << 10)
		if i := findMP3Sync(head); i >= 0 {
			br.Discard(i)
			return DecodeMP3(br, b)
		}
		return DecodeWAV(br, b)
	}
}

// DecodeFLAC streams a native FLAC file (any bit depth, rate and channel
// count FLAC carries) into b as 48 kHz stereo, like DecodeWAV. One decoder
// serves every consumer: URL playback, USB drive files and the Sendspin
// stream, all feeding this same Buffer. It always closes b.
//
// The decoder is github.com/mewkiz/flac (Unlicense; credited in the web UI
// footer). Frames are parsed one at a time, so a live stream works the same
// as a file read.
func DecodeFLAC(r io.Reader, b *Buffer) (err error) {
	defer func() { b.Close(err) }()
	s, err := flac.New(r) // parses "fLaC" + StreamInfo, skips the rest
	if err != nil {
		return err
	}
	defer s.Close()
	return decodeFLACStream(s, b)
}

// decodeFLACStream pumps a prepared FLAC stream into b; the CALLER closes b.
func decodeFLACStream(s *flac.Stream, b *Buffer) error {
	var (
		rs          *resampler
		frames      = make([]float32, 0, 8192)
		rate, bps   = int(s.Info.SampleRate), int(s.Info.BitsPerSample)
		streamCh    = int(s.Info.NChannels)
		resampledOK bool
	)
	for {
		f, err := s.ParseNext()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		// A frame header can carry the values or point back at StreamInfo
		// (mewkiz leaves those 0). FLAC pins rate/channels for the whole
		// stream; only the block size may vary, so the resampler is built
		// once and the values only cross-checked.
		if f.SampleRate != 0 {
			rate = int(f.SampleRate)
		}
		if f.BitsPerSample != 0 {
			bps = int(f.BitsPerSample)
		}
		ch := f.Channels.Count()
		if rate < 1 || bps < 8 || bps > 32 || ch < 1 || (streamCh != 0 && ch != streamCh) {
			return fmt.Errorf("flac: bad frame (%d Hz, %d bit, %d ch)", rate, bps, ch)
		}
		if rs == nil {
			rs = newResampler(rate)
		} else if !resampledOK && rs.step != float64(rate)/Rate {
			return fmt.Errorf("flac: sample rate changed mid-stream (%d Hz)", rate)
		}
		resampledOK = true
		frames = frames[:0]
		if frames, err = appendFrame(frames, f, bps); err != nil {
			return err
		}
		if out := rs.process(frames); len(out) > 0 && !b.Write(out) {
			return nil // aborted by the reader (stop, new media)
		}
	}
}

// FLACChunkDecoder decodes a chunked native FLAC stream — the Sendspin
// shape: the codec header ("fLaC" + STREAMINFO) arrives once at stream
// start, each network chunk carries whole frames. Bytes that do not form a
// complete frame at a chunk's end (a frame split across chunks) are kept
// for the next chunk. Output is interleaved stereo float32 at the stream's
// own rate: no resampling, the protocol negotiates 48 kHz.
type FLACChunkDecoder struct {
	leftover []byte
	rate     int
	ch       int
	bps      int
}

// NewFLACChunkDecoder validates the stream's codec header and pins the
// format every frame must match.
func NewFLACChunkDecoder(header []byte) (*FLACChunkDecoder, error) {
	s, err := flac.New(bytes.NewReader(header))
	if err != nil {
		return nil, err
	}
	d := &FLACChunkDecoder{
		rate: int(s.Info.SampleRate),
		ch:   int(s.Info.NChannels),
		bps:  int(s.Info.BitsPerSample),
	}
	if d.rate < 1 || (d.ch != 1 && d.ch != 2) || d.bps < 8 || d.bps > 32 {
		return nil, fmt.Errorf("flac: bad codec header (%d Hz, %d bit, %d ch)", d.rate, d.bps, d.ch)
	}
	return d, nil
}

// Rate is the stream's sample rate (frames are not resampled).
func (d *FLACChunkDecoder) Rate() int { return d.rate }

// Decode returns the chunk's samples. A chunk with no complete frame in it
// (yet) returns nil samples without error.
func (d *FLACChunkDecoder) Decode(data []byte) ([]float32, error) {
	buf := append(d.leftover, data...)
	d.leftover = nil
	br := bytes.NewReader(buf)
	var out []float32
	good := 0 // bytes consumed by complete frames
	for {
		f, err := frame.Parse(br)
		if err == nil {
			good = len(buf) - br.Len()
			if fr := int(f.SampleRate); fr != 0 && fr != d.rate {
				return nil, fmt.Errorf("flac: frame rate %d Hz, stream is %d Hz", fr, d.rate)
			}
			if out, err = appendFrame(out, f, d.bps); err != nil {
				return nil, err
			}
			continue
		}
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			// no (complete) frame left: keep the tail for the next chunk
			if tail := buf[good:]; len(tail) > 0 {
				d.leftover = append([]byte(nil), tail...)
			}
			return out, nil
		}
		return nil, fmt.Errorf("flac chunk: %w", err)
	}
}

// appendFrame converts one decoded FLAC frame to interleaved stereo float32
// (mono duplicated, >2 channels keep the front pair) and appends it to dst.
// defBps is used when the frame header defers the bit depth to STREAMINFO.
func appendFrame(dst []float32, f *frame.Frame, defBps int) ([]float32, error) {
	bps := int(f.BitsPerSample)
	if bps == 0 {
		bps = defBps
	}
	ch := f.Channels.Count()
	n := len(f.Subframes[0].Samples)
	if bps < 8 || bps > 32 || ch < 1 || len(f.Subframes) < ch {
		return dst, fmt.Errorf("flac: bad frame (%d bit, %d ch)", bps, ch)
	}
	scal := 1 / float32(uint32(1)<<(bps-1))
	for i := 0; i < n; i++ {
		l := float32(f.Subframes[0].Samples[i]) * scal
		r := l
		if ch > 1 { // more than stereo: keep the front pair
			r = float32(f.Subframes[1].Samples[i]) * scal
		}
		dst = append(dst, l, r)
	}
	return dst, nil
}
