// Package alarms holds the clock's own alarms: what they are, when each rings
// next, and the file they are kept in. The ringing itself is clockd's.
package alarms

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Max is how many alarms a clock keeps.
const Max = 10

// Alarm is one alarm. Days empty = rings once (then turns itself off).
type Alarm struct {
	ID      string `json:"id"`
	Time    string `json:"time"`           // "HH:MM", 24 h, local time
	Days    []int  `json:"days,omitempty"` // 0 = Sunday ... 6 = Saturday
	On      bool   `json:"on"`
	Label   string `json:"label,omitempty"`
	Sound   string `json:"sound,omitempty"` // a timer sound name; "" = the clock's timer sound
	Sunrise bool   `json:"sunrise,omitempty"`
}

// HM is the alarm's hour and minute.
func (a Alarm) HM() (h, m int, err error) {
	hs, ms, ok := strings.Cut(a.Time, ":")
	if !ok || len(hs) == 0 || len(hs) > 2 || len(ms) != 2 {
		return 0, 0, fmt.Errorf("time %q: want HH:MM", a.Time)
	}
	h, e1 := strconv.Atoi(hs)
	m, e2 := strconv.Atoi(ms)
	if e1 != nil || e2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, 0, fmt.Errorf("time %q: want HH:MM", a.Time)
	}
	return h, m, nil
}

// Repeats reports whether the alarm rings on chosen days (not once).
func (a Alarm) Repeats() bool { return len(a.Days) > 0 }

func (a Alarm) onDay(w time.Weekday) bool {
	if len(a.Days) == 0 {
		return true
	}
	for _, d := range a.Days {
		if time.Weekday(d) == w {
			return true
		}
	}
	return false
}

// Next is the first time after t the alarm rings, in t's location; zero when
// it is off or invalid.
func (a Alarm) Next(t time.Time) time.Time {
	h, m, err := a.HM()
	if !a.On || err != nil {
		return time.Time{}
	}
	y, mo, d := t.Date()
	for i := 0; i <= 7; i++ {
		c := time.Date(y, mo, d+i, h, m, 0, 0, t.Location())
		if c.After(t) && a.onDay(c.Weekday()) {
			return c
		}
	}
	return time.Time{}
}

// List is all of a clock's alarms.
type List []Alarm

// Next is the alarm that rings first after t.
func (l List) Next(t time.Time) (time.Time, Alarm, bool) {
	var best time.Time
	var which Alarm
	for _, a := range l {
		if n := a.Next(t); !n.IsZero() && (best.IsZero() || n.Before(best)) {
			best, which = n, a
		}
	}
	return best, which, !best.IsZero()
}

// AnyOn reports whether an alarm is switched on.
func (l List) AnyOn() bool {
	for _, a := range l {
		if a.On {
			return true
		}
	}
	return false
}

// Clean checks a list from the web UI or Home Assistant: valid times and
// days, ids given to new alarms, days sorted and unique, at most Max.
func (l List) Clean(sounds map[string]bool) (List, error) {
	if len(l) > Max {
		return nil, fmt.Errorf("at most %d alarms", Max)
	}
	out := make(List, 0, len(l))
	ids := map[string]bool{}
	for i, a := range l {
		if _, _, err := a.HM(); err != nil {
			return nil, fmt.Errorf("alarm %d: %w", i+1, err)
		}
		h, m, _ := a.HM()
		a.Time = fmt.Sprintf("%02d:%02d", h, m)
		seen := map[int]bool{}
		var days []int
		for _, d := range a.Days {
			if d < 0 || d > 6 {
				return nil, fmt.Errorf("alarm %d: day %d (want 0-6, Sunday = 0)", i+1, d)
			}
			if !seen[d] {
				seen[d] = true
				days = append(days, d)
			}
		}
		sort.Ints(days)
		a.Days = days
		if len([]rune(a.Label)) > 40 {
			return nil, fmt.Errorf("alarm %d: label longer than 40 characters", i+1)
		}
		if a.Sound != "" && sounds != nil && !sounds[a.Sound] {
			return nil, fmt.Errorf("alarm %d: unknown sound %q", i+1, a.Sound)
		}
		if a.ID == "" || ids[a.ID] || len(a.ID) > 32 {
			a.ID = newID(ids)
		}
		ids[a.ID] = true
		out = append(out, a)
	}
	return out, nil
}

func newID(taken map[string]bool) string {
	for n := time.Now().UnixNano(); ; n++ {
		id := strconv.FormatInt(n%1e9, 36)
		if !taken[id] {
			return id
		}
	}
}

// Load reads the alarms file; a missing file is an empty list.
func Load(path string) (List, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return List{}, nil
	}
	if err != nil {
		return nil, err
	}
	var l List
	if err := json.Unmarshal(data, &l); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return l, nil
}

// Save writes the list durably (data synced before the rename: UBIFS can
// otherwise leave a zero-byte file after a power cut).
func Save(path string, l List) error {
	if l == nil {
		l = List{}
	}
	data, _ := json.MarshalIndent(l, "", "  ")
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
