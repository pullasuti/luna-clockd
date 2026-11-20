package main

// Attempt clips: with every wake word / stop word detection and near miss, a
// short clip of what the microphone heard is saved (16 kHz mono, after the
// digital gain — exactly the signal the detector scored), so "why didn't it
// hear me" can be answered by ear from the web UI's Recent attempts list.
// The clips live in <state>/clips and are kept capped: the newest clipKeep,
// a few MB at most. Nothing leaves the clock; web.go serves one clip per
// request to a logged-in browser.

import (
	"encoding/binary"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"lenovo-satellite/internal/localmic"
)

const (
	clipSeconds    = 3          // saved per attempt
	clipKeep       = 40         // newest clips kept on disk
	clipNamePrefix = "attempt-" // names sort by time (Unix ms)
)

// attemptClips keeps a rolling buffer of the last clipSeconds of detector
// input. Every method runs on the mic goroutine (which produces the events)
// except path, which only reads immutable names.
type attemptClips struct {
	log    *slog.Logger
	dir    string
	buf    []int16 // ring buffer, len localmic.Rate*clipSeconds
	pos    int     // next write position
	filled int     // samples buffered (grows to len(buf))
	seq    int     // makes names unique within one millisecond
}

func newAttemptClips(log *slog.Logger, dir string) *attemptClips {
	return &attemptClips{log: log, dir: dir, buf: make([]int16, localmic.Rate*clipSeconds)}
}

// feed adds one mic packet (mic goroutine).
func (c *attemptClips) feed(s []int16) {
	for _, v := range s {
		c.buf[c.pos] = v
		c.pos++
		if c.pos == len(c.buf) {
			c.pos = 0
		}
	}
	c.filled = min(c.filled+len(s), len(c.buf))
}

// snapshot returns the buffered audio in order (mic goroutine).
func (c *attemptClips) snapshot() []int16 {
	out := make([]int16, c.filled)
	start := c.pos - c.filled
	if start < 0 {
		start += len(c.buf)
	}
	n := copy(out, c.buf[start:])
	copy(out[n:], c.buf[:])
	return out
}

// arm is called when an attempt is logged (mic goroutine). It returns the
// clip's file name for the history event, or "" when clips are off. The
// write happens in the background so the mic path never waits on the disk.
func (c *attemptClips) arm(enabled bool) string {
	if !enabled || c.filled < localmic.Rate/2 {
		return ""
	}
	// seq keeps names unique within one millisecond (and sortable: the
	// millisecond prefix has a fixed width for the next 260 years)
	c.seq++
	name := fmt.Sprintf("%s%013d-%03d.wav", clipNamePrefix, time.Now().UnixMilli(), c.seq%1000)
	s := c.snapshot()
	go func() {
		if err := writeWAVInt16(filepath.Join(c.dir, name), s); err != nil {
			c.log.Warn("attempt clip", "err", err)
			return
		}
		c.prune()
	}()
	return name
}

// prune keeps the newest clipKeep clips (background goroutine).
func (c *attemptClips) prune() {
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return
	}
	var names []string
	for _, e := range entries {
		if n := e.Name(); strings.HasPrefix(n, clipNamePrefix) && strings.HasSuffix(n, ".wav") && !e.IsDir() {
			names = append(names, n)
		}
	}
	if len(names) <= clipKeep {
		return
	}
	slices.Sort(names) // Unix ms names sort chronologically
	for _, n := range names[:len(names)-clipKeep] {
		if err := os.Remove(filepath.Join(c.dir, n)); err != nil && !os.IsNotExist(err) {
			c.log.Warn("attempt clip prune", "err", err)
		}
	}
}

// path validates a clip name coming from the web UI and returns its full
// path. Only names this package generates are accepted.
func (c *attemptClips) path(name string) (string, bool) {
	if !strings.HasPrefix(name, clipNamePrefix) || !strings.HasSuffix(name, ".wav") ||
		len(name) > len(clipNamePrefix)+32 || strings.ContainsAny(name, `/\`) {
		return "", false
	}
	for _, r := range name {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r == '-' || r == '_' || r == '.') {
			return "", false
		}
	}
	p := filepath.Join(c.dir, name)
	if _, err := os.Stat(p); err != nil {
		return "", false
	}
	return p, true
}

// writeWAVInt16 writes mono 16-bit samples as a WAV file (like wavfile.go).
func writeWAVInt16(path string, s []int16) error {
	data := make([]byte, 44+len(s)*2)
	copy(data, "RIFF")
	binary.LittleEndian.PutUint32(data[4:], uint32(36+len(s)*2))
	copy(data[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(data[16:], 16)
	binary.LittleEndian.PutUint16(data[20:], 1) // PCM
	binary.LittleEndian.PutUint16(data[22:], 1) // mono
	binary.LittleEndian.PutUint32(data[24:], uint32(localmic.Rate))
	binary.LittleEndian.PutUint32(data[28:], localmic.Rate*2)
	binary.LittleEndian.PutUint16(data[32:], 2)
	binary.LittleEndian.PutUint16(data[34:], 16)
	copy(data[36:], "data")
	binary.LittleEndian.PutUint32(data[40:], uint32(len(s)*2))
	for i, v := range s {
		binary.LittleEndian.PutUint16(data[44+i*2:], uint16(v))
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}
