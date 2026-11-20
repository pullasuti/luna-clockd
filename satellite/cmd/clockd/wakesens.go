package main

import "lenovo-satellite/internal/wake"

// Wake word sensitivity (voice.wake_sensitivity), like Voice PE's setting.
// The model manifests' cutoff (0.97 for okay_nabu and hey_jarvis) missed real
// attempts with a TV on (they peaked at 0.70 and 0.93), while noise never
// scored above 0.05. "slight" keeps the model's cutoff.
var wakeSensitivity = map[string]float64{
	"slight":   1, // = the model's own cutoff
	"moderate": 0.90,
	"very":     0.80,
}

// wakeThresholdPlaying is one step easier than the sensitivity, for while the
// clock plays sound ("very" goes to 0.75). Measured 2026-10-31: at 50 % volume
// the speaker reaches the mics ~28 dB above the voice and the vendor AEC's
// suppressor dims the wake word too.
func wakeThresholdPlaying(m wake.Model, sensitivity string) float64 {
	switch sensitivity {
	case "very":
		return min(m.Cutoff, 0.75)
	case "moderate":
		return wakeThreshold(m, "very")
	}
	return wakeThreshold(m, "moderate")
}

func wakeThreshold(m wake.Model, sensitivity string) float64 {
	t, ok := wakeSensitivity[sensitivity]
	if !ok {
		t = 1
	}
	return min(m.Cutoff, t)
}

// nearMiss remembers how close a word came, so a missed "Okay Nabu" leaves a
// trace: a score that rose above the floor and fell back (below 40 % of the
// floor) without reaching the threshold. It also keeps the loudest mic level
// seen during the attempt.
type nearMiss struct {
	peak    float64
	levelDB float64
	active  bool
}

// wakeNearMissFloor: wake word scores above this count as an attempt.
const wakeNearMissFloor = 0.5

// stopNearMissFloor is relative to the stop threshold (default 0.3).
func stopNearMissFloor(threshold float64) float64 { return threshold / 2 }

// track updates the state with the latest score and mic level. It returns
// ok=true with the peak score and loudest level when a near miss has ended.
func (n *nearMiss) track(score, floor, levelDB float64) (peak, level float64, ok bool) {
	if score >= floor*0.4 {
		if !n.active {
			n.active, n.peak, n.levelDB = true, 0, -100
		}
		n.peak = max(n.peak, score)
		n.levelDB = max(n.levelDB, levelDB)
		return 0, 0, false
	}
	if n.active {
		n.active = false
		if n.peak >= floor {
			return n.peak, n.levelDB, true
		}
	}
	return 0, 0, false
}

// level is the loudest mic level of the current attempt (for detections).
func (n *nearMiss) level(now float64) float64 {
	if n.active {
		return max(n.levelDB, now)
	}
	return now
}

func (n *nearMiss) reset() { n.active, n.peak = false, 0 }

func round2(x float64) float64 { return float64(int(x*100+0.5)) / 100 }
