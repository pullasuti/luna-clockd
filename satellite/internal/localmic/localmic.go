// Package localmic captures the raw PDM microphones (hw:0,3) and removes the
// speaker's echo with LocalVQE (the vqed helper process), as a replacement
// for the vendor iot_audio_capture. The speaker reference is clockd's own mix.
//
// The filter only runs while the speaker plays (and 1.5 s after); otherwise
// the microphone passes through, so it costs nothing when the clock is quiet.
package localmic

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yobert/alsa"

	"lenovo-satellite/internal/output"
)

const (
	Rate      = 16000
	vqeHop    = 256 // LocalVQE hop (16 ms); the helper's greeting says which
	readChunk = 512 // frames per capture read (32 ms)
	bufFrames = 4096

	refTarget = 320  // keep ~20 ms of reference queued (it leads its echo by the speaker buffer)
	refMax    = 1600 // more than 100 ms queued: drop the excess (clock drift, stalls)
	activeDB  = -75.0
	// nullLearnDB: the reference level from which the null learns (music at a
	// normal volume is ~-35 dBFS here)
	nullLearnDB = -55.0
	tailSamples = Rate * 3 / 2 // the speaker counts as playing for 1.5 s after its last sound
)

// Mic is the local capture + echo canceller.
type Mic struct {
	Helper, Model string // vqed binary and its .gguf model
	Log           *slog.Logger
	// SetReference hooks the speaker mix (48 kHz stereo float per period);
	// output.Output.SetReference.
	SetReference func(func(mix []float32))
	PreGainDB    float64 // into the canceller (the raw PDM signal is very quiet)
	PostGainDB   float64 // after it, to the level the wake word models are used to
	// AGCMaxDB is how much more gain the automatic gain may add on top of
	// PostGainDB for distant speech (iot_audio_capture does the same); it
	// backs off at once when a block would peak above -6 dBFS.
	AGCMaxDB float64
	// Args are extra helper arguments after Model (Model "" = none).
	Args []string

	// ErrLog, when set, receives the helper's stderr lines as they happen
	// (apmd's echo canceller switches, library errors).
	ErrLog func(line string)
	// Always runs the helper on every block, also when nothing plays (the
	// WebRTC helper's noise suppression is wanted in a quiet room too).
	Always bool

	hop int // samples per helper block, from its greeting

	mu     sync.Mutex
	ref    []int16
	dec    decimator
	filter bool // the canceller ran on the last block (for status)
	blocks struct{ filtered, passed uint64 }
	tap    TapFunc

	gainBits, gainDB atomic.Uint64 // float64 bits: capture gain, its dB below PreGainDB

	// NullOn turns the two-mic echo null on (checked per block; nil = off) and
	// NullPath keeps what it learned across restarts.
	NullOn   func() bool
	NullPath string
	// NullHold (nil = never): someone is probably talking and the music is
	// turned down for it, so the echo no longer dominates: the null keeps
	// working but must not learn (it would learn to remove the voice).
	NullHold func() bool
	nullP    atomic.Pointer[nullProc]
}

// TapFunc gets every block while set (16 kHz, hop samples each): the mono mic
// as the canceller gets it (after PreGainDB), the two mics at the same gain, the reference
// the canceller saw for this block, and the output sent on. It runs on the
// capture loop: copy and return quickly. The slices are reused.
type TapFunc func(mono, left, right, ref, out []int16)

// SetTap installs (nil removes) the block tap, for recording test captures.
func (m *Mic) SetTap(fn TapFunc) {
	m.mu.Lock()
	m.tap = fn
	m.mu.Unlock()
}

// block is one hop of capture: the mixed mono and the two mics.
type block struct{ mono, left, right []int16 }

// headroomDB is how far the automatic headroom may lower PreGainDB.
const headroomDB = 24

// gain is the current capture gain (linear, incl. the 32 -> 16 bit scale).
func (m *Mic) gain(maxG float64) float64 {
	if v := m.gainBits.Load(); v != 0 {
		return math.Float64frombits(v)
	}
	return maxG
}

func (m *Mic) setGain(g, maxG float64) {
	m.gainBits.Store(math.Float64bits(g))
	m.gainDB.Store(math.Float64bits(20 * math.Log10(g/maxG)))
}

// HeadroomDB is how much the automatic headroom lowers the pre-gain now (<= 0).
func (m *Mic) HeadroomDB() float64 { return math.Float64frombits(m.gainDB.Load()) }

// NullStats reports the two-mic null (zero before Run).
func (m *Mic) NullStats() NullStats {
	if p := m.nullP.Load(); p != nil {
		return p.Stats()
	}
	return NullStats{}
}

// Stats reports whether the canceller is running now and block counts.
func (m *Mic) Stats() (filtering bool, filtered, passed uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.filter, m.blocks.filtered, m.blocks.passed
}

// onMix runs on the playback thread: downmix, decimate to 16 kHz, queue.
func (m *Mic) onMix(mix []float32) {
	mono := m.dec.process(mix)
	m.mu.Lock()
	for _, v := range mono {
		m.ref = append(m.ref, int16(math.Round(float64(max(-1, min(1, v)))*32767)))
	}
	if len(m.ref) > refMax {
		m.ref = append(m.ref[:0], m.ref[len(m.ref)-refTarget:]...)
	}
	m.mu.Unlock()
}

// takeRef returns the next hop of reference (zeros when the queue runs dry).
func (m *Mic) takeRef(dst []int16) {
	m.mu.Lock()
	n := copy(dst, m.ref)
	m.ref = append(m.ref[:0], m.ref[n:]...)
	m.mu.Unlock()
	clear(dst[n:])
}

// helper is the running canceller process.
type helper struct {
	in     io.WriteCloser
	out    *bufio.Reader
	stderr *strings.Builder
	cmd    *exec.Cmd
}

func (h *helper) close() { h.in.Close(); h.cmd.Wait() }

// startHelper starts the canceller and reads its greeting (m.hop). It dies
// with ctx.
func (m *Mic) startHelper(ctx context.Context) (*helper, error) {
	var args []string
	if m.Model != "" {
		args = append(args, m.Model)
	}
	cmd := exec.CommandContext(ctx, m.Helper, append(args, m.Args...)...)
	h := &helper{cmd: cmd, stderr: &strings.Builder{}}
	errW := io.Writer(&limited{w: h.stderr, n: 4096})
	if m.ErrLog != nil { // helper chatter (apmd's "echo canceller on/off") goes to the log live
		errW = io.MultiWriter(errW, &lineLog{fn: m.ErrLog})
	}
	cmd.Stderr = errW
	var err error
	if h.in, err = cmd.StdinPipe(); err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", m.Helper, err)
	}
	h.out = bufio.NewReader(stdout)
	hello := make(chan string, 1)
	go func() { l, _ := h.out.ReadString('\n'); hello <- l }()
	select {
	case l := <-hello:
		if _, err := fmt.Sscanf(strings.TrimSpace(l), "ok %d", &m.hop); err != nil || m.hop < 32 || m.hop > 4096 {
			cmd.Process.Kill()
			h.close()
			return nil, fmt.Errorf("helper: %q %s", strings.TrimSpace(l), strings.TrimSpace(h.stderr.String()))
		}
	case <-time.After(10 * time.Second):
		cmd.Process.Kill()
		h.close()
		return nil, errors.New("helper did not start in 10 s")
	}
	return h, nil
}

// chain is everything after the capture: the two-mic null, the helper and the
// output gain. Its time is counted in samples, never read from a clock, so
// Replay gives the same output for the same input every time.
type chain struct {
	m    *Mic
	h    *helper
	log  *slog.Logger
	post float64
	agc  *agc
	null *nullProc

	ref, clean, raw, nulled []int16
	refDelay                []int16 // the null delays the mic by nullN: delay the reference too
	io16, res               []byte
	quiet                   int // samples since the reference last had sound
	saved                   int // null.frames at the last save
	sinceSave               int // samples
}

func (m *Mic) newChain(h *helper, log *slog.Logger) *chain {
	hop := m.hop
	c := &chain{m: m, h: h, log: log,
		post: math.Pow(10, m.PostGainDB/20), agc: newAGC(m.AGCMaxDB), null: newNullProc(),
		ref: make([]int16, hop), clean: make([]int16, hop), raw: make([]int16, hop), nulled: make([]int16, hop),
		refDelay: make([]int16, nullN), io16: make([]byte, 4*hop), res: make([]byte, 2*hop),
		quiet: tailSamples,
	}
	if m.NullPath != "" {
		if err := c.null.load(m.NullPath); err == nil {
			log.Info("two-mic null loaded", "learned_s", float64(c.null.frames)*nullHop/Rate)
		}
	}
	c.saved = c.null.frames
	return c
}

// close saves what the null learned since the last save.
func (c *chain) close() {
	if c.m.NullPath != "" && c.null.frames != c.saved {
		c.null.save(c.m.NullPath)
	}
}

// step runs one hop: b from the microphones, c.ref filled by the caller with
// the reference for it. After it c.raw is what the canceller got, c.ref the
// reference it got and c.clean the output.
func (c *chain) step(b block) (filtering bool, err error) {
	m, hop := c.m, c.m.hop
	blk := b.mono
	refDB := rmsDB(c.ref)
	if refDB > activeDB {
		c.quiet = 0
	} else if c.quiet < tailSamples {
		c.quiet += hop
	}
	playing := c.quiet < tailSamples
	if m.NullOn != nil && m.NullOn() {
		// learn while the speaker clearly plays (its echo dominates then)
		learn := refDB > nullLearnDB && (m.NullHold == nil || !m.NullHold())
		c.null.process(b.left, b.right, playing, learn, c.nulled)
		blk = c.nulled
		c.refDelay = append(c.refDelay, c.ref...)
		copy(c.ref, c.refDelay[:hop])
		c.refDelay = c.refDelay[hop:]
		c.sinceSave += hop
		if m.NullPath != "" && c.null.frames != c.saved && c.sinceSave > 60*Rate {
			if err := c.null.save(m.NullPath); err != nil {
				c.log.Warn("two-mic null: save", "err", err)
			}
			c.saved, c.sinceSave = c.null.frames, 0
		}
	}
	copy(c.raw, blk)
	filtering = m.Always || playing
	if filtering {
		for i := 0; i < hop; i++ {
			binary.LittleEndian.PutUint16(c.io16[2*i:], uint16(blk[i]))
			binary.LittleEndian.PutUint16(c.io16[2*(hop+i):], uint16(c.ref[i]))
		}
		if _, err := c.h.in.Write(c.io16); err != nil {
			return false, fmt.Errorf("helper write: %w %s", err, strings.TrimSpace(c.h.stderr.String()))
		}
		if _, err := io.ReadFull(c.h.out, c.res); err != nil {
			return false, fmt.Errorf("helper read: %w %s", err, strings.TrimSpace(c.h.stderr.String()))
		}
		for i := range c.clean {
			c.clean[i] = int16(binary.LittleEndian.Uint16(c.res[2*i:]))
		}
	} else {
		copy(c.clean, blk)
	}
	c.agc.apply(c.clean, c.post)
	return filtering, nil
}

// Run captures until ctx ends or something fails (the caller then falls back
// to the vendor capture). fn gets 16 kHz mono blocks; it must not keep them.
func (m *Mic) Run(ctx context.Context, fn func([]int16)) error {
	log := m.Log
	if log == nil {
		log = slog.Default()
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// helper first: no point taking the microphones if it can't start
	h, err := m.startHelper(ctx)
	if err != nil {
		return err
	}
	defer func() { cancel(); h.close() }()

	dev, cards, err := openCapture()
	if err != nil {
		return err
	}

	m.SetReference(m.onMix)
	defer m.SetReference(nil)
	log.Info("local echo canceller running", "helper", m.Helper, "model", m.Model)

	// capture on its own goroutine so a slow canceller hop (up to ~70 ms at
	// times) never overruns the ALSA buffer; blocks queue here instead
	blocks := make(chan block, 64)
	capErr := make(chan error, 1)
	go func() {
		defer close(blocks)
		capErr <- m.captureLoop(ctx, dev, cards, blocks, log)
	}()

	c := m.newChain(h, log)
	m.nullP.Store(c.null)
	defer c.close()
	for b := range blocks {
		m.takeRef(c.ref)
		filtering, err := c.step(b)
		if err != nil {
			return err
		}
		m.mu.Lock()
		m.filter = filtering
		if filtering {
			m.blocks.filtered++
		} else {
			m.blocks.passed++
		}
		tap := m.tap
		m.mu.Unlock()
		if tap != nil {
			tap(c.raw, b.left, b.right, c.ref, c.clean)
		}
		fn(c.clean)
	}
	if err := <-capErr; err != nil && ctx.Err() == nil {
		return err
	}
	return ctx.Err()
}

// Replay runs recorded audio through the same chain as Run, without the
// microphones and as fast as the helper goes: left and right are the two mics
// as the tap records them, ref the speaker reference in step with them (a
// capture taken with the null on has it nullN samples late: see
// NullDelay). fn gets what the tap would: the canceller's input, the
// reference it saw and the output, one hop at a time (reused slices).
// The same input gives the same output every time on the same machine.
func (m *Mic) Replay(ctx context.Context, left, right, ref []int16, fn func(in, ref, out []int16)) error {
	log := m.Log
	if log == nil {
		log = slog.Default()
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	h, err := m.startHelper(ctx)
	if err != nil {
		return err
	}
	defer func() { cancel(); h.close() }()
	c := m.newChain(h, log)
	defer c.close()
	hop := m.hop
	mono := make([]int16, hop)
	n := min(len(left), len(right), len(ref))
	for i := 0; i+hop <= n && ctx.Err() == nil; i += hop {
		l, r := left[i:i+hop], right[i:i+hop]
		for k := range mono {
			mono[k] = int16((int(l[k]) + int(r[k])) >> 1)
		}
		copy(c.ref, ref[i:i+hop])
		if _, err := c.step(block{mono, l, r}); err != nil {
			return err
		}
		fn(c.raw, c.ref, c.clean)
	}
	return ctx.Err()
}

// NullDelay is how many samples the two-mic null delays the microphone (and
// so the reference a capture recorded while it was on).
const NullDelay = nullN

// captureLoop runs capture on a real-time thread and reopens the device after
// an overrun (EPIPE: yobert/alsa can't re-prepare it), so a stall elsewhere
// costs a few blocks, not the microphone source. More than 5 in a minute is
// given up (the caller falls back).
func (m *Mic) captureLoop(ctx context.Context, dev *alsa.Device, cards []*alsa.Card, blocks chan<- block, log *slog.Logger) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := output.RealtimeThread(35); err != nil {
		log.Debug("real-time capture", "err", err)
	}
	var fails []time.Time
	for {
		err := m.capture(ctx, dev, blocks, log)
		dev.Close()
		alsa.CloseCards(cards)
		if err == nil || ctx.Err() != nil {
			return err
		}
		now := time.Now()
		fails = append(fails, now)
		for len(fails) > 0 && now.Sub(fails[0]) > time.Minute {
			fails = fails[1:]
		}
		if len(fails) > 5 {
			return fmt.Errorf("%w (%d times in a minute)", err, len(fails))
		}
		log.Warn("local mic: capture interrupted, reopening", "err", err)
		if dev, cards, err = openCapture(); err != nil {
			return err
		}
	}
}

// capture reads the PDM pair, mixes to mono, removes DC and sends hop blocks.
func (m *Mic) capture(ctx context.Context, dev *alsa.Device, blocks chan<- block, log *slog.Logger) error {
	// yobert/alsa sets start_threshold = buffer size: the first read must ask
	// for the whole buffer or the stream never starts
	first := make([]byte, bufFrames*2*4)
	buf := make([]byte, readChunk*2*4)
	var pend, pendL, pendR []int16
	var dcLX, dcLY, dcRX, dcRY float64 // one-pole DC blockers (~20 Hz), per mic
	// PreGainDB is applied here, before 16 bits: the raw PDM signal is so quiet
	// (a quiet room ~ -91 dBFS = 1 LSB in 16 bits) that quantising first and
	// amplifying after left distant speech a few coarse steps, 100x louder.
	// It is the most gain: with the clock playing loud the echo would clip
	// (0.4 % of samples in a shelf at +40 dB), and clipped echo can't be
	// cancelled. So each read is checked before scaling: a peak that would go
	// above -3 dBFS lowers the gain at once (no clipping), and it comes back
	// at 1 dB/s while peaks stay below -9 dBFS, at most headroomDB lower.
	maxG := math.Pow(10, m.PreGainDB/20) / 65536 // 32-bit -> 16-bit scale
	minG := maxG * math.Pow(10, -headroomDB/20)
	g := m.gain(maxG)
	const limit, relLimit = 0.708 * 32767, 0.355 * 32767 // -3, -9 dBFS
	const a = 0.992
	skip := Rate / 5 // the PDM start-up click: drop the first 0.2 s
	b := first
	for ctx.Err() == nil {
		if err := dev.Read(b); err != nil {
			return fmt.Errorf("capture: %w", err)
		}
		var peak float64
		for i := 0; i+8 <= len(b); i += 8 {
			peak = max(peak, math.Abs(float64(int32(binary.LittleEndian.Uint32(b[i:])))), math.Abs(float64(int32(binary.LittleEndian.Uint32(b[i+4:])))))
		}
		if skip <= 0 { // not on the start-up click
			if peak*g > limit {
				g = max(minG, limit*0.9/peak)
			} else if step := math.Pow(10, float64(len(b)/8)/Rate/20); peak*g*step < relLimit {
				g = min(maxG, g*step)
			}
			m.setGain(g, maxG)
		}
		for i := 0; i+8 <= len(b); i += 8 {
			l := float64(int32(binary.LittleEndian.Uint32(b[i:]))) * g
			r := float64(int32(binary.LittleEndian.Uint32(b[i+4:]))) * g
			yl := l - dcLX + a*dcLY
			dcLX, dcLY = l, yl
			yr := r - dcRX + a*dcRY
			dcRX, dcRY = r, yr
			if skip > 0 {
				skip--
				continue
			}
			pend = append(pend, clip16((yl+yr)/2))
			pendL, pendR = append(pendL, clip16(yl)), append(pendR, clip16(yr))
		}
		for len(pend) >= m.hop {
			blk := block{make([]int16, m.hop), make([]int16, m.hop), make([]int16, m.hop)}
			copy(blk.mono, pend)
			copy(blk.left, pendL)
			copy(blk.right, pendR)
			pend, pendL, pendR = pend[m.hop:], pendL[m.hop:], pendR[m.hop:]
			select {
			case blocks <- blk:
			default:
				log.Warn("local mic: canceller behind, dropping a block")
			}
		}
		b = buf
	}
	return nil
}

func openCapture() (*alsa.Device, []*alsa.Card, error) {
	cards, err := alsa.OpenCards()
	if err != nil {
		return nil, nil, err
	}
	for _, c := range cards {
		if c.Number != 0 {
			continue
		}
		ds, _ := c.Devices()
		for _, d := range ds {
			if d.Number != 3 || !d.Record {
				continue
			}
			if err := d.Open(); err != nil {
				alsa.CloseCards(cards)
				return nil, nil, fmt.Errorf("open hw:0,3 (is iot_audio_capture still running?): %w", err)
			}
			steps := []func() error{
				func() error { _, err := d.NegotiateChannels(2); return err },
				func() error { _, err := d.NegotiateRate(Rate); return err },
				func() error { _, err := d.NegotiateFormat(alsa.S32_LE); return err },
				func() error { _, err := d.NegotiatePeriodSize(vqeHop); return err },
				func() error { _, err := d.NegotiateBufferSize(bufFrames); return err },
				d.Prepare,
			}
			for _, s := range steps {
				if err := s(); err != nil {
					d.Close()
					alsa.CloseCards(cards)
					return nil, nil, fmt.Errorf("hw:0,3: %w", err)
				}
			}
			return d, cards, nil
		}
	}
	alsa.CloseCards(cards)
	return nil, nil, errors.New("hw:0,3 capture not found")
}

func clip16(v float64) int16 {
	return int16(max(-32768, min(32767, math.Round(v))))
}

func rmsDB(s []int16) float64 {
	var sum float64
	for _, v := range s {
		sum += float64(v) * float64(v)
	}
	return 10*math.Log10(sum/float64(len(s))+1e-9) - 90.3 // dBFS (20*log10(32768) = 90.3)
}

// decimator turns 48 kHz stereo into 16 kHz mono (windowed-sinc low-pass at
// 7 kHz, every third sample).
type decimator struct {
	hist  []float32
	out   []float32 // reused: process runs on the playback loop
	phase int
}

var taps = func() []float32 {
	const n = 49
	h := make([]float32, n)
	var sum float64
	for i := range h {
		x := float64(i - n/2)
		v := 1.0
		if x != 0 {
			v = math.Sin(2*math.Pi*7000/48000*x) / (2 * math.Pi * 7000 / 48000 * x)
		}
		v *= 0.54 - 0.46*math.Cos(2*math.Pi*float64(i)/(n-1))
		h[i] = float32(v)
		sum += v
	}
	for i := range h {
		h[i] /= float32(sum)
	}
	return h
}()

func (d *decimator) process(stereo []float32) []float32 {
	for i := 0; i+1 < len(stereo); i += 2 {
		d.hist = append(d.hist, (stereo[i]+stereo[i+1])/2)
	}
	out := d.out[:0]
	n := len(taps)
	i := d.phase
	for ; i+n <= len(d.hist); i += 3 {
		var acc float32
		for k, t := range taps {
			acc += t * d.hist[i+k]
		}
		out = append(out, acc)
	}
	// keep what the next call still needs
	keep := i
	d.hist = append(d.hist[:0], d.hist[keep:]...)
	d.phase = 0
	d.out = out
	return out
}

// lineLog calls fn once per completed line.
type lineLog struct {
	fn  func(string)
	buf []byte
}

func (l *lineLog) Write(p []byte) (int, error) {
	l.buf = append(l.buf, p...)
	for {
		i := bytes.IndexByte(l.buf, '\n')
		if i < 0 {
			break
		}
		if s := strings.TrimSpace(string(l.buf[:i])); s != "" {
			l.fn(s)
		}
		l.buf = l.buf[i+1:]
	}
	if len(l.buf) > 4096 { // a line that never ends
		l.buf = nil
	}
	return len(p), nil
}

type limited struct {
	w io.Writer
	n int
}

func (l *limited) Write(p []byte) (int, error) {
	if l.n > 0 {
		k := min(len(p), l.n)
		l.w.Write(p[:k])
		l.n -= k
	}
	return len(p), nil
}

// agc is a slow-rising, fast-falling gain: +3 dB/s up to max dB while the
// block peaks stay low, cut at once so no block peaks above -6 dBFS.
type agc struct {
	gainDB, maxDB float64
}

func newAGC(maxDB float64) *agc { return &agc{maxDB: maxDB} }

const (
	agcCeilDB  = -6.0
	agcRiseDBs = 3.0 // per second
)

func (a *agc) apply(s []int16, base float64) {
	var peak float64
	for _, v := range s {
		peak = max(peak, math.Abs(float64(v)))
	}
	peakDB := 20 * math.Log10(peak*base/32768+1e-9) // after the fixed gain
	if peakDB+a.gainDB > agcCeilDB {
		a.gainDB = max(0, agcCeilDB-peakDB) // fall at once
		if peakDB > agcCeilDB {
			a.gainDB = 0
		}
	} else {
		a.gainDB = min(a.maxDB, a.gainDB+agcRiseDBs*float64(len(s))/Rate)
	}
	g := base * math.Pow(10, a.gainDB/20)
	for i, v := range s {
		s[i] = clip16(float64(v) * g)
	}
}
