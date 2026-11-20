package main

import (
	"context"
	"math"
	"strconv"
	"time"

	"lenovo-satellite/internal/output"
)

// Startup tunes: a few seconds of a recognisable, public-domain melody (or our
// own), synthesised by output.Melody, so no recordings are involved.
type tune struct {
	ID, Name, About string
	Timbre          output.Timbre
	Notes           [][2]float64
}

// note frequency from a name like "C5", "F#4", "Eb5"; "-" is a rest.
func nf(s string) float64 {
	if s == "-" {
		return 0
	}
	semi := map[byte]int{'C': -9, 'D': -7, 'E': -5, 'F': -4, 'G': -2, 'A': 0, 'B': 2}[s[0]]
	i := 1
	switch s[1] {
	case '#':
		semi++
		i++
	case 'b':
		semi--
		i++
	}
	oct, _ := strconv.Atoi(s[i:])
	return 440 * math.Pow(2, float64(semi+12*(oct-4))/12)
}

// seq builds notes from "name:ms" pairs.
func seq(pairs ...any) [][2]float64 {
	var out [][2]float64
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, [2]float64{nf(pairs[i].(string)), float64(pairs[i+1].(int))})
	}
	return out
}

var tunes = []tune{
	{"westminster", "Big Ben", "Westminster Quarters, 1793", output.Bell,
		seq("G#4", 400, "E4", 400, "F#4", 400, "B3", 800, "B3", 400, "F#4", 400, "G#4", 400, "E4", 900)},
	{"zarathustra", "Also sprach Zarathustra", "Richard Strauss, 1896", output.Brass,
		seq("C4", 1150, "G4", 1150, "C5", 1350, "E5", 280, "Eb5", 1300)},
	{"morning", "Morning Mood", "Edvard Grieg, Peer Gynt, 1875", output.Legato, // even and flowing, not dotted
		seq("G5", 300, "E5", 300, "D5", 300, "C5", 300, "D5", 300, "E5", 300, "G5", 300, "E5", 300,
			"D5", 300, "C5", 300, "D5", 300, "E5", 900)},
	{"reveille", "Reveille", "bugle call", output.Brass,
		seq("G4", 140, "C5", 280, "E5", 140, "C5", 140, "G4", 140, "C5", 280, "E5", 140, "C5", 140, "G4", 140,
			"C5", 280, "E5", 140, "C5", 140, "E5", 140, "G5", 700)},
	{"fifth", "Beethoven's Fifth", "opening, 1808", output.Brass,
		seq("G4", 160, "G4", 160, "G4", 160, "Eb4", 1300)},
	{"ode", "Ode to Joy", "Beethoven, Ninth Symphony, 1824", output.Flute,
		seq("E5", 300, "E5", 300, "F5", 300, "G5", 300, "G5", 300, "F5", 300, "E5", 300, "D5", 700)},
	{"shipsbell", "Ship's bell", "two bells", output.Bell,
		seq("A5", 300, "A5", 900, "A5", 300, "A5", 1200)},
	{"cuckoo", "Cuckoo", "cuckoo clock", output.Flute,
		seq("E5", 220, "C5", 500, "E5", 220, "C5", 900)},
	{"haircut", "Shave and a haircut", "traditional", output.Flute,
		seq("C5", 400, "G4", 200, "G4", 200, "A4", 400, "G4", 400, "-", 400, "B4", 400, "C5", 700)},
	{"arpeggio", "Rising chord", "our own", output.Bell,
		seq("C5", 220, "E5", 220, "G5", 220, "C6", 1300)},
}

func tuneByID(id string) (tune, bool) {
	for _, t := range tunes {
		if t.ID == id {
			return t, true
		}
	}
	return tune{}, false
}

func (t tune) pcm() []float32 { return output.Melody(-12, t.Timbre, t.Notes...) }

// playTune plays a startup tune now (the web UI preview).
func (d *daemon) playTune(id string) bool {
	t, ok := tuneByID(id)
	if ok {
		d.out.Play(output.NewSamples(t.pcm(), 0), 1, "alert")
	}
	return ok
}

// startupTune plays the chosen tune once after the clock has powered up: not
// after a clockd restart (uptime over 10 minutes) and not during night mode.
// The clock may not know the time yet at boot, so it waits up to a minute for
// a believable date before checking the night schedule.
func (d *daemon) startupTune(ctx context.Context) {
	id := d.cfg.Get().Voice.StartupSound
	if _, ok := tuneByID(id); !ok {
		return
	}
	if f := readFields("/proc/uptime"); len(f) > 0 {
		if up, _ := strconv.ParseFloat(f[0], 64); up > 600 {
			return
		}
	}
	deadline := time.Now().Add(time.Minute)
	for time.Now().Year() < 2025 && time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
	select { // let the night schedule and the speaker settle
	case <-ctx.Done():
		return
	case <-time.After(3 * time.Second):
	}
	if night, _ := d.night.State(); night {
		d.log.Info("startup sound skipped: night mode")
		return
	}
	d.log.Info("startup sound", "tune", id)
	d.playTune(id)
}
