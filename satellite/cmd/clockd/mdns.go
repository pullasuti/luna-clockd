package main

import (
	"context"
	"log/slog"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/libp2p/zeroconf/v2"
	"github.com/ygelfand/go-esphome-device/mdns"
)

// setupIface is the setup hotspot's interface (setup-ap.sh).
const setupIface = "uap0"

// lanIPs returns the addresses worth advertising: IPv4 first, then routable
// IPv6; no loopback or link-local.
func lanIPs() []net.IP {
	byIface := map[string][]net.IP{}
	ifs, _ := net.Interfaces()
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok {
				byIface[ifc.Name] = append(byIface[ifc.Name], ipn.IP)
			}
		}
	}
	return advertisedIPs(byIface)
}

// advertisedIPs picks the addresses to advertise from each interface's. The
// setup hotspot's address only while it is the only IPv4 address: next to the
// home network's it would send Home Assistant to an address it can't reach
// (HA takes the host from the last discovery when a device is set up or
// reconfigured, e.g. after the setup ends and the key is entered).
func advertisedIPs(byIface map[string][]net.IP) []net.IP {
	var v4, v6, setup []net.IP
	for name, ips := range byIface {
		for _, ip := range ips {
			if ip.IsLoopback() || ip.IsLinkLocalUnicast() {
				continue
			}
			switch ip4 := ip.To4(); {
			case name == setupIface:
				if ip4 != nil {
					setup = append(setup, ip4)
				}
			case ip4 != nil:
				v4 = append(v4, ip4)
			default:
				v6 = append(v6, ip)
			}
		}
	}
	if len(v4) == 0 {
		v4 = setup
	}
	sort.Slice(v4, func(i, j int) bool { return v4[i].String() < v4[j].String() })
	sort.Slice(v6, func(i, j int) bool { return v6[i].String() < v6[j].String() })
	return append(v4, v6...)
}

// runMDNS advertises the ESPHome service and re-registers whenever the
// addresses change. At boot clockd starts before DHCP, and the library only
// reads the addresses once, so without this Home Assistant could be told no
// usable address (or an old one after a move). It looks at the addresses when
// kick fires (wifiWatch: Wi-Fi connected and the address answered; true =
// another network, advertise again even with the same address) and every 15 s
// regardless, for changes nobody announced.
// extraService is another mDNS service advertised next to ESPHome (Sendspin).
type extraService struct {
	Instance, Service string
	Port              int
	TXT               []string
}

func runMDNS(ctx context.Context, log *slog.Logger, kick <-chan bool, base mdns.Config, extra ...extraService) {
	var ad *mdns.Advertiser
	var extras []*zeroconf.Server
	closeExtras := func() {
		for _, s := range extras {
			s.Shutdown()
		}
		extras = nil
	}
	defer closeExtras()
	cur := "-"
	defer func() {
		if ad != nil {
			ad.Close()
		}
	}()
	for {
		ips := lanIPs()
		var parts []string
		for _, ip := range ips {
			parts = append(parts, ip.String())
		}
		key := strings.Join(parts, ",")
		if key != cur {
			if ad != nil {
				ad.Close()
				ad = nil
			}
			closeExtras()
			cur = key
			if len(ips) == 0 {
				log.Info("mDNS: no address yet, waiting")
			} else {
				c := base
				c.IPs = ips
				var err error
				if ad, err = mdns.Advertise(c); err != nil {
					log.Warn("mDNS advertising failed; add the device by IP", "err", err)
					cur = "-" // retry next round
				} else {
					log.Info("mDNS: advertising", "addresses", key)
				}
				for _, e := range extra {
					srv, err := zeroconf.RegisterProxy(e.Instance, e.Service, "local.", e.Port, base.Name, parts, e.TXT, nil)
					if err != nil {
						log.Warn("mDNS: advertising failed", "service", e.Service, "err", err)
						continue
					}
					extras = append(extras, srv)
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case again := <-kick:
			if again && cur != "" {
				cur = "-"
			}
		case <-time.After(15 * time.Second):
		}
	}
}
