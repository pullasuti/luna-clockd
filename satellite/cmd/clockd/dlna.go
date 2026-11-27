package main

import (
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	esphome "github.com/ygelfand/go-esphome-device"

	"lenovo-satellite/internal/config"
	"lenovo-satellite/internal/dlna"
	"lenovo-satellite/internal/output"
)

// DLNA (Audio > DLNA, both off by default; UPnP has no authentication):
//   - media server: the USB drive in Home Assistant's media browser ("DLNA
//     Digital Media Server" integration), VLC, Kodi, BubbleUPnP... Read-only,
//     .flac/.mp3/.wav only, confined to the mount like the USB player.
//   - renderer: control points send the clock a URL to play (HA's "DLNA
//     Digital Media Renderer", phone apps). Plays as the clock's media; pause
//     holds the stream, no seeking.
// Both share one HTTP port (dlnaPort) and SSDP on wlan0.

const dlnaPort = 8200

type dlnaCtl struct {
	d *daemon

	mu    sync.Mutex
	stack *dlna.Stack
	err   string
	lib   *usbLibrary
	pl    *dlnaPlayer
}

// apply starts, changes or stops the stack to match the settings.
func (c *dlnaCtl) apply(cfg config.DLNA) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !cfg.Server && !cfg.Renderer {
		if c.stack != nil {
			c.stack.Close()
			c.stack = nil
			c.d.log.Info("DLNA off")
		}
		c.err = ""
		return
	}
	if c.stack == nil {
		s, err := dlna.Start(dlna.Config{Name: c.d.friendly, Manufacturer: "clockd", Model: "Lenovo Smart Clock Essential",
			Version: version, Seed: macAddress(), Iface: "wlan0", Port: dlnaPort, Log: c.d.log})
		if err != nil {
			c.err = err.Error()
			c.d.log.Warn("DLNA did not start", "err", err)
			return
		}
		c.stack, c.err = s, ""
		c.d.log.Info("DLNA on", "port", dlnaPort)
	}
	if cfg.Server {
		if c.lib == nil {
			c.lib = &usbLibrary{meta: map[string]libMeta{}}
		}
		c.stack.SetLibrary(c.lib)
	} else {
		c.stack.SetLibrary(nil)
	}
	if cfg.Renderer {
		if c.pl == nil {
			c.pl = &dlnaPlayer{d: c.d}
		}
		c.stack.SetPlayer(c.pl)
	} else {
		c.stack.SetPlayer(nil)
	}
	c.d.log.Info("DLNA", "server", cfg.Server, "renderer", cfg.Renderer)
}

func (c *dlnaCtl) status() map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	m := map[string]any{"running": c.stack != nil, "port": dlnaPort}
	if c.err != "" {
		m["error"] = c.err
	}
	if c.stack != nil {
		for k, v := range c.stack.Status() {
			m[k] = v
		}
	}
	_, _, _, ok := usbMounted()
	m["drive"] = ok
	return m
}

// ---- the USB drive as a dlna.Library ----

type libMeta struct {
	size  int64
	mtime time.Time
	name  string
	dur   time.Duration
}

type usbLibrary struct {
	mu   sync.Mutex
	meta map[string]libMeta // header reads, by path (size+mtime checked)
}

// full maps a rooted library path into the mount, refusing anything outside.
func libFull(p string) (string, error) {
	if _, _, _, ok := usbMounted(); !ok {
		return "", errors.New("no drive mounted")
	}
	clean := path.Clean("/" + p)
	if clean != p && clean+"/" != p {
		return "", errors.New("bad path")
	}
	full := filepath.Join(usbMount, filepath.FromSlash(clean))
	if full != usbMount && !strings.HasPrefix(full, usbMount+string(filepath.Separator)) {
		return "", errors.New("path outside the drive")
	}
	return full, nil
}

func (l *usbLibrary) List(dir string) ([]dlna.Entry, error) {
	full, err := libFull(dir)
	if err != nil {
		return nil, err
	}
	fi, err := os.Lstat(full)
	if err != nil || !fi.IsDir() { // a symlink is never a directory here
		return nil, errors.New("no such folder")
	}
	es, err := os.ReadDir(full)
	if err != nil {
		return nil, err
	}
	var out []dlna.Entry
	for _, e := range es {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		info, err := e.Info() // lstat
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		p := path.Join(path.Clean("/"+dir), e.Name())
		switch {
		case info.IsDir():
			out = append(out, dlna.Entry{Name: e.Name(), Path: p, Dir: true})
		case info.Mode().IsRegular() && audioExt(e.Name()):
			out = append(out, dlna.Entry{Name: e.Name(), Path: p, Size: info.Size()})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Dir != out[j].Dir {
			return out[i].Dir
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out, nil
}

func (l *usbLibrary) Open(p string) (*os.File, error) {
	full, err := libFull(p)
	if err != nil {
		return nil, err
	}
	fi, err := os.Lstat(full)
	if err != nil || !fi.Mode().IsRegular() || !audioExt(full) {
		return nil, errors.New("no such file")
	}
	return os.Open(full)
}

func (l *usbLibrary) Meta(p string) (string, time.Duration) {
	full, err := libFull(p)
	if err != nil {
		return "", 0
	}
	fi, err := os.Lstat(full)
	if err != nil || !fi.Mode().IsRegular() {
		return "", 0
	}
	l.mu.Lock()
	m, ok := l.meta[p]
	l.mu.Unlock()
	if ok && m.size == fi.Size() && m.mtime.Equal(fi.ModTime()) {
		return m.name, m.dur
	}
	f, err := os.Open(full)
	if err != nil {
		return "", 0
	}
	info, _ := output.TrackMeta(f)
	f.Close()
	l.mu.Lock()
	if len(l.meta) > 5000 {
		clear(l.meta)
	}
	l.meta[p] = libMeta{fi.Size(), fi.ModTime(), info.Name, info.Duration}
	l.mu.Unlock()
	return info.Name, info.Duration
}

// UpdateID changes with the drive (device and the root's mtime).
func (l *usbLibrary) UpdateID() uint32 {
	dev, _, _, ok := usbMounted()
	if !ok {
		return 0
	}
	h := fnv.New32a()
	h.Write([]byte(dev))
	if fi, err := os.Stat(usbMount); err == nil {
		fmt.Fprint(h, fi.ModTime().UnixNano())
	}
	return h.Sum32() >> 1
}

// ---- the clock's media as a dlna.Player ----

// pausable plays a buffer and holds it (silence) while paused; the stream
// waits behind it.
type pausable struct {
	b      *output.Buffer
	paused atomic.Bool
	played atomic.Int64 // interleaved samples
}

func (p *pausable) Read(dst []float32) (int, bool) {
	if p.paused.Load() {
		clear(dst)
		return len(dst), true
	}
	n, ok := p.b.Read(dst)
	p.played.Add(int64(n))
	return n, ok
}

type dlnaPlayer struct {
	d *daemon

	mu       sync.Mutex
	v        *output.Voice
	src      *pausable
	muted    bool
	unmuteTo float32
}

func (p *dlnaPlayer) Play(uri, title string) error {
	d := p.d
	if d.sendspin != nil && d.sendspin.Playing() {
		d.sendspin.Pause()
	}
	d.stopMedia()
	name := title
	if name == "" {
		name = shortURL(uri)
	}
	d.log.Info("DLNA play", "url", shortURL(uri), "title", title)
	var src *pausable
	v := d.startMediaWith("dlna", name, func(buf *output.Buffer) error {
		return fetchAudio(d.log, uri, buf, 0)
	}, nil, func(b *output.Buffer) output.Source {
		src = &pausable{b: b}
		return src
	})
	p.mu.Lock()
	p.v, p.src = v, src
	p.mu.Unlock()
	return nil
}

func (p *dlnaPlayer) current() (*output.Voice, *pausable) {
	p.mu.Lock()
	v, src := p.v, p.src
	p.mu.Unlock()
	p.d.mu.Lock()
	cur := v != nil && p.d.media == v
	p.d.mu.Unlock()
	if !cur {
		return nil, nil
	}
	return v, src
}

func (p *dlnaPlayer) Pause() {
	if _, src := p.current(); src != nil {
		src.paused.Store(true)
		p.d.player.SetState(esphome.MediaPlayerPaused)
	}
}

func (p *dlnaPlayer) Resume() {
	if _, src := p.current(); src != nil {
		src.paused.Store(false)
		p.d.player.SetState(esphome.MediaPlayerPlaying)
	}
}

func (p *dlnaPlayer) Stop() {
	if v, _ := p.current(); v != nil {
		p.d.stopMedia()
	}
}

func (p *dlnaPlayer) Status() (bool, bool, time.Duration) {
	v, src := p.current()
	if v == nil {
		return false, false, 0
	}
	select {
	case <-v.Done():
		return false, false, 0
	default:
	}
	pos := time.Duration(src.played.Load()/output.Channels) * time.Second / output.Rate
	return true, src.paused.Load(), pos
}

func (p *dlnaPlayer) Volume() int { return int(p.d.player.Volume()*100 + 0.5) }

func (p *dlnaPlayer) SetVolume(v int) {
	p.mu.Lock()
	p.muted = false
	p.mu.Unlock()
	p.d.setVolume(float32(v) / 100)
}

func (p *dlnaPlayer) Muted() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.muted && p.d.player.Volume() > 0 { // turned up elsewhere
		p.muted = false
	}
	return p.muted
}

func (p *dlnaPlayer) SetMuted(m bool) {
	p.mu.Lock()
	if m == p.muted {
		p.mu.Unlock()
		return
	}
	p.muted = m
	if m {
		p.unmuteTo = p.d.player.Volume()
	}
	to := p.unmuteTo
	p.mu.Unlock()
	if m {
		p.d.setVolume(0)
	} else {
		p.d.setVolume(to)
	}
}

// ---- web UI ----

func (b *webBackend) DLNAStatus() map[string]any { return b.d.dlna.status() }
