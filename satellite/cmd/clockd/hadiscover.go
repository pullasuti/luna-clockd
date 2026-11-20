package main

import (
	"context"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/libp2p/zeroconf/v2"
)

// Finding Home Assistant for the API settings: HA announces itself over mDNS as
// _home-assistant._tcp, with its name, version and URLs in TXT. The clock
// can't look up .local names (no mDNS resolver for Go's lookups here), so a
// .local URL is offered by its IPv4 address instead.

type haFound struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
	URL     string `json:"url"`
}

// haURL picks the URL the clock should use for one announcement.
func haURL(txt map[string]string, ipv4 string, port int) string {
	raw := txt["internal_url"]
	if raw == "" {
		raw = txt["base_url"]
	}
	u, err := url.Parse(strings.TrimRight(raw, "/"))
	if err == nil && u.Host != "" && (u.Scheme == "http" || u.Scheme == "https") &&
		!strings.HasSuffix(strings.ToLower(u.Hostname()), ".local") {
		return u.String()
	}
	if ipv4 == "" {
		return ""
	}
	scheme := "http"
	if err == nil && u.Scheme == "https" {
		scheme = "https"
		if p := u.Port(); p != "" {
			port, _ = strconv.Atoi(p)
		}
	}
	return scheme + "://" + net.JoinHostPort(ipv4, strconv.Itoa(port))
}

func haFromEntry(e *zeroconf.ServiceEntry) (haFound, bool) {
	txt := map[string]string{}
	for _, t := range e.Text {
		if k, v, ok := strings.Cut(t, "="); ok {
			txt[k] = v
		}
	}
	ip := ""
	if len(e.AddrIPv4) > 0 {
		ip = e.AddrIPv4[0].String()
	}
	u := haURL(txt, ip, e.Port)
	if u == "" {
		return haFound{}, false
	}
	name := txt["location_name"]
	if name == "" {
		name = e.Instance
	}
	return haFound{Name: name, Version: txt["version"], URL: u}, true
}

// FindHomeAssistant browses for Home Assistant for a few seconds (a second
// round when the first one finds nothing: an answer can get lost, e.g. right
// after clockd started).
func (b *webBackend) FindHomeAssistant() any {
	seen := map[string]haFound{}
	for round := 0; round < 2 && len(seen) == 0; round++ {
		b.browseHA(seen)
	}
	out := []haFound{}
	for _, f := range seen {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name+out[i].URL < out[j].Name+out[j].URL })
	return out
}

func (b *webBackend) browseHA(seen map[string]haFound) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ch := make(chan *zeroconf.ServiceEntry, 8)
	done := make(chan struct{})
	go func() {
		for e := range ch {
			if f, ok := haFromEntry(e); ok {
				seen[f.URL] = f
			}
		}
		close(done)
	}()
	if err := zeroconf.Browse(ctx, "_home-assistant._tcp", "local.", ch); err != nil {
		b.d.log.Debug("Home Assistant discovery", "err", err)
	}
	<-ctx.Done()
	select { // Browse closes ch when it ends; don't hang if it didn't
	case <-done:
	case <-time.After(time.Second):
	}
}
