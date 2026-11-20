package main

// Leftover echo logging: after each sound the clock plays, one log line with
// how loud the microphone was while the clock played (its own sound, as the
// canceller sees it) and in the second after it stopped (the leftover echo
// and the room tail). Purpose: watch how much echo survives the canceller in
// daily use, per sound, without running the offline comparison — the numbers
// that decided the music-vs-wake work (see the notes, 2026-11-04). The mic
// signal here is post-gain, i.e. what the wake word detector hears. Mic
// goroutine only.

import (
	"math"
	"time"

	"lenovo-satellite/internal/localmic"
)

const (
	echoTailS  = 1 * time.Second        // measured after the sound ends
	echoMinDur = 300 * time.Millisecond // shorter sounds are not logged
)

type echoMeter struct {
	on     bool          // the clock is playing
	sumSq  float64       // energy while playing
	n      int64         // samples while playing
	dur    time.Duration // how long the sound played
	tail   time.Duration // remaining tail window; 0 = none pending
	tailSq float64
	tailN  int64
}

type echoResult struct {
	DuringDB float64 // mic RMS (dBFS) while the clock played
	TailDB   float64 // mic RMS (dBFS) in the second after it stopped
	Seconds  float64 // how long the sound played
}

// update processes one mic packet. sounding = the clock plays sound right
// now. ok=true exactly once per sound: when it ended, lasted at least
// echoMinDur and its tail window completed — log the result then.
func (e *echoMeter) update(sounding bool, s []int16) (echoResult, bool) {
	var sq float64
	for _, v := range s {
		sq += float64(v) * float64(v)
	}
	dt := time.Duration(len(s)) * time.Second / localmic.Rate
	if sounding {
		if !e.on {
			// a new sound: start clean (a pending tail from the previous
			// one is abandoned — no line for it)
			e.on, e.sumSq, e.n, e.dur, e.tail = true, 0, 0, 0, 0
		}
		e.sumSq += sq
		e.n += int64(len(s))
		e.dur += dt
		return echoResult{}, false
	}
	if e.on {
		e.on = false
		e.tail, e.tailSq, e.tailN = echoTailS, 0, 0
	}
	if e.tail <= 0 {
		return echoResult{}, false
	}
	e.tailSq += sq
	e.tailN += int64(len(s))
	e.tail -= dt
	if e.tail > 0 {
		return echoResult{}, false
	}
	res := echoResult{}
	if e.dur >= echoMinDur && e.n > 0 {
		res = echoResult{
			DuringDB: dbfs(math.Sqrt(e.sumSq/float64(e.n)) / 32768), // dbfs wants 0..1
			TailDB:   dbfs(math.Sqrt(e.tailSq/float64(max(e.tailN, 1))) / 32768),
			Seconds:  e.dur.Seconds(),
		}
	}
	e.sumSq, e.n, e.dur = 0, 0, 0
	return res, res.Seconds > 0
}
