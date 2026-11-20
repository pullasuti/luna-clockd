package main

import "testing"

func TestMediaLevelLowestWins(t *testing.T) {
	var got float32 = -1
	m := &mediaLevel{apply: func(g float32) { got = g }}
	step := func(what string, want float32) {
		t.Helper()
		if got != want {
			t.Fatalf("%s: level %v, want %v", what, got, want)
		}
	}
	m.set(lvlMute, 0)
	m.set(lvlTurn, duckGain)
	step("turn while muted", 0)
	m.clear(lvlTurn)
	step("turn ended while muted", 0) // was 1: the turn's end un-muted the player
	m.clear(lvlMute)
	step("unmuted", 1)

	m.set(lvlTurn, duckGain)
	m.set(lvlAnnounce, duckGain)
	m.clear(lvlAnnounce)
	step("announcement ended during a turn", duckGain)
	m.set(lvlDip, dipGain)
	step("dip during a turn", dipGain)
	m.clear(lvlDip)
	m.clear(lvlTurn)
	m.clear(lvlTurn) // a turn can be ended twice (button, then the run's end)
	step("all done", 1)
}

// A run's end that arrives after its turn was ended (button, stop word,
// timeout) or after the next turn began must leave things alone.
func TestFinishRunStale(t *testing.T) {
	for _, tc := range []struct {
		name string
		st   state
		turn uint64
	}{{"turn already ended", stIdle, 3}, {"a newer turn runs", stListening, 4}} {
		d := &daemon{st: tc.st, turn: tc.turn, continueReq: true}
		d.music.apply = func(float32) { t.Errorf("%s: the turn was ended again", tc.name) }
		d.finishRun(3)
		if d.st != tc.st {
			t.Errorf("%s: state changed to %v", tc.name, d.st)
		}
	}
}
