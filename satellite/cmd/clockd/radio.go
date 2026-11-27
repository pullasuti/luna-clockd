package main

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	esphome "github.com/ygelfand/go-esphome-device"

	"lenovo-satellite/internal/config"
	"lenovo-satellite/internal/fmradio"
	"lenovo-satellite/internal/output"
	"lenovo-satellite/internal/rtlsdr"
)

// FM radio from an RTL-SDR stick on the USB socket (drive mode): the pure-Go
// driver in internal/rtlsdr, stereo FM and RDS in internal/fmradio, played as
// media (startMedia), so volume, the stop word, ducking and Home Assistant's
// media player work as for any other media. The same run streams to network
// listeners (/radio/<token>.flac or .wav, web.go): one tuner, so they hear what the
// clock plays, or a station of their own while the clock plays nothing.
// Stations come from a search of the band (stereo pilot), kept in state.json.

const (
	fmStep = 100_000

	radioBlock   = 65536 // bytes of I/Q per USB read (17 ms at fmradio.WideRate)
	radioGain    = 0.8   // demodulated audio to the mixer
	radioPilotOK = 25.0  // dB: a station worth listing
)

// radioBand is the FM band and de-emphasis of the Radio.Region setting.
type radioBand struct {
	lo, hi int     // Hz
	tau    float64 // de-emphasis time constant
}

func (d *daemon) radioBand() radioBand {
	switch d.cfg.Get().Radio.Region {
	case "americas": // and South Korea
		return radioBand{87_500_000, 108_000_000, 75e-6}
	case "japan":
		return radioBand{76_000_000, 95_000_000, 50e-6}
	}
	return radioBand{87_500_000, 108_000_000, 50e-6}
}

func (b radioBand) check(hz int) error {
	if hz < b.lo || hz > b.hi {
		return fmt.Errorf("%.1f MHz is outside the FM band (%.1f-%.1f)", float64(hz)/1e6, float64(b.lo)/1e6, float64(b.hi)/1e6)
	}
	return nil
}

// radioGain is the tuner gain for the Radio.Reception setting: where the
// AGC starts (and how far it may raise the LNA, the stage that matters for
// weak stations) and the fixed gain of a station search.
func (d *daemon) radioGain() (agc *radioAGC, scan [3]int) {
	switch d.cfg.Get().Radio.Reception {
	case "weak":
		return &radioAGC{lna: 12, mixer: 10, vga: 8, lnaMax: 15}, [3]int{12, 10, 8}
	case "strong":
		return &radioAGC{lna: 3, mixer: 6, vga: 6, lnaMax: 4}, [3]int{3, 6, 6}
	}
	return &radioAGC{lna: 8, mixer: 8, vga: 6, lnaMax: 8}, [3]int{6, 6, 6}
}

type radioState struct {
	dev sync.Mutex // the stick: one user at a time (playing or searching)

	mu        sync.Mutex
	hz        int                 // tuned, 0 = not
	want      int                 // the station of the current (or starting) run
	speaker   bool                // that run plays on the clock (else for listeners only)
	quit      chan struct{}       // ends that run; nil = none
	listeners map[chan []byte]int // network streams (16-bit PCM blocks) and the blocks each lost
	name      string              // its RDS name ("" = none yet)
	stereo    bool
	searching bool    // a search runs
	progress  float64 // of the search, 0..1
	lastErr   string

	sel  *esphome.Select // the Home Assistant dropdown (radioha.go)
	opts []string        // its options
}

var errNoRadio = errors.New("no FM radio stick: plug an RTL-SDR stick into the USB socket and switch it to USB drive")

// haltLocked ends the current run (it lets go of the stick within a block).
func (r *radioState) haltLocked() {
	if r.quit != nil {
		close(r.quit)
		r.quit = nil
	}
	r.want, r.speaker = 0, false
}

// dropListenersLocked ends every network stream.
func (r *radioState) dropListenersLocked() {
	for c := range r.listeners {
		close(c)
	}
	r.listeners = nil
}

// radioStatus is the web UI's view of the radio.
func (d *daemon) radioStatus() map[string]any {
	_, prod, err := rtlsdr.Find()
	r := &d.radio
	r.mu.Lock()
	st := map[string]any{"stick": prod, "found": err == nil, "playing": r.hz, "searching": r.searching,
		"progress": math.Round(r.progress*100) / 100, "error": r.lastErr, "name": r.name, "stereo": r.stereo,
		"speaker": r.speaker, "listeners": len(r.listeners)}
	b := d.radioBand()
	st["band"] = []int{b.lo, b.hi}
	r.mu.Unlock()
	st["stream"] = "/radio/" + d.radioToken(false) + ".flac"
	s := config.LoadState(d.statePath)
	st["stations"], st["last"], st["favs"] = s.RadioStations, s.RadioLast, s.RadioFavs
	if len(s.RadioStations) == 0 {
		st["stations"] = []config.RadioStation{} // [] in JSON, not null
	}
	if len(s.RadioFavs) == 0 {
		st["favs"] = []int{}
	}
	return st
}

// radioMaxFavs: the Player card's favourite slots.
const radioMaxFavs = 6

// radioSetFavs saves the favourite stations (in the given order) and gives
// Home Assistant's dropdown the new list.
func (d *daemon) radioSetFavs(favs []int) error {
	if len(favs) > radioMaxFavs {
		return fmt.Errorf("at most %d favourites", radioMaxFavs)
	}
	var clean []int
	for _, hz := range favs {
		if hz < 76_000_000 || hz > 108_000_000 || hz%fmStep != 0 {
			return fmt.Errorf("%d Hz: a station on the 0.1 MHz raster, 76-108 MHz", hz)
		}
		if !slices.Contains(clean, hz) {
			clean = append(clean, hz)
		}
	}
	if err := config.UpdateState(d.statePath, func(s *config.State) { s.RadioFavs = clean }); err != nil {
		return err
	}
	d.radioStationsChanged()
	return nil
}

// radioToken is the secret part of the stream link (made on first use);
// renew makes a new one and cuts off everyone listening on the old one.
func (d *daemon) radioToken(renew bool) string {
	tok := config.LoadState(d.statePath).RadioStreamToken
	if tok != "" && !renew {
		return tok
	}
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return tok
	}
	tok = hex.EncodeToString(b)
	if err := config.UpdateState(d.statePath, func(s *config.State) { s.RadioStreamToken = tok }); err != nil {
		d.log.Warn("radio: save stream token", "err", err)
	}
	if renew {
		d.radio.mu.Lock()
		d.radio.dropListenersLocked()
		d.radio.mu.Unlock()
	}
	return tok
}

// radioPlay tunes hz (Hz, in the FM band) and plays it as media. Network
// listeners stay and hear the new station.
func (d *daemon) radioPlay(hz int) error {
	if err := d.radioBand().check(hz); err != nil {
		return err
	}
	node, _, err := rtlsdr.Find()
	if err != nil {
		return errNoRadio
	}
	r := &d.radio
	r.mu.Lock()
	if r.searching {
		r.mu.Unlock()
		return errors.New("a station search is running")
	}
	r.haltLocked()
	q := make(chan struct{})
	r.quit, r.want, r.speaker = q, hz, true
	r.mu.Unlock()
	d.stopMedia()
	if err := config.UpdateState(d.statePath, func(s *config.State) { s.RadioLast = hz }); err != nil {
		d.log.Warn("radio: save last station", "err", err)
	}
	d.log.Info("radio: play", "mhz", float64(hz)/1e6)
	d.startMedia("fm", fmt.Sprintf("FM %.1f MHz", float64(hz)/1e6), func(buf *output.Buffer) error {
		return d.radioRun(node, hz, buf, q)
	}, nil)
	return nil
}

// radioRetune switches a listeners-only run to hz (the web UI's station list
// while Listen here plays and the clock's speaker is off). With the speaker
// on it is radioPlay.
func (d *daemon) radioRetune(hz int) error {
	if err := d.radioBand().check(hz); err != nil {
		return err
	}
	node, _, err := rtlsdr.Find()
	if err != nil {
		return errNoRadio
	}
	r := &d.radio
	r.mu.Lock()
	if r.speaker {
		r.mu.Unlock()
		return d.radioPlay(hz)
	}
	if r.searching || len(r.listeners) == 0 {
		r.mu.Unlock()
		if r.searching {
			return errors.New("a station search is running")
		}
		return errors.New("nobody is listening")
	}
	r.haltLocked()
	q := make(chan struct{})
	r.quit, r.want = q, hz
	r.mu.Unlock()
	d.log.Info("radio: play for listeners", "mhz", float64(hz)/1e6)
	go d.radioRun(node, hz, nil, q)
	if err := config.UpdateState(d.statePath, func(s *config.State) { s.RadioLast = hz }); err != nil {
		d.log.Warn("radio: save last station", "err", err)
	}
	return nil
}

// radioListen adds a network listener: it hears whatever the tuner plays.
// With nothing playing it starts hz (0: the last station) for listeners only;
// a listeners-only run is retuned by a listener asking for another station,
// but the station playing on the clock is not taken away.
func (d *daemon) radioListen(hz int) (<-chan []byte, func(), error) {
	if hz != 0 {
		if err := d.radioBand().check(hz); err != nil {
			return nil, nil, err
		}
	}
	node, _, err := rtlsdr.Find()
	if err != nil {
		return nil, nil, errNoRadio
	}
	r := &d.radio
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.searching {
		return nil, nil, errors.New("a station search is running")
	}
	if r.speaker && hz != 0 && hz != r.want {
		return nil, nil, fmt.Errorf("the clock plays %.1f MHz", float64(r.want)/1e6)
	}
	if r.want == 0 || hz != 0 && hz != r.want {
		if hz == 0 {
			if hz = config.LoadState(d.statePath).RadioLast; hz == 0 {
				return nil, nil, errors.New("no station played yet: add ?mhz=91.2 to the link")
			}
		}
		r.haltLocked()
		q := make(chan struct{})
		r.quit, r.want = q, hz
		d.log.Info("radio: play for listeners", "mhz", float64(hz)/1e6)
		go d.radioRun(node, hz, nil, q)
	}
	c := make(chan []byte, 192) // ~3 s for a listener's Wi-Fi to catch up
	if r.listeners == nil {
		r.listeners = map[chan []byte]int{}
	}
	r.listeners[c] = 0
	return c, func() {
		r.mu.Lock()
		lost, ok := r.listeners[c]
		if ok {
			delete(r.listeners, c)
			close(c)
		}
		r.mu.Unlock()
		if lost > 0 { // the listener's network fell behind by more than the buffer
			d.log.Info("radio: a listener lost audio", "blocks", lost, "seconds", float64(lost)*radioBlock/2/fmradio.WideRate)
		}
	}, nil
}

// radioRun plays one station into buf (the clock's speaker; nil = for network
// listeners only) and to the listeners, until quit closes, the stick fails,
// or nobody is left: neither the speaker (stopped by the stop word, other
// media) nor any listener.
func (d *daemon) radioRun(node string, hz int, buf *output.Buffer, quit chan struct{}) error {
	r := &d.radio
	r.dev.Lock() // a station switch waits for the previous run to let go
	defer r.dev.Unlock()
	fail := func(err error) error {
		r.setErr(err)
		if buf != nil {
			buf.Close(err)
		}
		r.mu.Lock()
		if r.quit == quit { // nobody took over: the listeners end too
			r.quit, r.want, r.speaker = nil, 0, false
			r.dropListenersLocked()
		}
		r.mu.Unlock()
		return err
	}
	select {
	case <-quit: // replaced before it started
		if buf != nil {
			buf.Close(nil)
		}
		return nil
	default:
	}
	s, err := rtlsdr.Open(node, fmradio.WideRate)
	if err != nil {
		return fail(err)
	}
	defer s.Close()
	g, _ := d.radioGain()
	s.SetGain(g.lna, g.mixer, g.vga)
	got, err := s.Tune(hz)
	if err != nil {
		return fail(err)
	}
	d.log.Info("radio: tuned", "hz", got)
	r.mu.Lock()
	r.hz, r.lastErr, r.name, r.stereo = hz, "", "", false
	r.mu.Unlock()
	d.radioSync()
	defer func() {
		r.mu.Lock()
		if r.quit == quit {
			r.quit, r.want, r.speaker = nil, 0, false
		}
		if r.quit == nil {
			r.hz, r.name, r.stereo = 0, "", false
		}
		r.mu.Unlock()
		d.radioSync()
	}()

	dem := fmradio.New(d.radioBand().tau)
	spk := buf
	var pcm []float32
	n := 0
	err = s.Stream(quit, radioBlock, func(iq []byte) bool {
		if g.feed(iq) {
			s.SetGain(g.lna, g.mixer, g.vga)
		}
		pcm = dem.ProcessWide(iq, pcm[:0])
		for i := range pcm {
			pcm[i] *= radioGain
		}
		if n++; n%30 == 0 { // ~0.5 s: stereo and the RDS name
			d.radioNews(hz, dem)
		}
		if n == 120 { // ~2 s in: how far off the stick's crystal puts the station
			if off := dem.OffsetHz(); off > 2000 || off < -2000 {
				d.log.Info("radio: station off the tuned frequency (stick crystal)", "mhz", float64(hz)/1e6, "khz", int(off/100)/10.0)
			}
		}
		r.mu.Lock()
		listeners := len(r.listeners)
		if listeners > 0 {
			blk := pcm16(pcm) // shared, read-only
			for c := range r.listeners {
				select {
				case c <- blk:
				default: // a slow listener loses this block, the others don't wait
					r.listeners[c]++
				}
			}
		}
		r.mu.Unlock()
		if spk != nil && !spk.Write(pcm) {
			spk = nil // the speaker stopped
			r.mu.Lock()
			if r.quit == quit {
				r.speaker = false
			}
			r.mu.Unlock()
			d.radioSync()
			if listeners == 0 {
				return false
			}
			d.log.Info("radio: speaker stopped, playing on for listeners", "listeners", listeners)
		}
		// nobody left (the speaker off, the last listener gone): done. A
		// listener is added before its run starts, and a station change
		// keeps the listeners, so no grace time is needed.
		return listeners > 0 || spk != nil
	})
	if buf != nil {
		buf.Close(nil)
	}
	if err != nil && !errors.Is(err, rtlsdr.ErrStopped) {
		d.log.Warn("radio: stream ended", "err", err)
		return fail(err)
	}
	return nil
}

// pcm16 converts -1..1 samples to 16-bit little-endian PCM.
func pcm16(x []float32) []byte {
	b := make([]byte, 2*len(x))
	for i, v := range x {
		v = max(-1, min(1, v))
		binary.LittleEndian.PutUint16(b[2*i:], uint16(int16(v*32767)))
	}
	return b
}

// radioNews takes the stereo state and the RDS name from the demodulator; a
// new name scrolls once on the face, goes into the status and the station
// list.
func (d *daemon) radioNews(hz int, dem *fmradio.Demod) {
	name, _ := dem.StationName()
	r := &d.radio
	r.mu.Lock()
	r.stereo = dem.Stereo()
	changed := name != "" && name != r.name
	if changed {
		r.name = name
	}
	speaker := r.speaker
	r.mu.Unlock()
	if !changed {
		return
	}
	d.log.Info("radio: station name", "mhz", float64(hz)/1e6, "name", name)
	d.mu.Lock()
	if d.mediaName != "" && strings.HasPrefix(d.mediaName, "FM ") {
		d.mediaName = fmt.Sprintf("FM %.1f MHz · %s", float64(hz)/1e6, name)
	}
	d.mu.Unlock()
	if d.face != nil && speaker { // not while it plays only for listeners
		d.face.showText(name, 1)
	}
	if err := config.UpdateState(d.statePath, func(s *config.State) {
		for i := range s.RadioStations {
			if s.RadioStations[i].Hz == hz {
				s.RadioStations[i].Name = name
			}
		}
	}); err != nil {
		d.log.Warn("radio: save station name", "err", err)
	}
}

func (r *radioState) setErr(err error) {
	r.mu.Lock()
	r.lastErr = err.Error()
	r.mu.Unlock()
}

// radioAGC keeps the I/Q level between -25 and -10 dBFS with the tuner's VGA
// (3.5 dB steps), so strong and weak stations neither clip nor drown.
type radioAGC struct {
	lna, mixer, vga int
	lnaMax          int
	sum             float64
	n               int
}

// feed reports whether the gain changed (checked every ~0.5 s).
func (a *radioAGC) feed(iq []byte) bool {
	for k := 0; k+1 < len(iq); k += 16 { // every 8th I/Q pair is plenty
		x, y := float64(iq[k])-127.5, float64(iq[k+1])-127.5
		a.sum += x*x + y*y
		a.n += 2
	}
	if a.n < fmradio.WideRate/8 { // half a second
		return false
	}
	db := 10 * math.Log10(a.sum/float64(a.n)/(127.5*127.5)+1e-12)
	a.sum, a.n = 0, 0
	switch {
	case db > -10 && a.vga > 0:
		a.vga--
	case db > -10 && a.lna > 0:
		a.lna--
	case db < -25 && a.lna < a.lnaMax:
		a.lna++
	case db < -25 && a.vga < 15:
		a.vga++
	default:
		return false
	}
	return true
}

// radioSearch steps through the band and keeps the frequencies with a stereo
// pilot (about 30 s). It stops the radio first and plays nothing meanwhile.
func (d *daemon) radioSearch() error {
	node, _, err := rtlsdr.Find()
	if err != nil {
		return errNoRadio
	}
	r := &d.radio
	r.mu.Lock()
	if r.searching {
		r.mu.Unlock()
		return errors.New("a search is already running")
	}
	r.searching, r.progress, r.lastErr = true, 0, ""
	speaker := r.speaker
	r.haltLocked()
	r.dropListenersLocked()
	r.mu.Unlock()
	if speaker {
		d.stopMedia()
	}
	go func() {
		defer func() {
			r.mu.Lock()
			r.searching, r.progress = false, 1
			r.mu.Unlock()
		}()
		found, err := d.radioScan(node)
		if err != nil {
			r.setErr(err)
			d.log.Warn("radio: search failed", "err", err)
			return
		}
		if err := config.UpdateState(d.statePath, func(s *config.State) {
			for i := range found { // names learned earlier stay
				for _, old := range s.RadioStations {
					if old.Hz == found[i].Hz {
						found[i].Name = old.Name
					}
				}
			}
			s.RadioStations = found
		}); err != nil {
			d.log.Warn("radio: save stations", "err", err)
		}
		d.log.Info("radio: search done", "stations", len(found))
		d.radioStationsChanged()
	}()
	return nil
}

func (d *daemon) radioScan(node string) ([]config.RadioStation, error) {
	r := &d.radio
	r.dev.Lock()
	defer r.dev.Unlock()
	s, err := rtlsdr.Open(node, fmradio.WideRate)
	if err != nil {
		return nil, err
	}
	defer s.Close()
	_, g := d.radioGain()
	s.SetGain(g[0], g[1], g[2])
	band := d.radioBand()
	var hits []radioHit
	steps := (band.hi-band.lo)/fmStep + 1
	for i := 0; i < steps; i++ {
		hz := band.lo + i*fmStep
		r.mu.Lock()
		r.progress = float64(i) / float64(steps)
		r.mu.Unlock()
		if _, err := s.Tune(hz); err != nil {
			continue
		}
		dem := fmradio.New(band.tau)
		dem.MeasurePilot()
		skip, got := 2, 0
		if err := s.Stream(nil, radioBlock, func(iq []byte) bool {
			if skip > 0 { // the PLL and the FIFO settle
				skip--
				return true
			}
			dem.ProcessWide(iq, nil)
			got += len(iq)
			return got < 2*fmradio.WideRate/10 // 100 ms
		}); err != nil {
			return nil, err
		}
		hits = append(hits, radioHit{hz, dem.PilotDB()})
	}
	return pickStations(hits), nil
}

type radioHit struct {
	hz    int
	pilot float64
}

// pickStations keeps one entry per station: strongest first, a step is
// dropped when a kept one within 300 kHz is at least as good (its own
// neighbouring steps) or more than 10 dB better (a strong station leaking
// into the channels next to it).
func pickStations(hits []radioHit) []config.RadioStation {
	sorted := append([]radioHit(nil), hits...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].pilot > sorted[j].pilot })
	var kept []radioHit
	for _, h := range sorted {
		if h.pilot < radioPilotOK {
			break
		}
		ok := true
		for _, k := range kept {
			dist := abs(k.hz - h.hz)
			if dist <= fmStep || dist <= 3*fmStep && k.pilot-h.pilot > 10 {
				ok = false
				break
			}
		}
		if ok {
			kept = append(kept, h)
		}
	}
	out := make([]config.RadioStation, 0, len(kept))
	for _, k := range kept {
		out = append(out, config.RadioStation{Hz: k.hz, PilotDB: math.Round(k.pilot*10) / 10})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Hz < out[j].Hz })
	return out
}

// radioStop stops the radio if it plays.
// radioStop ends the radio: on the speaker and for every listener.
func (d *daemon) radioStop() {
	r := &d.radio
	r.mu.Lock()
	speaker := r.speaker
	r.haltLocked()
	r.dropListenersLocked()
	r.mu.Unlock()
	if speaker {
		d.stopMedia()
	}
	for i := 0; i < 50; i++ { // the run lets go of the stick within a block or two
		r.mu.Lock()
		playing := r.hz != 0
		r.mu.Unlock()
		if !playing {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}
