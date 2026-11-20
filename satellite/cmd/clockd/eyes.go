package main

import (
	"math/rand"
	"time"
)

// Animated "eyes" on the four big digits while the assistant is active.
// Each eye is one digit; the pair moves across the digits to glance. The
// colon between digits 2 and 3 doubles as a mouth while speaking.
//
// Glyph bits: a=0 b=1 c=2 d=3 e=4 f=5 g=6.
const (
	eyeOpen  byte = 0x3f // a b c d e f      "0"
	eyeUp    byte = 0x63 // a b f g          small upper ring
	eyeDown  byte = 0x5c // c d e g          small lower ring
	eyeShut  byte = 0x40 // g                "-"
	eyeHappy byte = 0x23 // a b f            arc
)

// Assistant moods the face can show.
const (
	moodNone = iota
	moodListening
	moodThinking
	moodSpeaking
)

// gesture is a short expression played over the resting eyes.
type gesture struct {
	left, right byte // glyphs (0 = resting glyph)
	lpos, rpos  int  // digit positions of the eyes
	until       time.Time
}

type eyes struct {
	rng  *rand.Rand
	mood int
	t0   time.Time // when the current mood started

	nextBlink, blinkEnd time.Time
	nextGesture         time.Time
	g                   *gesture
	mouthUntil          time.Time
	mouthOn             bool
}

func newEyes() *eyes { return &eyes{rng: rand.New(rand.NewSource(time.Now().UnixNano()))} }

func (e *eyes) dur(lo, hi time.Duration) time.Duration {
	return lo + time.Duration(e.rng.Int63n(int64(hi-lo)+1))
}

// pickGesture chooses a listening expression (weights sum to 100).
func (e *eyes) pickGesture(now time.Time) *gesture {
	r := e.rng.Intn(100)
	g := &gesture{lpos: 1, rpos: 2}
	switch {
	case r < 35: // glance left or right
		if e.rng.Intn(2) == 0 {
			g.lpos, g.rpos = 0, 1
		} else {
			g.lpos, g.rpos = 2, 3
		}
		g.until = now.Add(e.dur(400*time.Millisecond, 900*time.Millisecond))
	case r < 50: // look up
		g.left, g.right = eyeUp, eyeUp
		g.until = now.Add(e.dur(400*time.Millisecond, 700*time.Millisecond))
	case r < 60: // look down
		g.left, g.right = eyeDown, eyeDown
		g.until = now.Add(e.dur(300*time.Millisecond, 600*time.Millisecond))
	case r < 72: // wink
		if e.rng.Intn(2) == 0 {
			g.left = eyeShut
		} else {
			g.right = eyeShut
		}
		g.until = now.Add(250 * time.Millisecond)
	case r < 82: // wide, surprised
		g.lpos, g.rpos = 0, 3
		g.until = now.Add(e.dur(300*time.Millisecond, 500*time.Millisecond))
	default: // happy squint
		g.left, g.right = eyeHappy, eyeHappy
		g.until = now.Add(e.dur(500*time.Millisecond, 900*time.Millisecond))
	}
	return g
}

// frame returns the big-digit glyphs and the colon ("mouth") for mood at
// now; ok=false means no eyes.
func (e *eyes) frame(mood int, now time.Time) (g [4]byte, colon, ok bool) {
	if mood == moodNone {
		e.mood = moodNone
		return g, false, false
	}
	if mood != e.mood {
		e.mood, e.t0 = mood, now
		// something happens soon after a mood starts, so the face never looks frozen
		e.nextBlink = now.Add(e.dur(600*time.Millisecond, 1400*time.Millisecond))
		e.nextGesture = now.Add(e.dur(900*time.Millisecond, 1800*time.Millisecond))
		e.blinkEnd, e.g = time.Time{}, nil
	}
	t := now.Sub(e.t0)

	// blinks: slightly irregular so it doesn't look mechanical
	if !now.Before(e.nextBlink) {
		e.blinkEnd = now.Add(130 * time.Millisecond)
		e.nextBlink = now.Add(e.dur(2*time.Second, 5*time.Second))
		if e.rng.Intn(6) == 0 { // now and then a double blink
			e.nextBlink = now.Add(300 * time.Millisecond)
		}
	}
	blink := now.Before(e.blinkEnd)

	lpos, rpos := 1, 2
	left, right := eyeOpen, eyeOpen

	switch mood {
	case moodListening:
		if t < 250*time.Millisecond { // wake "pop": small eyes opening wide
			left, right = eyeDown, eyeDown
			blink = false
			break
		}
		if e.g != nil && !now.Before(e.g.until) {
			e.g = nil
			e.nextGesture = now.Add(e.dur(1500*time.Millisecond, 4*time.Second))
		}
		if e.g == nil && !now.Before(e.nextGesture) {
			e.g = e.pickGesture(now)
		}
		if e.g != nil {
			lpos, rpos = e.g.lpos, e.g.rpos
			if e.g.left != 0 {
				left = e.g.left
			}
			if e.g.right != 0 {
				right = e.g.right
			}
			if e.g.left == eyeShut || e.g.right == eyeShut {
				blink = false // a wink is its own blink
			}
		}
	case moodThinking:
		// looking up, drifting slowly across the digits and back
		left, right = eyeUp, eyeUp
		const period = 2400 * time.Millisecond
		ph := float64(t%period) / float64(period) // 0..1
		switch {
		case ph < 0.25:
			lpos = 1
		case ph < 0.5:
			lpos = 2
		case ph < 0.75:
			lpos = 1
		default:
			lpos = 0
		}
		rpos = lpos + 1
	case moodSpeaking:
		// centred eyes, the colon flickers like a talking mouth
		if !now.Before(e.mouthUntil) {
			e.mouthOn = !e.mouthOn
			if e.mouthOn {
				e.mouthUntil = now.Add(e.dur(90*time.Millisecond, 220*time.Millisecond))
			} else {
				e.mouthUntil = now.Add(e.dur(60*time.Millisecond, 250*time.Millisecond))
			}
		}
		colon = e.mouthOn
	}

	if blink {
		left, right = eyeShut, eyeShut
	}
	g[lpos], g[rpos] = left, right
	return g, colon, true
}

// mood maps the satellite state to an eyes mood.
func (d *daemon) mood() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	switch {
	case d.st == stListening:
		return moodListening
	case d.st == stResponding && !d.speaking:
		return moodThinking
	case d.st == stResponding:
		return moodSpeaking
	}
	return moodNone
}
