package main

import (
	"net"
	"testing"
	"time"

	"github.com/libp2p/zeroconf/v2"
)

func TestPeerList(t *testing.T) {
	p := newPeers("lenovo-clock")
	now := time.Now()
	entry := func(port int, ip string, txt ...string) *zeroconf.ServiceEntry {
		return &zeroconf.ServiceEntry{Port: port, Text: txt, AddrIPv4: []net.IP{net.ParseIP(ip)}}
	}
	p.add(entry(80, "10.0.0.9", "app=clockd", "id=lenovo-clock-d202", "name=Lenovo Smart Clock D202", "ver=0.53.0"), now.Add(-4*time.Minute))
	p.add(entry(8080, "10.0.0.7", "app=clockd", "id=kitchen"), now)
	p.add(entry(80, "10.0.0.8", "app=clockd", "id=lenovo-clock"), now) // this clock
	p.add(entry(80, "10.0.0.3", "id=printer"), now)                    // not a clock
	p.add(entry(80, "10.0.0.4", "app=clockd", "id=old"), now.Add(-6*time.Minute))
	got := p.list(now)
	if len(got) != 2 {
		t.Fatalf("got %d clocks: %+v", len(got), got)
	}
	if got[0].Name != "Lenovo Smart Clock D202" || got[0].URL != "http://10.0.0.9/" || got[0].Online || got[0].Version != "0.53.0" {
		t.Errorf("d202: %+v", got[0])
	}
	if got[1].Name != "kitchen" || got[1].URL != "http://10.0.0.7:8080/" || !got[1].Online {
		t.Errorf("kitchen: %+v", got[1])
	}
}
