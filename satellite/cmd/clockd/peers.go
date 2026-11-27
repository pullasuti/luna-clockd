package main

import (
	"context"
	"log/slog"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/libp2p/zeroconf/v2"
)

// Other clocks on the network: every clockd advertises its web UI as
// _http._tcp with app=clockd in TXT, and browses for the others, so the web UI
// can offer a clock picker. Only the local network (mDNS); each clock keeps
// its own login.

const (
	peerService = "_http._tcp"
	peerEvery   = time.Minute     // browse interval
	peerListen  = 4 * time.Second // per browse
	peerGone    = 5 * time.Minute // not seen this long: dropped
	peerOffline = 3 * time.Minute // not seen this long: shown as offline
)

// peerTXT is what this clock advertises next to its web UI.
func peerTXT(name, friendly string) []string {
	return []string{"app=clockd", "id=" + name, "name=" + friendly, "ver=" + version, "path=/"}
}

type peer struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Version  string    `json:"version"`
	URL      string    `json:"url"`
	IP       string    `json:"-"` // IPv4, for the wake word vote
	Self     bool      `json:"self"`
	Online   bool      `json:"online"`
	LastSeen time.Time `json:"-"`
}

type peerList struct {
	mu   sync.Mutex
	self string
	m    map[string]*peer
}

func newPeers(self string) *peerList { return &peerList{self: self, m: map[string]*peer{}} }

// add records one browse result; only clockd entries, not this clock.
func (p *peerList) add(e *zeroconf.ServiceEntry, now time.Time) {
	txt := map[string]string{}
	for _, t := range e.Text {
		if k, v, ok := strings.Cut(t, "="); ok {
			txt[k] = v
		}
	}
	if txt["app"] != "clockd" || txt["id"] == "" || txt["id"] == p.self || len(e.AddrIPv4) == 0 {
		return
	}
	host := e.AddrIPv4[0].String()
	if e.Port != 80 {
		host = net.JoinHostPort(host, strconv.Itoa(e.Port))
	}
	name := txt["name"]
	if name == "" {
		name = txt["id"]
	}
	p.mu.Lock()
	p.m[txt["id"]] = &peer{ID: txt["id"], Name: name, Version: txt["ver"], URL: "http://" + host + "/",
		IP: e.AddrIPv4[0].String(), LastSeen: now}
	p.mu.Unlock()
}

// list returns the other clocks, by name, with stale ones dropped.
func (p *peerList) list(now time.Time) []peer {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []peer
	for id, x := range p.m {
		age := now.Sub(x.LastSeen)
		if age > peerGone {
			delete(p.m, id)
			continue
		}
		c := *x
		c.Online = age <= peerOffline
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (p *peerList) run(ctx context.Context, log *slog.Logger) {
	for {
		bctx, cancel := context.WithTimeout(ctx, peerListen)
		ch := make(chan *zeroconf.ServiceEntry, 16)
		done := make(chan struct{})
		go func() {
			for e := range ch {
				p.add(e, time.Now())
			}
			close(done)
		}()
		if err := zeroconf.Browse(bctx, peerService, "local.", ch); err != nil {
			log.Debug("clock discovery", "err", err)
		}
		<-bctx.Done()
		cancel()
		select { // Browse closes ch when it ends; don't hang if it didn't
		case <-done:
		case <-time.After(time.Second):
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(peerEvery):
		}
	}
}
