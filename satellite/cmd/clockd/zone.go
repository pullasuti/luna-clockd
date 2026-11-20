package main

import (
	"path/filepath"
	"time"

	"lenovo-satellite/internal/config"
	"lenovo-satellite/internal/timesync"
)

// Time zone: one zone for everything clockd shows or logs, set once at start.
//
// The zone is the web UI's setting (config.json timezone) or, if that is
// empty, the one Home Assistant sent last (state.json timezone; HA sends
// POSIX rules such as "EET-2EEST,M3.5.0/3,M10.5.0/4", which Go's TZ variable
// doesn't parse, hence timesync.LoadZone). initZone sets time.Local from it
// before anything else runs, so every time.Now(), log line and formatted time
// is in the clock's zone; the firmware's own zone (US Pacific on a reset
// clock) is never used. No zone yet: UTC.
//
// A different zone later (HA reports a new one, or the setting changes) is
// saved and clockd restarts itself to pick it up, instead of switching zones
// under running goroutines. start.sh exports the same saved zone as TZ for its
// own lines and the helpers.

// startZone is the zone clockd started with ("" = none known, UTC).
var startZone string

// effectiveZone is the zone in use for a config / state pair.
func effectiveZone(cfgTZ, stateTZ string) string {
	if cfgTZ != "" {
		return cfgTZ
	}
	return stateTZ
}

// initZone sets time.Local from the saved settings. Call first thing in main.
func initZone(stateDir string) {
	cfgTZ := ""
	if c, err := config.Load(filepath.Join(stateDir, "config.json")); err == nil {
		cfgTZ = c.Get().Timezone
	}
	name := effectiveZone(cfgTZ, config.LoadState(filepath.Join(stateDir, "state.json")).Timezone)
	time.Local = time.UTC
	if name == "" {
		return
	}
	if loc, err := timesync.LoadZone(name); err == nil {
		time.Local, startZone = loc, name
	}
}
