package localmic

import (
	"encoding/binary"
	"math"
	"math/cmplx"
	"os"
	"sync"
)

// Two-mic echo null. The speaker sits in the same housing a few cm from both
// PDM mics, so its sound reaches them with a fixed relative transfer function
// H(f) = mic1/mic2. Subtracting H*mic2 from mic1 removes the speaker and keeps
// most of a distant voice (its relative transfer differs).
//
// It runs on a 4096/2048 STFT (sqrt-Hann, perfect reconstruction) with a fixed
// delay of nullN samples (256 ms) on everything, so the reference given to the
// canceller is delayed the same. H is learned while the clock plays (the echo
// dominates then) and saved; while nothing plays the output is the plain
// average of the two mics, crossfaded over two frames.
//
// The frame length is what the null's depth depends on: H is the ratio of two
// room responses, long and two-sided in time, and one factor per bin can only
// stand for a response much shorter than the frame. On the rig's music run
// (2026-11-10, tools/wwtest/replay.py) 512 took 21 dB of echo out, 1024 25 dB,
// 2048 28 dB and 4096 29.5 dB, where the two mics' own noise ends it (30 dB);
// the voice lost under 1 dB in each. A causal FIR from the same H did worse
// than the 512 frame at any length.

const (
	nullN   = 4096
	nullHop = nullN / 2
	nullBin = nullN/2 + 1
	// learning: exponential average over ~8 s of playing frames
	nullAlpha = 1.0 / (8 * 16000 / nullHop)
	nullFade  = 0.5 // crossfade step per frame
)

type nullProc struct {
	win        [nullN]float64
	inL, inR   []float64 // input not yet framed (< nullN after framing)
	ola        [nullN]float64
	out        []float64 // finished output, FIFO
	sab        [nullBin]complex128
	sbb        [nullBin]float64
	frames     int     // learned frames
	mix        float64 // 0 = plain average, 1 = null
	fa, fb, fy []complex128
	inE, outE  float64 // echo energy in/out while nulling (for status)

	mu    sync.Mutex
	stats NullStats
}

// NullStats is the null's state for the status page.
type NullStats struct {
	Active    bool    `json:"active"`
	Learned   float64 `json:"learned_s"`    // seconds of playing audio learned from
	Reduction float64 `json:"reduction_db"` // recent mic1 -> output reduction while nulling
}

func newNullProc() *nullProc {
	p := &nullProc{fa: make([]complex128, nullN), fb: make([]complex128, nullN), fy: make([]complex128, nullN)}
	for i := range p.win {
		p.win[i] = math.Sqrt(0.5 - 0.5*math.Cos(2*math.Pi*float64(i)/nullN))
	}
	p.out = make([]float64, nullN) // the fixed delay
	return p
}

// process takes the two mics for one block and returns the same number of
// output samples, nullN samples late. active: the clock is playing (null and
// learn); learn: the echo dominates this block.
func (p *nullProc) process(left, right []int16, active, learn bool, dst []int16) {
	for i := range left {
		p.inL = append(p.inL, float64(left[i]))
		p.inR = append(p.inR, float64(right[i]))
	}
	for len(p.inL) >= nullN {
		p.frame(active, learn)
		p.inL, p.inR = p.inL[nullHop:], p.inR[nullHop:]
	}
	for i := range dst {
		dst[i] = clip16(p.out[i])
	}
	p.out = p.out[len(dst):]
	p.mu.Lock()
	p.stats.Active = p.mix > 0.5
	p.stats.Learned = float64(p.frames) * nullHop / Rate
	if p.inE > 0 && p.outE > 0 {
		p.stats.Reduction = 10 * math.Log10(p.inE/p.outE)
	}
	p.mu.Unlock()
}

func (p *nullProc) frame(active, learn bool) {
	// Nothing plays and the crossfade is over: the plain average needs no
	// transform (window twice, overlap-add: the same samples, nullN late).
	if !active && p.mix == 0 {
		for i := 0; i < nullN; i++ {
			p.ola[i] += p.win[i] * p.win[i] * (p.inL[i] + p.inR[i]) / 2
		}
		p.emit()
		return
	}
	// both (real) mics in one transform: z = left + i*right
	for i := 0; i < nullN; i++ {
		p.fy[i] = complex(p.win[i]*p.inL[i], p.win[i]*p.inR[i])
	}
	fft(p.fy, false)
	for k := 0; k < nullBin; k++ {
		z, zc := p.fy[k], cmplx.Conj(p.fy[(nullN-k)%nullN])
		d := z - zc
		p.fa[k], p.fb[k] = (z+zc)/2, complex(imag(d)/2, -real(d)/2)
	}
	if active && learn {
		for k := 0; k < nullBin; k++ {
			a, b := p.fa[k], p.fb[k]
			p.sab[k] += complex(nullAlpha, 0) * (a*cmplx.Conj(b) - p.sab[k])
			p.sbb[k] += nullAlpha * (real(b)*real(b) + imag(b)*imag(b) - p.sbb[k])
		}
		p.frames++
	}
	target := 0.0
	if active && p.frames*nullHop >= 2*Rate { // at least 2 s learned
		target = 1
	}
	if p.mix < target {
		p.mix = math.Min(target, p.mix+nullFade)
	} else if p.mix > target {
		p.mix = math.Max(target, p.mix-nullFade)
	}
	var ein, eout float64
	for k := 0; k < nullBin; k++ {
		a, b := p.fa[k], p.fb[k]
		avg := (a + b) / 2
		y := avg
		if p.mix > 0 {
			var h complex128
			if p.sbb[k] > 0 {
				h = p.sab[k] / complex(p.sbb[k], 0)
			}
			n := a - h*b
			y = complex(1-p.mix, 0)*avg + complex(p.mix, 0)*n
			ein += real(a)*real(a) + imag(a)*imag(a)
			eout += real(n)*real(n) + imag(n)*imag(n)
		}
		p.fy[k] = y
		if k > 0 && k < nullN/2 {
			p.fy[nullN-k] = cmplx.Conj(y)
		}
	}
	if p.mix > 0 && active {
		const a = 0.05
		p.inE += a * (ein - p.inE)
		p.outE += a * (eout - p.outE)
	}
	fft(p.fy, true)
	for i := 0; i < nullN; i++ {
		p.ola[i] += p.win[i] * real(p.fy[i])
	}
	p.emit()
}

// emit moves the finished half frame to the output.
func (p *nullProc) emit() {
	p.out = append(p.out, p.ola[:nullHop]...)
	copy(p.ola[:], p.ola[nullHop:])
	clear(p.ola[nullHop:])
}

// Stats for the status page.
func (p *nullProc) Stats() NullStats {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stats
}

// save / load the learned spectra (so a restart doesn't start from nothing).
func (p *nullProc) save(path string) error {
	b := make([]byte, 8+nullBin*24)
	binary.LittleEndian.PutUint64(b, uint64(p.frames))
	for k := 0; k < nullBin; k++ {
		o := 8 + 24*k
		binary.LittleEndian.PutUint64(b[o:], math.Float64bits(real(p.sab[k])))
		binary.LittleEndian.PutUint64(b[o+8:], math.Float64bits(imag(p.sab[k])))
		binary.LittleEndian.PutUint64(b[o+16:], math.Float64bits(p.sbb[k]))
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (p *nullProc) load(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if len(b) != 8+nullBin*24 {
		return os.ErrInvalid
	}
	p.frames = int(binary.LittleEndian.Uint64(b))
	for k := 0; k < nullBin; k++ {
		o := 8 + 24*k
		p.sab[k] = complex(math.Float64frombits(binary.LittleEndian.Uint64(b[o:])), math.Float64frombits(binary.LittleEndian.Uint64(b[o+8:])))
		p.sbb[k] = math.Float64frombits(binary.LittleEndian.Uint64(b[o+16:]))
	}
	return nil
}

// fftTw[k] = exp(-2*pi*i*k/nullN), the twiddle factors for sizes up to nullN.
var fftTw = func() []complex128 {
	t := make([]complex128, nullN/2)
	for k := range t {
		t[k] = cmplx.Exp(complex(0, -2*math.Pi*float64(k)/nullN))
	}
	return t
}()

// fft is an in-place radix-2 complex FFT (len a power of two up to nullN);
// inverse scales by 1/n.
func fft(a []complex128, inverse bool) {
	n := len(a)
	for i, j := 1, 0; i < n; i++ {
		bit := n >> 1
		for ; j&bit != 0; bit >>= 1 {
			j ^= bit
		}
		j ^= bit
		if i < j {
			a[i], a[j] = a[j], a[i]
		}
	}
	for size := 2; size <= n; size <<= 1 {
		half, step := size/2, nullN/size
		for start := 0; start < n; start += size {
			lo, hi := a[start:start+half], a[start+half:start+size]
			for k := range lo {
				w := fftTw[k*step]
				if inverse {
					w = complex(real(w), -imag(w))
				}
				u, v := lo[k], hi[k]*w
				lo[k], hi[k] = u+v, u-v
			}
		}
	}
	if inverse {
		for i := range a {
			a[i] /= complex(float64(n), 0)
		}
	}
}
