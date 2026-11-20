package main

import (
	"testing"

	"lenovo-satellite/internal/output"
)

func longestGapMs(pcm []float32) int {
	frames := len(pcm) / output.Channels
	longest, run := 0, 0
	for i := 0; i < 2*frames; i++ { // wraps around: the tone loops
		if pcm[(i%frames)*output.Channels] == 0 {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	return min(longest, frames) * 1000 / output.Rate
}

func TestAlarmSounds(t *testing.T) {
	for name := range alarmSounds {
		pcm := alarmTone(name)
		cycle := len(pcm) / output.Channels * 1000 / output.Rate
		total := (alarmRepeats(pcm) + 1) * cycle
		if total < alarmMs-cycle || total > alarmMs {
			t.Errorf("%s rings %d ms, want about 5 minutes", name, total)
		}
		if gap := longestGapMs(pcm); gap < 500 {
			t.Errorf("%s: longest pause %d ms, too short to say stop", name, gap)
		}
	}
	if gap := longestGapMs(alarmTone("gentle")); gap < 1600 {
		t.Fatalf("gentle pause %d ms, want >= 1600", gap)
	}
	if len(alarmTone("nonsense")) != len(alarmTone("gentle")) {
		t.Fatal("unknown names should fall back to gentle")
	}
}
