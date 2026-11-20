package main

import (
	"context"
	"log/slog"
	"sync"
	"time"

	esphome "github.com/ygelfand/go-esphome-device"
	"github.com/ygelfand/go-esphome-device/api"
	"google.golang.org/protobuf/proto"
)

// sunInfo follows Home Assistant's sun.sun entity (next_setting /
// next_rising) over the ESPHome API, for a night period from sunset to
// sunrise. Nothing is computed or fetched on the device itself.
type sunInfo struct {
	log *slog.Logger

	mu       sync.Mutex
	conn     *esphome.Conn
	nextSet  time.Time
	nextRise time.Time
	updated  time.Time
}

const sunEntity = "sun.sun"

func (s *sunInfo) Handle(ctx context.Context, c *esphome.Conn, msg proto.Message) error {
	switch m := msg.(type) {
	case *api.SubscribeHomeAssistantStatesRequest:
		s.mu.Lock()
		s.conn = c
		s.mu.Unlock()
		for _, attr := range []string{"next_setting", "next_rising"} {
			if err := c.Send(&api.SubscribeHomeAssistantStateResponse{EntityId: sunEntity, Attribute: attr}); err != nil {
				s.log.Warn("sun subscribe", "err", err)
				return nil
			}
		}
	case *api.HomeAssistantStateResponse:
		if m.GetEntityId() != sunEntity {
			return nil
		}
		t, err := time.Parse(time.RFC3339, m.GetState())
		if err != nil {
			return nil
		}
		s.mu.Lock()
		changed := false
		switch m.GetAttribute() {
		case "next_setting":
			changed = !t.Equal(s.nextSet)
			s.nextSet = t
		case "next_rising":
			changed = !t.Equal(s.nextRise)
			s.nextRise = t
		}
		s.updated = time.Now()
		set, rise := s.nextSet, s.nextRise
		s.mu.Unlock()
		if changed && !set.IsZero() && !rise.IsZero() {
			s.log.Info("sun", "next_sunset", set.Local().Format("Jan 2 15:04"), "next_sunrise", rise.Local().Format("Jan 2 15:04"))
		}
	}
	return nil
}

func (s *sunInfo) ConnClosed(c *esphome.Conn) {
	s.mu.Lock()
	if s.conn == c {
		s.conn = nil
	}
	s.mu.Unlock()
}

// times returns the next sunset and sunrise (ok = known and not stale).
func (s *sunInfo) times() (set, rise time.Time, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	set, rise = s.nextSet, s.nextRise
	// the attributes move on at every sunset/sunrise; anything over two days
	// old means we lost Home Assistant
	ok = !set.IsZero() && !rise.IsZero() && time.Since(s.updated) < 48*time.Hour
	return
}

// sunNight reports whether now is between sunset+startOff and sunrise+endOff.
// HA gives the *next* setting and rising; the previous ones are taken as a
// day earlier (off by a few minutes at most, fine for a night light).
func sunNight(now, nextSet, nextRise time.Time, startOff, endOff time.Duration) bool {
	set := nextSet
	if set.After(nextRise) {
		set = set.Add(-24 * time.Hour) // it is night now: the sunset was yesterday evening
	}
	// this night, and the previous one (a positive end offset reaches past sunrise)
	for _, back := range []time.Duration{0, 24 * time.Hour} {
		s, r := set.Add(-back).Add(startOff), nextRise.Add(-back).Add(endOff)
		if !now.Before(s) && now.Before(r) {
			return true
		}
	}
	return false
}
