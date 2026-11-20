// Package output owns the speaker (hw:0,2). It mixes any number of sources,
// writes continuously (silence when idle, so the AEC reference never stops)
// and serves the Cast mixer-service loopback that iot_audio_capture uses as
// its echo-cancellation reference (abstract socket @/tmp/mixer-service).
package output

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"os/exec"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yobert/alsa"
	"golang.org/x/sys/unix"
)

const (
	Rate         = 48000
	Channels     = 2
	period       = 480 // 10 ms
	bufferFrames = period * 8

	// The amp hisses quietly whenever it is powered, so it is switched off
	// after ampIdle of digital silence and back on with the first sound.
	// Switching is pop-free (Engineering Notes 2026-11-12e). The sound that
	// wakes it is still a full buffer (80 ms) away from the speaker; ampWarm
	// periods of extra silence can be put in front of it if the amp turns
	// out to need longer than that.
	ampIdle   = 10 * time.Second
	ampWarm   = 6    // periods (10 ms each)
	ampSilent = 1e-5 // below 16-bit resolution
)

// Source produces interleaved stereo float32 samples at 48 kHz. Read must not
// block: return 0 with ok=true when nothing is ready yet (an underrun plays as
// silence), and ok=false once the source is finished.
type Source interface {
	Read(dst []float32) (n int, ok bool)
}

// Voice is one playing source.
type Voice struct {
	src     Source
	gain    float32
	group   string
	done    chan struct{}
	stopped bool
}

// Done is closed when the voice has finished or was stopped.
func (v *Voice) Done() <-chan struct{} { return v.done }

type Output struct {
	Log *slog.Logger

	mu        sync.Mutex
	voices    []*Voice
	volume    float32
	groupGain map[string]float32

	lbMu      sync.Mutex
	lbClients []*lbClient

	// recent loudness per group, stamped with when it reaches the speaker
	lvMu   sync.Mutex
	levels map[string]*levelRing

	// tap records what is sent to the speaker (echo canceller experiments)
	mixSums   map[string]float64 // mixPeriod scratch (playback loop only)
	mixVoices []*Voice
	tapMu     sync.Mutex
	tap       []float32
	tapLeft   int
	tapDone   func([]float32)

	// ref receives every period of the mix as it goes to the speaker (the
	// local echo canceller's reference); nil when unused
	ref atomic.Pointer[func([]float32)]

	ampWant atomic.Bool
	ampKick chan struct{}
}

// setAmp asks for the amp to be powered or not; ampLoop does the switching so
// the playback thread never waits for it.
func (o *Output) setAmp(on bool) {
	o.ampWant.Store(on)
	select {
	case o.ampKick <- struct{}{}:
	default:
	}
}

func (o *Output) ampLoop(ctx context.Context) {
	on := true // Run powered it
	for {
		select {
		case <-ctx.Done():
			return
		case <-o.ampKick:
		}
		for want := o.ampWant.Load(); want != on; want = o.ampWant.Load() {
			val := "off"
			if want {
				val = "on"
			}
			if err := exec.Command("amixer", "-c0", "cset", "name=Dummy Power", val).Run(); err != nil {
				o.Log.Warn("amp power switch failed", "to", val, "err", err)
			} else {
				o.Log.Debug("amp power", "to", val)
			}
			on = want
		}
	}
}

// SetReference registers fn to receive each period of the mix (48 kHz stereo
// float, after volume and clipping) right after it is written to the speaker;
// nil stops it. fn runs on the playback thread and must not block.
func (o *Output) SetReference(fn func(mix []float32)) {
	if fn == nil {
		o.ref.Store(nil)
		return
	}
	o.ref.Store(&fn)
}

// Tap records the next d of the mix exactly as it goes to the speaker
// (48 kHz stereo float, after volume and clipping) and hands it to done.
func (o *Output) Tap(d time.Duration, done func(samples []float32)) {
	o.tapMu.Lock()
	n := int(d.Seconds()*Rate) * Channels
	// allocated here: growing it in tapWrite copied megabytes on the playback loop
	o.tap, o.tapLeft, o.tapDone = make([]float32, 0, n), n, done
	o.tapMu.Unlock()
}

func (o *Output) tapWrite(mix []float32) {
	o.tapMu.Lock()
	if o.tapLeft <= 0 {
		o.tapMu.Unlock()
		return
	}
	n := min(len(mix), o.tapLeft)
	o.tap = append(o.tap, mix[:n]...)
	o.tapLeft -= n
	var done func([]float32)
	var got []float32
	if o.tapLeft == 0 {
		done, got, o.tap, o.tapDone = o.tapDone, o.tap, nil, nil
	}
	o.tapMu.Unlock()
	if done != nil {
		go done(got)
	}
}

// levelRing holds the RMS of the last periods of one group (before the volume,
// so a quiet setting still has a level), with the time each becomes audible.
type levelRing struct {
	at  [64]time.Time
	rms [64]float32
	n   int
}

// Level is the loudest RMS (0-1) of a group's sound that reaches the speaker
// between from and to; 0 when nothing of the group played then.
func (o *Output) Level(group string, from, to time.Time) float32 {
	o.lvMu.Lock()
	defer o.lvMu.Unlock()
	r := o.levels[group]
	if r == nil {
		return 0
	}
	var m float32
	for i := range r.at {
		if t := r.at[i]; !t.Before(from) && !t.After(to) {
			m = max(m, r.rms[i])
		}
	}
	return m
}

// mixPeriod mixes one period of every voice into mix and records each group's
// level as audible at the given time.
func (o *Output) mixPeriod(mix, tmp []float32, audible time.Time) {
	for i := range mix {
		mix[i] = 0
	}
	// reused between periods: no allocations on the real-time loop
	if o.mixSums == nil {
		o.mixSums = map[string]float64{}
	}
	sums := o.mixSums
	clear(sums)
	o.mu.Lock()
	vol := o.volume * o.volume // rough perceptual curve
	o.mixVoices = append(o.mixVoices[:0], o.voices...)
	for _, v := range o.mixVoices {
		n, ok := v.src.Read(tmp)
		g := v.gain * vol
		if gg, has := o.groupGain[v.group]; has {
			g *= gg
		}
		var sq float64
		for i := 0; i < n; i++ {
			mix[i] += tmp[i] * g
			x := float64(tmp[i] * v.gain)
			sq += x * x
		}
		sums[v.group] += sq
		if !ok {
			o.remove(v)
		}
	}
	o.mu.Unlock()
	o.lvMu.Lock()
	if o.levels == nil {
		o.levels = map[string]*levelRing{}
	}
	for g, sq := range sums {
		r := o.levels[g]
		if r == nil {
			r = &levelRing{}
			o.levels[g] = r
		}
		i := r.n % len(r.at)
		r.at[i], r.rms[i] = audible, float32(math.Sqrt(sq/float64(len(mix))))
		r.n++
	}
	o.lvMu.Unlock()
}

func New(log *slog.Logger) *Output {
	return &Output{Log: log, volume: 0.5, groupGain: map[string]float32{}, ampKick: make(chan struct{}, 1)}
}

// SetVolume sets the master volume (0-1, applied with a perceptual curve).
func (o *Output) SetVolume(v float32) {
	o.mu.Lock()
	o.volume = v
	o.mu.Unlock()
}

// SetGroupGain scales every voice in a group (used for ducking media).
func (o *Output) SetGroupGain(group string, g float32) {
	o.mu.Lock()
	o.groupGain[group] = g
	o.mu.Unlock()
}

// Play starts a source in the given group ("media", "voice", "alert"...).
func (o *Output) Play(src Source, gain float32, group string) *Voice {
	v := &Voice{src: src, gain: gain, group: group, done: make(chan struct{})}
	o.mu.Lock()
	o.voices = append(o.voices, v)
	o.mu.Unlock()
	return v
}

// Stop ends a voice early.
func (o *Output) Stop(v *Voice) {
	if v == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.remove(v)
}

// StopGroup ends every voice in a group.
func (o *Output) StopGroup(group string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, v := range append([]*Voice(nil), o.voices...) {
		if v.group == group {
			o.remove(v)
		}
	}
}

// Busy reports whether any voice in the group is playing.
func (o *Output) Busy(group string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, v := range o.voices {
		if v.group == group {
			return true
		}
	}
	return false
}

func (o *Output) remove(v *Voice) {
	for i, x := range o.voices {
		if x == v {
			o.voices = append(o.voices[:i], o.voices[i+1:]...)
			if !v.stopped {
				v.stopped = true
				close(v.done)
			}
			return
		}
	}
}

// Run opens the device and plays until ctx is done.
func (o *Output) Run(ctx context.Context) error {
	if err := exec.Command("amixer", "-c0", "cset", "name=Dummy Power", "on").Run(); err != nil {
		o.Log.Warn("amp power on failed", "err", err)
	}
	o.ampWant.Store(true)
	go o.ampLoop(ctx)
	ln, err := net.Listen("unix", "@/tmp/mixer-service")
	if err != nil {
		return fmt.Errorf("mixer-service socket: %w", err)
	}
	go o.acceptLoopback(ctx, ln)
	defer ln.Close()

	type lbItem struct {
		at  time.Time
		msg []byte
	}
	lbq := make(chan lbItem, 512)
	go func() {
		for it := range lbq {
			time.Sleep(time.Until(it.at))
			o.sendLoopback(it.msg)
		}
	}()
	defer close(lbq)

	// The device is reopened after any write error (an underrun leaves it in a
	// state yobert/alsa cannot re-prepare), so playback problems never stop the daemon.
	for ctx.Err() == nil {
		err := o.session(ctx, func(at time.Time, msg []byte) {
			select {
			case lbq <- lbItem{at: at, msg: msg}:
			default: // pacer is behind; drop rather than block playback
			}
		})
		if ctx.Err() != nil {
			break
		}
		o.Log.Warn("playback session ended, reopening", "err", err)
		select {
		case <-ctx.Done():
		case <-time.After(200 * time.Millisecond):
		}
	}
	return ctx.Err()
}

func (o *Output) session(ctx context.Context, loopback func(time.Time, []byte)) error {
	// Keep the real-time loop on its own OS thread with a raised priority so
	// HTTP fetches, decoding and GC on other threads cannot starve it.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := unix.Setpriority(unix.PRIO_PROCESS, unix.Gettid(), -15); err != nil {
		o.Log.Debug("raise playback priority", "err", err)
	}
	// Real-time scheduling on top: with the microphone processing and a music
	// stream running, nice -15 alone still underran the 80 ms buffer every
	// 5-30 s. The loop blocks in ALSA writes, so it can't hog a core.
	if err := RealtimeThread(40); err != nil {
		o.Log.Debug("real-time playback", "err", err)
	}

	dev, cards, err := openPlayback()
	if err != nil {
		return err
	}
	defer alsa.CloseCards(cards)
	defer dev.Close()

	latency := time.Duration(bufferFrames) * time.Second / Rate
	mix := make([]float32, period*Channels)
	tmp := make([]float32, period*Channels)
	out := make([]byte, period*Channels*4)
	lb := make([]byte, period*Channels*4)
	// everything that follows the speaker (tap, echo reference, loopback) gets
	// every period written, silence included, so their timing never shifts
	emit := func(mix []float32, out, lb []byte) error {
		if err := dev.Write(out, period); err != nil {
			return err
		}
		o.tapWrite(mix)
		if fn := o.ref.Load(); fn != nil {
			(*fn)(mix)
		}
		if o.hasLoopback() {
			loopback(time.Now().Add(latency), audioMsg(monoUs()+latency.Microseconds(), lb))
		}
		return nil
	}
	var zmix []float32
	var zout []byte
	if ampWarm > 0 {
		zmix, zout = make([]float32, period*Channels), make([]byte, period*Channels*4)
	}
	o.setAmp(true) // a session that failed may have left it off
	ampOn, quiet := true, 0
	const idlePeriods = int(ampIdle / (time.Second * period / Rate))
	for ctx.Err() == nil {
		o.mixPeriod(mix, tmp, time.Now().Add(latency))

		sound := false
		for i, s := range mix {
			if s > 1 {
				s = 1
			} else if s < -1 {
				s = -1
			}
			if s > ampSilent || s < -ampSilent {
				sound = true
			}
			mix[i] = s
			binary.LittleEndian.PutUint32(out[i*4:], uint32(int32(float64(s)*2147483647)))
			binary.LittleEndian.PutUint32(lb[i*4:], math.Float32bits(s))
		}
		switch {
		case sound && !ampOn:
			ampOn, quiet = true, 0
			o.setAmp(true)
			for i := 0; i < ampWarm; i++ {
				if err := emit(zmix, zout, zout); err != nil {
					return err
				}
			}
		case sound:
			quiet = 0
		case ampOn:
			if quiet++; quiet >= idlePeriods {
				ampOn = false
				o.setAmp(false)
			}
		}
		if err := emit(mix, out, lb); err != nil {
			return err
		}
	}
	return ctx.Err()
}

// RealtimeThread switches the calling OS thread (runtime.LockOSThread first)
// to SCHED_FIFO at prio (1-99).
func RealtimeThread(prio int) error {
	return unix.SchedSetAttr(0, &unix.SchedAttr{Size: unix.SizeofSchedAttr, Policy: unix.SCHED_FIFO, Priority: uint32(prio)}, 0)
}

func openPlayback() (*alsa.Device, []*alsa.Card, error) {
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
			if d.Number != 2 || !d.Play {
				continue
			}
			if err := d.Open(); err != nil {
				alsa.CloseCards(cards)
				return nil, nil, err
			}
			if _, err := d.NegotiateChannels(Channels); err != nil {
				return nil, nil, err
			}
			if _, err := d.NegotiateRate(Rate); err != nil {
				return nil, nil, err
			}
			if _, err := d.NegotiateFormat(alsa.S32_LE); err != nil {
				return nil, nil, err
			}
			if _, err := d.NegotiatePeriodSize(period); err != nil {
				return nil, nil, err
			}
			if _, err := d.NegotiateBufferSize(bufferFrames); err != nil {
				return nil, nil, err
			}
			if err := d.Prepare(); err != nil {
				return nil, nil, err
			}
			return d, cards, nil
		}
	}
	alsa.CloseCards(cards)
	return nil, nil, fmt.Errorf("hw:0,2 playback not found")
}

// ---- mixer-service loopback (AEC reference) ----

type lbClient struct {
	c  net.Conn
	mu sync.Mutex
}

func (o *Output) hasLoopback() bool {
	o.lbMu.Lock()
	defer o.lbMu.Unlock()
	return len(o.lbClients) > 0
}

func (o *Output) sendLoopback(msg []byte) {
	o.lbMu.Lock()
	cl := append([]*lbClient(nil), o.lbClients...)
	o.lbMu.Unlock()
	for _, c := range cl {
		c.c.SetWriteDeadline(time.Now().Add(100 * time.Millisecond))
		if _, err := c.c.Write(msg); err != nil {
			o.dropLoopback(c)
		}
	}
}

func (o *Output) dropLoopback(c *lbClient) {
	o.lbMu.Lock()
	defer o.lbMu.Unlock()
	for i, x := range o.lbClients {
		if x == c {
			o.lbClients = append(o.lbClients[:i], o.lbClients[i+1:]...)
			c.c.Close()
			return
		}
	}
}

func (o *Output) acceptLoopback(ctx context.Context, ln net.Listener) {
	for ctx.Err() == nil {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go o.serveMixerClient(c)
	}
}

// serveMixerClient handles one connection. Only loopback requests are supported;
// everything else is read and ignored.
func (o *Output) serveMixerClient(c net.Conn) {
	cl := &lbClient{c: c}
	defer o.dropLoopback(cl)
	hdr := make([]byte, 2)
	for {
		if _, err := io.ReadFull(c, hdr); err != nil {
			c.Close()
			return
		}
		body := make([]byte, binary.BigEndian.Uint16(hdr))
		if _, err := io.ReadFull(c, body); err != nil {
			c.Close()
			return
		}
		if len(body) < 6 || binary.LittleEndian.Uint16(body) != 0 {
			continue
		}
		pad := int(binary.BigEndian.Uint32(body[2:]))
		if pad > 3 || 6+pad > len(body) {
			continue
		}
		if hasField(body[6:len(body)-pad], 10) { // Generic.loopback_request
			c.SetWriteDeadline(time.Now().Add(time.Second))
			if _, err := c.Write(metadataMsg(streamConfig())); err != nil {
				c.Close()
				return
			}
			o.lbMu.Lock()
			o.lbClients = append(o.lbClients, cl)
			o.lbMu.Unlock()
			o.Log.Info("AEC loopback client subscribed")
		}
	}
}

func monoUs() int64 { return time.Now().UnixMicro() } // only differences matter

func metadataMsg(proto []byte) []byte {
	pad := (4 - len(proto)%4) % 4
	body := binary.BigEndian.AppendUint16(nil, 0)
	body = binary.BigEndian.AppendUint32(body, uint32(pad))
	body = append(body, proto...)
	body = append(body, make([]byte, pad)...)
	return append(binary.BigEndian.AppendUint16(nil, uint16(len(body))), body...)
}

func audioMsg(ts int64, samples []byte) []byte {
	msg := make([]byte, 2+14+len(samples))
	binary.BigEndian.PutUint16(msg[0:], uint16(14+len(samples)))
	binary.LittleEndian.PutUint16(msg[2:], 1)
	binary.LittleEndian.PutUint64(msg[4:], uint64(ts))
	copy(msg[16:], samples)
	return msg
}

func streamConfig() []byte {
	var sc []byte
	sc = pbField(sc, 1, 2) // SAMPLE_FORMAT_FLOAT_I
	sc = pbField(sc, 2, Rate)
	sc = pbField(sc, 3, Channels)
	b := pbVarint(nil, 9<<3|2) // Generic.stream_config
	b = pbVarint(b, uint64(len(sc)))
	return append(b, sc...)
}

func pbVarint(b []byte, v uint64) []byte {
	for v >= 0x80 {
		b = append(b, byte(v)|0x80)
		v >>= 7
	}
	return append(b, byte(v))
}

func pbField(b []byte, field int, v uint64) []byte {
	return pbVarint(pbVarint(b, uint64(field<<3)), v)
}

func hasField(b []byte, want int) bool {
	for i := 0; i < len(b); {
		key, n := binary.Uvarint(b[i:])
		if n <= 0 {
			return false
		}
		i += n
		if int(key>>3) == want {
			return true
		}
		switch key & 7 {
		case 0:
			_, n = binary.Uvarint(b[i:])
			i += n
		case 1:
			i += 8
		case 2:
			l, n := binary.Uvarint(b[i:])
			i += n + int(l)
		case 5:
			i += 4
		default:
			return false
		}
	}
	return false
}

// Latency is how long a sample handed to the mixer takes to reach the
// speaker (the ALSA buffer), for scheduling synchronised playback.
func Latency() time.Duration { return time.Duration(bufferFrames) * time.Second / Rate }
