package main

import (
	"sync"
	"time"
)

// wakeEvent is one wake word / stop word detection or near miss, kept for the
// web UI so it's easy to see whether the sensitivity fits.
type wakeEvent struct {
	At        time.Time `json:"at"`
	Kind      string    `json:"kind"` // "detected" or "near_miss"
	Word      string    `json:"word"` // phrase, e.g. "Okay Nabu" or "Stop"
	Stop      bool      `json:"stop"` // the stop word (only listened for while something plays)
	Score     float64   `json:"score"`
	Threshold float64   `json:"threshold"`
	LevelDB   float64   `json:"level_db"`          // loudest mic level (dBFS) during the attempt
	Playing   bool      `json:"playing,omitempty"` // the clock was playing sound (wake word only)
	Test      bool      `json:"test,omitempty"`    // during a wake word test run: no turn was started
	Clip      string    `json:"clip,omitempty"`    // attempt clip file name (clips.go), playable in the UI
}

const wakeHistoryMax = 50

type wakeHistory struct {
	mu sync.Mutex
	ev []wakeEvent // oldest first
}

func (h *wakeHistory) add(e wakeEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ev = append(h.ev, e)
	if len(h.ev) > wakeHistoryMax {
		h.ev = append([]wakeEvent(nil), h.ev[len(h.ev)-wakeHistoryMax:]...)
	}
}

// list returns the events, newest first.
func (h *wakeHistory) list() []wakeEvent {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]wakeEvent, len(h.ev))
	for i, e := range h.ev {
		out[len(h.ev)-1-i] = e
	}
	return out
}
