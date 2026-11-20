package main

import (
	"os"
	"testing"
	"time"

	"lenovo-satellite/internal/localmic"
)

func TestAttemptClipsRing(t *testing.T) {
	c := newAttemptClips(nil, t.TempDir())
	// fill more than the ring holds: only the newest clipSeconds survive
	n := localmic.Rate*clipSeconds + localmic.Rate/2
	s := make([]int16, 160) // 10 ms packets
	for i := 0; i < n/len(s); i++ {
		for j := range s {
			s[j] = int16(i*len(s) + j)
		}
		c.feed(s)
	}
	got := c.snapshot()
	if len(got) != localmic.Rate*clipSeconds {
		t.Fatalf("snapshot %d samples, want %d", len(got), localmic.Rate*clipSeconds)
	}
	// the ring wraps: newest last, oldest exactly the first sample still
	// held, all consecutive (the wrap copy went to the right place)
	if got[len(got)-1] != int16(n-1) {
		t.Fatalf("newest sample %d, want %d", got[len(got)-1], n-1)
	}
	if got[0] != int16(n-len(got)) {
		t.Fatalf("oldest sample %d, want %d", got[0], n-len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i] != got[i-1]+1 {
			t.Fatalf("not consecutive at %d: %d after %d", i, got[i], got[i-1])
		}
	}
}

func TestAttemptClipsArmDisabledAndEmpty(t *testing.T) {
	c := newAttemptClips(nil, t.TempDir())
	if name := c.arm(false); name != "" {
		t.Fatalf("arm(false) = %q, want empty", name)
	}
	c.feed(make([]int16, 160))
	if name := c.arm(true); name != "" {
		t.Fatalf("arm before half a second of audio = %q, want empty", name)
	}
}

func TestAttemptClipsArmWritesAndPrunes(t *testing.T) {
	dir := t.TempDir()
	c := newAttemptClips(nil, dir)
	packet := make([]int16, 160)
	for i := range packet {
		packet[i] = 1000
	}
	for i := 0; i < localmic.Rate/len(packet); i++ { // 1 s of audio
		c.feed(packet)
	}
	total := clipKeep + 5
	names := make([]string, total)
	for i := range names {
		names[i] = c.arm(true)
		if names[i] == "" {
			t.Fatalf("arm %d returned no name", i)
		}
	}
	if names[0] == names[total-1] {
		t.Fatalf("names not unique: %q twice", names[0])
	}
	// arm writes in the background; wait for all of them
	deadline := time.Now().Add(5 * time.Second)
	for {
		entries, _ := os.ReadDir(dir)
		if len(entries) >= total || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	c.prune() // deterministic here; the writer goroutine also calls it
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != clipKeep {
		t.Fatalf("%d clips kept, want %d", len(entries), clipKeep)
	}
	// the newest clip survives and is a valid WAV of the right length
	p, ok := c.path(names[total-1])
	if !ok {
		t.Fatalf("newest clip %q not found", names[total-1])
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) != 44+len(c.snapshot())*2 || string(b[:4]) != "RIFF" {
		t.Fatalf("clip is not a WAV of the right size: %d bytes", len(b))
	}
	// the oldest was pruned and is no longer served
	if _, ok := c.path(names[0]); ok {
		t.Fatalf("pruned clip %q still served", names[0])
	}
	// path validation: nothing outside the clips dir
	for _, bad := range []string{"", "attempt-1.wav/../../etc/passwd", "../config.json",
		"attempt-a.wav", "attempt--1.WAV", "attempt-123.wav.exe", "attempt-..-.wav"} {
		if _, ok := c.path(bad); ok {
			t.Fatalf("path(%q) accepted", bad)
		}
	}
}
