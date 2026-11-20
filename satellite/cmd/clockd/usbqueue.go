package main

import (
	"math/rand/v2"
	"path"
	"strings"
	"time"
)

// usbQueue is a folder of audio files played through: when a file ends on its
// own, the next one in the play order starts (a stop, a skip or any new media
// replaces the whole queue). order is the play order (name order, or shuffled
// with the current track first); oi points at the current entry.
type usbQueue struct {
	tracks []usbTrack
	order  []int // indices into tracks
	oi     int   // position in order
	repeat repeatMode
}

// usbTrack is one file of the queue; dur and name come from its header
// (0 / "" when unknown — the transport line then shows elapsed only and the
// file name).
type usbTrack struct {
	path string
	dur  time.Duration
	name string // "Artist - Title" from the tags, if any
}

// repeatMode is the behaviour past the last track.
type repeatMode string

const (
	repOff repeatMode = "off" // stop after the folder (the default)
	repAll repeatMode = "all" // wrap to the first track
	repOne repeatMode = "one" // replay the current track
)

func parseRepeat(s string) (repeatMode, bool) {
	switch s {
	case "off":
		return repOff, true
	case "all":
		return repAll, true
	case "one":
		return repOne, true
	}
	return repOff, false
}

// idx is the current track's index in tracks.
func (q *usbQueue) idx() int { return q.order[q.oi] }

// track is the current track.
func (q *usbQueue) track() usbTrack { return q.tracks[q.idx()] }

// nextOrder returns the ⏭ target: the next entry in the play order, wrapping
// in repeat-all; ok=false means past the end and playback stops.
func (q *usbQueue) nextOrder() (int, bool) {
	if i := q.oi + 1; i < len(q.order) {
		return i, true
	}
	if q.repeat == repAll && len(q.order) > 0 {
		return 0, true
	}
	return q.oi, false
}

// endOrder is where a track that ended on its own goes: itself in repeat-one,
// otherwise the same as ⏭.
func (q *usbQueue) endOrder() (int, bool) {
	if q.repeat == repOne {
		return q.oi, true
	}
	return q.nextOrder()
}

// prevOrder returns the ⏮ target: a track heard for over restartAfter restarts
// from its beginning, a fresh one goes back an entry (wrapping in repeat-all,
// otherwise clamping at the start, where it restarts).
func (q *usbQueue) prevOrder(elapsed, restartAfter time.Duration) int {
	if elapsed > restartAfter {
		return q.oi // restart the current track
	}
	if q.oi > 0 {
		return q.oi - 1
	}
	if q.repeat == repAll && len(q.order) > 1 {
		return len(q.order) - 1
	}
	return 0
}

// usbRestartAfter is how long a track must play before ⏮ restarts it instead
// of going back a file.
const usbRestartAfter = 5 * time.Second

// setShuffle rebuilds the play order: on, the current track first and the rest
// scattered; off, name order around the current track. perm returns the
// scattered order of the tracks that are not playing (shuffledOrder in the
// daemon, fixed in tests).
func (q *usbQueue) setShuffle(on bool, perm func(n int) []int) {
	cur := q.idx()
	if !on {
		q.order = nameOrder(len(q.tracks))
		q.oi = cur
		return
	}
	rest := make([]int, 0, len(q.tracks)-1)
	for i := range q.tracks {
		if i != cur {
			rest = append(rest, i)
		}
	}
	scatter := perm(len(rest))
	q.order = make([]int, 0, len(q.tracks))
	q.order = append(q.order, cur)
	for _, p := range scatter {
		q.order = append(q.order, rest[p])
	}
	q.oi = 0
}

// nameOrder is 0..n-1.
func nameOrder(n int) []int {
	o := make([]int, n)
	for i := range o {
		o[i] = i
	}
	return o
}

// shuffledOrder returns a random permutation of 0..n-1.
func shuffledOrder(n int) []int {
	o := nameOrder(n)
	rand.Shuffle(len(o), func(i, j int) { o[i], o[j] = o[j], o[i] })
	return o
}

// queueFromList picks the audio files out of a directory listing (in the
// order Files() sorted them) and finds where chosen sits in it. A chosen file
// missing from the listing still plays, alone.
func queueFromList(entries []ent, chosen string) *usbQueue {
	var tracks []usbTrack
	for _, e := range entries {
		if !e.Dir && !e.Link && audioExt(e.Name) {
			tracks = append(tracks, usbTrack{path: e.Path})
		}
	}
	q := &usbQueue{tracks: tracks, order: nameOrder(len(tracks))}
	for i, t := range tracks {
		if t.path == chosen {
			q.oi = i
			return q
		}
	}
	q.tracks, q.order, q.oi = []usbTrack{{path: chosen}}, []int{0}, 0
	return q
}

// audioExt is the USB player's file whitelist.
func audioExt(name string) bool {
	switch strings.ToLower(path.Ext(name)) {
	case ".flac", ".wav", ".mp3":
		return true
	}
	return false
}
