package main

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"lenovo-satellite/internal/config"
	"lenovo-satellite/internal/display"
)

// face drives the LED clock face: time, colon, weekday, status icons and
// short overlays (e.g. the volume level).
type face struct {
	log *slog.Logger
	cfg *config.Store
	tk  *timeKeeper
	wx  *weather
	amb *ambient
	nt  *nightCtl
	// mood reports what the assistant is doing (moodNone, moodListening, ...).
	mood func() int
	eyes *eyes
	// haDown reports whether Home Assistant has been unreachable for a while.
	haDown func() bool
	// sleepLeft: the sleep timer's time left (0 = none): minutes on the small digits
	sleepLeft   func() time.Duration
	lastLostMsg time.Time
	tick        atomic.Int64 // last loop pass (unix ns): the update health check

	mu           sync.Mutex
	overlay      string
	overlayUntil time.Time
	code         string // web login code, shown above everything else
	brightness   int    // current display brightness (1..16)
	wakeUntil    time.Time
	blank        bool // display switched off for the night
	muted        bool
	ringing      bool
	alarmSet     bool       // an alarm is on (or snoozed): the alarm icon stays lit
	av           *alarmView // setting or browsing alarms on the clock (alarmset.go)
	testUntil    time.Time  // every segment lit at full brightness (photographing the overlay)
	teaserStart  time.Time  // a luna teaser runs (teaser.go)
	teaserSeed   uint64
	teaserV      teaserTiming
	shown        display.Frame // last frame sent to the chips, for the web UI picture
	shownAt      time.Time
	queue        []*message
	cur          *message
}

// message is a text scrolled across the big digits.
type message struct {
	text    string
	strip   []byte
	repeats int
	start   time.Time
}

const maxQueued = 10

// showText queues a scrolling message (repeats >= 1).
func (f *face) showText(text string, repeats int) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	repeats = max(1, min(repeats, 20))
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.queue) >= maxQueued {
		f.queue = f.queue[1:]
	}
	f.queue = append(f.queue, &message{text: text, strip: display.Strip(text), repeats: repeats})
	f.log.Info("display message queued", "text", text, "repeats", repeats, "queued", len(f.queue))
}

// textIdle: no message is showing or queued.
func (f *face) textIdle() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cur == nil && len(f.queue) == 0
}

// clearText drops the current and queued messages.
func (f *face) clearText() {
	f.mu.Lock()
	f.queue, f.cur = nil, nil
	f.mu.Unlock()
}

// scrollWindow returns the current message view, advancing the queue.
func (f *face) scrollWindow(step time.Duration) ([4]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for {
		if f.cur == nil {
			if len(f.queue) == 0 {
				return [4]byte{}, false
			}
			f.cur, f.queue = f.queue[0], f.queue[1:]
			f.cur.start = time.Now()
		}
		n := int(time.Since(f.cur.start) / step)
		steps := display.ScrollSteps(f.cur.strip)
		if n/steps < f.cur.repeats {
			return display.Window(f.cur.strip, n%steps), true
		}
		f.cur = nil
	}
}

// flash shows text on the big digits for d, then returns to the clock.
func (f *face) flash(text string, d time.Duration) {
	f.mu.Lock()
	f.overlay, f.overlayUntil = text, time.Now().Add(d)
	f.mu.Unlock()
}

// wake shows the face for a while when it is off for the night.
func (f *face) wake(d time.Duration) {
	f.mu.Lock()
	f.wakeUntil = time.Now().Add(d)
	f.mu.Unlock()
}

// setCode shows a web login code ("" clears it).
func (f *face) setCode(code string) {
	f.mu.Lock()
	f.code = code
	f.mu.Unlock()
}

func (f *face) setMuted(m bool) {
	f.mu.Lock()
	f.muted = m
	f.mu.Unlock()
}

func (f *face) setAlarmSet(on bool) {
	f.mu.Lock()
	f.alarmSet = on
	f.mu.Unlock()
}

// alarmView is what the face shows while alarms are browsed or set on the
// clock: the big digits (blink marks the part being changed), the days on the
// weekday marks, AM/PM and the alarm icon.
type alarmView struct {
	big       string
	blink     [4]bool
	colon     bool
	days      []int // weekday marks (0 = Sunday)
	daysBlink bool
	am, pm    bool
	ampmBlink bool
	alarmIcon bool
}

// setAlarmView shows v instead of the time (nil returns to the clock).
func (f *face) setAlarmView(v *alarmView) {
	f.mu.Lock()
	f.av = v
	f.mu.Unlock()
}

func (f *face) setRinging(r bool) {
	f.mu.Lock()
	f.ringing = r
	f.mu.Unlock()
}

// testPattern lights every segment of both chips at full brightness for d
// (0 ends it): for photographing the face with all symbols visible.
func (f *face) testPattern(d time.Duration) {
	f.mu.Lock()
	f.testUntil = time.Now().Add(min(d, 10*time.Minute))
	f.mu.Unlock()
}

// snapshot returns what the display shows now; ok is false when the face is
// not running (display disabled or unavailable).
func (f *face) snapshot() (fr display.Frame, brightness int, ok bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.shown, f.brightness, time.Since(f.shownAt) < 2*time.Second
}

func (f *face) run(ctx context.Context) {
	if !f.cfg.Get().Display.Enabled {
		return
	}
	disp, err := display.Open()
	if err != nil {
		f.log.Warn("display unavailable (is lenovo_alarm still running?)", "err", err)
		return
	}
	defer disp.Close()
	f.log.Info("clock face running")
	// the startup teaser: the original first one, unless the night schedule
	// has the display off (a glitch show would light a dark bedroom). The
	// schedule is asked directly: nightCtl's state isn't set before its first
	// tick, and the light sensor has no reading yet, so "off when dark" skips too.
	if c := f.cfg.Get(); c.Display.StartupAnimation {
		skip := false
		if f.nt != nil && f.nt.scheduled(c.Night) {
			skip = c.Night.Display == "off" || c.Night.Display == "off_dark"
		}
		if !skip {
			f.playTeaser(teaserFirst)
			f.log.Info("startup teaser")
		}
	}

	brightness := -1
	var lastStep time.Time
	// the night's start and end fade the face over Night.FadeMinutes, at the
	// same pace as the night light: one brightness step every nightStep
	nightWas, nightSeen := false, false
	var nightFadeUntil time.Time
	var nightStep time.Duration
	// closing animation: when a short message ends, its letters drop out
	// left to right before the time returns
	const closeStep = 200 * time.Millisecond
	var lastOverlay [4]byte
	var hadOverlay bool
	var closeStart time.Time
	t := time.NewTicker(40 * time.Millisecond)
	defer t.Stop()
	for {
		f.tick.Store(time.Now().UnixNano())
		c := f.cfg.Get().Display
		lvl, haveLvl := 0.0, false
		if f.amb != nil {
			lvl, haveLvl = f.amb.Level()
		}
		// follow: the brightness for the room's light between lo and hi, with
		// hysteresis (stay put unless the ideal level is clearly different)
		follow := func(lo, hi int) int {
			cont := autoBrightness(lvl, lo, hi)
			if brightness < 0 || math.Abs(cont-float64(brightness)) > 0.7 {
				return int(math.Round(cont))
			}
			return brightness
		}
		target, fade := c.Brightness, c.AutoBrightness
		if c.AutoBrightness && haveLvl {
			target = follow(c.BrightnessMin, c.Brightness)
		}
		// night: the night brightness (also above the day's; with DimFollow the
		// most it gets, dimmer in a darker room) or the face off
		blank := false
		if f.nt != nil {
			active, dark := f.nt.State()
			if nightSeen && active != nightWas {
				nightFadeUntil = time.Now().Add(time.Duration(f.cfg.Get().Night.FadeMinutes) * time.Minute)
				nightStep = 0 // set once the new target is known (below)
			}
			nightWas, nightSeen = active, true
			if active {
				n := f.cfg.Get().Night
				switch n.Display {
				case "dim":
					target = n.DisplayBrightness
					if n.DimFollow && haveLvl {
						target, fade = follow(min(c.BrightnessMin, n.DisplayBrightness), n.DisplayBrightness), true
					}
				case "off":
					blank = true
				case "off_dark":
					blank = dark
				}
			}
		}
		f.mu.Lock()
		if time.Now().Before(f.wakeUntil) || f.code != "" || f.av != nil || len(f.queue) > 0 || f.cur != nil || time.Now().Before(f.overlayUntil) || !f.teaserStart.IsZero() {
			blank = false
		}
		f.blank = blank
		f.mu.Unlock()
		if target != brightness {
			switch {
			case brightness >= 0 && time.Now().Before(nightFadeUntil):
				// the night fade: steps spread evenly over what is left of it
				if nightStep == 0 {
					nightStep = time.Until(nightFadeUntil) / time.Duration(max(1, abs(target-brightness)))
				}
				if time.Since(lastStep) >= nightStep {
					if target > brightness {
						brightness++
					} else {
						brightness--
					}
					lastStep = time.Now()
				}
			case brightness < 0 || !fade:
				brightness = target // first frame / manual change: at once
			case time.Since(lastStep) >= 150*time.Millisecond:
				// auto: fade one step at a time
				if target > brightness {
					brightness++
				} else {
					brightness--
				}
				lastStep = time.Now()
			}
			disp.SetBrightness(brightness)
			f.mu.Lock()
			f.brightness = brightness
			f.mu.Unlock()
		}
		now := f.tk.Now()
		f.mu.Lock()
		overlay := ""
		if time.Now().Before(f.overlayUntil) {
			overlay = f.overlay
		}
		if f.code != "" {
			overlay = f.code
		}
		muted, ringing, alarmSet, av := f.muted, f.ringing, f.alarmSet, f.av
		f.mu.Unlock()

		var fr display.Frame
		blinkOn := now.Nanosecond() < 500_000_000
		if overlay != "" {
			var tmp display.Frame
			tmp.Big(overlay)
			copy(lastOverlay[:], tmp.A[:4])
			hadOverlay = true
		} else if hadOverlay {
			hadOverlay = false
			closeStart = time.Now()
		}
		closing := overlay == "" && !closeStart.IsZero() && time.Since(closeStart) < 4*closeStep
		var eyeG [4]byte
		eyesOn, mouth := false, false
		if c.Eyes && overlay == "" && f.mood != nil && f.eyes != nil {
			eyeG, mouth, eyesOn = f.eyes.frame(f.mood(), time.Now())
		}
		if av != nil && overlay == "" {
			fr.Big(av.big)
			for i, b := range av.blink {
				if b && !blinkOn {
					fr.A[i] = 0
				}
			}
			fr.Set("colon", av.colon)
		} else if eyesOn {
			fr.SetBig(eyeG)
			fr.Set("colon", mouth)
		} else if closing {
			g := lastOverlay
			for i := 0; i <= int(time.Since(closeStart)/closeStep) && i < 4; i++ {
				g[i] = 0
			}
			fr.SetBig(g)
		} else if win, ok := f.scrollWindow(time.Duration(c.ScrollMs) * time.Millisecond); ok && overlay == "" {
			fr.SetBig(win)
		} else if overlay != "" {
			fr.Big(overlay)
		} else {
			h := now.Hour()
			if c.Clock24h {
				fr.Big(fmt.Sprintf("%02d%02d", h, now.Minute()))
			} else {
				if h = h % 12; h == 0 {
					h = 12
				}
				fr.Big(fmt.Sprintf("%2d%02d", h, now.Minute()))
				fr.Set("am", now.Hour() < 12)
				fr.Set("pm", now.Hour() >= 12)
			}
			fr.Set("colon", !c.ColonBlink || blinkOn)
		}
		fr.Weekday(now.Weekday())
		fr.Set("mute", muted)
		fr.Set("alarm", ringing && blinkOn || !ringing && alarmSet)
		if av != nil {
			for i := 0; i < 7; i++ {
				fr.Set(fmt.Sprintf("W%d", i+1), false)
			}
			if !av.daysBlink || blinkOn {
				for _, d := range av.days {
					fr.Set(fmt.Sprintf("W%d", d+1), true)
				}
			}
			show := !av.ampmBlink || blinkOn
			fr.Set("am", av.am && show)
			fr.Set("pm", av.pm && show)
			fr.Set("alarm", av.alarmIcon)
		}
		if f.wx != nil {
			wv := f.wx.view()
			if wv.Valid {
				fr.Temperature(wv.Temp)
				fr.Set("celsius", wv.Unit == "C")
				fr.Set("fahrenheit", wv.Unit == "F")
			}
			for _, ic := range wv.Icons {
				fr.Set(ic, true)
			}
		}
		if f.haDown != nil && f.haDown() {
			switch c.HALostHint {
			case "icon":
				fr.Set("warning", true)
			case "icon_blink":
				fr.Set("warning", blinkOn)
			case "small":
				fr.Set("minus", false)
				fr.Set("hundred", false)
				fr.Set("celsius", false)
				fr.Set("fahrenheit", false)
				fr.Small("--")
			case "message":
				if time.Since(f.lastLostMsg) > time.Minute {
					f.lastLostMsg = time.Now()
					f.showText("no home assistant", 1)
				}
			}
		}
		if f.sleepLeft != nil {
			if left := f.sleepLeft(); left > 0 {
				// minutes left (over the temperature and the HA hint), rounded up (1..199 with the "1" LED), no unit
				mins := min(int((left+time.Minute-1)/time.Minute), 199)
				fr.Set("minus", false)
				fr.Set("celsius", false)
				fr.Set("fahrenheit", false)
				fr.Temperature(mins)
			}
		}
		if blank {
			fr = display.Frame{}
		}
		f.mu.Lock()
		testing := time.Now().Before(f.testUntil)
		if !f.teaserStart.IsZero() {
			if out, ok := teaserFrame(fr, time.Since(f.teaserStart), f.teaserSeed, f.teaserV); ok {
				fr = out
			} else {
				f.teaserStart = time.Time{}
			}
		}
		f.mu.Unlock()
		if testing {
			for i := range fr.A {
				fr.A[i], fr.B[i] = 0x7f, 0x7f // grids 4-5 of chip A too, in case something hides there
			}
			if brightness != 16 {
				disp.SetBrightness(16)
				brightness = 16 // the normal logic fades back afterwards
			}
		}
		disp.Show(fr)
		f.mu.Lock()
		f.shown, f.shownAt = fr, time.Now()
		f.mu.Unlock()

		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// lastTick is when the face loop last ran (zero: not running).
func (f *face) lastTick() time.Time {
	if n := f.tick.Load(); n != 0 {
		return time.Unix(0, n)
	}
	return time.Time{}
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
