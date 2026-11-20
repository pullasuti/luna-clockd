package main

import (
	"context"
	"log/slog"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	esphome "github.com/ygelfand/go-esphome-device"
	"github.com/ygelfand/go-esphome-device/api"
	"google.golang.org/protobuf/proto"

	"lenovo-satellite/internal/config"
	"lenovo-satellite/internal/haapi"
)

// weatherIcons maps Home Assistant weather conditions to clock-face icons.
var weatherIcons = map[string][]string{
	"sunny":           {"sun"},
	"clear-night":     {"sun"},
	"partlycloudy":    {"sun", "cloudy"},
	"cloudy":          {"cloudy"},
	"fog":             {"fog"},
	"rainy":           {"rain"},
	"pouring":         {"rain", "cloudy"},
	"lightning":       {"thunder"},
	"lightning-rainy": {"thunder", "rain"},
	"snowy":           {"snow"},
	"snowy-rainy":     {"snow", "rain"},
	"hail":            {"snow", "rain"},
	"windy":           {"wind"},
	"windy-variant":   {"wind", "cloudy"},
	"exceptional":     {"warning"},
}

// weatherView is what the clock face shows.
type weatherView struct {
	Valid     bool
	Temp      int
	Unit      string // "C" or "F"
	Icons     []string
	Condition string
	Updated   time.Time
}

// weather subscribes to a user-selected Home Assistant weather entity over the
// ESPHome API: its state (condition) and the temperature / temperature_unit
// attributes. Home Assistant pushes changes; nothing is fetched from the internet.
type weather struct {
	log *slog.Logger
	cfg *config.Store

	sel *esphome.Select // "Weather entity" dropdown on the Home Assistant device page
	ha  *haapi.Manager  // lists the weather entities for its options

	mu         sync.Mutex
	conn       *esphome.Conn
	subscribed string // entity the current connection is subscribed to
	condition  string
	temp       float64
	hasTemp    bool
	unit       string
	updated    time.Time
}

// noneOption stands for "no weather on the clock" in the Home Assistant
// dropdown (a select option can't be empty).
const noneOption = "(none)"

// entitySelect builds the config select, so the weather entity can be picked
// from a dropdown in Home Assistant as well as in the web UI.
func (w *weather) entitySelect() *esphome.Select {
	w.sel = &esphome.Select{
		Base:    esphome.Base{ObjectID: "weather_entity", Name: "Weather entity", Category: esphome.CategoryConfig, Icon: "mdi:weather-partly-rainy"},
		Options: []string{noneOption, w.entity()},
		OnCommand: func(v string) {
			if v == noneOption {
				v = ""
			}
			if err := w.cfg.Update(func(c *config.Config) { c.WeatherEntity = v }); err != nil {
				w.log.Warn("save weather entity", "err", err)
			}
			w.sel.Set(w.option(w.entity()))
			w.subscribe()
		},
	}
	w.sel.Set(w.option(w.entity()))
	return w.sel
}

// option is how the clock's entity id appears in the dropdown.
func (w *weather) option(id string) string {
	if strings.TrimSpace(id) == "" {
		return noneOption
	}
	return id
}

// refreshOptions lists the weather entities from Home Assistant (the HA API
// from the web UI settings) and makes them the dropdown's options.
func (w *weather) refreshOptions() {
	if w.ha == nil || !w.ha.Configured() || w.sel == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	list, err := w.ha.Entities(ctx, "weather")
	if err != nil {
		return // keep the previous options
	}
	opts := []string{noneOption}
	for _, e := range list {
		opts = append(opts, e.ID)
	}
	w.sel.SetOptions(opts)
	if cur := w.option(w.entity()); cur != noneOption && !slices.Contains(opts, cur) {
		opts = append(opts, cur) // a typed-in entity that HA doesn't list stays choosable
		w.sel.SetOptions(opts)
	}
}

func (w *weather) entity() string { return strings.TrimSpace(w.cfg.Get().WeatherEntity) }

func (w *weather) Handle(ctx context.Context, c *esphome.Conn, msg proto.Message) error {
	switch m := msg.(type) {
	case *api.SubscribeHomeAssistantStatesRequest:
		w.mu.Lock()
		w.conn, w.subscribed = c, ""
		w.mu.Unlock()
		go w.refreshOptions() // each HA connection re-lists the entities
		w.subscribe()
	case *api.HomeAssistantStateResponse:
		w.onState(m.GetEntityId(), m.GetAttribute(), m.GetState())
	}
	return nil
}

func (w *weather) ConnClosed(c *esphome.Conn) {
	w.mu.Lock()
	if w.conn == c {
		w.conn, w.subscribed = nil, ""
	}
	w.mu.Unlock()
}

// subscribe asks Home Assistant for the configured entity. Called on connect
// and when the setting changes (Home Assistant accepts new subscriptions at any time).
func (w *weather) subscribe() {
	ent := w.entity()
	w.mu.Lock()
	c := w.conn
	if c == nil || ent == "" || ent == w.subscribed {
		if ent == "" {
			w.condition, w.hasTemp = "", false
		}
		w.mu.Unlock()
		return
	}
	w.subscribed = ent
	w.condition, w.hasTemp, w.unit = "", false, ""
	w.mu.Unlock()
	for _, attr := range []string{"", "temperature", "temperature_unit"} {
		if err := c.Send(&api.SubscribeHomeAssistantStateResponse{EntityId: ent, Attribute: attr}); err != nil {
			w.log.Warn("weather subscribe", "err", err)
			return
		}
	}
	w.log.Info("weather entity subscribed", "entity", ent)
}

func (w *weather) onState(entity, attr, value string) {
	if entity != w.entity() {
		return // an old subscription
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	switch attr {
	case "":
		w.condition = value
		if value == "unavailable" || value == "unknown" {
			w.condition = ""
		}
		w.log.Info("weather", "condition", value)
	case "temperature":
		v, err := strconv.ParseFloat(value, 64)
		w.temp, w.hasTemp = v, err == nil
	case "temperature_unit":
		w.unit = value
	}
	w.updated = time.Now()
}

func (w *weather) view() weatherView {
	w.mu.Lock()
	defer w.mu.Unlock()
	v := weatherView{Condition: w.condition, Updated: w.updated, Icons: weatherIcons[w.condition]}
	// a value Home Assistant hasn't refreshed for 6 h is not shown
	if w.hasTemp && time.Since(w.updated) < 6*time.Hour {
		v.Valid = true
		v.Temp = int(math.Round(w.temp))
		v.Unit = "C"
		if strings.Contains(strings.ToUpper(w.unit), "F") {
			v.Unit = "F"
		}
	}
	return v
}
