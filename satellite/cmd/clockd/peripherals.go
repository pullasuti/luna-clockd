package main

import (
	"context"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	esphome "github.com/ygelfand/go-esphome-device"

	"github.com/ygelfand/go-esphome-device/api"

	"lenovo-satellite/internal/config"
	"lenovo-satellite/internal/hw"
	"lenovo-satellite/internal/output"
)

const keypad = "/dev/input/event0"

// peripherals: buttons, mute switch, status LEDs and night light, plus their
// Home Assistant entities.
type peripherals struct {
	leds         hw.StatusLEDs
	muteSensor   *esphome.BinarySensor
	buttons      *esphome.Event
	nightLight   *esphome.Light
	statusLED    *esphome.Light
	muteSwitch   *esphome.Switch
	wakeSound    *esphome.Switch
	stopWord     *esphome.Switch
	ledBright    *esphome.Number
	loginReq     *esphome.TextSensor // the waiting web login, for approving from HA
	nextAlarm    *esphome.TextSensor // the next clock alarm (timestamp, "" = none)
	alarmRinging *esphome.BinarySensor
	alarmStop    *esphome.Button
	alarmSnooze  *esphome.Button
	loginOK      *esphome.Button
	restartDev   *esphome.Button
	restartD     *esphome.Button
	socTemp      *esphome.Sensor
	wifiRSSI     *esphome.Sensor
	wifiNet      *esphome.TextSensor
	proximity    *esphome.Sensor
	wakeEv       *esphome.Event
	idleColour   [4]hw.RGB // what Home Assistant set the status LEDs to
	speech       speechEnv // follows the spoken answer for the speaking animation
	errorUntil   time.Time
	nlPulsing    atomic.Bool // the night light is following the voice (restore when done)
	// web UI preview of a state
	previewState int
	previewUntil time.Time
}

func (d *daemon) buildPeripherals() []esphome.Entity {
	p := &d.hw
	p.muteSensor = &esphome.BinarySensor{Base: esphome.Base{ObjectID: "mic_muted", Name: "Mic muted"}}
	p.buttons = &esphome.Event{
		Base:        esphome.Base{ObjectID: "buttons", Name: "Button"},
		DeviceClass: "button",
		Types:       []string{"play", "alarm", "volume_up", "volume_down"},
	}
	p.nightLight = &esphome.Light{
		Base:                esphome.Base{ObjectID: "night_light", Name: "Night light"},
		SupportedColorModes: []esphome.ColorMode{esphome.ColorModeBrightness},
	}
	p.nightLight.OnCommand = func(s esphome.LightState) {
		level := 0
		if s.On {
			level = int(s.Brightness*254) + 1
		}
		d.setNightLightByHand(level)
	}
	p.statusLED = &esphome.Light{
		// a configuration entity: Home Assistant does not expose those to
		// Assist, which took the status LEDs for room lights ("set the lights
		// in the office") and left them on. Still on the device page.
		Base:                esphome.Base{ObjectID: "status_leds", Name: "Status LEDs", Category: esphome.CategoryConfig},
		SupportedColorModes: []esphome.ColorMode{esphome.ColorModeRGB},
	}
	p.statusLED.OnCommand = func(s esphome.LightState) {
		var c hw.RGB
		if s.On {
			c = hw.RGB{
				R: uint8(s.Red * s.Brightness * 255),
				G: uint8(s.Green * s.Brightness * 255),
				B: uint8(s.Blue * s.Brightness * 255),
			}
		}
		d.mu.Lock()
		p.idleColour = [4]hw.RGB{c, c, c, c}
		d.mu.Unlock()
		p.statusLED.Set(s)
		d.updateLEDs()
	}
	p.muteSwitch = &esphome.Switch{Base: esphome.Base{ObjectID: "mute", Name: "Mute microphone", Icon: "mdi:microphone-off"}}
	p.muteSwitch.OnCommand = func(v bool) { d.setSoftMuted(v) }
	p.wakeSound = &esphome.Switch{Base: esphome.Base{ObjectID: "wake_sound", Name: "Wake sound", Category: esphome.CategoryConfig, Icon: "mdi:bell-ring"}}
	p.wakeSound.OnCommand = func(v bool) {
		d.cfg.Update(func(c *config.Config) { c.Voice.WakeSound = v })
		d.syncSettings()
	}
	p.stopWord = &esphome.Switch{Base: esphome.Base{ObjectID: "stop_word", Name: "Stop word", Category: esphome.CategoryConfig, Icon: "mdi:hand-back-left"}}
	p.stopWord.OnCommand = func(v bool) {
		d.cfg.Update(func(c *config.Config) { c.Voice.StopWord = v })
		d.syncSettings()
	}
	p.ledBright = &esphome.Number{Base: esphome.Base{ObjectID: "led_brightness", Name: "LED brightness", Category: esphome.CategoryConfig, Icon: "mdi:brightness-6"},
		Min: 0, Max: 100, Step: 5, Unit: "%", Mode: api.NumberMode_NUMBER_MODE_SLIDER}
	p.ledBright.OnCommand = func(v float32) {
		d.cfg.Update(func(c *config.Config) { c.LEDs.Brightness = int(v + 0.5) })
		d.syncSettings()
		d.updateLEDs()
	}
	// restore the last level (it used to be switched off on every start)
	d.setNightLight(config.LoadState(d.statePath).NightLight)
	p.statusLED.Set(esphome.LightState{On: false, Brightness: 1, ColorMode: esphome.ColorModeRGB, Red: 1, Green: 1, Blue: 1})
	d.syncSettings()
	// web logins can be approved from Home Assistant too
	p.loginReq = &esphome.TextSensor{Base: esphome.Base{ObjectID: "web_login_request", Name: "Web login request",
		Icon: "mdi:account-key", Category: esphome.CategoryDiagnostic}}
	p.loginReq.Set("none")
	p.loginOK = &esphome.Button{Base: esphome.Base{ObjectID: "approve_web_login", Name: "Approve web login",
		Icon: "mdi:account-check", Category: esphome.CategoryConfig}}
	p.loginOK.OnPress = func() {
		if d.auth != nil && d.auth.Approve("home assistant", "") {
			d.log.Info("web login approved from Home Assistant")
		}
	}
	// Wi-Fi and proximity: diagnostics, updated from main (watchWiFi/watchProximity)
	p.wifiRSSI = &esphome.Sensor{Base: esphome.Base{ObjectID: "wifi_signal", Name: "Wi-Fi signal",
		Icon: "mdi:wifi", Category: esphome.CategoryDiagnostic},
		Unit: "dBm", DeviceClass: "signal_strength", StateClass: esphome.StateClassMeasurement, Decimals: 0}
	p.wifiNet = &esphome.TextSensor{Base: esphome.Base{ObjectID: "wifi_network", Name: "Wi-Fi network",
		Icon: "mdi:wifi", Category: esphome.CategoryDiagnostic}}
	p.proximity = &esphome.Sensor{Base: esphome.Base{ObjectID: "proximity", Name: "Proximity",
		Icon: "mdi:hand-front-right", Category: esphome.CategoryDiagnostic},
		StateClass: esphome.StateClassMeasurement, Decimals: 0}

	p.socTemp = &esphome.Sensor{Base: esphome.Base{ObjectID: "soc_temperature", Name: "SoC temperature",
		Icon: "mdi:thermometer", Category: esphome.CategoryDiagnostic},
		Unit: "°C", DeviceClass: "temperature", StateClass: esphome.StateClassMeasurement, Decimals: 1}

	// Restart buttons: the whole clock (kernel restart after a sync;
	// start.sh brings clockd back), or clockd alone (start.sh restarts it).
	p.restartDev = &esphome.Button{Base: esphome.Base{ObjectID: "restart", Name: "Restart",
		Category: esphome.CategoryConfig}, DeviceClass: "restart"}
	p.restartDev.OnPress = func() {
		d.log.Warn("clock restart requested from Home Assistant")
		go func() {
			time.Sleep(500 * time.Millisecond) // let the log line and the API reply out
			// init on these clocks ignores sys.powerctl (and the toolbox command
			// only sets it), so restart through the kernel after a sync
			syscall.Sync()
			if err := syscall.Reboot(syscall.LINUX_REBOOT_CMD_RESTART); err != nil {
				d.log.Error("clock restart failed", "err", err)
			}
		}()
	}
	p.restartD = &esphome.Button{Base: esphome.Base{ObjectID: "restart_clockd", Name: "Restart clockd",
		Icon: "mdi:restart", Category: esphome.CategoryConfig}}
	p.restartD.OnPress = func() {
		d.log.Info("clockd restart requested from Home Assistant")
		if d.restart != nil {
			go func() { time.Sleep(300 * time.Millisecond); d.restart() }()
		}
	}
	// Wake word / stop word detections and near misses, for HA automations and
	// for later "nearest clock answers" (the phrase is the event type; Types are
	// filled in from the installed models when the entities are built).
	p.wakeEv = &esphome.Event{Base: esphome.Base{ObjectID: "wake_word", Name: "Wake word"},
		DeviceClass: "", Types: wakeEventTypes(d)}

	p.nextAlarm = &esphome.TextSensor{Base: esphome.Base{ObjectID: "next_alarm", Name: "Next alarm", Icon: "mdi:alarm"}, DeviceClass: "timestamp"}
	p.alarmRinging = &esphome.BinarySensor{Base: esphome.Base{ObjectID: "alarm_ringing", Name: "Alarm ringing", Icon: "mdi:alarm-bell"}}
	p.alarmRinging.Set(false)
	p.alarmStop = &esphome.Button{Base: esphome.Base{ObjectID: "alarm_stop", Name: "Stop alarm", Icon: "mdi:alarm-off"}}
	p.alarmStop.OnPress = func() { d.stopAlarm() }
	p.alarmSnooze = &esphome.Button{Base: esphome.Base{ObjectID: "alarm_snooze", Name: "Snooze alarm", Icon: "mdi:alarm-snooze"}}
	p.alarmSnooze.OnPress = func() { d.snoozeAlarm() }
	return []esphome.Entity{p.nextAlarm, p.alarmRinging, p.alarmStop, p.alarmSnooze, p.muteSensor, p.buttons, p.nightLight, p.statusLED, p.muteSwitch, p.wakeSound, p.stopWord, p.ledBright, p.loginReq, p.loginOK, p.restartDev, p.restartD, p.socTemp, p.wifiRSSI, p.wifiNet, p.proximity, p.wakeEv}
}

// runButtons reads the keypad until ctx ends (retrying if the device goes away).
func (d *daemon) runButtons(ctx context.Context) {
	if down, err := hw.KeyDown(keypad, hw.KeyMute); err != nil {
		d.log.Warn("read mute switch", "err", err)
	} else {
		d.setMuted(down)
	}
	for ctx.Err() == nil {
		err := hw.ReadKeys(ctx, keypad, d.onKey)
		if ctx.Err() == nil {
			d.log.Warn("keypad", "err", err)
			time.Sleep(time.Second)
		}
	}
}

func (d *daemon) onKey(e hw.KeyEvent) {
	// A ringing alarm: the alarm button stops it, any other button snoozes it.
	if e.Value == 1 && d.ringingAlarm() != "" {
		if e.Code == hw.KeyAlarm {
			d.hw.buttons.Trigger("alarm")
			d.stopAlarm()
		} else {
			d.snoozeAlarm()
		}
		return
	}
	if e.Value == 1 && e.Code != hw.KeyMute {
		d.face.wake(10 * time.Second) // any button shows a face that is off for the night
	}
	if e.Code == hw.KeyMute {
		if e.Value != 2 {
			d.setMuted(e.Value == 1)
		}
		return
	}
	// While a web login (or the setup page) waits, holding the action button
	// for 0.7 s approves it right away (chime + "good", so you know when to let
	// go). A shorter press does what it always does, on release.
	if e.Code == hw.KeyPlay && d.auth != nil && (d.auth.Pending() || d.setup.confirm.Pending() || d.playHeld()) {
		switch e.Value {
		case 1:
			d.mu.Lock()
			d.playDown = time.Now()
			d.playUsed = false
			pressed := d.playDown
			d.mu.Unlock()
			time.AfterFunc(700*time.Millisecond, func() {
				d.mu.Lock()
				still := d.playDown.Equal(pressed)
				if still {
					d.playUsed = true
				}
				d.mu.Unlock()
				if still && d.setup.confirm.Approve() {
					d.log.Info("setup page confirmed with the button")
					d.out.Play(output.NewSamples(d.chimeWake, 0), 1, "alert")
					d.face.flash("good", 1500*time.Millisecond)
					return
				}
				if still && d.auth.Approve("button", "") {
					d.log.Info("web login approved with the button")
					d.out.Play(output.NewSamples(d.chimeWake, 0), 1, "alert")
					d.face.flash("good", 1500*time.Millisecond)
				}
			})
		case 0:
			d.mu.Lock()
			used := d.playUsed
			d.playDown, d.playUsed = time.Time{}, false
			d.mu.Unlock()
			if !used {
				d.hw.buttons.Trigger("play")
				d.onPlayButton()
			}
		}
		return
	}
	if (e.Code == hw.KeyAlarm || e.Code == hw.KeyVolumeDown) && d.stockCombo(e) {
		return
	}
	if (e.Code == hw.KeyVolumeUp || e.Code == hw.KeyVolumeDown) && d.volCombo(e) {
		return
	}
	if e.Code == hw.KeyAlarm && e.Value == 1 {
		d.hw.buttons.Trigger("alarm")
	}
	if d.alarmUI.key(e) { // the alarm button; volume while alarms are shown
		return
	}
	if e.Value == 0 && (e.Code == hw.KeyVolumeUp || e.Code == hw.KeyVolumeDown) {
		d.stopVolumeRepeat()
		return
	}
	if e.Value != 1 { // act on press; auto-repeat is our own (startVolumeRepeat)
		return
	}
	switch e.Code {
	case hw.KeyVolumeUp:
		d.hw.buttons.Trigger("volume_up")
		d.changeVolume(+0.05)
		d.startVolumeRepeat(+0.05)
	case hw.KeyVolumeDown:
		d.hw.buttons.Trigger("volume_down")
		d.changeVolume(-0.05)
		d.startVolumeRepeat(-0.05)
	case hw.KeyPlay:
		d.hw.buttons.Trigger("play")
		d.onPlayButton()
	}
}

// volCombo: both volume buttons held together for 5 s turn the setup hotspot
// on or off. Pressing the second one undoes the first one's volume step; the
// buttons do nothing else until both are released. Reports whether the event
// was used.
func (d *daemon) volCombo(e hw.KeyEvent) bool {
	if e.Value == 2 {
		return false // auto-repeat: ignored by onKey anyway
	}
	d.mu.Lock()
	if e.Code == hw.KeyVolumeUp {
		d.volUpDown = e.Value == 1
	} else {
		d.volDnDown = e.Value == 1
	}
	both, in := d.volUpDown && d.volDnDown, d.comboOn
	switch {
	case both && !in:
		at := time.Now()
		d.comboOn, d.comboAt = true, at
		d.mu.Unlock()
		d.stopVolumeRepeat()
		if e.Code == hw.KeyVolumeUp { // the first one was volume down
			d.changeVolume(+0.05)
		} else {
			d.changeVolume(-0.05)
		}
		time.AfterFunc(5*time.Second, func() {
			d.mu.Lock()
			ok := d.comboAt.Equal(at) && d.volUpDown && d.volDnDown
			d.mu.Unlock()
			if ok && d.setup != nil {
				d.out.Play(output.NewSamples(d.chimeWake, 0), 1, "alert")
				d.setup.Toggle()
			}
		})
		return true
	case in:
		if !d.volUpDown && !d.volDnDown {
			d.comboOn = false
		}
		d.mu.Unlock()
		return true
	}
	d.mu.Unlock()
	return false
}

// playHeld: the action button went down while a login was pending and is
// still held (its release must not trigger a normal press).
func (d *daemon) playHeld() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return !d.playDown.IsZero()
}

// startVolumeRepeat keeps stepping the volume while the button is held:
// after 0.4 s, every 150 ms, until release (or 5 s, in case a release is lost).
func (d *daemon) startVolumeRepeat(step float32) {
	d.stopVolumeRepeat()
	stop := make(chan struct{})
	d.mu.Lock()
	d.volStop = stop
	d.mu.Unlock()
	go func() {
		select {
		case <-stop:
			return
		case <-time.After(400 * time.Millisecond):
		}
		t := time.NewTicker(150 * time.Millisecond)
		defer t.Stop()
		limit := time.After(5 * time.Second)
		for {
			select {
			case <-stop:
				return
			case <-limit:
				return
			case <-t.C:
				d.changeVolume(step)
			}
		}
	}()
}

func (d *daemon) stopVolumeRepeat() {
	d.mu.Lock()
	if d.volStop != nil {
		close(d.volStop)
		d.volStop = nil
	}
	d.mu.Unlock()
}

// haDown: Home Assistant has been disconnected for over 30 s (short
// reconnects and restarts don't count).
func (d *daemon) haDown() bool {
	if d.sat == nil {
		return false
	}
	up := d.sat.Subscribed() // outside d.mu: the library calls back into code that takes it
	d.mu.Lock()
	defer d.mu.Unlock()
	if up || d.haSeen.IsZero() {
		d.haSeen = time.Now()
		return false
	}
	return time.Since(d.haSeen) > 30*time.Second
}

// syncSettings publishes config values to their Home Assistant entities.
func (d *daemon) syncSettings() {
	c := d.cfg.Get()
	d.hw.wakeSound.Set(c.Voice.WakeSound)
	d.hw.stopWord.Set(c.Voice.StopWord)
	d.hw.ledBright.Set(float32(c.LEDs.Brightness))
	d.syncChime()
}

// setMuted follows the hardware mute switch.
func (d *daemon) setMuted(m bool) {
	d.mu.Lock()
	was := d.hwMuted
	d.hwMuted = m
	d.mu.Unlock()
	if m != was {
		d.log.Info("mic mute switch", "muted", m)
	}
	d.applyMute()
}

// setSoftMuted is the web UI / Home Assistant mute. The hardware switch always
// wins: while it is on, the mic stays muted whatever this says.
func (d *daemon) setSoftMuted(m bool) {
	d.mu.Lock()
	was := d.softMuted
	d.softMuted = m
	d.mu.Unlock()
	if m != was {
		d.log.Info("software mic mute", "muted", m)
		if err := config.UpdateState(d.statePath, func(s *config.State) { s.MicMuted = m }); err != nil {
			d.log.Warn("save state", "err", err)
		}
	}
	d.applyMute()
}

// isMuted reports the effective mute (hardware switch or soft mute).
func (d *daemon) isMuted() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.muted
}

func (d *daemon) applyMute() {
	d.mu.Lock()
	m := d.hwMuted || d.softMuted
	d.muted = m
	soft := d.softMuted
	st := d.st
	d.mu.Unlock()
	d.face.setMuted(m)
	d.hw.muteSensor.Set(m)
	if d.hw.muteSwitch != nil {
		d.hw.muteSwitch.Set(soft)
	}
	if m && st == stListening {
		d.sat.StopTurn()
		d.endTurn()
	}
	d.updateLEDs()
}

// setVolume changes the speaker volume and saves it (2 s after the last
// change, so holding a volume button doesn't write on every step).
func (d *daemon) setVolume(v float32) {
	v = max(0, min(v, 1))
	d.player.SetVolume(v)
	d.mu.Lock()
	d.vol = v
	d.shushLvl, d.shushOn = 1, false // drops any shush level
	d.out.SetVolume(v)
	if d.volSave != nil {
		d.volSave.Stop()
	}
	d.volSave = time.AfterFunc(2*time.Second, func() {
		if err := config.UpdateState(d.statePath, func(s *config.State) { s.Volume = &v }); err != nil {
			d.log.Warn("save volume", "err", err)
		}
	})
	d.mu.Unlock()
}

func (d *daemon) changeVolume(delta float32) {
	v := max(0, min(d.player.Volume()+delta, 1))
	d.setVolume(v)
	d.face.flash(fmt.Sprintf("%3d", int(v*100+0.5)), 1500*time.Millisecond)
	d.out.Play(output.NewSamples(d.chimeTick, 0), 1, "alert")
}

// onPlayButton: stop what is happening, or start a push-to-talk turn.
func (d *daemon) onPlayButton() {
	if d.stopAlarm() {
		return
	}
	d.mu.Lock()
	st, muted, media := d.st, d.muted, d.media
	d.mu.Unlock()
	switch {
	case st != stIdle:
		d.log.Info("turn cancelled by button")
		d.sat.StopTurn()
		d.out.StopGroup("voice")
		d.endTurn()
	case media != nil:
		d.stopMedia()
	case muted:
		d.out.Play(output.NewSamples(d.chimeError, 0), 1, "alert")
	default:
		go d.startTurn("", true)
	}
}

// stopAlarm stops a ringing timer or alarm (an alarm's snooze too); it
// reports whether one was ringing.
func (d *daemon) stopAlarm() bool {
	id := d.ringingAlarm()
	if !d.stopRinging() {
		return false
	}
	if id != "" {
		d.log.Info("alarm stopped")
		d.alarms.clearSnooze()
	}
	return true
}

// stopRinging silences the ringing sound.
func (d *daemon) stopRinging() bool {
	d.mu.Lock()
	a := d.alarm
	d.alarm, d.alarmID = nil, ""
	d.mu.Unlock()
	if a == nil {
		return false
	}
	d.out.Stop(a)
	d.face.setRinging(false)
	d.updateLEDs()
	return true
}

// onStopWord: "stop" silences whatever is sounding, most urgent first.
func (d *daemon) onStopWord() {
	d.log.Info("stop word detected")
	if d.stopAlarm() {
		return
	}
	d.mu.Lock()
	st, media, ann := d.st, d.media, d.announcing
	d.continueReq = false
	if ann {
		d.annCancel = true
	}
	d.mu.Unlock()
	switch {
	case st != stIdle:
		d.sat.StopTurn()
		d.out.StopGroup("voice")
		d.endTurn()
	case ann:
		d.out.StopGroup("voice")
	case media != nil:
		d.stopMedia()
	case d.sendspin != nil && d.sendspin.Playing():
		d.sendspin.Pause()
	}
}

// ledColours renders a config colour.
func ledColours(spec string) [4]hw.RGB {
	switch spec {
	case "classic":
		return hw.ClassicColours
	case "classic_dim":
		var c [4]hw.RGB
		for i, g := range hw.ClassicColours {
			c[i] = g.Scale(0.2)
		}
		return c
	case "":
		return [4]hw.RGB{}
	}
	var r, g, b uint8
	fmt.Sscanf(spec, "#%02x%02x%02x", &r, &g, &b)
	x := hw.RGB{R: r, G: g, B: b}
	return [4]hw.RGB{x, x, x, x}
}

// LED states, also the preview indexes used by the web UI.
const (
	ledListening = iota
	ledThinking
	ledSpeaking
	ledMuted
	ledRinging
	ledIdle
	ledError
	ledStock // the back-to-stock hold (not a web UI preview)
)

// pulse is a slow breathing level (listening), 2 s period. LEDs look bright
// even at low duty, so the wave is squared (gamma) and dips to 3 %.
func pulse(t time.Duration) float64 {
	w := 0.5 - 0.5*math.Cos(2*math.Pi*t.Seconds()/2.0)
	return 0.03 + 0.97*w*w
}

// chase gives each LED a level for a light running back and forth (thinking).
func chase(t time.Duration) [4]float64 {
	const period = 1.2 // seconds for 0 -> 3 -> 0
	ph := math.Mod(t.Seconds(), period) / period
	x := 3 * (1 - math.Abs(2*ph-1)) // triangle wave 0..3..0
	var l [4]float64
	for i := range l {
		l[i] = max(0.08, 1-0.75*math.Abs(float64(i)-x))
	}
	return l
}

// noise is smooth value noise in 0..1 (hash of the integer steps, smoothstep
// between them). The web UI has the same function for its LED preview.
func noise(x float64) float64 {
	i := math.Floor(x)
	f := x - i
	f = f * f * (3 - 2*f)
	a, b := hash01(int64(i)), hash01(int64(i)+1)
	return a + (b-a)*f
}

func hash01(i int64) float64 {
	x := uint32(i) * 2654435761
	x ^= x >> 15
	x *= 2246822519
	x ^= x >> 13
	return float64(x) / 4294967295
}

// voice is a speech-like loudness envelope: syllables at ~5 Hz with some
// faster flicker, and short gaps between "words".
func voice(t float64) float64 {
	v := 0.65*noise(t*5.3) + 0.35*noise(t*11.7+17)
	v = max(0, min(1, (v-0.2)/0.6))
	if noise(t*0.9+3) < 0.3 { // a pause between words
		v *= 0.15
	}
	return v
}

// speak gives each LED a level while speaking: the middle two follow the voice,
// the outer two follow it a moment later and weaker, so pulses spread outwards.
func speak(t time.Duration) [4]float64 {
	s := t.Seconds()
	mid, out := voice(s), voice(s-0.09)
	return [4]float64{0.06 + 0.6*out, 0.06 + 0.94*mid, 0.06 + 0.94*mid, 0.06 + 0.6*out}
}

// speechEnv turns the loudness of the spoken answer into LED levels. The
// level is taken where the sound reaches the speaker, so the LEDs move with
// what is heard. A slowly falling peak works as automatic gain: syllables
// stand out whether the voice is loud or soft.
type speechEnv struct {
	mu     sync.Mutex // updateLEDs also runs outside the LED loop
	peakDB float64
	level  float64
	hist   [3]float64 // previous levels, for the outer LEDs' delay
	active bool
}

// frame returns the LED levels while speaking: from the answer's loudness when
// it is playing (playing=true), otherwise the imitation (fallback).
func (e *speechEnv) frame(playing bool, rms float32, fallback [4]float64) [4]float64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !playing {
		e.active = false
		return fallback
	}
	return e.step(rms)
}

// step takes the loudest RMS (0-1) of the last LED frame and returns the
// four LED levels, the outer pair a couple of frames behind.
func (e *speechEnv) step(rms float32) [4]float64 {
	db := 20 * math.Log10(float64(rms)+1e-9)
	if !e.active {
		e.peakDB, e.active = max(db, -45), true
	}
	e.peakDB = max(db, e.peakDB-0.1, -45) // falls ~2.5 dB/s; never below -45 dBFS (hiss stays dark)
	// Tuned offline against a speech-like track (scratch envtune.py): middle
	// LEDs well below full most of the time, brighter on strong syllables.
	v := max(0, min(1, (db-(e.peakDB-45))/45))
	v = math.Pow(v, 3.5) // only the stronger syllables come close to full
	if v > e.level {
		e.level = 0.6*e.level + 0.4*v // soft onsets
	} else {
		e.level = 0.55*e.level + 0.45*v // and a short tail
	}
	copy(e.hist[1:], e.hist[:2])
	e.hist[0] = e.level
	// the outer LEDs only join in on the stronger parts, a moment later, so
	// pulses spread outwards instead of all four peaking together
	// (user tuning on the real clock: centre toned down, sides as in v0.39.1)
	mid, out := e.level, max(0, (e.hist[2]-0.45)/0.55)
	return [4]float64{0.06 + 0.5*out, 0.06 + 0.59*mid, 0.06 + 0.59*mid, 0.06 + 0.5*out}
}

// runLEDs animates the status LEDs (writes only happen when a frame changes).
func (d *daemon) runLEDs(ctx context.Context) {
	t := time.NewTicker(40 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.updateLEDs()
		}
	}
}

func (d *daemon) ledState() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	if time.Now().Before(d.hw.previewUntil) {
		return d.hw.previewState
	}
	switch {
	case !d.stockAt.IsZero():
		return ledStock
	case time.Now().Before(d.hw.errorUntil):
		return ledError
	case d.alarm != nil:
		return ledRinging
	case d.st == stListening:
		return ledListening
	case d.st == stResponding && !d.speaking:
		return ledThinking
	case d.st == stResponding:
		return ledSpeaking
	case d.muted:
		return ledMuted
	}
	return ledIdle
}

// previewLEDs shows one state's LEDs (with animation) for 3 s.
func (d *daemon) previewLEDs(state int) {
	d.mu.Lock()
	d.hw.previewState = max(ledListening, min(state, ledRinging))
	d.hw.previewUntil = time.Now().Add(3 * time.Second)
	d.mu.Unlock()
}

// updateLEDs shows the assistant state on the four status LEDs.
func (d *daemon) updateLEDs() {
	state := d.ledState()
	d.mu.Lock()
	idle := d.hw.idleColour
	d.mu.Unlock()
	lc := d.cfg.Get().LEDs
	now := time.Duration(time.Now().UnixNano())
	var c [4]hw.RGB
	switch state {
	case ledError:
		c = ledColours("#ff0000")
	case ledStock:
		// warm amber, one more LED every 3 s
		n := 1 + int(d.stockHeld()/(3*time.Second))
		amber := ledColours("#ff8000")
		for i := 0; i < min(n, 4); i++ {
			c[i] = amber[i]
		}
	case ledRinging:
		// blink 0.5 s on / 0.5 s off at full brightness: it has to be noticed
		if (now/(500*time.Millisecond))%2 == 0 {
			c = ledColours(lc.Ringing)
		}
		if err := d.hw.leds.Set(c); err != nil {
			d.log.Debug("status LEDs", "err", err)
		}
		return
	case ledListening:
		c = ledColours(lc.Listening)
		if lc.Animate {
			f := pulse(now)
			for i := range c {
				c[i] = c[i].Scale(f)
			}
		}
	case ledThinking:
		c = ledColours(lc.Thinking)
		if lc.Animate {
			l := chase(now)
			for i := range c {
				c[i] = c[i].Scale(l[i])
			}
		}
	case ledSpeaking:
		c = ledColours(lc.Responding)
		if lc.Animate {
			// the real answer when it is playing, the imitation while waiting for it
			t := time.Now()
			l := d.hw.speech.frame(d.out.Busy("voice"), d.out.Level("voice", t.Add(-40*time.Millisecond), t), speak(now))
			for i := range c {
				c[i] = c[i].Scale(l[i])
			}
			if lc.SpeakNightLight {
				d.pulseNightLight((l[1] + l[2]) / 2)
			}
		}
	case ledMuted:
		c = ledColours(lc.Muted)
		if lc.Muted == "" {
			c = idle
		}
	default:
		c = idle
	}
	if state != ledSpeaking || !lc.Animate || !lc.SpeakNightLight {
		d.endNightLightPulse()
	}
	if lc.Brightness < 100 {
		for i := range c {
			c[i] = c[i].Scale(float64(lc.Brightness) / 100)
		}
	}
	if err := d.hw.leds.Set(c); err != nil {
		d.log.Debug("status LEDs", "err", err)
	}
}

// Night light pulse while speaking: up to nlPulseDepth steps (of 255) above
// its own level, following the voice; gentler during the night schedule and
// never from off at night. The hardware is written directly, so Home
// Assistant's state and any fade keep their intended level.
const (
	nlPulseDepth      = 110
	nlPulseDepthNight = 35
)

func (d *daemon) pulseNightLight(v float64) {
	base := d.nightLevel()
	depth := nlPulseDepth
	if d.night != nil {
		if night, _ := d.night.State(); night {
			if base == 0 {
				d.endNightLightPulse()
				return
			}
			depth = nlPulseDepthNight
		}
	}
	v = max(0, min(v, 1))
	// near full brightness there is no room above: the pulse moves down so it
	// still spans its whole depth (at 255 it swings between 145 and 255)
	lo := min(base, 255-depth)
	if err := hw.SetNightLight(lo + int(math.Round(float64(depth)*v))); err == nil {
		d.hw.nlPulsing.Store(true)
	}
}

func (d *daemon) endNightLightPulse() {
	if !d.hw.nlPulsing.Swap(false) {
		return
	}
	hw.SetNightLight(d.nightLevel())
}

// flashError shows red on the status LEDs for a second.
func (d *daemon) flashError() {
	d.mu.Lock()
	d.hw.errorUntil = time.Now().Add(time.Second)
	d.mu.Unlock()
	d.updateLEDs()
	time.AfterFunc(1100*time.Millisecond, d.updateLEDs)
}

// wakeEventTypes is what the wake word event may report: each installed model's
// id and phrase (detection), plus near misses and the stop word.
func wakeEventTypes(d *daemon) []string {
	var ts []string
	for _, m := range d.allModels {
		ts = append(ts, m.ID, m.ID+"_near")
	}
	return append(ts, "stop", "stop_near")
}

// reportWake sends a detection (near = a near miss) to Home Assistant.
func (d *daemon) reportWake(id string, near bool) {
	if d.hw.wakeEv == nil {
		return
	}
	t := id
	if near {
		t += "_near"
	}
	d.hw.wakeEv.Trigger(t)
}
