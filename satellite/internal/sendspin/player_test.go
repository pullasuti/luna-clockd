package sendspin

import (
	"strings"
	"testing"
	"time"

	"github.com/Sendspin/sendspin-go/pkg/protocol"
)

func ramp(n int, v float32) []float32 {
	s := make([]float32, n*channels)
	for i := range s {
		s[i] = v
	}
	return s
}

func TestSchedSourceTiming(t *testing.T) {
	now := time.Now()
	s := &schedSource{gain: 1, now: func() time.Time { return now }}
	// a 20 ms chunk due 5 ms from now
	s.push(now.Add(5*time.Millisecond), ramp(rate/50, 0.5))
	dst := make([]float32, (rate/100)*channels) // 10 ms
	s.Read(dst)
	silent := 0
	for i := 0; i < len(dst); i += channels {
		if dst[i] != 0 {
			break
		}
		silent++
	}
	// ~5 ms (240 frames) of silence before the chunk starts; allow scheduling slack
	if silent != 240 {
		t.Fatalf("silence before the chunk: %d frames, want ~240", silent)
	}
	if dst[len(dst)-1] != 0.5 {
		t.Fatal("chunk audio missing at the end of the first read")
	}
}

func TestSchedSourceDropsLateAndContinues(t *testing.T) {
	now := time.Now()
	s := &schedSource{gain: 0.5, now: func() time.Time { return now }}
	s.push(now.Add(-time.Second), ramp(rate/50, 1))        // long past: dropped
	s.push(now.Add(-5*time.Millisecond), ramp(rate/50, 1)) // half played already
	dst := make([]float32, (rate/100)*channels)
	s.Read(dst)
	if s.late != 1 {
		t.Fatalf("late chunks %d, want 1", s.late)
	}
	if dst[0] != 0.5 {
		t.Fatalf("expected gain-scaled audio at once, got %v", dst[0])
	}
	// 10 ms later: the remaining 5 ms plays, then silence
	now = now.Add(10 * time.Millisecond)
	s.Read(dst)
	if dst[0] != 0.5 || dst[len(dst)-1] != 0 {
		t.Fatalf("continuation: first %v last %v", dst[0], dst[len(dst)-1])
	}
}

func TestPCM16(t *testing.T) {
	got := pcm16([]byte{0x00, 0x80, 0xff, 0x7f, 0, 0})
	if got[0] != -1 || got[1] < 0.9999 || got[2] != 0 {
		t.Fatalf("%v", got)
	}
}

// The server sends ~5.4 s ahead (1 MB buffer_capacity of PCM); the chunk due
// next must survive until it plays.
func TestQueueKeepsServerLead(t *testing.T) {
	now := time.Now()
	s := &schedSource{gain: 1, now: func() time.Time { return now }}
	for i := 0; i < 300; i++ { // 6 s of 20 ms chunks, the first due in 10 ms
		s.push(now.Add(10*time.Millisecond+time.Duration(i)*20*time.Millisecond), ramp(rate/50, 0.25))
	}
	dst := make([]float32, (rate/50)*channels)
	s.Read(dst)
	if dst[len(dst)-1] != 0.25 {
		t.Fatal("the next chunk was dropped from the queue")
	}
}

// With a few ms of jitter in when Read is called, a continuous stream must come
// out continuous: every sample exactly once, in order.
func TestContinuousUnderJitter(t *testing.T) {
	now := time.Now()
	s := &schedSource{gain: 1, now: func() time.Time { return now }}
	n := 0
	for i := 0; i < 100; i++ { // 2 s of 20 ms chunks with a sample counter
		c := make([]float32, (rate/50)*channels)
		for k := 0; k < len(c); k += channels {
			c[k], c[k+1] = float32(n), float32(n)
			n++
		}
		s.push(now.Add(time.Duration(i)*20*time.Millisecond), c)
	}
	dst := make([]float32, (rate/100)*channels) // 10 ms reads
	jitter := []time.Duration{0, 3 * time.Millisecond, -2 * time.Millisecond, 4 * time.Millisecond, -3 * time.Millisecond}
	want := float32(0)
	base := now
	for r := 0; r < 150; r++ {
		now = base.Add(time.Duration(r)*10*time.Millisecond + jitter[r%len(jitter)])
		s.Read(dst)
		for k := 0; k < len(dst); k += channels {
			if r == 0 && k == 0 {
				want = dst[0] // start wherever the first read lands
			}
			if dst[k] != want {
				t.Fatalf("read %d frame %d: got sample %v, want %v (skip or repeat)", r, k/channels, dst[k], want)
			}
			want++
		}
	}
}

// Time stamps come from the other end: chunks that all claim the same far-off
// moment are never due and never out of the time window, so size bounds them.
func TestQueueBoundedBySize(t *testing.T) {
	now := time.Now()
	s := &schedSource{gain: 1, now: func() time.Time { return now }}
	at := now.Add(24 * time.Hour)
	for i := 0; i < 200; i++ { // 200 s of audio, all "due" at the same moment
		s.push(at, ramp(rate, 0.25))
	}
	n := 0
	for _, c := range s.q {
		n += len(c.s)
	}
	if n > frames(maxQueue)*channels {
		t.Fatalf("queue holds %d samples, limit %d", n, frames(maxQueue)*channels)
	}
}

func TestNowPlayingCapped(t *testing.T) {
	long := strings.Repeat("ä", 5000)
	np := nowPlaying(&protocol.MetadataState{Title: &long})
	if len([]rune(np)) != maxNowPlaying {
		t.Fatalf("%d characters", len([]rune(np)))
	}
}
