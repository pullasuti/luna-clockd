package main

import "sync"

// mediaLevel is the one owner of the music's level (the "media" group gain).
// A turn, an announcement, the player's mute and the listening-closer dip
// each ask for their own level and take it back when they are done; the
// lowest one asked for applies. They used to write the gain themselves, so
// whichever ended first put the music back for all of them (the end of a
// turn un-muted a muted player).
type mediaLevel struct {
	mu    sync.Mutex
	want  map[string]float32
	apply func(g float32) // called with mu held, so levels arrive in order
}

// who asks
const (
	lvlTurn     = "turn"
	lvlAnnounce = "announce"
	lvlMute     = "mute"
	lvlDip      = "dip"
)

const duckGain = 0.15 // music under a turn or an announcement

// set asks for level g (0-1) on behalf of who.
func (m *mediaLevel) set(who string, g float32) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.want == nil {
		m.want = map[string]float32{}
	}
	m.want[who] = g
	m.update()
}

// clear takes who's request back.
func (m *mediaLevel) clear(who string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.want, who)
	m.update()
}

func (m *mediaLevel) update() {
	g := float32(1)
	for _, w := range m.want {
		g = min(g, w)
	}
	m.apply(g)
}
