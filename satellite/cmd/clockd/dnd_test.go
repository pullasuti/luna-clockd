package main

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"lenovo-satellite/internal/config"
)

func TestDNDSchedule(t *testing.T) {
	d := &daemon{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	c := newDNDCtl(d)
	at := func(h, m int) time.Time { return time.Date(2026, 11, 21, h, m, 0, 0, time.Local) }
	times := config.DND{Schedule: "times", Start: "22:00", End: "07:00"}
	for _, x := range []struct {
		h, m int
		want bool
	}{{21, 59, false}, {22, 0, true}, {3, 0, true}, {6, 59, true}, {7, 0, false}, {12, 0, false}} {
		if got := c.scheduled(times, at(x.h, x.m)); got != x.want {
			t.Errorf("%02d:%02d: %v, want %v", x.h, x.m, got, x.want)
		}
	}
	if c.scheduled(config.DND{Schedule: "none"}, at(23, 0)) || c.scheduled(config.DND{Schedule: "night"}, at(23, 0)) {
		t.Error("none / night (no night controller) scheduled")
	}
	var nilCtl *dndCtl
	if nilCtl.On() {
		t.Error("a nil controller is on")
	}
}
