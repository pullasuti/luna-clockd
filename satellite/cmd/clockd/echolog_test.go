package main

import (
	"math"
	"testing"

	"lenovo-satellite/internal/localmic"
)

// packets of 160 samples = 10 ms at localmic.Rate.
func TestEchoMeter(t *testing.T) {
	var e echoMeter
	loud := make([]int16, 160)  // the clock plays: mic hears echo
	quiet := make([]int16, 160) // after it stops: just the tail
	for i := range loud {
		loud[i] = 10000 // -10.3 dBFS
	}
	for i := range quiet {
		quiet[i] = 100 // -50.3 dBFS
	}
	packets := localmic.Rate / 160

	// a 1 s sound, then silence: exactly one line, correct numbers
	for i := 0; i < packets; i++ {
		if _, ok := e.update(true, loud); ok {
			t.Fatal("line during playback")
		}
	}
	var got echoResult
	lines := 0
	for i := 0; i < packets+5; i++ {
		r, ok := e.update(false, quiet)
		if ok {
			got, lines = r, lines+1
		}
	}
	if lines != 1 {
		t.Fatalf("%d lines after the sound, want 1", lines)
	}
	if d := math.Abs(got.DuringDB - dbfs(10000.0/32768)); d > 0.01 {
		t.Errorf("during %v dBFS, want %v", got.DuringDB, dbfs(10000.0/32768))
	}
	if d := math.Abs(got.TailDB - dbfs(100.0/32768)); d > 0.01 {
		t.Errorf("tail %v dBFS, want %v", got.TailDB, dbfs(100.0/32768))
	}
	if got.Seconds < 0.99 || got.Seconds > 1.01 {
		t.Errorf("seconds %v, want 1", got.Seconds)
	}
	// after the line, silence produces nothing more
	for i := 0; i < 5; i++ {
		if _, ok := e.update(false, quiet); ok {
			t.Fatal("extra line without a new sound")
		}
	}
}

func TestEchoMeterShortSoundNoLine(t *testing.T) {
	var e echoMeter
	s := make([]int16, 160)
	for i := range s {
		s[i] = 30000
	}
	for i := 0; i < 20; i++ { // 0.2 s: under echoMinDur
		e.update(true, s)
	}
	for i := 0; i < localmic.Rate/160+5; i++ {
		if _, ok := e.update(false, make([]int16, 160)); ok {
			t.Fatal("a 0.2 s sound produced a line")
		}
	}
}

func TestEchoMeterNewSoundCancelsTail(t *testing.T) {
	var e echoMeter
	s := make([]int16, 160)
	for i := range s {
		s[i] = 20000
	}
	for i := 0; i < localmic.Rate/160; i++ {
		e.update(true, s)
	}
	e.update(false, s) // tail starts
	e.update(false, s)
	// a new sound interrupts the tail: its duration counts from scratch
	for i := 0; i < localmic.Rate/160; i++ {
		if _, ok := e.update(true, s); ok {
			t.Fatal("line during the second playback")
		}
	}
	var got echoResult
	for i := 0; i < localmic.Rate/160+5; i++ {
		r, ok := e.update(false, make([]int16, 160))
		if ok {
			got = r
		}
	}
	if got.Seconds < 0.99 || got.Seconds > 1.01 {
		t.Errorf("second sound measured %v s, want 1", got.Seconds)
	}
}
