package main

import "testing"

func TestHAURL(t *testing.T) {
	for _, c := range []struct {
		txt  map[string]string
		ip   string
		port int
		want string
	}{
		{map[string]string{"internal_url": "http://homeassistant.local:8123"}, "192.0.2.5", 8123, "http://192.0.2.5:8123"},
		{map[string]string{"internal_url": "https://ha.example.lan:8123/"}, "192.0.2.5", 8123, "https://ha.example.lan:8123"},
		{map[string]string{"internal_url": "http://192.0.2.9:8123"}, "192.0.2.5", 8123, "http://192.0.2.9:8123"},
		{map[string]string{"base_url": "https://homeassistant.local:8443"}, "192.0.2.5", 8123, "https://192.0.2.5:8443"},
		{map[string]string{}, "192.0.2.5", 8123, "http://192.0.2.5:8123"},
		{map[string]string{"internal_url": "ftp://x"}, "192.0.2.5", 8123, "http://192.0.2.5:8123"},
		{map[string]string{}, "", 8123, ""},
	} {
		if got := haURL(c.txt, c.ip, c.port); got != c.want {
			t.Errorf("haURL(%v, %q) = %q, want %q", c.txt, c.ip, got, c.want)
		}
	}
}
