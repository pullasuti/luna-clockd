package main

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	esphome "github.com/ygelfand/go-esphome-device"

	"lenovo-satellite/internal/config"
)

// The FM radio in Home Assistant: an "FM radio" dropdown with Off and the
// favourite stations, or without favourites the stations of the last search
// (with their RDS names; weak ones without a name left out), showing what
// plays on the clock's speaker. HA reads a
// dropdown's options only when it connects, so after a search clockd asks HA
// to reconnect (between voice turns); names learned while playing reach the
// list with the next search or restart.

const radioOff = "Off"

// radioOption is a station's dropdown entry ("91.2 MHz · POOKI").
func radioOption(s config.RadioStation) string {
	o := radioMHz(s.Hz)
	if s.Name != "" {
		o += " · " + s.Name
	}
	return o
}

// radioListed: weak stations without an RDS name stay out of the dropdown (as
// the web UI keeps them behind "Show weak stations"); a name means RDS came
// through, so it is listenable.
func radioListed(s config.RadioStation) bool { return s.PilotDB >= 35 || s.Name != "" }

func radioMHz(hz int) string { return fmt.Sprintf("%.1f MHz", float64(hz)/1e6) }

// radioOptionHz is the station of an entry: its leading frequency.
func radioOptionHz(o string) (int, bool) {
	f, err := strconv.ParseFloat(strings.TrimSuffix(strings.Fields(o + " MHz")[0], "MHz"), 64)
	if err != nil || f < 76 || f > 108 {
		return 0, false
	}
	return int(math.Round(f*10)) * fmStep, true
}

// radioOptions: Off, then the favourites (in their order) when there are
// any, else every listed station of the last search.
func (d *daemon) radioOptions() []string {
	opts := []string{radioOff}
	st := config.LoadState(d.statePath)
	if len(st.RadioFavs) > 0 {
		for _, hz := range st.RadioFavs {
			s := config.RadioStation{Hz: hz}
			for _, x := range st.RadioStations {
				if x.Hz == hz {
					s.Name = x.Name
				}
			}
			opts = append(opts, radioOption(s))
		}
		return opts
	}
	for _, s := range st.RadioStations {
		if radioListed(s) {
			opts = append(opts, radioOption(s))
		}
	}
	return opts
}

// radioAddEntity registers the dropdown (at startup when stations are known,
// or after the first search that found some).
func (d *daemon) radioAddEntity() {
	r := &d.radio
	opts := d.radioOptions()
	sel := &esphome.Select{
		Base:    esphome.Base{ObjectID: "fm_radio", Name: "FM radio", Icon: "mdi:radio"},
		Options: opts,
		OnCommand: func(v string) {
			if v == radioOff {
				d.radioStop()
			} else if hz, ok := radioOptionHz(v); ok {
				if err := d.radioPlay(hz); err != nil {
					d.log.Warn("radio from Home Assistant", "err", err)
				}
			}
			d.radioSync()
		},
	}
	sel.Set(radioOff)
	if err := d.ents.Add(sel); err != nil {
		d.log.Warn("radio: Home Assistant dropdown", "err", err)
		return
	}
	r.mu.Lock()
	r.sel, r.opts = sel, opts
	r.mu.Unlock()
}

// radioSync shows in the dropdown what plays on the speaker (a frequency that
// is not in the list shows as unknown in HA).
func (d *daemon) radioSync() {
	r := &d.radio
	r.mu.Lock()
	sel, hz, on := r.sel, r.want, r.speaker
	v := radioOff
	if on && hz != 0 {
		v = radioMHz(hz)
		for _, o := range r.opts {
			if h, ok := radioOptionHz(o); ok && h == hz {
				v = o
			}
		}
	}
	r.mu.Unlock()
	if sel != nil && sel.Get() != v {
		sel.Set(v)
	}
}

// radioStationsChanged gives the dropdown the new station list after a search
// and lets HA reconnect to read it (not during a voice turn: then it sees the
// list at its next reconnect).
func (d *daemon) radioStationsChanged() {
	opts := d.radioOptions()
	r := &d.radio
	r.mu.Lock()
	sel, same := r.sel, slices.Equal(opts, r.opts)
	if sel != nil {
		r.opts = opts
	}
	r.mu.Unlock()
	switch {
	case sel == nil && len(opts) > 1 && d.ents != nil:
		d.radioAddEntity() // the first stations: the dropdown appears
	case sel == nil || same:
		return
	default:
		sel.SetOptions(opts)
	}
	d.mu.Lock()
	idle := d.st == stIdle
	d.mu.Unlock()
	if idle && d.esp != nil {
		d.log.Info("radio: new station list, Home Assistant reconnects to read it", "stations", len(opts)-1)
		d.esp.Reconnect()
	}
}
