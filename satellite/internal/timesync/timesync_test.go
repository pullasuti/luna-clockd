package timesync

import (
	"encoding/binary"
	"net"
	"testing"
	"time"
)

// fakeServer answers SNTP requests with a clock that is `skew` ahead.
func fakeServer(t *testing.T, skew time.Duration, stratum byte) string {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	go func() {
		buf := make([]byte, 48)
		for {
			n, addr, err := pc.ReadFrom(buf)
			if err != nil || n < 48 {
				return
			}
			resp := make([]byte, 48)
			resp[0] = 0x24 // version 4, mode 4
			resp[1] = stratum
			copy(resp[24:32], buf[40:48]) // originate = client's transmit
			now := time.Now().Add(skew)
			putNTP(resp[32:], now)
			putNTP(resp[40:], now)
			pc.WriteTo(resp, addr)
		}
	}()
	return pc.LocalAddr().String()
}

func TestQueryNTP(t *testing.T) {
	srv := fakeServer(t, 90*time.Second, 2)
	off, rtt, err := QueryNTP(srv, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if d := off - 90*time.Second; d > 50*time.Millisecond || d < -50*time.Millisecond {
		t.Errorf("offset %v, want ~90s", off)
	}
	if rtt < 0 || rtt > 100*time.Millisecond {
		t.Errorf("rtt %v", rtt)
	}
}

func TestQueryNTPUnsynced(t *testing.T) {
	srv := fakeServer(t, 0, 0)
	if _, _, err := QueryNTP(srv, time.Second); err == nil {
		t.Error("stratum 0 accepted")
	}
}

func TestNTPRoundTrip(t *testing.T) {
	now := time.Unix(1793000000, 123456789)
	b := make([]byte, 8)
	putNTP(b, now)
	if got := getNTP(b); got.Sub(now) > time.Microsecond || now.Sub(got) > time.Microsecond {
		t.Errorf("round trip %v -> %v", now, got)
	}
	_ = binary.BigEndian
}
