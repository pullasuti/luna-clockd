// Package hw covers the clock's small peripherals: buttons and the mic mute
// switch (gpio_keypad), the four "Google" status LEDs (SN3112) and the rear
// night light (PWM LED).
package hw

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"sync"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Linux key codes reported by gpio_keypad (/dev/input/event0).
const (
	KeyMute       = 116 // KEY_POWER: the mic mute slide switch, held DOWN while muted
	KeyVolumeDown = 114
	KeyVolumeUp   = 115
	KeyPlay       = 139 // KEY_MENU, the play/pause (action) button
	KeyAlarm      = 141 // KEY_SETUP
)

// KeyEvent is one press/release. Value: 1 down, 0 up, 2 autorepeat.
type KeyEvent struct {
	Code  uint16
	Value int32
}

// ReadKeys streams key events from dev until ctx ends.
func ReadKeys(ctx context.Context, dev string, fn func(KeyEvent)) error {
	f, err := os.Open(dev)
	if err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { f.Close() })
	defer stop()
	// struct input_event on 64-bit: timeval (16) + type (2) + code (2) + value (4)
	const size = 24
	buf := make([]byte, size*16)
	for {
		n, err := f.Read(buf)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		for off := 0; off+size <= n; off += size {
			typ := binary.LittleEndian.Uint16(buf[off+16:])
			if typ != 1 { // EV_KEY
				continue
			}
			fn(KeyEvent{
				Code:  binary.LittleEndian.Uint16(buf[off+18:]),
				Value: int32(binary.LittleEndian.Uint32(buf[off+20:])),
			})
		}
	}
}

// KeyDown reports whether a key is currently held (EVIOCGKEY), e.g. the mute switch at startup.
func KeyDown(dev string, code uint16) (bool, error) {
	f, err := os.Open(dev)
	if err != nil {
		return false, err
	}
	defer f.Close()
	var bits [96]byte // KEY_MAX 0x2ff -> 768 bits
	// EVIOCGKEY(len) = _IOC(_IOC_READ, 'E', 0x18, len)
	req := uintptr(2<<30 | len(bits)<<16 | 'E'<<8 | 0x18)
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, f.Fd(), req, uintptr(unsafe.Pointer(&bits[0]))); e != 0 {
		return false, e
	}
	return bits[code/8]&(1<<(code%8)) != 0, nil
}

// RGB is one status LED colour, 0-255 per channel.
type RGB struct{ R, G, B uint8 }

func (c RGB) Scale(f float64) RGB {
	return RGB{uint8(float64(c.R) * f), uint8(float64(c.G) * f), uint8(float64(c.B) * f)}
}

// ClassicColours: blue, red, yellow, green, left to right.
var ClassicColours = [4]RGB{{B: 255}, {R: 255}, {R: 255, G: 180}, {G: 255}}

const sn3112 = "/sys/devices/platform/soc/ff800000.aobus/ff805000.i2c/i2c-1/1-0054/sn3112_gva"

// StatusLEDs drives the four RGB LEDs. Never read the sysfs file: the
// driver's show() sets every channel to 0x80.
type StatusLEDs struct {
	mu   sync.Mutex
	last [12]byte
	set  bool
}

// Set writes all four LEDs (LED1 = leftmost). The hardware channel order is G, R, B.
func (s *StatusLEDs) Set(c [4]RGB) error {
	var b [12]byte
	for i, x := range c {
		b[i*3], b[i*3+1], b[i*3+2] = x.G, x.R, x.B
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.set && b == s.last {
		return nil
	}
	fd, err := unix.Open(sn3112, unix.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	// The driver reports a short write although all bytes are applied; write exactly once.
	if _, err := unix.Write(fd, b[:]); err != nil && err != unix.EINVAL {
		return fmt.Errorf("sn3112: %w", err)
	}
	s.last, s.set = b, true
	return nil
}

// Last returns the colours last written (all off before the first write).
func (s *StatusLEDs) Last() [4]RGB {
	s.mu.Lock()
	defer s.mu.Unlock()
	var c [4]RGB
	for i := range c {
		c[i] = RGB{G: s.last[i*3], R: s.last[i*3+1], B: s.last[i*3+2]}
	}
	return c
}

// SetAll sets every LED to one colour.
func (s *StatusLEDs) SetAll(c RGB) error { return s.Set([4]RGB{c, c, c, c}) }

const nightLight = "/sys/class/leds/night_light/brightness"

// SetNightLight sets the rear night light, 0 (off) - 255.
func SetNightLight(level int) error {
	level = max(0, min(level, 255))
	return os.WriteFile(nightLight, []byte(fmt.Sprintf("%d\n", level)), 0)
}
