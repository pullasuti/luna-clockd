package main

import (
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"time"
)

// The setup hotspot's DNS: every name is the clock (a phone that joins then
// opens the setup page by itself). It replaces the vendor dnsmasq 2.51 (old,
// root-started) for DNS; DHCP is busybox udhcpd (setup-ap.sh). The old route
// stays behind the flag file use-dnsmasq. Only reachable from uap0 (the
// address is uap0's; setup-ap.sh's firewall lets DNS in there).
//
// A answers carry the clock's address; AAAA and other types get an empty
// NOERROR answer (IPv6 is off on uap0), other opcodes NOTIMP, and malformed
// packets no answer at all.

const setupDNSTTL = 10 // s: a phone that rejoins later must ask again

// setupDNS serves on addr (UDP and TCP port 53) until Close.
type setupDNS struct {
	ip   netip.Addr
	udp  net.PacketConn
	tcp  net.Listener
	wg   sync.WaitGroup
	log  *slog.Logger
	once sync.Once
}

func startSetupDNS(log *slog.Logger, ip netip.Addr, port int) (*setupDNS, error) {
	addr := netip.AddrPortFrom(ip, uint16(port)).String()
	u, err := net.ListenPacket("udp4", addr)
	if err != nil {
		return nil, err
	}
	t, err := net.Listen("tcp4", addr)
	if err != nil {
		u.Close()
		return nil, err
	}
	s := &setupDNS{ip: ip, udp: u, tcp: t, log: log}
	s.wg.Add(2)
	go s.serveUDP()
	go s.serveTCP()
	return s, nil
}

func (s *setupDNS) Close() {
	s.once.Do(func() {
		s.udp.Close()
		s.tcp.Close()
		s.wg.Wait()
	})
}

func (s *setupDNS) serveUDP() {
	defer s.wg.Done()
	buf := make([]byte, 1500)
	for {
		n, from, err := s.udp.ReadFrom(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		if out := dnsAnswer(buf[:n], s.ip); out != nil {
			s.udp.WriteTo(out, from)
		}
	}
}

// TCP: some resolvers retry over it; one query per connection is enough.
func (s *setupDNS) serveTCP() {
	defer s.wg.Done()
	var conns sync.WaitGroup
	defer conns.Wait()
	for {
		c, err := s.tcp.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			time.Sleep(100 * time.Millisecond)
			continue
		}
		conns.Add(1)
		go func() {
			defer conns.Done()
			defer c.Close()
			c.SetDeadline(time.Now().Add(5 * time.Second))
			var l [2]byte
			if _, err := io.ReadFull(c, l[:]); err != nil {
				return
			}
			q := make([]byte, binary.BigEndian.Uint16(l[:]))
			if _, err := io.ReadFull(c, q); err != nil {
				return
			}
			if out := dnsAnswer(q, s.ip); out != nil {
				c.Write(append(binary.BigEndian.AppendUint16(nil, uint16(len(out))), out...))
			}
		}()
	}
}

// dnsAnswer builds the reply to one query, or nil to drop it.
func dnsAnswer(q []byte, ip netip.Addr) []byte {
	if len(q) < 12 || q[2]&0x80 != 0 { // too short, or a response
		return nil
	}
	opcode := (q[2] >> 3) & 0xF
	qd := binary.BigEndian.Uint16(q[4:6])
	reply := func(rcode byte, question []byte, answers int) []byte {
		out := make([]byte, 12, 12+len(question)+16)
		copy(out, q[:2])                // id
		out[2] = 0x80 | q[2]&0x79 | 0x4 // QR, opcode and RD kept, AA
		out[3] = rcode
		if question != nil {
			out[5] = 1
		}
		out[7] = byte(answers)
		return append(out, question...)
	}
	if opcode != 0 {
		return reply(4, nil, 0) // NOTIMP
	}
	if qd != 1 {
		return reply(1, nil, 0) // FORMERR
	}
	// the question: labels up to the root, then type and class
	i := 12
	for {
		if i >= len(q) {
			return nil
		}
		l := int(q[i])
		if l == 0 {
			i++
			break
		}
		if l&0xC0 != 0 || i+1+l > len(q) { // no compression in a question
			return nil
		}
		i += 1 + l
		if i-12 > 255 {
			return nil
		}
	}
	if i+4 > len(q) {
		return nil
	}
	question := q[12 : i+4]
	qtype := binary.BigEndian.Uint16(q[i:])
	qclass := binary.BigEndian.Uint16(q[i+2:])
	if qclass != 1 || (qtype != 1 && qtype != 255) {
		return reply(0, question, 0) // NOERROR, no data
	}
	out := reply(0, question, 1)
	a := ip.As4()
	out = append(out, 0xC0, 12, 0, 1, 0, 1) // name -> question, A, IN
	out = binary.BigEndian.AppendUint32(out, setupDNSTTL)
	out = append(out, 0, 4, a[0], a[1], a[2], a[3])
	return out
}

// setupDNSCtl keeps the server up while the hotspot is on (setupMode starts
// and stops it); with the flag file use-dnsmasq the script's dnsmasq does DNS.
type setupDNSCtl struct {
	mu  sync.Mutex
	srv *setupDNS
}

func (c *setupDNSCtl) start(log *slog.Logger) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.srv != nil {
		return
	}
	// uap0 has its address once setup-ap.sh returns; retry briefly anyway
	var err error
	for range 10 {
		if c.srv, err = startSetupDNS(log, setupAPIP, 53); err == nil {
			log.Info("setup hotspot DNS on", "addr", setupAPIP.String())
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
	log.Warn("setup hotspot DNS did not start: phones won't open the setup page by themselves", "err", err)
}

func (c *setupDNSCtl) stop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.srv != nil {
		c.srv.Close()
		c.srv = nil
	}
}

var setupAPIP = netip.MustParseAddr("192.168.255.249") // setup-ap.sh's IP
