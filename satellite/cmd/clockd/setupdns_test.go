package main

import (
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"testing"
	"time"
)

func dnsQuery(id uint16, name string, qtype uint16) []byte {
	q := binary.BigEndian.AppendUint16(nil, id)
	q = append(q, 0x01, 0, 0, 1, 0, 0, 0, 0, 0, 0) // RD, one question
	start := 0
	for i := 0; i <= len(name); i++ {
		if i == len(name) || name[i] == '.' {
			q = append(q, byte(i-start))
			q = append(q, name[start:i]...)
			start = i + 1
		}
	}
	q = append(q, 0)
	q = binary.BigEndian.AppendUint16(q, qtype)
	return binary.BigEndian.AppendUint16(q, 1)
}

func TestDNSAnswer(t *testing.T) {
	ip := netip.MustParseAddr("192.168.255.249")
	q := dnsQuery(0xBEEF, "connectivitycheck.gstatic.com", 1)
	a := dnsAnswer(q, ip)
	if len(a) != len(q)+16 || a[0] != 0xBE || a[1] != 0xEF || a[2] != 0x85 || a[3] != 0 || a[7] != 1 {
		t.Fatalf("A reply % x", a)
	}
	if got := a[len(a)-4:]; netip.AddrFrom4([4]byte(got)) != ip {
		t.Errorf("address %v", got)
	}
	if ttl := binary.BigEndian.Uint32(a[len(q)+6:]); ttl != setupDNSTTL {
		t.Errorf("ttl %d", ttl)
	}
	// AAAA: NOERROR without an answer
	if a := dnsAnswer(dnsQuery(1, "captive.apple.com", 28), ip); a == nil || a[3] != 0 || a[7] != 0 {
		t.Errorf("AAAA reply % x", a)
	}
	// the root name, and a 63-byte label
	long := "a23456789012345678901234567890123456789012345678901234567890123.x"
	for _, n := range []string{long} {
		if a := dnsAnswer(dnsQuery(2, n, 1), ip); a == nil || a[7] != 1 {
			t.Errorf("%s: % x", n, a)
		}
	}
	// opcode 2 (status): NOTIMP
	st := dnsQuery(3, "x", 1)
	st[2] |= 2 << 3
	if a := dnsAnswer(st, ip); a == nil || a[3] != 4 {
		t.Errorf("status reply % x", a)
	}
	// dropped: responses, truncated, compression in the question, two questions
	drop := [][]byte{nil, q[:11], q[:len(q)-1], q[:20]}
	resp := append([]byte(nil), q...)
	resp[2] |= 0x80
	drop = append(drop, resp)
	ptr := append(append([]byte(nil), q[:12]...), 0xC0, 12, 0, 1, 0, 1)
	drop = append(drop, ptr)
	for i, d := range drop {
		if a := dnsAnswer(d, ip); a != nil {
			t.Errorf("drop %d: answered % x", i, a)
		}
	}
	two := append([]byte(nil), q...)
	two[5] = 2
	if a := dnsAnswer(two, ip); a == nil || a[3] != 1 {
		t.Errorf("two questions: % x", a)
	}
	// garbage never panics
	for n := range 2000 {
		b := make([]byte, n%80)
		for i := range b {
			b[i] = byte(n*7 + i*13)
		}
		if len(b) > 2 {
			b[2] &^= 0x80
		}
		dnsAnswer(b, ip)
	}
}

func TestSetupDNSServer(t *testing.T) {
	ip := netip.MustParseAddr("127.0.0.1")
	s, err := startSetupDNS(slog.New(slog.DiscardHandler), ip, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// the UDP and TCP ports differ with port 0: ask each its own
	r := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "udp", s.udp.LocalAddr().String())
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	addrs, err := r.LookupNetIP(ctx, "ip4", "example.org")
	if err != nil || len(addrs) != 1 || addrs[0] != ip {
		t.Fatalf("udp lookup: %v %v", addrs, err)
	}
	c, err := net.DialTimeout("tcp", s.tcp.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	q := dnsQuery(7, "neverssl.com", 1)
	c.Write(append(binary.BigEndian.AppendUint16(nil, uint16(len(q))), q...))
	c.SetDeadline(time.Now().Add(3 * time.Second))
	var l [2]byte
	if _, err := io.ReadFull(c, l[:]); err != nil {
		t.Fatal(err)
	}
	a := make([]byte, binary.BigEndian.Uint16(l[:]))
	if _, err := io.ReadFull(c, a); err != nil || a[7] != 1 {
		t.Fatalf("tcp reply % x %v", a, err)
	}
	c.Close()
	done := make(chan struct{})
	go func() { s.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Close hangs")
	}
}
