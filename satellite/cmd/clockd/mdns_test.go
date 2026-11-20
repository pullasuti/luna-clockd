package main

import (
	"fmt"
	"net"
	"testing"
)

func TestAdvertisedIPs(t *testing.T) {
	ip := net.ParseIP
	for _, c := range []struct {
		in   map[string][]net.IP
		want string
	}{
		// setup only: phones on the hotspot find the clock by name
		{map[string][]net.IP{"uap0": {ip("192.168.255.249"), ip("fe80::1")}}, "[192.168.255.249]"},
		// joined the home network while the hotspot is still up: home address only
		{map[string][]net.IP{"uap0": {ip("192.168.255.249")}, "wlan0": {ip("192.0.2.10"), ip("fe80::2"), ip("2001:db8::5")}},
			"[192.0.2.10 2001:db8::5]"},
		{map[string][]net.IP{"wlan0": {ip("192.0.2.10")}, "lo": {ip("127.0.0.1")}}, "[192.0.2.10]"},
		{map[string][]net.IP{}, "[]"},
	} {
		if got := fmt.Sprint(advertisedIPs(c.in)); got != c.want {
			t.Errorf("%v: %s, want %s", c.in, got, c.want)
		}
	}
}
