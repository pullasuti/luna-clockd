// seg: TM1652 segment display tool for the Lenovo Smart Clock Essential.
//
// The clock face is driven by two TM1652 chips, one per UART (ttyS2, ttyS3),
// 19200 baud 8O1, write-only. Commands are framed by idle gaps:
//
//	0x08 [data...]   display data from address 0
//	0x18 ctrl        control: bit0 mode (1 = 6x7, 0 = 5x8), bits1-3 segment
//	                 current (bit-reversed), bits4-7 duty (bit-reversed)
//
// Usage:
//
//	seg sniff <seconds>            interpose ptys over /dev/ttyS2 and /dev/ttyS3 (bind mounts),
//	                               log every frame and forward it to the real UARTs
//	seg raw <chip 2|3> <hex>...    send one frame, e.g.: seg raw 2 08 ff ff ff ff ff
//	seg walk <seconds>             light one icon bit at a time; the digits show its
//	                               code C-GB (chip, grid, bit) for visual mapping
package main

import (
	"encoding/hex"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

func die(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...); os.Exit(1) }

// openUART opens a TM1652 line at 19200 8O1, raw.
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
	cfmakeraw(t)
	t.Cflag &^= unix.CBAUD | unix.CSIZE | unix.CSTOPB | unix.CRTSCTS
	t.Cflag |= unix.B19200 | unix.CS8 | unix.PARENB | unix.PARODD | unix.CLOCAL
	t.Ispeed, t.Ospeed = unix.B19200, unix.B19200
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, t); err != nil {
		unix.Close(fd)
		return -1, err
	}
	return fd, nil
}

func cfmakeraw(t *unix.Termios) {
	t.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP | unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON
	t.Oflag &^= unix.OPOST
	t.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
	t.Cc[unix.VMIN], t.Cc[unix.VTIME] = 1, 0
}

// openPTY returns the master fd and the slave path.
func openPTY() (int, string, error) {
	m, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		return -1, "", err
	}
	if err := unix.IoctlSetPointerInt(m, unix.TIOCSPTLCK, 0); err != nil {
		return -1, "", err
	}
	n, err := unix.IoctlGetInt(m, unix.TIOCGPTN)
	if err != nil {
		return -1, "", err
	}
	return m, "/dev/pts/" + strconv.Itoa(n), nil
}

func main() {
	if len(os.Args) < 3 {
		die("usage: seg sniff <seconds> | seg raw <2|3> <hex bytes...>")
	}
	switch os.Args[1] {
	case "raw":
		fd, err := openUART("/dev/ttyS" + os.Args[2])
		if err != nil {
			die("open: %v", err)
		}
		b, err := hex.DecodeString(strings.Join(os.Args[3:], ""))
		if err != nil {
			die("hex: %v", err)
		}
		if _, err := unix.Write(fd, b); err != nil {
			die("write: %v", err)
		}
		unix.IoctlSetInt(fd, unix.TCSBRK, 1) // tcdrain
		time.Sleep(10 * time.Millisecond)    // idle gap ends the frame
	case "walk":
		sec, _ := strconv.ParseFloat(os.Args[2], 64)
		walk(time.Duration(sec * float64(time.Second)))
	case "leds": // seg leds <hex bytes, up to 12: LED1 R G B, LED2 R G B ...>
		b, err := hex.DecodeString(strings.Join(os.Args[2:], ""))
		if err != nil {
			die("hex: %v", err)
		}
		// never read this file: the driver's show() sets every channel to 0x80
		const f = "/sys/devices/platform/soc/ff800000.aobus/ff805000.i2c/i2c-1/1-0054/sn3112_gva"
		if err := os.WriteFile(f, b, 0); err != nil {
			die("write: %v", err)
		}
	case "codes": // seg codes <seconds> <ctrl hex> 3-03 3-04 ...
		sec, _ := strconv.ParseFloat(os.Args[2], 64)
		c, _ := strconv.ParseUint(os.Args[3], 16, 8)
		walkCodes(time.Duration(sec*float64(time.Second)), os.Args[4:], byte(c))
	case "sniff":
		sec, _ := strconv.Atoi(os.Args[2])
		names := []string{"ttyS2", "ttyS3"}
		if len(os.Args) > 3 {
			names = strings.Split(os.Args[3], ",")
		}
		sniffList(time.Duration(sec)*time.Second, names)
	case "sniff1": // the bluetooth uart: seg sniff1 <seconds> [ttyS1]
		sec, _ := strconv.Atoi(os.Args[2])
		sniffList(time.Duration(sec)*time.Second, []string{"ttyS1"})
	default:
		die("unknown command")
	}
}

func sniffList(d time.Duration, names []string) {
	type tap struct {
		name   string
		real   int
		master int
		slave  string
	}
	var taps []*tap
	for _, n := range names {
		real, err := openUART("/dev/" + n)
		if err != nil {
			die("open real %s: %v", n, err)
		}
		m, slave, err := openPTY()
		if err != nil {
			die("pty: %v", err)
		}
		if err := unix.Mount(slave, "/dev/"+n, "", unix.MS_BIND, ""); err != nil {
			die("bind %s over /dev/%s: %v", slave, n, err)
		}
		fmt.Printf("%s: %s bound over /dev/%s\n", n, slave, n)
		taps = append(taps, &tap{name: n, real: real, master: m, slave: slave})
	}
	cleanup := func() {
		for _, t := range taps {
			if err := unix.Unmount("/dev/"+t.name, unix.MNT_DETACH); err != nil {
				fmt.Printf("unmount /dev/%s: %v\n", t.name, err)
			}
		}
	}
	defer cleanup()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	var mu sync.Mutex
	start := time.Now()
	for _, t := range taps {
		go func(t *tap) {
			buf := make([]byte, 256)
			var frame []byte
			pfd := []unix.PollFd{{Fd: int32(t.master), Events: unix.POLLIN}}
			for {
				// a gap of >3 ms ends a frame (19200 baud = 0.57 ms per byte)
				n, _ := unix.Poll(pfd, 3)
				if n <= 0 || pfd[0].Revents&unix.POLLIN == 0 {
					if len(frame) > 0 {
						mu.Lock()
						fmt.Printf("%8.3f %s % x\n", time.Since(start).Seconds(), t.name, frame)
						mu.Unlock()
						frame = nil
					}
					if pfd[0].Revents&unix.POLLHUP != 0 {
						time.Sleep(20 * time.Millisecond) // no writer yet
					}
					continue
				}
				r, err := unix.Read(t.master, buf)
				if err != nil || r <= 0 {
					time.Sleep(20 * time.Millisecond)
					continue
				}
				frame = append(frame, buf[:r]...)
				unix.Write(t.real, buf[:r])
			}
		}(t)
	}
	select {
	case <-time.After(d):
	case <-sig:
	}
}

// 7-segment glyphs, bit0 = a ... bit6 = g (confirmed from lenovo_alarm's "16:30" frame).
var glyph = map[byte]byte{
	'0': 0x3f, '1': 0x06, '2': 0x5b, '3': 0x4f, '4': 0x66, '5': 0x6d, '6': 0x7d, '7': 0x07,
	'8': 0x7f, '9': 0x6f, '-': 0x40, ' ': 0x00,
}

func frame(fd int, b ...byte) {
	unix.Write(fd, b)
	unix.IoctlSetInt(fd, unix.TCSBRK, 1)
	time.Sleep(8 * time.Millisecond)
}

func walk(step time.Duration) { walkCodes(step, nil, 0x5d) }

// walkCodes lights only the given codes ("3-03", ...); nil = all icon positions.
func walkCodes(step time.Duration, codes []string, ctrl byte) {
	c2, err := openUART("/dev/ttyS2")
	if err != nil {
		die("ttyS2: %v", err)
	}
	c3, err := openUART("/dev/ttyS3")
	if err != nil {
		die("ttyS3: %v", err)
	}
	frame(c2, 0x18, ctrl)
	frame(c3, 0x18, ctrl)
	show := func(code string, chip, grid, bit int) {
		d2 := make([]byte, 6) // chip 2: grids 0-3 = digits
		for i := 0; i < 4; i++ {
			d2[i] = glyph[code[i]]
		}
		d3 := make([]byte, 6)
		if chip == 3 {
			d3[grid] = 1 << bit
		} else {
			d2[grid] |= 1 << bit
		}
		frame(c2, append([]byte{0x08}, d2...)...)
		frame(c3, append([]byte{0x08}, d3...)...)
		fmt.Printf("%s  chip %d grid %d bit %d\n", code, chip, grid, bit)
	}
	if codes != nil {
		for _, c := range codes {
			var chip, g, b int
			fmt.Sscanf(c, "%d-%1d%1d", &chip, &g, &b)
			show(c, chip, g, b)
			time.Sleep(step)
		}
		frame(c2, 0x08, 0, 0, 0, 0, 0, 0)
		frame(c3, 0x08, 0, 0, 0, 0, 0, 0)
		fmt.Println("done")
		return
	}
	// chip 2 grids 4-5 (beyond the four digits), then all of chip 3
	for _, g := range []int{4, 5} {
		for b := 0; b < 7; b++ {
			show(fmt.Sprintf("2-%d%d", g, b), 2, g, b)
			time.Sleep(step)
		}
	}
	for g := 0; g < 6; g++ {
		for b := 0; b < 7; b++ {
			show(fmt.Sprintf("3-%d%d", g, b), 3, g, b)
			time.Sleep(step)
		}
	}
	frame(c2, 0x08, 0, 0, 0, 0, 0, 0)
	frame(c3, 0x08, 0, 0, 0, 0, 0, 0)
	fmt.Println("done")
}
