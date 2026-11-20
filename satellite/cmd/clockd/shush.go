package main

// Shush: while music plays, a hand moving toward the clock turns the volume
// down proportionally (the STK3311's proximity channel), and it restores when
// the hand leaves. The proximity raw value idles around 5-25 and reads 80-100
// for a hand a few cm away, so the duck follows the rise over the idle level.
//
// The master volume (out.SetVolume) is used, not the saved device volume, so
// nothing is persisted and HA's volume state is untouched; the alarm is never
// ducked.

// shushFactor is the volume multiplier for a proximity rise over idle:
// no ducking up to 10, down to 20 % at 60 and above.
func shushFactor(rise float32) float32 {
	const start, floor, span = float32(10), float32(0.2), float32(50)
	if rise <= start {
		return 1
	}
	f := 1 - (rise-start)/span*(1-floor)
	if f < floor {
		return floor
	}
	return f
}

// shushTick is called ~4x/s from the ambient loop with the current proximity
// reading and its slowly tracked idle level.
func (d *daemon) shushTick(prox, base float32) {
	if !d.cfg.Get().Voice.Shush {
		d.shushOff()
		return
	}
	// only music (Sendspin or fetched media), not announcements or a ringing alarm
	d.mu.Lock()
	playing := d.media != nil || (d.sendspin != nil && d.sendspin.Playing())
	ringing := d.alarm != nil
	d.mu.Unlock()
	if !playing || ringing {
		d.shushOff()
		return
	}
	target := shushFactor(prox - base)
	// d.mu is held over SetVolume here, in shushOff and in setVolume: a
	// volume change between reading the volume and applying it would
	// otherwise be overwritten by the older one
	d.mu.Lock()
	defer d.mu.Unlock()
	// smooth toward the target so small sensor jumps don't step the volume
	d.shushLvl += 0.35 * (target - d.shushLvl)
	applied := d.shushLvl
	if applied > 0.995 {
		applied = 1
	}
	d.out.SetVolume(d.vol * applied)
	d.shushOn = applied < 1
}

// shushOff restores the set volume once, when ducking ends.
func (d *daemon) shushOff() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.shushOn {
		d.out.SetVolume(d.vol)
	}
	d.shushLvl, d.shushOn = 1, false
}
