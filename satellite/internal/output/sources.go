package output

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"sync"
)

// Buffer is a non-blocking Source fed by a producer goroutine.
//
// It is a fixed ring: the reader is the real-time playback loop, and growing a
// slice under the lock (the old way) copied megabytes while the speaker waited:
// underruns about every 5-10 s with a URL stream.
type Buffer struct {
	mu       sync.Mutex
	ring     []float32
	r, n     int // read position, samples buffered
	written  int // samples ever written (written-buffered = played so far)
	finished bool
	err      error
	space    *sync.Cond
	max      int
}

// NewBuffer makes a buffer holding at most maxSeconds of audio; writers block when full.
func NewBuffer(maxSeconds float64) *Buffer {
	b := &Buffer{max: int(maxSeconds * Rate * Channels)}
	b.ring = make([]float32, b.max)
	b.space = sync.NewCond(&b.mu)
	return b
}

func (b *Buffer) Read(dst []float32) (int, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := min(len(dst), b.n)
	k := copy(dst[:n], b.ring[b.r:])
	copy(dst[k:n], b.ring)
	b.r = (b.r + n) % len(b.ring)
	b.n -= n
	if n > 0 {
		b.space.Broadcast()
	}
	return n, !(b.finished && b.n == 0)
}

// Write appends samples, blocking while the buffer is full. It returns false
// if the buffer was closed by the reader side (Abort).
func (b *Buffer) Write(s []float32) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for len(s) > 0 {
		for b.n >= b.max && !b.finished {
			b.space.Wait()
		}
		if b.finished {
			return false
		}
		w := (b.r + b.n) % len(b.ring)
		n := min(len(s), b.max-b.n, len(b.ring)-w)
		copy(b.ring[w:], s[:n])
		b.n += n
		b.written += n
		s = s[n:]
	}
	return true
}

// Close marks the end of the stream.
func (b *Buffer) Close(err error) {
	b.mu.Lock()
	b.finished, b.err = true, err
	b.space.Broadcast()
	b.mu.Unlock()
}

// Abort drops pending audio and unblocks the writer.
func (b *Buffer) Abort() {
	b.mu.Lock()
	b.finished = true
	b.n = 0
	b.space.Broadcast()
	b.mu.Unlock()
}

// Progress reports how many samples have been written and how many are still
// waiting to play (both interleaved): written-buffered is what reached the
// speaker so far. The transport line's elapsed time comes from this.
func (b *Buffer) Progress() (written, buffered int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.written, b.n
}

// Samples is a fixed in-memory source (chimes, alarms).
type Samples struct {
	pcm  []float32
	pos  int
	Loop int // extra repetitions; -1 forever
}

func NewSamples(pcm []float32, loop int) *Samples { return &Samples{pcm: pcm, Loop: loop} }

func (s *Samples) Read(dst []float32) (int, bool) {
	n := 0
	for n < len(dst) {
		if s.pos >= len(s.pcm) {
			if s.Loop == 0 {
				return n, false
			}
			if s.Loop > 0 {
				s.Loop--
			}
			s.pos = 0
		}
		c := copy(dst[n:], s.pcm[s.pos:])
		s.pos += c
		n += c
	}
	return n, true
}

// DecodeWAV streams a RIFF WAV (PCM 8/16/24/32-bit or float32, any rate and
// channel count) into b as 48 kHz stereo. Streaming WAVs with unknown length
// (0 or 0xFFFFFFFF data size, as Home Assistant's ffmpeg proxy sends) are
// read until EOF. It always closes b.
func DecodeWAV(r io.Reader, b *Buffer) (err error) {
	br, format, ch, rate, bits, _, err := wavData(r)
	if err != nil {
		b.Close(err)
		return err
	}
	return decodeWAVData(br, format, ch, rate, bits, b)
}

// wavData parses a RIFF WAV up to (and positioned at) the start of its data
// chunk, returning the data chunk's declared size (0 = unknown).
func wavData(r io.Reader) (br *bufio.Reader, format, ch, rate, bits int, dataSize uint32, err error) {
	br = bufio.NewReaderSize(r, 64*1024)
	var hdr [12]byte
	if _, err := io.ReadFull(br, hdr[:]); err != nil {
		return br, 0, 0, 0, 0, 0, err
	}
	if string(hdr[0:4]) != "RIFF" || string(hdr[8:12]) != "WAVE" {
		return br, 0, 0, 0, 0, 0, errors.New("not a WAV file")
	}
	for {
		var ck [8]byte
		if _, err := io.ReadFull(br, ck[:]); err != nil {
			return br, 0, 0, 0, 0, 0, err
		}
		size := binary.LittleEndian.Uint32(ck[4:])
		id := string(ck[0:4])
		if id == "fmt " {
			f := make([]byte, size)
			if _, err := io.ReadFull(br, f); err != nil {
				return br, 0, 0, 0, 0, 0, err
			}
			format = int(binary.LittleEndian.Uint16(f[0:]))
			ch = int(binary.LittleEndian.Uint16(f[2:]))
			rate = int(binary.LittleEndian.Uint32(f[4:]))
			bits = int(binary.LittleEndian.Uint16(f[14:]))
			if format == 0xFFFE && len(f) >= 26 { // WAVE_FORMAT_EXTENSIBLE
				format = int(binary.LittleEndian.Uint16(f[24:]))
			}
			if size%2 == 1 {
				br.ReadByte()
			}
			continue
		}
		if id == "data" {
			return br, format, ch, rate, bits, size, nil
		}
		if _, err := br.Discard(int(size + size%2)); err != nil {
			return br, 0, 0, 0, 0, 0, err
		}
	}
}

// decodeWAVData decodes PCM samples from br (positioned at a data chunk) into
// b as 48 kHz stereo. It always closes b.
func decodeWAVData(br *bufio.Reader, format, ch, rate, bits int, b *Buffer) (err error) {
	defer func() { b.Close(err) }()
	if ch < 1 || rate < 1 || (format != 1 && format != 3) {
		return fmt.Errorf("unsupported WAV: format %d, %d ch, %d Hz, %d bit", format, ch, rate, bits)
	}
	bps := bits / 8
	frameBytes := bps * ch
	sample := func(p []byte) float32 {
		switch {
		case format == 3 && bits == 32:
			return math.Float32frombits(binary.LittleEndian.Uint32(p))
		case bits == 8:
			return (float32(p[0]) - 128) / 128
		case bits == 16:
			return float32(int16(binary.LittleEndian.Uint16(p))) / 32768
		case bits == 24:
			return float32(int32(uint32(p[0])<<8|uint32(p[1])<<16|uint32(p[2])<<24)) / 2147483648
		default:
			return float32(int32(binary.LittleEndian.Uint32(p))) / 2147483648
		}
	}

	rs := newResampler(rate)
	in := make([]byte, frameBytes*1024)
	frames := make([]float32, 0, 2048)
	for {
		n, rerr := io.ReadFull(br, in)
		n -= n % frameBytes
		frames = frames[:0]
		for off := 0; off < n; off += frameBytes {
			l := sample(in[off:])
			r := l
			if ch > 1 {
				r = sample(in[off+bps:])
			}
			frames = append(frames, l, r)
		}
		if out := rs.process(frames); len(out) > 0 && !b.Write(out) {
			return nil // aborted
		}
		if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
			return nil
		}
		if rerr != nil {
			return rerr
		}
	}
}

// resampler converts stereo audio to 48 kHz by linear interpolation.
type resampler struct {
	step   float64 // input frames per output frame
	pos    float64
	prevL  float32
	prevR  float32
	primed bool
}

func newResampler(inRate int) *resampler { return &resampler{step: float64(inRate) / Rate} }

func (r *resampler) process(in []float32) []float32 {
	if r.step == 1 {
		return append([]float32(nil), in...)
	}
	frames := len(in) / 2
	if frames == 0 {
		return nil
	}
	at := func(i int) (float32, float32) {
		if i < 0 {
			return r.prevL, r.prevR
		}
		return in[2*i], in[2*i+1]
	}
	if !r.primed {
		r.prevL, r.prevR, r.primed = in[0], in[1], true
	}
	var out []float32
	// pos is relative to the previous chunk's last frame (index -1).
	for ; r.pos < float64(frames-1)+1; r.pos += r.step {
		i := int(math.Floor(r.pos)) - 1
		f := float32(r.pos - math.Floor(r.pos))
		l0, r0 := at(i)
		l1, r1 := at(i + 1)
		out = append(out, l0+(l1-l0)*f, r0+(r1-r0)*f)
	}
	r.pos -= float64(frames)
	r.prevL, r.prevR = in[2*(frames-1)], in[2*(frames-1)+1]
	return out
}

// Tone synthesises a short sine tone sequence for chimes: each note is
// (frequency Hz, duration ms); amplitude in dBFS.
func Tone(db float64, notes ...[2]float64) []float32 {
	amp := math.Pow(10, db/20)
	var out []float32
	for _, n := range notes {
		frames := int(n[1] * Rate / 1000)
		fade := Rate / 200 // 5 ms
		for i := 0; i < frames; i++ {
			env := 1.0
			if i < fade {
				env = float64(i) / float64(fade)
			} else if frames-i < fade {
				env = float64(frames-i) / float64(fade)
			}
			s := float32(amp * env * math.Sin(2*math.Pi*n[0]*float64(i)/Rate))
			out = append(out, s, s)
		}
	}
	return out
}
