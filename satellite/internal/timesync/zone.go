package timesync

import (
	"encoding/binary"
	"strings"
	"time"
)

// LoadZone accepts an IANA name ("Europe/Helsinki") or a POSIX TZ rule
// ("EET-2EEST,M3.5.0/3,M10.5.0/4", which is what Home Assistant sends over the
// ESPHome API).
func LoadZone(tz string) (*time.Location, error) {
	if loc, err := time.LoadLocation(tz); err == nil {
		return loc, nil
	} else if !looksPOSIX(tz) {
		return nil, err
	}
	return time.LoadLocationFromTZData(tz, posixTZif(tz))
}

// looksPOSIX: a POSIX rule starts with an abbreviation (or <...>) directly
// followed by a signed offset, e.g. "EET-2", "<+03>-3", "CET-1CEST,M3.5.0,...".
// IANA names ("Europe/Helsinki", "UTC") have no digit before any "/".
func looksPOSIX(s string) bool {
	head := s
	if i := strings.IndexByte(s, ','); i >= 0 {
		head = s[:i]
	}
	return head != "" && !strings.Contains(head, "/") && strings.ContainsAny(head, "0123456789")
}

// posixTZif builds a TZif v2 file whose only content is the POSIX rule in the
// footer. Go applies the footer rule to every time after the last transition,
// so one dummy transition at 1970 makes the rule apply to all modern dates.
func posixTZif(rule string) []byte {
	block := func(timeSize int) []byte {
		var b []byte
		u32 := func(v uint32) { b = binary.BigEndian.AppendUint32(b, v) }
		b = append(b, "TZif2"...)
		b = append(b, make([]byte, 15)...)
		u32(0) // isutcnt
		u32(0) // isstdcnt
		u32(0) // leapcnt
		u32(1) // timecnt
		u32(1) // typecnt
		u32(4) // charcnt
		if timeSize == 8 {
			b = binary.BigEndian.AppendUint64(b, 0)
		} else {
			u32(0)
		}
		b = append(b, 0)          // transition -> type 0
		b = append(b, 0, 0, 0, 0) // utoff 0
		b = append(b, 0, 0)       // isdst, desigidx
		b = append(b, "UTC\x00"...)
		return b
	}
	out := block(4)
	out = append(out, block(8)...)
	return append(out, "\n"+rule+"\n"...)
}
