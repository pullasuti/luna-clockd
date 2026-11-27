package main

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"strconv"
	"strings"
	"sync"
	"time"

	esphome "github.com/ygelfand/go-esphome-device"

	"lenovo-satellite/internal/config"
	"lenovo-satellite/internal/output"
)

// Sleep sounds and the sleep timer: local, no Home Assistant needed.
//
// A sleep sound is generated noise (output.Noise) played as the clock's media,
// so everything that stops media stops it too: the play button, "stop", the
// web UI, a new track from Home Assistant. The sleep timer fades out whatever
// plays as media (a sleep sound, the USB player, HA media, Sendspin) over its
// last minute and then stops it.
//
// The last sound and timer length are remembered (state.json): a sleep sound
// started while none plays and no timer runs gets the last timer again, so
// "rain" from Home Assistant at bedtime stops by itself like last night.
// While a timer runs, the face's small digits show its minutes left instead
// of the temperature.

const lvlSleep = "sleep" // the timer's fade, one of the media level requests

// sleepFade is how long the timer takes to fade the sound out at the end.
const sleepFade = 60 * time.Second

// sleepTimerChoices are the timer lengths offered (minutes).
var sleepTimerChoices = []int{15, 30, 45, 60, 90, 120}

type sleepCtl struct {
	d *daemon

	mu      sync.Mutex
	sound   string // the sleep sound playing ("" = none)
	voice   *output.Voice
	end     time.Time // the timer's end (zero = no timer)
	total   time.Duration
	cancel  context.CancelFunc
	soundEl *esphome.Select // Home Assistant dropdowns
	timerEl *esphome.Select
}

func newSleepCtl(d *daemon) *sleepCtl { return &sleepCtl{d: d} }

const offOption = "Off"

func soundName(id string) string {
	for _, s := range output.NoiseSounds {
		if s.ID == id {
			return s.Name
		}
	}
	return ""
}

func soundByName(name string) string {
	for _, s := range output.NoiseSounds {
		if s.Name == name {
			return s.ID
		}
	}
	return ""
}

func timerOption(min int) string { return fmt.Sprintf("%d min", min) }

// entities are the Home Assistant dropdowns: the sleep sound and the timer.
func (s *sleepCtl) entities() []esphome.Entity {
	opts := []string{offOption}
	for _, n := range output.NoiseSounds {
		opts = append(opts, n.Name)
	}
	s.soundEl = &esphome.Select{
		Base:    esphome.Base{ObjectID: "sleep_sound", Name: "Sleep sound", Icon: "mdi:weather-night"},
		Options: opts,
		OnCommand: func(v string) {
			if v == offOption {
				s.stopSound()
				return
			}
			if id := soundByName(v); id != "" {
				_ = s.play(id)
			}
		},
	}
	s.soundEl.Set(offOption)
	topts := []string{offOption}
	for _, m := range sleepTimerChoices {
		topts = append(topts, timerOption(m))
	}
	s.timerEl = &esphome.Select{
		Base:    esphome.Base{ObjectID: "sleep_timer", Name: "Sleep timer", Icon: "mdi:timer-sand"},
		Options: topts,
		OnCommand: func(v string) {
			m, _ := strconv.Atoi(strings.TrimSuffix(v, " min"))
			_ = s.setTimer(m) // "Off" -> 0 cancels
		},
	}
	s.timerEl.Set(offOption)
	return []esphome.Entity{s.soundEl, s.timerEl}
}

// play starts sleep sound id as the clock's media (replacing any media).
func (s *sleepCtl) play(id string) error {
	if !output.NoiseKnown(id) {
		return fmt.Errorf("unknown sleep sound %q", id)
	}
	d := s.d
	s.mu.Lock()
	fresh := s.sound == "" && s.end.IsZero()
	s.mu.Unlock()
	d.stopMedia()
	if d.sendspin != nil && d.sendspin.Playing() {
		d.sendspin.Pause()
	}
	src := output.NewNoise(id, rand.Uint64())
	v := d.out.Play(src, 1, "media")
	d.mu.Lock()
	d.media, d.mediaBuf, d.mediaName, d.mediaSrc = v, nil, "Sleep sound: "+soundName(id), "sleep"
	d.mu.Unlock()
	d.player.SetState(esphome.MediaPlayerPlaying)
	s.mu.Lock()
	s.sound, s.voice = id, v
	s.mu.Unlock()
	if s.soundEl != nil {
		s.soundEl.Set(soundName(id))
	}
	d.log.Info("sleep sound", "sound", id)
	var last *int
	if d.statePath != "" {
		if err := config.UpdateState(d.statePath, func(st *config.State) { st.SleepSound, last = id, st.SleepTimer }); err != nil {
			d.log.Warn("sleep sound: state not saved", "err", err)
		}
	}
	if fresh && last != nil && *last > 0 {
		d.log.Info("sleep timer: the last one again", "minutes", *last)
		_ = s.setTimer(*last)
	}
	go func() {
		<-v.Done() // stopped: stopMedia, another media, the timer
		d.mu.Lock()
		if d.media == v {
			d.media, d.mediaBuf, d.mediaName, d.mediaSrc = nil, nil, "", ""
		}
		d.mu.Unlock()
		s.mu.Lock()
		mine := s.voice == v
		if mine {
			s.sound, s.voice = "", nil
		}
		s.mu.Unlock()
		if mine && s.soundEl != nil {
			s.soundEl.Set(offOption)
		}
	}()
	return nil
}

// stopSound stops the sleep sound if one plays (not other media).
func (s *sleepCtl) stopSound() {
	s.mu.Lock()
	v := s.voice
	s.mu.Unlock()
	if v == nil {
		return
	}
	s.d.mu.Lock()
	cur := s.d.media == v
	s.d.mu.Unlock()
	if cur {
		s.d.stopMedia()
	} else {
		s.d.out.Stop(v)
	}
}

// setTimer starts (or restarts) the sleep timer; 0 cancels it.
func (s *sleepCtl) setTimer(minutes int) error {
	if minutes < 0 || minutes > 12*60 {
		return errors.New("sleep timer: 0-720 minutes")
	}
	if s.d.statePath != "" {
		if err := config.UpdateState(s.d.statePath, func(st *config.State) { st.SleepTimer = &minutes }); err != nil {
			s.d.log.Warn("sleep timer: state not saved", "err", err)
		}
	}
	s.mu.Lock()
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
	s.end, s.total = time.Time{}, 0
	s.mu.Unlock()
	s.d.music.clear(lvlSleep)
	if minutes == 0 {
		s.d.log.Info("sleep timer off")
		if s.timerEl != nil {
			s.timerEl.Set(offOption)
		}
		return nil
	}
	total := time.Duration(minutes) * time.Minute
	ctx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	s.end, s.total, s.cancel = time.Now().Add(total), total, cancel
	end := s.end
	s.mu.Unlock()
	if s.timerEl != nil {
		s.timerEl.Set(timerOption(minutes))
	}
	s.d.log.Info("sleep timer", "minutes", minutes)
	go s.run(ctx, end)
	return nil
}

// run waits for the timer, fades the media out over its last minute, then
// stops it.
func (s *sleepCtl) run(ctx context.Context, end time.Time) {
	fadeAt := end.Add(-sleepFade)
	select {
	case <-ctx.Done():
		return
	case <-time.After(time.Until(fadeAt)):
	}
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		left := time.Until(end)
		if left <= 0 {
			break
		}
		f := float32(left) / float32(sleepFade)
		s.d.music.set(lvlSleep, f*f) // quadratic: sounds like an even fade
		select {
		case <-ctx.Done():
			return // cancelled or restarted (setTimer cleared the fade)
		case <-tick.C:
		}
	}
	s.d.log.Info("sleep timer: stopping playback")
	s.d.music.set(lvlSleep, 0)
	s.d.stopMedia()
	if sp := s.d.sendspin; sp != nil && sp.Playing() {
		sp.Pause()
	}
	time.Sleep(300 * time.Millisecond) // let the mixer drop the voices before the level returns
	s.mu.Lock()
	if ctx.Err() == nil {
		s.end, s.total, s.cancel = time.Time{}, 0, nil
	}
	s.mu.Unlock()
	if ctx.Err() == nil {
		s.d.music.clear(lvlSleep)
		if s.timerEl != nil {
			s.timerEl.Set(offOption)
		}
	}
}

// left is the time left on the sleep timer (0 = no timer).
func (s *sleepCtl) left() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.end.IsZero() {
		return 0
	}
	return max(time.Until(s.end), 0)
}

// Status is the web UI's view.
func (s *sleepCtl) Status() map[string]any {
	s.mu.Lock()
	sound, total, end := s.sound, s.total, s.end
	s.mu.Unlock()
	sounds := make([]map[string]string, 0, len(output.NoiseSounds))
	for _, n := range output.NoiseSounds {
		sounds = append(sounds, map[string]string{"id": n.ID, "name": n.Name})
	}
	left := 0
	if !end.IsZero() {
		left = int(max(time.Until(end), 0).Seconds())
	}
	m := map[string]any{
		"sound":         sound,
		"sounds":        sounds,
		"timer_left":    left,
		"timer_total":   int(total.Seconds()),
		"timer_choices": sleepTimerChoices,
	}
	if s.d.statePath != "" {
		st := config.LoadState(s.d.statePath)
		m["last_sound"] = st.SleepSound
		if st.SleepTimer != nil {
			m["last_timer"] = *st.SleepTimer
		}
	}
	return m
}
