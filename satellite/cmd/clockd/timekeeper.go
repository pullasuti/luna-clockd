package main

import (
	"context"
	"log/slog"
	"sync"
	"time"
	_ "time/tzdata" // timezone rules without relying on the firmware's zoneinfo

	esphome "github.com/ygelfand/go-esphome-device"
	"github.com/ygelfand/go-esphome-device/api"
	"google.golang.org/protobuf/proto"

	"lenovo-satellite/internal/config"
	"lenovo-satellite/internal/timesync"
)

// timeKeeper owns the system clock and records the zone Home Assistant sends
// (the zone itself is set at start, see zone.go).
//
// Sources (config.time_source):
//   - homeassistant: GetTimeRequest on every API connection and every 6 h
//   - ntp: SNTP from config.ntp_server only (never a built-in server)
//   - both: either, whichever answers
//   - none: leave the clock alone
type timeKeeper struct {
	log       *slog.Logger
	cfg       *config.Store
	statePath string

	mu       sync.Mutex
	conn     *esphome.Conn
	asked    map[*esphome.Conn]bool
	lastSync time.Time
	lastSrc  string

	restart func() // restart clockd (a new zone takes effect on restart)
}

func newTimeKeeper(log *slog.Logger, cfg *config.Store, statePath string) *timeKeeper {
	tk := &timeKeeper{log: log, cfg: cfg, statePath: statePath, asked: map[*esphome.Conn]bool{}}
	if startZone == "" {
		log.Info("timezone", "tz", "UTC", "note", "no zone from Home Assistant yet")
	} else {
		log.Info("timezone", "tz", startZone)
	}
	return tk
}

func (tk *timeKeeper) useHA() bool {
	s := tk.cfg.Get().TimeSource
	return s == config.TimeHomeAssistant || s == config.TimeBoth
}

func (tk *timeKeeper) useNTP() bool {
	c := tk.cfg.Get()
	return (c.TimeSource == config.TimeNTP || c.TimeSource == config.TimeBoth) && c.NTPServer != ""
}

// Now is the current local time (time.Local is the clock's zone, zone.go).
func (tk *timeKeeper) Now() time.Time { return time.Now() }

// saveZone records the zone Home Assistant sent.
func (tk *timeKeeper) saveZone(name string) {
	if name == "" || name == config.LoadState(tk.statePath).Timezone {
		return
	}
	if _, err := timesync.LoadZone(name); err != nil {
		tk.log.Warn("unknown timezone", "tz", name, "err", err)
		return
	}
	if err := config.UpdateState(tk.statePath, func(st *config.State) { st.Timezone = name }); err != nil {
		tk.log.Warn("save state", "err", err)
		return
	}
	tk.checkZone()
}

// checkZone restarts clockd when the zone in use (setting, else HA's) is no
// longer the one it started with.
func (tk *timeKeeper) checkZone() {
	z := effectiveZone(tk.cfg.Get().Timezone, config.LoadState(tk.statePath).Timezone)
	if z == startZone {
		return
	}
	tk.log.Info("timezone changed: restarting clockd", "from", startZone, "to", z)
	if tk.restart != nil {
		go func() { time.Sleep(time.Second); tk.restart() }()
	}
}

func (tk *timeKeeper) apply(src string, offset time.Duration) {
	stepped, err := timesync.Step(offset, 500*time.Millisecond)
	if err != nil {
		tk.log.Warn("set clock", "source", src, "err", err)
		return
	}
	tk.mu.Lock()
	tk.lastSync, tk.lastSrc = time.Now(), src
	tk.mu.Unlock()
	if stepped {
		tk.log.Info("clock set", "source", src, "offset", offset.Round(time.Millisecond))
	} else {
		tk.log.Debug("clock in sync", "source", src, "offset", offset.Round(time.Millisecond))
	}
}

// Handle is an esphome Handler: it asks every new API connection for the time
// and applies the answer.
func (tk *timeKeeper) Handle(ctx context.Context, c *esphome.Conn, msg proto.Message) error {
	switch m := msg.(type) {
	case *api.DeviceInfoRequest, *api.ListEntitiesRequest, *api.SubscribeStatesRequest:
		tk.mu.Lock()
		first := !tk.asked[c]
		tk.asked[c] = true
		tk.conn = c
		tk.mu.Unlock()
		if first && tk.useHA() {
			go tk.request(c)
		}
	case *api.GetTimeResponse:
		tk.saveZone(m.GetTimezone())
		if tk.useHA() && m.GetEpochSeconds() > 0 {
			ha := time.Unix(int64(m.GetEpochSeconds()), 0)
			// whole-second resolution: only step when off by more than a second
			if off := time.Until(ha); off > time.Second || off < -time.Second {
				tk.apply("homeassistant", off)
			} else {
				tk.mu.Lock()
				tk.lastSync, tk.lastSrc = time.Now(), "homeassistant"
				tk.mu.Unlock()
			}
		}
	}
	return nil
}

func (tk *timeKeeper) ConnClosed(c *esphome.Conn) {
	tk.mu.Lock()
	delete(tk.asked, c)
	if tk.conn == c {
		tk.conn = nil
	}
	tk.mu.Unlock()
}

func (tk *timeKeeper) request(c *esphome.Conn) {
	time.Sleep(500 * time.Millisecond) // let the handshake finish
	if err := c.Send(&api.GetTimeRequest{}); err != nil {
		tk.log.Debug("time request", "err", err)
	}
}

// Run polls NTP (when configured) and refreshes Home Assistant time every 6 h.
func (tk *timeKeeper) Run(ctx context.Context) {
	nextNTP, nextHA := time.Now(), time.Now().Add(6*time.Hour)
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		if tk.useNTP() && !time.Now().Before(nextNTP) {
			c := tk.cfg.Get()
			off, rtt, err := timesync.QueryNTP(c.NTPServer, 3*time.Second)
			if err != nil {
				tk.log.Warn("NTP query failed", "server", c.NTPServer, "err", err)
				nextNTP = time.Now().Add(time.Minute)
			} else {
				tk.log.Debug("NTP", "server", c.NTPServer, "offset", off, "rtt", rtt)
				tk.apply("ntp "+c.NTPServer, off)
				nextNTP = time.Now().Add(time.Duration(c.NTPIntervalMinutes) * time.Minute)
			}
		}
		if tk.useHA() && !time.Now().Before(nextHA) {
			tk.mu.Lock()
			c := tk.conn
			tk.mu.Unlock()
			if c != nil {
				go tk.request(c)
			}
			nextHA = time.Now().Add(6 * time.Hour)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// syncNow asks Home Assistant (and the NTP server, if configured) right away.
func (tk *timeKeeper) syncNow() {
	tk.mu.Lock()
	c := tk.conn
	tk.mu.Unlock()
	if c != nil && tk.useHA() {
		go tk.request(c)
	}
	if tk.useNTP() {
		go func() {
			srv := tk.cfg.Get().NTPServer
			off, _, err := timesync.QueryNTP(srv, 3*time.Second)
			if err != nil {
				tk.log.Warn("NTP query failed", "server", srv, "err", err)
				return
			}
			tk.apply("ntp "+srv, off)
		}()
	}
}
