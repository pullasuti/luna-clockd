package main

import (
	"testing"
	"time"

	"lenovo-satellite/internal/output"
)

// now playing: the USB queue and the clock's media first, a radio playing only
// for listeners next, Music Assistant last
func TestNowPlaying(t *testing.T) {
	d := &daemon{}
	if n := d.nowPlayingLocked(false); n.Source != "" {
		t.Errorf("idle: %+v", n)
	}
	if n := d.nowPlayingLocked(true); n.Source != "music_assistant" || !n.OnClock {
		t.Errorf("sendspin: %+v", n)
	}
	d.radio.hz, d.radio.name, d.radio.listeners = 104_900_000, "NRJ", map[chan []byte]int{make(chan []byte): 0}
	if n := d.nowPlayingLocked(true); n.Source != "fm" || n.OnClock || n.Title != "NRJ" || n.Detail != "FM 104.9 MHz" || n.Listeners != 1 {
		t.Errorf("listeners only: %+v", n)
	}
	d.media, d.mediaSrc, d.radio.speaker = &output.Voice{}, "fm", true
	if n := d.nowPlayingLocked(false); n.Source != "fm" || !n.OnClock {
		t.Errorf("on the speaker: %+v", n)
	}
	d.mediaSrc, d.mediaName, d.radio.hz = "sleep", "Sleep sound: Rain", 0
	if n := d.nowPlayingLocked(false); n.Source != "sleep" || n.Title != "Rain" {
		t.Errorf("sleep sound: %+v", n)
	}
	q := &usbQueue{tracks: []usbTrack{{path: "/a/01 One.flac"}, {path: "/a/02 Two.flac", name: "Two"}}, order: []int{0, 1}}
	d.usbPaused = &usbPaused{q: q, elapsed: time.Second}
	if n := d.nowPlayingLocked(false); n.Source != "usb" || !n.Paused || n.Title != "01 One.flac" || n.Detail != "1 of 2" {
		t.Errorf("usb paused: %+v", n)
	}
}
