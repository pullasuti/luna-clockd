// Package timesync sets the system clock from Home Assistant or a user-chosen
// NTP server. It never picks a server on its own.
package timesync

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"time"

	"golang.org/x/sys/unix"
)

const ntpEpochOffset = 2208988800 // seconds from 1900-01-01 to 1970-01-01

// QueryNTP asks one server (host or host:port) for the time with a single
// SNTPv4 request (RFC 4330) and returns the clock offset to apply
// (server time minus local time) and the round-trip delay.
func QueryNTP(server string, timeout time.Duration) (offset, rtt time.Duration, err error) {
	if server == "" {
		return 0, 0, errors.New("no NTP server configured")
	}
	if _, _, err := net.SplitHostPort(server); err != nil {
		server = net.JoinHostPort(server, "123")
	}
	conn, err := net.DialTimeout("udp", server, timeout)
	if err != nil {
		return 0, 0, err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(timeout))

	req := make([]byte, 48)
	req[0] = 0x23 // LI 0, version 4, mode 3 (client)
	t1 := time.Now()
	putNTP(req[40:], t1) // transmit timestamp, echoed back as originate
	if _, err := conn.Write(req); err != nil {
		return 0, 0, err
	}
	resp := make([]byte, 48)
	n, err := conn.Read(resp)
	t4 := time.Now()
	if err != nil {
		return 0, 0, err
	}
	if n < 48 {
		return 0, 0, fmt.Errorf("short NTP reply (%d bytes)", n)
	}
	mode, stratum := resp[0]&7, resp[1]
	if mode != 4 && mode != 5 {
		return 0, 0, fmt.Errorf("unexpected NTP mode %d", mode)
	}
	if stratum == 0 || stratum > 15 {
		return 0, 0, fmt.Errorf("server unsynchronised (stratum %d)", stratum)
	}
	if binary.BigEndian.Uint64(resp[24:]) != binary.BigEndian.Uint64(req[40:]) {
		return 0, 0, errors.New("NTP reply does not match request")
	}
	t2, t3 := getNTP(resp[32:]), getNTP(resp[40:])
	offset = (t2.Sub(t1) + t3.Sub(t4)) / 2
	rtt = t4.Sub(t1) - t3.Sub(t2)
	return offset, rtt, nil
}

func putNTP(b []byte, t time.Time) {
	sec := uint64(t.Unix() + ntpEpochOffset)
	frac := uint64(t.Nanosecond()) << 32 / 1e9
	binary.BigEndian.PutUint64(b, sec<<32|frac)
}

func getNTP(b []byte) time.Time {
	v := binary.BigEndian.Uint64(b)
	sec, frac := int64(v>>32)-ntpEpochOffset, int64((v&0xffffffff)*1e9>>32)
	return time.Unix(sec, frac)
}

// Step sets the system clock by offset when it is larger than minStep.
// It reports whether the clock was changed.
func Step(offset, minStep time.Duration) (bool, error) {
	if offset < minStep && offset > -minStep {
		return false, nil
	}
	return true, Set(time.Now().Add(offset))
}

// Set sets the system clock (needs root).
func Set(t time.Time) error {
	tv := unix.NsecToTimeval(t.UnixNano())
	return unix.Settimeofday(&tv)
}
