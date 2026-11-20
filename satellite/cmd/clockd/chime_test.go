package main

import (
	"math"
	"testing"
)

func TestShipsBells(t *testing.T) {
	for _, c := range []struct{ h, m, n int }{{0, 0, 8}, {0, 30, 1}, {1, 0, 2}, {3, 30, 7}, {4, 0, 8}, {12, 30, 1}, {23, 30, 7}} {
		if got := shipsBells(c.h, c.m); got != c.n {
			t.Errorf("%02d:%02d: %d bells, want %d", c.h, c.m, got, c.n)
		}
	}
	if hour12(0) != 12 || hour12(13) != 1 || hour12(12) != 12 {
		t.Error("hour12")
	}
}

// every chime, every hour and half hour: sound, no NaN, within level, not too long
func TestChimes(t *testing.T) {
	for _, c := range chimes {
		for h := range 24 {
			for _, m := range []int{0, 30} {
				p := c.pcm(h, m)
				var peak float64
				for _, v := range p {
					if math.IsNaN(float64(v)) {
						t.Fatalf("%s %d:%02d: NaN", c.ID, h, m)
					}
					peak = max(peak, math.Abs(float64(v)))
				}
				if peak < 0.02 || peak > 0.26 {
					t.Errorf("%s %d:%02d: peak %.3f", c.ID, h, m, peak)
				}
				if sec := float64(len(p)) / 2 / 48000; sec > 45 {
					t.Errorf("%s %d:%02d: %.0f s", c.ID, h, m, sec)
				}
			}
		}
	}
}

// Big Ben: the whole tune only at noon; 11:00 has one phrase but one strike
// less, so noon is ~3 phrases longer.
func TestBigBenNoon(t *testing.T) {
	c, _ := chimeByID("westminster")
	noon, eleven, midnight := len(c.pcm(12, 0)), len(c.pcm(11, 0)), len(c.pcm(0, 0))
	if sec := float64(noon-eleven) / 2 / 48000; sec < 6 {
		t.Errorf("noon only %.1f s longer than 11:00", sec)
	}
	if midnight >= noon {
		t.Errorf("midnight plays the full tune too")
	}
}
