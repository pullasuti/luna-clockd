package main

import (
	"io"
	"log/slog"
	"math"
	"testing"
	"time"
)

// clearer wins; within 0.5 dB the score decides, then the id (the same
// answer on every clock)
func TestVoteClaimBetter(t *testing.T) {
	a := voteClaim{ID: "b", SNR: 20, Score: 0.9}
	if !a.better(voteClaim{ID: "a", SNR: 18, Score: 1}) {
		t.Error("2 dB clearer should win")
	}
	if !(voteClaim{ID: "b", SNR: 20.2, Score: 0.99}).better(voteClaim{ID: "a", SNR: 20, Score: 0.97}) {
		t.Error("near-equal level: higher score should win")
	}
	x, y := voteClaim{ID: "a", SNR: 20, Score: 0.97}, voteClaim{ID: "b", SNR: 20, Score: 0.97}
	if !x.better(y) || y.better(x) {
		t.Error("tie: the smaller id wins, on both sides")
	}
}

// only a clearer claim for the same phrase within the window beats ours
func TestVoteBeaten(t *testing.T) {
	w := newWakeVote("me", slog.New(slog.NewTextHandler(io.Discard, nil)), func() []string { return nil })
	now := time.Now()
	mine := voteClaim{ID: "me", Phrase: "Okay Nabu", SNR: 15, Score: 0.97}
	w.add(voteClaim{ID: "old", Phrase: "Okay Nabu", SNR: 30}, now.Add(-2*time.Second)) // too old
	w.add(voteClaim{ID: "other", Phrase: "Hey Jarvis", SNR: 30}, now)                  // other word
	w.add(voteClaim{ID: "worse", Phrase: "Okay Nabu", SNR: 10}, now)
	if c, lost := w.beaten(mine, now.Add(-voteBefore)); lost {
		t.Fatalf("lost to %+v", c)
	}
	w.add(voteClaim{ID: "near", Phrase: "Okay Nabu", SNR: 22}, now.Add(-500*time.Millisecond)) // before our detection
	w.add(voteClaim{ID: "nearer", Phrase: "Okay Nabu", SNR: 25}, now.Add(100*time.Millisecond))
	c, lost := w.beaten(mine, now.Add(-voteBefore))
	if !lost || c.ID != "nearer" {
		t.Fatalf("got %+v %v, want lost to nearer", c, lost)
	}
}

// a clock alone (no peers, or no socket) answers at once
func TestVoteAlone(t *testing.T) {
	w := newWakeVote("me", slog.New(slog.NewTextHandler(io.Discard, nil)), func() []string { return []string{"192.0.2.7"} })
	start := time.Now()
	if !w.decide("Okay Nabu", 10, 0.97) || time.Since(start) > 50*time.Millisecond {
		t.Error("without a socket the clock should answer at once")
	}
}

// quiet room then a word 20 dB louder: about 20 dB
func TestWakeLevelSNR(t *testing.T) {
	var l wakeLevel
	if l.snr() != 0 {
		t.Error("nothing heard yet should be 0")
	}
	tone := func(amp float64, n int) []int16 {
		s := make([]int16, n)
		for i := range s {
			s[i] = int16(amp * math.Sin(float64(i)*0.3))
		}
		return s
	}
	l.feed(tone(100, 16000*3))     // 3 s of room
	l.feed(tone(1000, 16000*7/10)) // 0.7 s of wake word, 10x the amplitude
	if got := l.snr(); math.Abs(got-20) > 1.5 {
		t.Errorf("snr %.1f dB, want about 20", got)
	}
}
