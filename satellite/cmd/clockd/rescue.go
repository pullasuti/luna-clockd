package main

import (
	"context"
	"log/slog"
	"os"
	"strconv"
	"syscall"
	"time"

	"lenovo-satellite/internal/display"
	"lenovo-satellite/internal/hw"
)

// Rescue mode (clockd -rescue): start.sh runs it instead of clockd when clockd
// keeps exiting within a minute of starting. The buttons are clockd's (the
// vendor services are stopped), so without it a crash-looping clockd would also
// take away the back-to-stock hold, the only way out on a locked clock without
// ssh. It does as little as possible: no settings, network, audio or Home
// Assistant, only the face ("Err"), the status LEDs and the keypad. Holding the
// alarm button and volume down for 12 s hands the clock back to the stock
// software, as in clockd; a power cycle tries clockd again.

// rescueHold follows the alarm + volume down hold.
type rescueHold struct {
	alarm, volDn bool
	at           time.Time // zero: not held
}

func (h *rescueHold) key(e hw.KeyEvent, now time.Time) {
	if e.Value == 2 { // autorepeat
		return
	}
	switch e.Code {
	case hw.KeyAlarm:
		h.alarm = e.Value == 1
	case hw.KeyVolumeDown:
		h.volDn = e.Value == 1
	default:
		return
	}
	switch {
	case !h.alarm || !h.volDn:
		h.at = time.Time{}
	case h.at.IsZero():
		h.at = now
	}
}

// held: how long the hold has run (0 = not held).
func (h *rescueHold) held(now time.Time) time.Duration {
	if h.at.IsZero() {
		return 0
	}
	return now.Sub(h.at) + time.Nanosecond
}

// rescueView is what the face and the LEDs show: "Err" and dim red while
// waiting; while held, the seconds left and one more amber LED every 3 s.
func rescueView(held time.Duration) (string, [4]hw.RGB) {
	var c [4]hw.RGB
	if held <= 0 {
		for i := range c {
			c[i] = hw.RGB{R: 80}
		}
		return "Err", c
	}
	n := 1 + int(held/(3*time.Second))
	for i := 0; i < min(n, 4); i++ {
		c[i] = hw.RGB{R: 255, G: 128}
	}
	return strconv.Itoa(max(1, stockHoldSteps-int(held/time.Second))), c
}

func rescue(stateDir string) int {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	log.Warn("rescue mode: clockd kept exiting; hold alarm + volume down for 12 s for the stock software, or power-cycle to try clockd again", "version", version)

	disp, err := display.Open()
	if err != nil {
		log.Error("rescue: face", "err", err) // carry on: the LEDs still show the hold
	}
	var leds hw.StatusLEDs
	show := func(text string, c [4]hw.RGB) {
		if disp != nil {
			var f display.Frame
			f.Big(text)
			disp.Show(f)
		}
		if err := leds.Set(c); err != nil {
			log.Debug("rescue: LEDs", "err", err)
		}
	}

	keys := make(chan hw.KeyEvent, 16)
	go func() {
		for {
			err := hw.ReadKeys(context.Background(), keypad, func(e hw.KeyEvent) { keys <- e })
			log.Error("rescue: keypad", "err", err)
			time.Sleep(5 * time.Second)
		}
	}()

	var h rescueHold
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case e := <-keys:
			was := !h.at.IsZero()
			h.key(e, time.Now())
			switch on := !h.at.IsZero(); {
			case on && !was:
				log.Info("rescue: back-to-stock hold started")
			case !on && was:
				log.Info("rescue: back-to-stock hold let go: nothing changed")
			}
		case <-tick.C:
		}
		held := h.held(time.Now())
		if held < stockHoldSteps*time.Second {
			show(rescueView(held))
			continue
		}
		if err := handBack(stateDir); err != nil {
			log.Error("rescue: back to stock failed", "err", err)
			h.at = time.Time{} // let go and hold again to retry
			show("Err", [4]hw.RGB{{R: 255}, {R: 255}, {R: 255}, {R: 255}})
			time.Sleep(4 * time.Second)
			continue
		}
		log.Warn("rescue: back to stock: the clock restarts into the stock software")
		show("bye", [4]hw.RGB{})
		time.Sleep(2 * time.Second)
		syscall.Sync()
		if err := syscall.Reboot(syscall.LINUX_REBOOT_CMD_RESTART); err != nil {
			log.Error("rescue: clock restart failed", "err", err)
			return 1
		}
		return 0
	}
}
