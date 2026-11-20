package main

import (
	"testing"

	"lenovo-satellite/internal/config"
)

func TestChooseNames(t *testing.T) {
	const mac = "10:2C:6B:00:1A:2B"
	for _, c := range []struct {
		why            string
		st             config.State
		existing       bool
		name, friendly string
		changed        bool
	}{
		{"new clock", config.State{}, false, "lenovo-clock-1a2b", "Lenovo Smart Clock 1A2B", true},
		{"set up before unique names", config.State{}, true, "lenovo-clock", "Lenovo Smart Clock", true},
		{"stored", config.State{DeviceName: "kitchen-clock", FriendlyName: "Kitchen"}, false, "kitchen-clock", "Kitchen", false},
		{"stored without friendly", config.State{DeviceName: "lenovo-clock"}, true, "lenovo-clock", "Lenovo Smart Clock", false},
	} {
		n, f, ch := chooseNames(c.st, c.existing, mac)
		if n != c.name || f != c.friendly || ch != c.changed {
			t.Errorf("%s: got %q %q %v", c.why, n, f, ch)
		}
	}
	if h := hotspotName(mac); h != "LenovoClock-1A2B" {
		t.Errorf("hotspot %q", h)
	}
}
