package main

import (
	"testing"
	"time"
)

func q3() *usbQueue {
	return &usbQueue{
		tracks: []usbTrack{{path: "/a"}, {path: "/b"}, {path: "/c"}},
		order:  nameOrder(3),
	}
}

func TestQueueFromList(t *testing.T) {
	entries := []ent{
		{Name: "..", Path: "/", Dir: true},
		{Name: "sub", Path: "/sub", Dir: true},
		// files in Files()' sort order (case-insensitive): apple, Apple, Beach, mid, notes, zeta
		{Name: "apple.flac", Path: "/apple.flac"},
		{Name: "Apple.wav", Path: "/Apple.wav"},
		{Name: "Beach.flac", Path: "/Beach.flac"},
		{Name: "mid.flac", Path: "/mid.flac"},
		{Name: "notes.txt", Path: "/notes.txt"},
		{Name: "zeta link", Path: "/zeta", Link: true},
	}
	want := []string{"/apple.flac", "/Apple.wav", "/Beach.flac", "/mid.flac"}
	q := queueFromList(entries, "/Beach.flac")
	if len(q.tracks) != len(want) || len(q.order) != len(want) {
		t.Fatalf("queue %v, want %v", q.tracks, want)
	}
	for i := range want {
		if q.tracks[i].path != want[i] || q.order[i] != i {
			t.Fatalf("queue %v order %v, want %v in name order", q.tracks, q.order, want)
		}
	}
	if q.idx() != 2 {
		t.Errorf("current %d, want 2", q.idx())
	}
	// a chosen file not in the listing plays alone
	q = queueFromList(entries, "/gone.flac")
	if len(q.tracks) != 1 || q.tracks[0].path != "/gone.flac" || q.idx() != 0 {
		t.Errorf("missing file: %v", q.tracks)
	}
	q = queueFromList(nil, "/gone.flac")
	if len(q.tracks) != 1 {
		t.Errorf("nil listing: %v", q.tracks)
	}
}

func TestQueuePrev(t *testing.T) {
	q := q3()
	q.oi = 1
	if got := q.prevOrder(1*time.Second, 5*time.Second); got != 0 { // fresh track: back one
		t.Errorf("prev on a fresh track: %d, want 0", got)
	}
	if got := q.prevOrder(10*time.Second, 5*time.Second); got != 1 { // 10 s in: restart current
		t.Errorf("prev 10 s into a track: %d, want 1 (restart)", got)
	}
	q.oi = 0
	if got := q.prevOrder(10*time.Second, 5*time.Second); got != 0 { // first track: restart
		t.Errorf("prev on the first track: %d, want 0", got)
	}
	q.repeat = repAll
	if got := q.prevOrder(1*time.Second, 5*time.Second); got != 2 { // fresh, repeat all: wrap
		t.Errorf("prev wrapping in repeat-all: %d, want 2", got)
	}
}

func TestQueueNextAndRepeat(t *testing.T) {
	q := q3()
	q.oi = 1
	if i, ok := q.nextOrder(); !ok || i != 2 {
		t.Errorf("next from 1: %d ok=%v, want 2", i, ok)
	}
	q.oi = 2
	if _, ok := q.nextOrder(); ok {
		t.Error("next at the last track must end playback in repeat-off")
	}
	q.repeat = repAll
	if i, ok := q.nextOrder(); !ok || i != 0 {
		t.Errorf("next at the last in repeat-all: %d ok=%v, want wrap to 0", i, ok)
	}
	q.repeat = repOne
	if i, ok := q.endOrder(); !ok || i != 2 {
		t.Errorf("auto-advance in repeat-one: %d ok=%v, want to stay on 2", i, ok)
	}
	// the ⏭ button always moves on, even in repeat-one
	q.oi = 0
	if i, ok := q.nextOrder(); !ok || i != 1 {
		t.Errorf("skip in repeat-one: %d ok=%v, want 1", i, ok)
	}
}

// identity and reverse perms for deterministic shuffle tests
func idPerm(n int) []int { return nameOrder(n) }

func revPerm(n int) []int {
	o := nameOrder(n)
	for i, j := 0, n-1; i < j; i, j = i+1, j-1 {
		o[i], o[j] = o[j], o[i]
	}
	return o
}

func TestQueueShuffle(t *testing.T) {
	q := q3()
	q.oi = 1
	// reverse the not-playing tracks: order becomes b (current), c, a
	q.setShuffle(true, revPerm)
	if q.order[0] != 1 || len(q.order) != 3 || q.oi != 0 {
		t.Fatalf("shuffled order %v at %d, want current (1) first at 0", q.order, q.oi)
	}
	if q.order[1] != 2 || q.order[2] != 0 {
		t.Errorf("shuffled order %v, want [1 2 0]", q.order)
	}
	q.oi = 2 // playing track 0 (a) through the shuffled order
	q.setShuffle(false, idPerm)
	if q.order[0] != 0 || q.order[1] != 1 || q.order[2] != 2 || q.oi != 0 {
		t.Errorf("name order restored: %v at %d", q.order, q.oi)
	}
	// shuffledOrder returns a permutation
	p := shuffledOrder(8)
	seen := map[int]bool{}
	for _, v := range p {
		if v < 0 || v >= 8 || seen[v] {
			t.Fatalf("shuffledOrder gave %v", p)
		}
		seen[v] = true
	}
}

func TestParseRepeat(t *testing.T) {
	for s, want := range map[string]repeatMode{"off": repOff, "all": repAll, "one": repOne} {
		if m, ok := parseRepeat(s); !ok || m != want {
			t.Errorf("parseRepeat(%q) = %v ok=%v", s, m, ok)
		}
	}
	if _, ok := parseRepeat("nope"); ok {
		t.Error("parseRepeat accepted garbage")
	}
}
