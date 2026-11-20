// Package sendspin is a small Sendspin player (Music Assistant's synchronized
// multi-room audio protocol) built on the pure-Go protocol and clock-sync
// packages of github.com/Sendspin/sendspin-go. Its high-level Receiver needs
// cgo (libopus) for opus, so this player advertises FLAC (decoded with the
// same pure-Go decoder as USB playback; the server sends the STREAMINFO once
// and whole FLAC frames per chunk) and raw PCM at the output's native
// 48 kHz / 16-bit / stereo as the fallback: Music Assistant transcodes
// either way, and the clock needs neither libopus nor a resampler.
//
// Music Assistant finds the player over mDNS (_sendspin._tcp, advertised by
// clockd) and connects to ws://<clock>:<port>/sendspin.
package sendspin

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	stdsync "sync"
	"time"

	"github.com/Sendspin/sendspin-go/pkg/protocol"
	"github.com/Sendspin/sendspin-go/pkg/sync"
	"github.com/gorilla/websocket"

	"lenovo-satellite/internal/output"
)

const (
	Service  = "_sendspin._tcp"
	Path     = "/sendspin"
	rate     = output.Rate
	channels = output.Channels
)

// Player serves one Sendspin connection at a time (a new one replaces the old).
type Player struct {
	Addr        string // listen address, e.g. ":8928"
	Name        string
	ClientID    string
	Version     string
	Out         *output.Output
	Log         *slog.Logger
	StaticDelay time.Duration // extra delay to line up with other speakers
	// OnMetadata gets "artist - title" when a new track plays ("" when stopped).
	// Metadata the server sends while nothing plays (it repeats the last track
	// on every connect) is held until a stream starts, and each track is passed
	// once, so a clockd restart or a pause/resume doesn't show it again.
	OnMetadata func(nowPlaying string)

	metaPending, metaShown string // under mu
	// OnState reports whether a stream is playing.
	OnState func(playing bool)

	mu     stdsync.Mutex
	conn   *protocol.Client
	src    *schedSource
	flac   *output.FLACChunkDecoder // non-nil while a flac stream runs
	voice  *output.Voice
	volume int // 0..100
	muted  bool
}

func (p *Player) Run(ctx context.Context) error {
	if p.volume == 0 {
		p.volume = 100
	}
	// Music Assistant is not a browser and sends no Origin. A request that has
	// one comes from a web page, which could be any page open on the LAN:
	// there is no login here, so those are refused.
	up := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return r.Header.Get("Origin") == "" }}
	mux := http.NewServeMux()
	mux.HandleFunc(Path, func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		go p.serve(ctx, c, r.RemoteAddr)
	})
	ln, err := net.Listen("tcp", p.Addr)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}
	go func() { <-ctx.Done(); srv.Close() }()
	p.Log.Info("sendspin player listening", "addr", p.Addr, "name", p.Name)
	if err := srv.Serve(ln); err != nil && ctx.Err() == nil {
		return err
	}
	return nil
}

func (p *Player) serve(ctx context.Context, ws *websocket.Conn, peer string) {
	cfg := protocol.Config{
		ClientID: p.ClientID,
		Name:     p.Name,
		Version:  1,
		DeviceInfo: protocol.DeviceInfo{
			ProductName: "Lenovo Smart Clock", Manufacturer: "clockd", SoftwareVersion: p.Version,
		},
		PlayerV1Support: protocol.PlayerV1Support{
			// flac first (a fraction of the PCM bandwidth, decoded with the
			// same pure-Go decoder as USB playback); pcm stays as fallback
			SupportedFormats: []protocol.AudioFormat{
				{Codec: "flac", Channels: channels, SampleRate: rate, BitDepth: 16},
				{Codec: "pcm", Channels: channels, SampleRate: rate, BitDepth: 16},
			},
			BufferCapacity:    1 << 20,
			SupportedCommands: []string{"volume", "mute"},
		},
	}
	c := protocol.NewClientFromConn(cfg, ws)
	if err := c.Start(); err != nil {
		p.Log.Warn("sendspin handshake failed", "peer", peer, "err", err)
		return
	}
	p.mu.Lock()
	old := p.conn
	p.conn = c
	p.mu.Unlock()
	if old != nil {
		old.Close() // the server reconnected
	}
	name := ""
	if h := c.ServerHello(); h != nil {
		name = h.Name
	}
	p.Log.Info("sendspin server connected", "peer", peer, "server", name)
	p.sendState(c)

	cs := sync.NewClockSync()
	syncTick := time.NewTicker(time.Second)
	defer syncTick.Stop()
	syncs := 0
	var lastDiag time.Time
	c.SendTimeSync(time.Now().UnixMicro())

	defer func() {
		p.stopStream()
		p.mu.Lock()
		if p.conn == c {
			p.conn = nil
		}
		p.mu.Unlock()
		c.Close()
		p.Log.Info("sendspin server disconnected", "peer", peer)
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.Done():
			return
		case <-syncTick.C:
			// a burst at the start, then every 5 s
			syncs++
			if syncs < 10 || syncs%5 == 0 {
				c.SendTimeSync(time.Now().UnixMicro())
			}
		case t := <-c.TimeSyncResp:
			cs.ProcessSyncResponse(t.ClientTransmitted, t.ServerReceived, t.ServerTransmitted, time.Now().UnixMicro())
		case s := <-c.StreamStart:
			if s.Player == nil {
				continue
			}
			f := s.Player
			flacOK := f.Codec == "flac" && f.SampleRate == rate && f.Channels == channels && f.BitDepth == 16 && f.CodecHeader != ""
			pcmOK := f.Codec == "pcm" && f.SampleRate == rate && f.Channels == channels && f.BitDepth == 16
			if !flacOK && !pcmOK {
				p.Log.Warn("sendspin: unsupported stream format", "codec", f.Codec, "rate", f.SampleRate, "channels", f.Channels, "bits", f.BitDepth)
				continue
			}
			var dec *output.FLACChunkDecoder
			if flacOK {
				hdr, err := base64.StdEncoding.DecodeString(f.CodecHeader)
				if err == nil {
					dec, err = output.NewFLACChunkDecoder(hdr)
				}
				if err != nil {
					p.Log.Warn("sendspin: bad flac codec header", "err", err)
					continue
				}
				if dec.Rate() != rate {
					p.Log.Warn("sendspin: flac codec header rate mismatch", "header", dec.Rate())
					continue
				}
			}
			p.mu.Lock()
			p.flac = dec
			p.mu.Unlock()
			p.startStream(f.Codec)
		case <-c.StreamClear:
			p.mu.Lock()
			if p.src != nil {
				p.src.clear()
			}
			p.mu.Unlock()
		case <-c.StreamEnd:
			p.stopStream()
		case ch := <-c.AudioChunks:
			p.mu.Lock()
			src, dec := p.src, p.flac
			p.mu.Unlock()
			if src != nil {
				at := cs.ServerToLocalTime(ch.Timestamp)
				var samples []float32
				if dec != nil {
					var err error
					if samples, err = dec.Decode(ch.Data); err != nil {
						p.Log.Warn("sendspin flac chunk dropped", "err", err)
					}
				} else {
					samples = pcm16(ch.Data)
				}
				if len(samples) > 0 {
					src.push(at, samples)
				}
				if time.Since(lastDiag) > 30*time.Second {
					lastDiag = time.Now()
					rtt, q := cs.GetStats()
					src.mu.Lock()
					queued, late := len(src.q), src.late
					src.mu.Unlock()
					p.Log.Info("sendspin diag", "chunk_in", time.Until(at).Round(time.Millisecond), "server_ts", ch.Timestamp,
						"bytes", len(ch.Data), "queued", queued, "late", late, "sync_rtt_us", rtt, "sync_quality", q, "gain", src.gain)
				}
			}
		case cmd := <-c.ControlMsgs:
			p.mu.Lock()
			switch cmd.Command {
			case "volume":
				p.volume = max(0, min(cmd.Volume, 100))
			case "mute":
				p.muted = cmd.Mute
			}
			if p.src != nil {
				p.src.setGain(p.gain())
			}
			p.mu.Unlock()
			p.sendState(c)
		case st := <-c.ServerState:
			if st.Metadata != nil {
				p.mu.Lock()
				p.metaPending = nowPlaying(st.Metadata)
				p.mu.Unlock()
				p.showMeta()
			}
		case <-c.GroupUpdate:
		case <-c.ArtworkChunks:
		}
	}
}

func nowPlaying(m *protocol.MetadataState) string {
	title, artist := deref(m.Title), deref(m.Artist)
	np := title
	if title != "" && artist != "" {
		np = artist + " - " + title
	}
	// it scrolls over the face: a title from the other end has no say in how long
	if r := []rune(np); len(r) > maxNowPlaying {
		np = string(r[:maxNowPlaying-1]) + "…"
	}
	return np
}

const maxNowPlaying = 120

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func (p *Player) gain() float32 {
	if p.muted {
		return 0
	}
	v := float32(p.volume) / 100
	return v * v // perceptual curve
}

func (p *Player) sendState(c *protocol.Client) {
	p.mu.Lock()
	st := protocol.PlayerState{State: "synchronized", Volume: p.volume, Muted: p.muted}
	p.mu.Unlock()
	if err := c.SendState(st); err != nil {
		p.Log.Debug("sendspin state", "err", err)
	}
}

func (p *Player) startStream(codec string) {
	p.mu.Lock()
	if p.src != nil {
		p.src.clear()
		p.mu.Unlock()
		return
	}
	p.src = &schedSource{ahead: output.Latency() + p.StaticDelay, gain: p.gain()}
	p.voice = p.Out.Play(p.src, 1, "media")
	p.mu.Unlock()
	p.Log.Info("sendspin stream started", "codec", codec)
	p.showMeta()
	if p.OnState != nil {
		p.OnState(true)
	}
}

func (p *Player) stopStream() {
	p.mu.Lock()
	v := p.voice
	was := p.src != nil
	p.src, p.voice, p.flac = nil, nil, nil
	p.mu.Unlock()
	if v != nil {
		p.Out.Stop(v)
	}
	if was {
		p.Log.Info("sendspin stream ended")
		if p.OnState != nil {
			p.OnState(false)
		}
		if p.OnMetadata != nil {
			p.OnMetadata("")
		}
	}
}

// Stop ends local playback (e.g. the stop word); the server is told nothing,
// so it may send the stream again.
// Pause silences the clock and asks the server (Music Assistant) to pause
// its queue, by sending the controller command over the connection. Pause,
// not stop: a spoken "stop" should pick up where it left off when playback
// is asked for again, and it must reach the server at all — without the
// command the server keeps its "playing" state, keeps streaming into a muted
// player, and a clock restart re-establishes the stream and the music comes
// back on its own (seen 2026-11-06).
func (p *Player) Pause() {
	p.stopStream()
	p.mu.Lock()
	c := p.conn
	p.mu.Unlock()
	if c == nil {
		return
	}
	if err := c.Send("client/command", map[string]any{
		"controller": map[string]any{"command": "pause"},
	}); err != nil {
		p.Log.Warn("sendspin pause command", "err", err)
	}
}

// Connected reports whether a Sendspin server (Music Assistant) is connected.
func (p *Player) Connected() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.conn != nil
}

// showMeta passes the pending track to OnMetadata if a stream plays and it
// hasn't been shown yet.
func (p *Player) showMeta() {
	p.mu.Lock()
	np := p.metaPending
	show := p.src != nil && np != "" && np != p.metaShown
	if show {
		p.metaShown = np
	}
	p.mu.Unlock()
	if show && p.OnMetadata != nil {
		p.OnMetadata(np)
	}
}

// Playing reports whether a stream is playing.
func (p *Player) Playing() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.src != nil
}

func pcm16(b []byte) []float32 {
	s := make([]float32, len(b)/2)
	for i := range s {
		s[i] = float32(int16(binary.LittleEndian.Uint16(b[2*i:]))) / 32768
	}
	return s
}

// ---- scheduled playback ----

type chunk struct {
	at time.Time // local time of the first frame
	s  []float32 // interleaved stereo
}

// schedSource plays chunks at their (server-synchronised) local time. ahead is
// how far the speaker lags the moment Read is called (the ALSA buffer), so a
// frame is handed to the mixer that long before it is due.
type schedSource struct {
	mu    stdsync.Mutex
	q     []chunk
	ahead time.Duration
	gain  float32
	late  int // chunks dropped because they arrived too late
	// locked: playing continuously. Small timing errors (mixer wake-up
	// jitter, clock drift) are then ignored instead of corrected sample by
	// sample, which clicked at every chunk boundary; only errors beyond
	// lockTolerance re-align.
	locked bool
	now    func() time.Time // clock (tests); nil = time.Now
}

// maxQueue must exceed how far ahead the server sends: it fills the advertised
// buffer_capacity (1 MB). That is ~5.4 s of 48 kHz/16-bit stereo PCM but
// several times more as FLAC, so the cap is generous: at 5 s the queue
// dropped the chunk about to play, every time, and nothing was heard.
const maxQueue = 30 * time.Second

const lockTolerance = 15 * time.Millisecond

func (s *schedSource) push(at time.Time, samples []float32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.q = append(s.q, chunk{at: at, s: samples})
	n := 0
	for _, c := range s.q {
		n += len(c.s)
	}
	// by time stamps, and by size: time stamps come from the other end, and
	// chunks that all claim the same far-off moment would never be dropped
	for len(s.q) > 1 && (s.q[len(s.q)-1].at.Sub(s.q[0].at) > maxQueue || n > frames(maxQueue)*channels) {
		n -= len(s.q[0].s)
		s.q = s.q[1:]
	}
}

func (s *schedSource) clear() {
	s.mu.Lock()
	s.q = nil
	s.locked = false
	s.mu.Unlock()
}

func (s *schedSource) setGain(g float32) {
	s.mu.Lock()
	s.gain = g
	s.mu.Unlock()
}

func frames(d time.Duration) int { return int(d.Seconds() * rate) }

// Read fills dst (interleaved stereo) with what should be heard when dst
// reaches the speaker: silence before the next chunk is due, late audio dropped.
func (s *schedSource) Read(dst []float32) (int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	clock := time.Now
	if s.now != nil {
		clock = s.now
	}
	start := clock().Add(s.ahead) // when dst[0] will be heard
	i := 0
	for i < len(dst) {
		if len(s.q) == 0 {
			clear(dst[i:])
			break
		}
		c := &s.q[0]
		// position of dst[i] inside the chunk, in samples
		off := frames(start.Add(time.Duration(i/channels)*time.Second/rate).Sub(c.at)) * channels
		if s.locked && abs(off) <= frames(lockTolerance)*channels {
			off = 0 // close enough: stay continuous
		}
		switch {
		case off >= len(c.s): // entirely in the past
			s.q = s.q[1:]
			s.late++
			s.locked = false
		case off < 0: // not due yet: silence until it is
			s.locked = false
			n := min(-off, len(dst)-i)
			clear(dst[i : i+n])
			i += n
		default:
			s.locked = true
			n := copy(dst[i:], c.s[off:])
			for k := i; k < i+n; k++ {
				dst[k] *= s.gain
			}
			i += n
			if off+n >= len(c.s) {
				s.q = s.q[1:]
			} else {
				// keep the rest; re-anchor so the next Read continues seamlessly
				c.at = c.at.Add(time.Duration((off+n)/channels) * time.Second / rate)
				c.s = c.s[off+n:]
			}
		}
	}
	return len(dst), true
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

func (s *schedSource) String() string { return fmt.Sprintf("sendspin(%d queued)", len(s.q)) }
