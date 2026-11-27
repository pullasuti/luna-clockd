package main

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"lenovo-satellite/internal/hw"
)

func TestNotifyLEDs(t *testing.T) {
	d := &daemon{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := d.notifyLEDs("Orange", 0, ""); err != nil {
		t.Fatal(err)
	}
	if d.ledState() != ledNotify || d.hw.notifyColour[0] != (hw.RGB{R: 0xff, G: 0x60}) {
		t.Errorf("state %d colour %v", d.ledState(), d.hw.notifyColour[0])
	}
	if left := time.Until(d.hw.notifyUntil); left < 9*time.Second || left > 10*time.Second {
		t.Errorf("0 s gave %v, want 10 s", left)
	}
	if err := d.notifyLEDs("#00ff80", 99999, "blink"); err != nil || time.Until(d.hw.notifyUntil) > time.Hour {
		t.Errorf("hex/limit: %v %v", err, time.Until(d.hw.notifyUntil))
	}
	for _, bad := range [][2]string{{"mauve", ""}, {"#12345", ""}, {"#gg0000", ""}, {"red", "strobe"}} {
		if err := d.notifyLEDs(bad[0], 5, bad[1]); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
	d.st = stListening // a voice turn shows over it
	if d.ledState() != ledListening {
		t.Error("notification over a voice turn")
	}
	d.st = stIdle
	if !d.clearLEDNotify() || d.ledState() == ledNotify {
		t.Error("clear")
	}
}

// the frame actually shown, per style (v1.6.4-1.6.14 wrote a dark one)
func TestNotifyFrame(t *testing.T) {
	d := &daemon{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	orange := hw.RGB{R: 0xff, G: 0x60}
	d.notifyLEDs("orange", 5, "solid")
	if c := d.notifyFrame(0); c != [4]hw.RGB{orange, orange, orange, orange} {
		t.Errorf("solid: %v", c)
	}
	d.notifyLEDs("orange", 5, "blink")
	if c := d.notifyFrame(100 * time.Millisecond); c[0] != orange {
		t.Errorf("blink on: %v", c)
	}
	if c := d.notifyFrame(600 * time.Millisecond); c[0] != (hw.RGB{}) {
		t.Errorf("blink off: %v", c)
	}
	d.notifyLEDs("orange", 5, "pulse")
	if c := d.notifyFrame(time.Second); c[0].R < 0xf0 { // the top of the 2 s breath
		t.Errorf("pulse peak: %v", c)
	}
}
