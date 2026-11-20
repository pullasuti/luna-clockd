package main

import (
	"strings"

	"lenovo-satellite/internal/config"
)

// Device names. Two clocks must not share a name (Home Assistant and mDNS
// tell devices apart by it), so a new install takes the end of its MAC address:
// "lenovo-clock-1a2b" / "Lenovo Smart Clock 1A2B". A clock that was set up
// before names were unique keeps "lenovo-clock", so Home Assistant doesn't lose
// it. The choice is stored in state.json and never changes by itself; -name
// and -friendly on the command line still win.

const (
	legacyName     = "lenovo-clock"
	legacyFriendly = "Lenovo Smart Clock"
)

// macSuffix is the last two bytes of the MAC as 4 lowercase hex digits.
func macSuffix(mac string) string {
	h := strings.ToLower(strings.ReplaceAll(mac, ":", ""))
	if len(h) < 4 {
		return "0000"
	}
	return h[len(h)-4:]
}

// chooseNames returns the device name and friendly name. existing: the clock
// was already set up (it has an API key) before this logic existed.
func chooseNames(st config.State, existing bool, mac string) (name, friendly string, changed bool) {
	if st.DeviceName != "" {
		return st.DeviceName, cmp(st.FriendlyName, legacyFriendly), false
	}
	if existing {
		return legacyName, legacyFriendly, true
	}
	sfx := macSuffix(mac)
	return legacyName + "-" + sfx, legacyFriendly + " " + strings.ToUpper(sfx), true
}

// hotspotName is the setup network's name: "LenovoClock-1A2B".
func hotspotName(mac string) string {
	return "LenovoClock-" + strings.ToUpper(macSuffix(mac))
}

func cmp(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
