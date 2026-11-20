// Package display drives the Lenovo Smart Clock Essential's LED clock face:
// two TM1652 chips on /dev/ttyS2 and /dev/ttyS3, 19200 baud 8O1, write-only.
//
// Frames (separated by idle gaps):
//
//	0x08 d0..d5   display data, grids 0-5 (6 digits x 7 segments mode)
//	0x18 ctrl     bit0 = 1 (6x7 mode), bits1-3 segment current (bit-reversed),
//	              bits4-7 duty cycle (bit-reversed)
//
// Segment map (from sniffing lenovo_alarm and a visual walk, see notes):
//
//	chip A (ttyS2) grids 0-3  big digits, standard glyphs (bit0 = a ... bit6 = g)
//	chip B (ttyS3) grid 0     small digit 5 (standard glyph)
//	               grid 1     small digit 6 (standard glyph)
//	               grid 2     alarm, mute, colon (bits 2+3), AM, PM
//	               grid 3     minus, "1" (2 bits), °F, °C, fog, warning
//	               grid 4     sun, cloudy, wind, snow, rain, thunder
//	               grid 5     W1..W7 weekdays (W1 = Sunday ... W7 = Saturday)
package display

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// Icon is one named LED group: chip B grid and bit mask.
type Icon struct {
	Grid uint8
	Mask byte
}

var Icons = map[string]Icon{
	// grid 2
	"alarm": {2, 1 << 0}, // L1, alarm clock
	"mute":  {2, 1 << 1}, // L2, crossed microphone
	"colon": {2, 0x0c},   // L3+L4, needs both bits (each alone lights the dots unevenly)
	"am":    {2, 1 << 4}, // L5
	"pm":    {2, 1 << 5}, // L6
	// grid 3: temperature row
	"minus":      {3, 1 << 0}, // N1
	"hundred":    {3, 0x06},   // N2+N3, the "1" of 1xx
	"fahrenheit": {3, 1 << 3}, // N4
	"celsius":    {3, 1 << 4}, // N5
	"fog":        {3, 1 << 5}, // N6, three vertical lines
	"warning":    {3, 1 << 6}, // N7, "!"
	// grid 4: weather
	"sun":     {4, 1 << 0}, // K1
	"cloudy":  {4, 1 << 1}, // K2
	"wind":    {4, 1 << 2}, // K3
	"snow":    {4, 1 << 3}, // K4
	"rain":    {4, 1 << 4}, // K5
	"thunder": {4, 1 << 5}, // K6
	// grid 5: weekdays, W1 = Sunday ... W7 = Saturday
	"W1": {5, 1 << 0}, "W2": {5, 1 << 1}, "W3": {5, 1 << 2}, "W4": {5, 1 << 3},
	"W5": {5, 1 << 4}, "W6": {5, 1 << 5}, "W7": {5, 1 << 6},
}

// WeatherIcons are the mutually exclusive condition icons.
var WeatherIcons = []string{"sun", "cloudy", "wind", "snow", "rain", "thunder", "fog"}

// Font maps characters to 7-segment glyphs (bit0 = a ... bit6 = g). Letters
// that 7 segments cannot show well use the usual compromises.
var Font = map[rune]byte{
	'0': 0x3f, '1': 0x06, '2': 0x5b, '3': 0x4f, '4': 0x66, '5': 0x6d, '6': 0x7d, '7': 0x07,
	'8': 0x7f, '9': 0x6f, ' ': 0x00, '-': 0x40, '_': 0x08, '=': 0x48, '\'': 0x02, '"': 0x22,
	'°': 0x63, '?': 0x53, '[': 0x39, ']': 0x0f, '(': 0x39, ')': 0x0f,
	'a': 0x77, 'b': 0x7c, 'c': 0x58, 'd': 0x5e, 'e': 0x79, 'f': 0x71, 'g': 0x3d, 'h': 0x74,
	'i': 0x04, 'j': 0x1e, 'k': 0x75, 'l': 0x38, 'm': 0x37, 'n': 0x54, 'o': 0x5c, 'p': 0x73,
	'q': 0x67, 'r': 0x50, 's': 0x6d, 't': 0x78, 'u': 0x1c, 'v': 0x3e, 'w': 0x2a, 'x': 0x76,
	'y': 0x6e, 'z': 0x5b,
}

// Glyph returns the segments for r (case-insensitive; unknown -> blank).
func Glyph(r rune) byte {
	if g, ok := Font[r]; ok {
		return g
	}
	if g, ok := Font[[]rune(strings.ToLower(string(r)))[0]]; ok {
		return g
	}
	return 0
}

// Frame is the full contents of both chips.
type Frame struct {
	A [6]byte // big digits in grids 0-3
	B [6]byte // small digits + icons
}

func (f *Frame) Set(name string, on bool) {
	ic, ok := Icons[name]
	if !ok {
		return
	}
	if on {
		f.B[ic.Grid] |= ic.Mask
	} else {
		f.B[ic.Grid] &^= ic.Mask
	}
}

// Big writes up to 4 characters to the big digits, right-aligned.
func (f *Frame) Big(s string) {
	r := []rune(s)
	for i := 0; i < 4; i++ {
		f.A[i] = 0
		if j := len(r) - 4 + i; j >= 0 {
			f.A[i] = Glyph(r[j])
		}
	}
}

// Small writes up to 2 characters to the small digits, right-aligned.
func (f *Frame) Small(s string) {
	r := []rune(s)
	for i := 0; i < 2; i++ {
		f.B[i] = 0
		if j := len(r) - 2 + i; j >= 0 {
			f.B[i] = Glyph(r[j])
		}
	}
}

// Temperature shows -199..199 on the small digits with the minus and "1" LEDs.
// -1..-9 put the minus on the free small digit right next to the number
// (the minus LED sits further left and would leave a gap); from -10 on both
// small digits are taken and the minus LED shows the sign.
func (f *Frame) Temperature(t int) {
	if t < 0 && t > -10 {
		f.Set("minus", false)
		f.Set("hundred", false)
		f.Small(fmt.Sprintf("-%d", -t))
		return
	}
	f.Set("minus", t < 0)
	if t < 0 {
		t = -t
	}
	f.Set("hundred", t >= 100)
	if t >= 100 {
		f.Small(fmt.Sprintf("%02d", t%100)) // 105 -> "1" LEDs + "05"
	} else {
		f.Small(fmt.Sprintf("%2d", t))
	}
}

// Weekday lights W1 (Sunday) .. W7 (Saturday).
func (f *Frame) Weekday(d time.Weekday) {
	for i := 0; i < 7; i++ {
		f.Set(fmt.Sprintf("W%d", i+1), int(d) == i)
	}
}

// Display owns both UARTs.
type Display struct {
	mu         sync.Mutex
	fdA, fdB   int
	last       Frame
	sent       bool
	brightness int // 0 = off, 1..16
}

// Open opens both TM1652 lines. The vendor lenovo_alarm must not be running.
func Open() (*Display, error) {
	a, err := openUART("/dev/ttyS2")
	if err != nil {
		return nil, fmt.Errorf("ttyS2: %w", err)
	}
	b, err := openUART("/dev/ttyS3")
	if err != nil {
		unix.Close(a)
		return nil, fmt.Errorf("ttyS3: %w", err)
	}
	d := &Display{fdA: a, fdB: b, brightness: 10}
	d.SetBrightness(10)
	return d, nil
}

func (d *Display) Close() {
	d.Show(Frame{})
	unix.Close(d.fdA)
	unix.Close(d.fdB)
}

// SetBrightness sets the duty cycle 0 (off) .. 16 (full).
func (d *Display) SetBrightness(level int) {
	level = max(0, min(level, 16))
	d.mu.Lock()
	defer d.mu.Unlock()
	d.brightness = level
	ctrl := control(level, 3)
	send(d.fdA, 0x18, ctrl)
	send(d.fdB, 0x18, ctrl)
}

// control builds the TM1652 control byte: 6x7 mode, current step (0-7),
// duty (1-16; 0 = display off). The chip's duty field is 0-15 and field 0
// lights nothing (seen on the clock), so level n is field n and 16 shares
// 15's field: level 1 is the dimmest lit face.
func control(duty, current int) byte {
	if duty == 0 {
		return 0x01 // all bits zero apart from the mode = display off
	}
	c := byte(1)
	c |= reverse3(byte(current&7)) << 1
	c |= reverse4(byte(min(duty, 15)&15)) << 4
	return c
}

func reverse3(b byte) byte { return (b&1)<<2 | b&2 | (b&4)>>2 }
func reverse4(b byte) byte { return (b&1)<<3 | (b&2)<<1 | (b&4)>>1 | (b&8)>>3 }

// Show sends a frame (only the chips whose contents changed).
func (d *Display) Show(f Frame) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.sent || f.A != d.last.A {
		send(d.fdA, append([]byte{0x08}, f.A[:]...)...)
	}
	if !d.sent || f.B != d.last.B {
		send(d.fdB, append([]byte{0x08}, f.B[:]...)...)
	}
	d.last, d.sent = f, true
}

func send(fd int, b ...byte) {
	unix.Write(fd, b)
	unix.IoctlSetInt(fd, unix.TCSBRK, 1) // tcdrain
	time.Sleep(4 * time.Millisecond)     // idle gap ends the frame
}

func openUART(path string) (int, error) {
	fd, err := unix.Open(path, unix.O_WRONLY|unix.O_NOCTTY, 0)
	if err != nil {
		return -1, err
	}
	t, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		unix.Close(fd)
		return -1, err
	}
	t.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP | unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON
	t.Oflag &^= unix.OPOST
	t.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
	t.Cflag &^= unix.CBAUD | unix.CSIZE | unix.CSTOPB | unix.CRTSCTS
	t.Cflag |= unix.B19200 | unix.CS8 | unix.PARENB | unix.PARODD | unix.CLOCAL
	t.Ispeed, t.Ospeed = unix.B19200, unix.B19200
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, t); err != nil {
		unix.Close(fd)
		return -1, err
	}
	return fd, nil
}
