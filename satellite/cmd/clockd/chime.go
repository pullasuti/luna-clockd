package main

import (
	"context"
	"time"

	esphome "github.com/ygelfand/go-esphome-device"

	"lenovo-satellite/internal/config"
	"lenovo-satellite/internal/output"
)

// Hourly chimes: local, made on the clock like the startup tunes. Most are
// low-key (one soft bell, two notes, a wooden knock); the counting ones and
// Big Ben are there for those who want them. Quiet time is the night mode or
// own times; chimes are skipped while an alarm, a timer or the assistant
// sounds, and (by default) while media plays.

type chime struct {
	ID, Name, About string
	// pcm is the chime for hour h (0-23) and minute m (0 or 30).
	pcm func(h, m int) []float32
}

// chimeDB is the peak level of a chime before the chime volume: as the
// startup tunes.
const chimeDB = -12

// count returns n strikes of note f, every gap ms.
func strikes(f string, n, gap int) [][2]float64 {
	var out [][2]float64
	for range n {
		out = append(out, [2]float64{nf(f), float64(gap)})
	}
	return out
}

func hour12(h int) int {
	if h %= 12; h == 0 {
		return 12
	}
	return h
}

// shipsBells counts the half-hours of a four-hour watch: 00:30 one bell,
// 04:00 eight bells, struck in pairs.
func shipsBells(h, m int) int {
	n := (h%4)*2 + m/30
	if n == 0 {
		return 8
	}
	return n
}

func scaled(p []float32, g float32) []float32 {
	for i := range p {
		p[i] *= g
	}
	return p
}

// Westminster Quarters: the five phrases (Wikipedia, "Westminster Quarters").
var wq = [5][]string{
	{"G#4", "F#4", "E4", "B3"},
	{"E4", "G#4", "F#4", "B3"},
	{"E4", "F#4", "G#4", "E4"},
	{"G#4", "E4", "F#4", "B3"},
	{"B3", "F#4", "G#4", "E4"},
}

func westminster(phrases ...int) [][2]float64 {
	var out [][2]float64
	for _, p := range phrases {
		for i, n := range wq[p] {
			ms := 520.0
			if i == 3 {
				ms = 1300
			}
			out = append(out, [2]float64{nf(n), ms})
		}
	}
	return out
}

var chimes = []chime{
	{"soft", "Soft bell", "one gentle bell", func(h, m int) []float32 {
		p := output.Melody(chimeDB, output.SoftBell, seq("E5", 300)...)
		if m == 30 {
			return scaled(p, 0.6)
		}
		return p
	}},
	{"twonote", "Two notes", "a gentle ding-dong", func(h, m int) []float32 {
		if m == 30 {
			return scaled(output.Melody(chimeDB, output.SoftBell, seq("E5", 300)...), 0.6)
		}
		return output.Melody(chimeDB, output.SoftBell, seq("E5", 650, "C5", 300)...)
	}},
	{"knock", "Wood knock", "two soft knocks, the quietest", func(h, m int) []float32 {
		if m == 30 {
			return scaled(output.Melody(chimeDB, output.Wood, seq("A4", 200)...), 0.7)
		}
		return output.Melody(chimeDB, output.Wood, seq("A4", 260, "E4", 200)...)
	}},
	{"count", "Bell count", "a soft bell strikes the hours", func(h, m int) []float32 {
		if m == 30 {
			return scaled(output.Melody(chimeDB, output.SoftBell, seq("G4", 300)...), 0.7)
		}
		return output.Melody(chimeDB, output.SoftBell, strikes("G4", hour12(h), 1300)...)
	}},
	{"ships", "Ship's bells", "the bells of a watch, every half hour", func(h, m int) []float32 {
		var notes [][2]float64
		n := shipsBells(h, m)
		for i := range n {
			gap := 320.0
			if i%2 == 1 {
				gap = 900
			}
			notes = append(notes, [2]float64{nf("A5"), gap})
		}
		return output.Melody(chimeDB, output.SoftBell, notes...)
	}},
	{"cuckoo", "Cuckoo", "calls the hours", func(h, m int) []float32 {
		n := hour12(h)
		if m == 30 {
			n = 1
		}
		var notes [][2]float64
		for range n {
			notes = append(notes, seq("E5", 220, "C5", 480)...)
		}
		return output.Melody(chimeDB, output.Flute, notes...)
	}},
	{"westminster", "Big Ben", "full Westminster tune at noon, one phrase other hours, then the hours", func(h, m int) []float32 {
		if m == 30 {
			return output.Melody(chimeDB, output.Bell, westminster(1, 2)...)
		}
		// the whole tune only at noon (spec: "full tune only at noon, a short
		// one other hours"); other hours its last phrase
		tune := westminster(4)
		if h == 12 {
			tune = westminster(1, 2, 3, 4)
		}
		notes := append(tune, [2]float64{0, 1200})
		notes = append(notes, strikes("E3", hour12(h), 1700)...)
		return output.Melody(chimeDB, output.Bell, notes...)
	}},
}

func chimeByID(id string) (chime, bool) {
	for _, c := range chimes {
		if c.ID == id {
			return c, true
		}
	}
	return chime{}, false
}

// chimeQuiet reports whether the quiet time is on now.
func (d *daemon) chimeQuiet(c config.Chime, t time.Time) bool {
	switch c.Quiet {
	case "night":
		on, _ := d.night.State()
		return on
	case "times":
		s, err1 := parseClock(c.QuietStart)
		e, err2 := parseClock(c.QuietEnd)
		return err1 == nil && err2 == nil && inNight(t.Hour()*60+t.Minute(), s, e)
	}
	return false
}

// playChime plays chime c for h:m at the chime volume.
func (d *daemon) playChime(c chime, h, m int, vol int) {
	g := float32(min(max(vol, 10), 100)) / 100
	d.out.Play(output.NewSamples(c.pcm(h, m), 0), g, "alert")
}

// chimeLoop chimes on the hour (and half hour). It looks once a second and
// remembers the last slot, so a clock that jumps (time sync) never chimes
// twice nor late: a slot more than 20 s past is skipped.
func (d *daemon) chimeLoop(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	last := ""
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		now := d.face.tk.Now()
		if now.Year() < 2025 || now.Second() > 20 || now.Minute()%30 != 0 {
			continue
		}
		slot := now.Format("2006-01-02T15:04")
		if slot == last {
			continue
		}
		last = slot
		c := d.cfg.Get().Chime
		ch, ok := chimeByID(c.Sound)
		if !ok {
			continue
		}
		h, m := now.Hour(), now.Minute()
		why := ""
		switch {
		case m == 30 && !c.Half && ch.ID != "ships":
			continue
		case d.chimeQuiet(c, now):
			why = "quiet time"
		case d.dnd.On():
			why = "do not disturb"
		case d.out.Busy("alert") || d.out.Busy("voice"):
			why = "something else sounds"
		case c.NotWhilePlaying && d.out.Busy("media"):
			why = "media playing"
		}
		if why != "" {
			d.log.Info("chime skipped", "chime", ch.ID, "why", why)
			continue
		}
		d.log.Info("chime", "chime", ch.ID, "at", slot)
		d.playChime(ch, h, m, c.Volume)
	}
}

// chimeEntity is the "Hourly chime" dropdown in Home Assistant.
func (d *daemon) chimeEntity() *esphome.Select {
	opts := []string{offOption}
	for _, c := range chimes {
		opts = append(opts, c.Name)
	}
	sel := &esphome.Select{
		Base:    esphome.Base{ObjectID: "hourly_chime", Name: "Hourly chime", Category: esphome.CategoryConfig, Icon: "mdi:bell-outline"},
		Options: opts,
	}
	sel.OnCommand = func(v string) {
		id := ""
		for _, c := range chimes {
			if c.Name == v {
				id = c.ID
			}
		}
		if err := d.cfg.Update(func(c *config.Config) { c.Chime.Sound = id }); err != nil {
			d.log.Warn("hourly chime", "err", err)
		}
		d.syncChime()
	}
	d.chimeSel = sel
	d.syncChime()
	return sel
}

// syncChime shows the configured chime in Home Assistant.
func (d *daemon) syncChime() {
	if d.chimeSel == nil {
		return
	}
	v := offOption
	if c, ok := chimeByID(d.cfg.Get().Chime.Sound); ok {
		v = c.Name
	}
	d.chimeSel.Set(v)
}
