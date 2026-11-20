package display

import (
	"testing"
	"time"
)

func TestControlMatchesVendor(t *testing.T) {
	// lenovo_alarm sends 0x5d: 6x7 mode, current 3, duty field 10. Decode and re-encode.
	// 0x5d = 0101 1101: mode 1, current bits 110 -> reverse -> 3, duty bits 0101 -> reverse 1010 = 10 -> level 10
	if got := control(10, 3); got != 0x5d {
		t.Errorf("control(10,3) = %#x, want 0x5d", got)
	}
	if control(0, 3) != 0x01 {
		t.Error("off")
	}
	// duty field 0 lights nothing: level 1 must be field 1, 16 the top field 15
	if got := control(1, 3) >> 4; got != reverse4(1) {
		t.Errorf("control(1,3) duty bits = %#x, want field 1", got)
	}
	if control(16, 3) != control(15, 3) {
		t.Error("16 should share the top field with 15")
	}
}

func TestFrame(t *testing.T) {
	var f Frame
	f.Big("1630")
	if f.A != [6]byte{0x06, 0x7d, 0x4f, 0x3f, 0, 0} {
		t.Errorf("big 1630 = % x (want lenovo_alarm's 06 7d 4f 3f)", f.A)
	}
	f.Temperature(-105)
	if f.B[0] != Font['0'] || f.B[1] != Font['5'] || f.B[3]&0x07 != 0x07 {
		t.Errorf("temp -105 = % x", f.B)
	}
	for _, c := range []struct {
		t          int
		b0, b1     byte
		minus, one bool
	}{
		{-1, Font['-'], Font['1'], false, false}, // minus right next to the digit, no gap
		{-9, Font['-'], Font['9'], false, false},
		{-10, Font['1'], Font['0'], true, false}, // two digits: the minus LED
		{-23, Font['2'], Font['3'], true, false},
		{0, 0, Font['0'], false, false},
		{7, 0, Font['7'], false, false},
		{21, Font['2'], Font['1'], false, false},
	} {
		f.Temperature(-105) // start from a busy frame: every case must clear what it doesn't use
		f.Temperature(c.t)
		minus, one := f.B[3]&Icons["minus"].Mask != 0, f.B[3]&Icons["hundred"].Mask == Icons["hundred"].Mask
		if f.B[0] != c.b0 || f.B[1] != c.b1 || minus != c.minus || one != c.one {
			t.Errorf("temp %d: digits % x minus %v hundred %v", c.t, f.B[:2], minus, one)
		}
	}
	f.Weekday(time.Saturday)
	if f.B[5] != 1<<6 {
		t.Errorf("saturday = %#x", f.B[5])
	}
	f.Big("hi")
	if f.A[0] != 0 || f.A[2] != Font['h'] {
		t.Errorf("right align: % x", f.A)
	}
}
