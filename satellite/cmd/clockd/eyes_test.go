package main

import (
	"math/rand"
	"testing"
	"time"
)

func countEyes(g [4]byte) int {
	n := 0
	for _, b := range g {
		if b != 0 {
			n++
		}
	}
	return n
}

func TestEyesListening(t *testing.T) {
	e := newEyes()
	e.rng = rand.New(rand.NewSource(1)) // deterministic run
	t0 := time.Now()
	if _, _, ok := e.frame(moodNone, t0); ok {
		t.Fatal("eyes without a mood")
	}
	g, _, ok := e.frame(moodListening, t0)
	if !ok || g != [4]byte{0, eyeDown, eyeDown, 0} {
		t.Fatalf("wake pop: %x", g)
	}
	seen := map[[4]byte]bool{}
	firstChange := time.Duration(-1)
	for ms := 300; ms < 180000; ms += 40 {
		at := time.Duration(ms) * time.Millisecond
		g, colon, _ := e.frame(moodListening, t0.Add(at))
		seen[g] = true
		if colon {
			t.Fatal("mouth while listening")
		}
		if countEyes(g) != 2 {
			t.Fatalf("not two eyes: %x", g)
		}
		if firstChange < 0 && g != [4]byte{0, eyeOpen, eyeOpen, 0} {
			firstChange = at
		}
	}
	if firstChange > 2*time.Second {
		t.Errorf("eyes still for %v after waking", firstChange)
	}
	for name, want := range map[string][4]byte{
		"centred": {0, eyeOpen, eyeOpen, 0}, "blink": {0, eyeShut, eyeShut, 0},
		"left": {eyeOpen, eyeOpen, 0, 0}, "right": {0, 0, eyeOpen, eyeOpen},
		"up": {0, eyeUp, eyeUp, 0}, "wide": {eyeOpen, 0, 0, eyeOpen}, "happy": {0, eyeHappy, eyeHappy, 0},
	} {
		if !seen[want] {
			t.Errorf("never showed %s (%x) in 3 min", name, want)
		}
	}
}

func TestEyesThinkingAndSpeaking(t *testing.T) {
	e := newEyes()
	e.rng = rand.New(rand.NewSource(1))
	t0 := time.Now()
	pos := map[int]bool{}
	for ms := 0; ms < 5000; ms += 40 {
		g, _, ok := e.frame(moodThinking, t0.Add(time.Duration(ms)*time.Millisecond))
		if !ok || countEyes(g) != 2 {
			t.Fatalf("thinking: %x", g)
		}
		for i, b := range g {
			if b == eyeUp {
				pos[i] = true
			}
		}
	}
	if len(pos) < 3 {
		t.Errorf("thinking eyes don't drift: %v", pos)
	}
	mouth := map[bool]bool{}
	for ms := 0; ms < 3000; ms += 40 {
		_, colon, _ := e.frame(moodSpeaking, t0.Add(time.Duration(ms)*time.Millisecond))
		mouth[colon] = true
	}
	if !mouth[true] || !mouth[false] {
		t.Error("speaking mouth doesn't move")
	}
}
