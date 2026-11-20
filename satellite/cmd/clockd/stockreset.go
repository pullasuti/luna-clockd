package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"lenovo-satellite/internal/hw"
	"lenovo-satellite/internal/output"
)

// Back to stock without adb: holding the alarm button and volume down together
// for 12 s (the same hold as the stock factory reset) removes the "enabled" flag
// and restarts the clock, which then boots the stock Cast software. Holding the
// two again there runs the vendor's own factory reset. Nothing is deleted here:
// clockd's files stay until that reset (or until someone sets the flag again).
//
// While held: the status LEDs turn amber one by one and the face counts down
// the seconds; the last five seconds and zero are the radio time signal (the
// classic six pips: five short, the long one at zero); then a chime. Letting
// go of either button cancels.

const (
	stockHoldSteps = 12  // seconds
	stockPipFull   = -16 // dBFS: the five pips and the long one
)

// stockPip: the step plays a short pip (the last five seconds; the earlier
// ones are silent, so a short hold such as deleting an alarm stays quiet).
func stockPip(s int) bool { return s >= stockHoldSteps-5 }

// castCLIWrapper is device/system/cast_cli (a test keeps the two equal). Older
// installs have a wrapper Cast could not run (it used the root-only
// /sbin/busybox), which would leave a handed-back clock without Cast and
// without its reset; it is replaced before handing back.
const castCLIWrapper = `#!/system/bin/sh
# Installed as /system/chrome/cast_cli (stock binary kept as cast_cli.stock).
# While clockd is active, every Cast start/stop request is a no-op.
# Runs as user chrome: /system/bin/sh, not /sbin/busybox (root-only, 0700),
# or Cast can't start at all without clockd. That shell has test but no [.
if test -f /tmp/clockd.active; then
    exit 0
fi
exec /system/chrome/cast_cli.stock "$@"
`

// stockCombo follows the alarm and volume down buttons. It reports whether it
// used the event (the buttons do nothing else while the hold runs).
func (d *daemon) stockCombo(e hw.KeyEvent) bool {
	if e.Value == 2 {
		d.mu.Lock()
		on := !d.stockAt.IsZero()
		d.mu.Unlock()
		return on
	}
	down := e.Value == 1
	d.mu.Lock()
	if e.Code == hw.KeyAlarm {
		d.stockAlarm = down
	} else {
		d.stockVolDn = down
	}
	on := !d.stockAt.IsZero()
	switch {
	case !on && d.stockAlarm && d.stockVolDn && !d.volUpDown:
		at := time.Now()
		d.stockAt = at
		d.mu.Unlock()
		d.stopVolumeRepeat()
		undone := d.alarmUI.comboStarted(e.Code == hw.KeyAlarm)
		if e.Code == hw.KeyAlarm && !undone { // volume down came first and stepped the volume
			d.changeVolume(+0.05)
		}
		d.log.Info("back-to-stock hold started (alarm + volume down)")
		go d.stockHold(at)
		return true
	case on:
		if !down {
			held := time.Since(d.stockAt)
			d.stockAt = time.Time{}
			if e.Code == hw.KeyVolumeDown {
				d.volDnDown = false // its press may have gone to volCombo
			}
			d.mu.Unlock()
			d.face.flash("", 0)
			if d.alarmUI.comboReleased(held) { // a short pair press in the alarm view: delete
				return true
			}
			d.log.Info("back-to-stock hold let go: nothing changed")
			return true
		}
		d.mu.Unlock()
		return true
	}
	d.mu.Unlock()
	return false
}

// stockHeld: how long the back-to-stock hold has run (0 = not running).
func (d *daemon) stockHeld() time.Duration {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stockAt.IsZero() {
		return 0
	}
	return time.Since(d.stockAt) + time.Nanosecond
}

func (d *daemon) stockHold(at time.Time) {
	still := func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		return d.stockAt.Equal(at)
	}
	// the face counts down; the end is the radio time signal: five short 1 kHz
	// pips in the last five seconds, the long one at zero
	for s := 0; s < stockHoldSteps; s++ {
		if !still() {
			return
		}
		d.face.flash(strconv.Itoa(stockHoldSteps-s), 1500*time.Millisecond)
		if stockPip(s) {
			d.out.Play(output.NewSamples(output.Tone(stockPipFull, [2]float64{1000, 100}), 0), 1, "alert")
		}
		time.Sleep(time.Until(at.Add(time.Duration(s+1) * time.Second)))
	}
	if !still() {
		return
	}
	d.out.Play(output.NewSamples(output.Tone(stockPipFull, [2]float64{1000, 500}), 0), 1, "alert")
	err := handBack(d.stateDir)
	time.Sleep(time.Until(at.Add((stockHoldSteps + 1) * time.Second))) // the long pip rings out first
	if err != nil {
		d.mu.Lock()
		d.stockAt = time.Time{}
		d.mu.Unlock()
		d.log.Error("back to stock failed: clockd stays", "err", err)
		d.out.Play(output.NewSamples(d.chimeError, 0), 1, "alert")
		d.face.flash("err", 4*time.Second)
		return
	}
	d.log.Warn("back to stock: the clock restarts into the stock software")
	d.out.Play(output.NewSamples(output.Melody(-12, output.Bell, seq("C5", 160, "E5", 160, "G5", 160, "C6", 1200)...), 0), 1, "alert")
	d.face.flash("bye", 10*time.Second)
	time.Sleep(2500 * time.Millisecond)
	syscall.Sync()
	if err := syscall.Reboot(syscall.LINUX_REBOOT_CMD_RESTART); err != nil {
		d.log.Error("clock restart failed", "err", err)
	}
}

// handBack makes the next boot a stock one: a working Cast wrapper first, then
// the flag goes.
func handBack(stateDir string) error {
	if err := fixCastWrapper("/system/chrome/cast_cli"); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(stateDir, "enabled")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// fixCastWrapper replaces our old cast_cli wrapper (the stock binary, or the
// current wrapper, is left alone).
func fixCastWrapper(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if !bytes.HasPrefix(b, []byte("#!/sbin/busybox")) {
		return nil
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(castCLIWrapper), 0o755); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
