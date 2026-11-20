// Package dlna is a small UPnP/DLNA stack for the clock: SSDP discovery, the
// device and service descriptions, SOAP control and GENA eventing, with two
// devices on top: a MediaServer for the USB drive (ContentDirectory) and a
// MediaRenderer (AVTransport + RenderingControl) that plays what a control
// point (Home Assistant, BubbleUPnP, ...) sends it. Written for clockd from
// the UPnP Device Architecture 1.0 and the AV specs; no code taken from
// elsewhere. Unauthenticated by nature (UPnP has no auth): clockd runs it only
// when switched on.
package dlna

import (
	"bufio"
	"bytes"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	ssdpAddr   = "239.255.255.250:1900"
	ssdpMaxAge = 1800
)

// advert is what one device announces: its UUID, description URL path and
// the notification types (root device, uuid, device type, service types).
type advert struct {
	uuid     string
	descPath string
	types    []string // deviceType and serviceTypes
}

func (a advert) targets() []string {
	return append([]string{"upnp:rootdevice", "uuid:" + a.uuid}, a.types...)
}

func (a advert) usn(nt string) string {
	if nt == "uuid:"+a.uuid {
		return nt
	}
	return "uuid:" + a.uuid + "::" + nt
}

// ssdp answers M-SEARCH and sends NOTIFY alive/byebye for the adverts.
type ssdp struct {
	log    *slog.Logger
	port   int // HTTP port of the descriptions
	server string

	mu      sync.Mutex
	adverts []advert
	conn    *net.UDPConn
	stop    chan struct{}
	done    sync.WaitGroup
	ifname  string
}

func (s *ssdp) start(ifname string) error {
	ifi, err := net.InterfaceByName(ifname)
	if err != nil {
		return err
	}
	group, _ := net.ResolveUDPAddr("udp4", ssdpAddr)
	c, err := net.ListenMulticastUDP("udp4", ifi, group)
	if err != nil {
		return fmt.Errorf("ssdp: %w", err)
	}
	s.mu.Lock()
	s.conn, s.stop, s.ifname = c, make(chan struct{}), ifname
	s.mu.Unlock()
	s.done.Add(2)
	go s.listen(c)
	go s.announceLoop()
	return nil
}

func (s *ssdp) close() {
	s.mu.Lock()
	c, stop := s.conn, s.stop
	s.conn, s.stop = nil, nil
	s.mu.Unlock()
	if c == nil {
		return
	}
	close(stop)
	c.Close()
	s.done.Wait()
	s.notifyAll("ssdp:byebye")
}

func (s *ssdp) setAdverts(a []advert) {
	s.mu.Lock()
	old := s.adverts
	s.adverts = a
	running := s.conn != nil
	s.mu.Unlock()
	if !running {
		return
	}
	// devices that went away say goodbye, new ones say hello
	keep := map[string]bool{}
	for _, x := range a {
		keep[x.uuid] = true
	}
	for _, x := range old {
		if !keep[x.uuid] {
			s.notify(x, "ssdp:byebye")
		}
	}
	s.notifyAll("ssdp:alive")
}

// localIP is the address of ours the peer reaches (no packet is sent).
func localIP(peer net.IP) net.IP {
	c, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: peer, Port: 1900})
	if err != nil {
		return nil
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).IP
}

func (s *ssdp) ifaceIP() net.IP {
	ifi, err := net.InterfaceByName(s.ifname)
	if err != nil {
		return nil
	}
	addrs, _ := ifi.Addrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil {
			return n.IP.To4()
		}
	}
	return nil
}

func (s *ssdp) listen(c *net.UDPConn) {
	defer s.done.Done()
	buf := make([]byte, 2048)
	for {
		n, from, err := c.ReadFromUDP(buf)
		if err != nil {
			return
		}
		st, mx, ok := parseMSearch(buf[:n])
		if !ok {
			continue
		}
		s.mu.Lock()
		ads := s.adverts
		s.mu.Unlock()
		var replies []string
		for _, a := range ads {
			for _, nt := range a.targets() {
				if st == "ssdp:all" || st == nt {
					replies = append(replies, a.descPath+"\x00"+nt+"\x00"+a.usn(nt))
				}
			}
		}
		if len(replies) == 0 {
			continue
		}
		go s.reply(from, mx, replies)
	}
}

// parseMSearch reads an M-SEARCH request: its search target and MX.
func parseMSearch(b []byte) (st string, mx int, ok bool) {
	r := bufio.NewReader(bytes.NewReader(b))
	req, err := http.ReadRequest(r)
	if err != nil || req.Method != "M-SEARCH" {
		return "", 0, false
	}
	if strings.Trim(req.Header.Get("MAN"), `"`) != "ssdp:discover" {
		return "", 0, false
	}
	st = strings.TrimSpace(req.Header.Get("ST"))
	fmt.Sscan(req.Header.Get("MX"), &mx)
	return st, mx, st != ""
}

func (s *ssdp) reply(to *net.UDPAddr, mx int, replies []string) {
	// spread the answers over up to MX seconds (at most 1: be quick)
	wait := time.Duration(min(max(mx, 0), 1)) * time.Second
	if wait > 0 {
		time.Sleep(time.Duration(rand.Int64N(int64(wait))))
	}
	ip := localIP(to.IP)
	if ip == nil {
		return
	}
	c, err := net.DialUDP("udp4", nil, to)
	if err != nil {
		return
	}
	defer c.Close()
	for _, r := range replies {
		p := strings.SplitN(r, "\x00", 3)
		msg := "HTTP/1.1 200 OK\r\n" +
			fmt.Sprintf("CACHE-CONTROL: max-age=%d\r\n", ssdpMaxAge) +
			"DATE: " + time.Now().UTC().Format(http.TimeFormat) + "\r\n" +
			"EXT:\r\n" +
			fmt.Sprintf("LOCATION: http://%s:%d%s\r\n", ip, s.port, p[0]) +
			"SERVER: " + s.server + "\r\n" +
			"ST: " + p[1] + "\r\n" +
			"USN: " + p[2] + "\r\n\r\n"
		c.Write([]byte(msg))
	}
}

func (s *ssdp) announceLoop() {
	defer s.done.Done()
	s.mu.Lock()
	stop := s.stop
	s.mu.Unlock()
	// at start, once more shortly after (UDP is lossy), then well within max-age
	wait := 3 * time.Second
	s.notifyAll("ssdp:alive")
	for {
		select {
		case <-stop:
			return
		case <-time.After(wait):
		}
		s.notifyAll("ssdp:alive")
		wait = ssdpMaxAge / 2 * time.Second
	}
}

func (s *ssdp) notifyAll(nts string) {
	s.mu.Lock()
	ads := s.adverts
	s.mu.Unlock()
	for _, a := range ads {
		s.notify(a, nts)
	}
}

func (s *ssdp) notify(a advert, nts string) {
	ip := s.ifaceIP()
	if ip == nil {
		return
	}
	group, _ := net.ResolveUDPAddr("udp4", ssdpAddr)
	c, err := net.DialUDP("udp4", &net.UDPAddr{IP: ip}, group)
	if err != nil {
		return
	}
	defer c.Close()
	for _, nt := range a.targets() {
		msg := "NOTIFY * HTTP/1.1\r\nHOST: " + ssdpAddr + "\r\n" +
			"NT: " + nt + "\r\nNTS: " + nts + "\r\nUSN: " + a.usn(nt) + "\r\n"
		if nts == "ssdp:alive" {
			msg += fmt.Sprintf("CACHE-CONTROL: max-age=%d\r\nLOCATION: http://%s:%d%s\r\nSERVER: %s\r\n",
				ssdpMaxAge, ip, s.port, a.descPath, s.server)
		}
		c.Write([]byte(msg + "\r\n"))
	}
}
