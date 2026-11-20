// btinit: bring up the clock's Bluetooth radio (BCM4345C5 on /dev/ttyS1, HCI over H4).
//
//	btinit probe                 send HCI Reset at each baud rate, print what answers
//	btinit load <file.hcd>       Broadcom patchram download (HCD records), then baud switch
//	btinit attach                put ttyS1 into the kernel HCI line discipline (hci0 appears)
//	load implies attach by default; -no-attach skips it. Nothing runs from the chip's
//	firmware; this only makes /dev/hci0 exist so clockd can scan.
package main

import (
	"encoding/binary"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

const dev = "/dev/ttyS1"

// flow turns on RTS/CTS hardware flow control (the chip only transmits while
// the host's RTS is asserted).
var flow bool

// The UART's clock is 24 MHz / 3 = 8 MHz and the divider is an integer, so the
// real rate is 8 MHz / round(8 MHz / baud): 115200 (-> 115942), 1000000,
// 2000000 and 4000000 are usable; 921600 is 3.5 % off, 1500000 really runs at
// 1600000 and 3000000 at 2666667, which the chip cannot read.
var probeBauds = []int{115200, 1000000, 2000000, 4000000, 921600, 1500000, 3000000}

func die(f string, a ...any) { fmt.Fprintf(os.Stderr, "btinit: "+f+"\n", a...); os.Exit(1) }

func openUART(baud int) (*os.File, error) {
	f, err := os.OpenFile(dev, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	speed := speedOf(baud)
	if speed == 0 {
		f.Close()
		return nil, fmt.Errorf("unsupported baud %d", baud)
	}
	t, err := unix.IoctlGetTermios(int(f.Fd()), unix.TCGETS)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("tcgets: %w", err)
	}
	t.Iflag = unix.IGNPAR
	t.Oflag = 0
	t.Lflag = 0
	t.Cflag = unix.CS8 | unix.CLOCAL | unix.CREAD | speed
	if flow {
		t.Cflag |= unix.CRTSCTS
	}
	t.Ispeed, t.Ospeed = 0, 0
	// VMIN 0 / VTIME 1: a read waits up to 100 ms and then returns 0 bytes.
	t.Cc[unix.VMIN], t.Cc[unix.VTIME] = 0, 1
	if err := unix.IoctlSetTermios(int(f.Fd()), unix.TCSETS, t); err != nil {
		f.Close()
		return nil, fmt.Errorf("tcsets: %w", err)
	}
	return f, nil
}

func speedOf(baud int) uint32 {
	switch baud {
	case 115200:
		return unix.B115200
	case 230400:
		return unix.B230400
	case 460800:
		return unix.B460800
	case 921600:
		return unix.B921600
	case 1000000:
		return unix.B1000000
	case 1500000:
		return unix.B1500000
	case 2000000:
		return unix.B2000000
	case 4000000:
		return unix.B4000000
	case 3000000:
		return unix.B3000000
	}
	return 0
}

// readFor collects bytes for d and returns them (H4 framing not parsed).
func readFor(f *os.File, d time.Duration) []byte {
	var out []byte
	deadline := time.Now().Add(d)
	buf := make([]byte, 512)
	for time.Now().Before(deadline) {
		if n := rd(f, buf); n > 0 {
			out = append(out, buf[:n]...)
		}
	}
	return out
}

// rd reads what the UART has, waiting at most the termios VTIME (100 ms), and
// returns the byte count (0 = nothing yet). It goes around os.File.Read on
// purpose: Go turns a 0-byte read into io.EOF, and the first version of this
// tool took that for "port closed" and stopped listening after microseconds -
// which is why the chip looked dead (2026-11-06) while it answered every Reset.
func rd(f *os.File, buf []byte) int {
	n, err := unix.Read(int(f.Fd()), buf)
	if err != nil || n < 0 {
		time.Sleep(5 * time.Millisecond)
		return 0
	}
	return n
}

func main() {
	noAttach := flag.Bool("no-attach", false, "after load, don't attach the HCI line discipline")
	baud := flag.Int("baud", 115200, "UART rate the chip listens at (vendor default)")
	flag.BoolVar(&flow, "flow", false, "RTS/CTS hardware flow control")
	wait := flag.Duration("wait", 400*time.Millisecond, "probe: how long to wait for an answer")
	scopeFor := flag.Duration("for", 2*time.Second, "scope: how long to sample")
	scopeMask := flag.Uint("mask", 0x200f00, "scope: GPIOX bits to watch (8 TX, 9 RX, 10 CTS, 11 RTS, 21 power)")
	flag.Parse()
	cmd := flag.Arg(0)
	switch cmd {
	case "probe":
		// A powered, H4-capable chip answers HCI Reset with a Command Complete event.
		bauds := probeBauds
		if flag.NArg() > 1 {
			bauds = nil
			for _, a := range flag.Args()[1:] {
				var v int
				fmt.Sscan(a, &v)
				bauds = append(bauds, v)
			}
		}
		for _, pb := range bauds {
			f, err := openUART(pb)
			if err != nil {
				fmt.Printf("%d open: %v\n", pb, err)
				continue
			}
			if pre := readFor(f, 100*time.Millisecond); len(pre) > 0 {
				fmt.Printf("%d before -> %s\n", pb, hex.EncodeToString(pre))
			}
			f.Write([]byte{0x01, 0x03, 0x0c, 0x00}) // HCI Reset
			r := readFor(f, *wait)
			fmt.Printf("%d reset -> %s\n", pb, hexOr("silence", r))
			f.Close()
		}
	case "load":
		hcd := flag.Arg(1)
		if hcd == "" {
			die("load needs the .hcd path")
		}
		if err := load(hcd, !*noAttach, *baud); err != nil {
			die("%v", err)
		}
	case "listen":
		// what the chip says on its own after a power cycle (no writes)
		for _, b := range probeBauds {
			f, err := openUART(b)
			if err != nil {
				continue
			}
			if r := readFor(f, 3*time.Second); len(r) > 0 {
				fmt.Printf("%-8d says %s\n", b, hex.EncodeToString(r))
			} else {
				fmt.Printf("%-8d quiet\n", b)
			}
			f.Close()
		}
	case "scope":
		if err := scope(*scopeFor, uint32(*scopeMask)); err != nil {
			die("%v", err)
		}
	case "sysup":
		// what clockd does: HCI socket, HCIDEVUP ioctl (id from argv), bind,
		// filter - for comparing against hciconfig on the same live device
		id := 0
		if v := flag.Arg(1); v != "" {
			fmt.Sscan(v, &id)
		}
		if ents, _ := os.ReadDir("/sys/class/bluetooth"); true {
			names := []string{}
			for _, e := range ents {
				names = append(names, e.Name())
			}
			fmt.Println("sysfs:", names)
		}
		sock, err := unix.Socket(unix.AF_BLUETOOTH, unix.SOCK_RAW|unix.SOCK_CLOEXEC, 1)
		if err != nil {
			die("socket: %v", err)
		}
		_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(sock), 0x400448c9, uintptr(id))
		fmt.Printf("HCIDEVUP(0x400448c9, %d): errno %v (%s)\n", id, errno, errno.Error())
		if errno == 0 {
			if err := unix.Bind(sock, &unix.SockaddrHCI{Dev: uint16(id), Channel: 0}); err != nil {
				die("bind: %v", err)
			}
			var filt [14]byte
			filt[4], filt[5] = 0xff, 0xff
			filt[8], filt[9] = 0xff, 0xff
			_, _, e2 := unix.Syscall6(unix.SYS_SETSOCKOPT, uintptr(sock), 0, 2, uintptr(unsafe.Pointer(&filt[0])), 14, 0)
			fmt.Println("setsockopt errno:", e2)
			// LE scan enable (passive params already set by whoever, or defaults)
			if _, _, e3 := unix.Syscall6(unix.SYS_SENDTO, uintptr(sock), uintptr(unsafe.Pointer(&[]byte{0x0c, 0x20, 0x02, 0x01, 0x00}[0])), 5, 0, 0, 0); e3 != 0 {
				fmt.Println("scan enable send errno:", e3)
			}
			buf := make([]byte, 512)
			ts := unix.NsecToTimespec(3_000_000_000)
			unix.Ppoll([]unix.PollFd{{Fd: int32(sock), Events: unix.POLLIN}}, &ts, nil)
			n, _ := unix.Read(sock, buf)
			fmt.Printf("read %d bytes: % x\n", n, buf[:n])
		}
	case "attach":
		if err := attach(*baud); err != nil {
			die("%v", err)
		}
	default:
		fmt.Println("usage: btinit probe | load <file.hcd> | attach")
		os.Exit(2)
	}
}

func hexOr(s string, b []byte) string {
	if len(b) == 0 {
		return s
	}
	return hex.EncodeToString(b)
}

// The .hcd on this clock is a plain sequence of HCI command packets, each
// [opcode LE 2][plen 1][payload], without the H4 type byte and without expected-
// event records: send each as H4 and wait for its Command Complete (0x0E).
// It begins with a "BRCmcfgS" config block (vendor opcode 0xFC4C) and ends with
// the launch; the chip reboeps into the firmware.
func load(path string, doAttach bool, baud int) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	f, err := openUART(baud)
	if err != nil {
		return err
	}
	defer f.Close()
	drain(f)
	f.Write([]byte{0x01, 0x03, 0x0c, 0x00})
	if ev := waitCC(f, 0x0c03, 3*time.Second); ev != nil {
		fmt.Printf("reset -> %s\n", hex.EncodeToString(ev))
	} else {
		fmt.Println("reset -> silence (continuing)")
	}
	// Broadcom "Download Minidriver": puts the ROM into download mode; the
	// records that follow are Write RAM (0xfc4c) and end with Launch RAM (0xfc4e).
	f.Write([]byte{0x01, 0x2e, 0xfc, 0x00})
	if ev := waitCC(f, 0xfc2e, 2*time.Second); ev == nil {
		return fmt.Errorf("no answer to Download Minidriver")
	}
	time.Sleep(50 * time.Millisecond)
	i, n := 0, 0
	for i+4 <= len(data) {
		op := binary.LittleEndian.Uint16(data[i:])
		pl := int(data[i+2])
		if i+3+pl > len(data) {
			return fmt.Errorf("short packet at %d", i)
		}
		pkt := append([]byte{0x01}, data[i:i+3+pl]...)
		i += 3 + pl
		if _, err := f.Write(pkt); err != nil {
			return fmt.Errorf("write packet %d: %w", n, err)
		}
		ev := waitCC(f, op, 2*time.Second)
		if ev == nil {
			return fmt.Errorf("packet %d (opcode %#06x): no Command Complete", n, op)
		}
		if ev[0] == 0xff { // vendor "byte" acks: not errors, just unusual
			n++
			continue
		}
		if ev[0] == 0x0e && len(ev) >= 6 && ev[5] != 0 {
			return fmt.Errorf("packet %d (opcode %#06x): status %#x", n, op, ev[5])
		}
		n++
		if n%32 == 0 {
			fmt.Printf("\r%d packets", n)
		}
	}
	fmt.Printf("\r%d packets done\n", n)
	// the chip restarts into the patched firmware (same 115200 baud)
	time.Sleep(400 * time.Millisecond)
	drain(f)
	f.Write([]byte{0x01, 0x03, 0x0c, 0x00})
	if ev := waitCC(f, 0x0c03, 2*time.Second); ev == nil {
		return fmt.Errorf("no answer to HCI Reset after the download")
	} else {
		fmt.Printf("HCI Reset -> %s\n", hex.EncodeToString(ev))
	}
	for _, q := range []struct {
		name string
		op   uint16
	}{{"local version", 0x1001}, {"BD_ADDR", 0x1009}, {"local name", 0x0c14}} {
		f.Write([]byte{0x01, byte(q.op), byte(q.op >> 8), 0x00})
		ev := waitCC(f, q.op, 2*time.Second)
		if q.op == 0x0c14 && len(ev) > 6 {
			name := ev[6:]
			for k, c := range name {
				if c == 0 {
					name = name[:k]
					break
				}
			}
			fmt.Printf("%s -> %q\n", q.name, name)
			continue
		}
		fmt.Printf("%s -> %s\n", q.name, hexOr("silence", ev))
	}
	if doAttach {
		f.Close()
		return attach(baud)
	}
	return nil
}

// drain throws away whatever is in the input buffer.
func drain(f *os.File) { readFor(f, 100*time.Millisecond) }

// waitCC reads until a Command Complete for op (or a vendor event), skipping
// unrelated events, and returns it without the H4 byte: event code, length,
// packet count, opcode (2), status, return values.
func waitCC(f *os.File, op uint16, d time.Duration) []byte {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		ev := readPacket(f, time.Until(deadline))
		if ev == nil {
			continue
		}
		if ev[0] == 0x0e { // Command Complete: [0e] [plen] [num] [opcode LE 2] [status]...
			if len(ev) >= 5 && binary.LittleEndian.Uint16(ev[3:]) == op {
				return ev
			}
			continue // somebody else's completion
		}
		if ev[0] == 0xff || ev[0] == 0x0f {
			return ev // vendor / status, caller decides
		}
	}
	return nil
}

// readPacket reads one H4 event packet: type byte, then header, then payload,
// however slowly the chip sends them.
func readPacket(f *os.File, d time.Duration) []byte {
	if d <= 0 {
		return nil
	}
	deadline := time.Now().Add(d)
	var buf []byte
	// type, event code, plen
	for len(buf) < 3 {
		if !time.Now().Before(deadline) {
			return nil
		}
		one := make([]byte, 1)
		if rd(f, one) == 1 {
			buf = append(buf, one[0])
		}
	}
	need := 3 + int(buf[2]) // type, event code, plen, then plen bytes
	for len(buf) < need && time.Now().Before(deadline) {
		grow := make([]byte, need-len(buf))
		n := rd(f, grow)
		if n > 0 {
			buf = append(buf, grow[:n]...)
		}
	}
	if len(buf) < need {
		return nil
	}
	return buf[1:need] // event code first
}

// attach puts ttyS1 into the kernel HCI line discipline (N_HCI); the Bluetooth
// subsystem then creates hci0 and manages the UART.
//
// The line discipline lives only as long as the port stays open, so attach does
// not return: run it in the background and kill it to detach (like hciattach).
func attach(baud int) error {
	flow = true // the kernel's H4 driver expects RTS/CTS
	f, err := openUART(baud)
	if err != nil {
		return err
	}
	defer f.Close()
	const (
		N_HCI           = 15
		HCIUARTSETPROTO = 0x400455c8 // _IOW('U', 200, int)
		HCI_UART_H4     = 0
	)
	if err := unix.IoctlSetPointerInt(int(f.Fd()), unix.TIOCSETD, N_HCI); err != nil {
		return fmt.Errorf("TIOCSETD N_HCI: %w", err)
	}
	if err := unix.IoctlSetInt(int(f.Fd()), HCIUARTSETPROTO, HCI_UART_H4); err != nil {
		return fmt.Errorf("HCIUARTSETPROTO H4: %w", err)
	}
	fmt.Println("line discipline set, H4; hci0 should appear (holding the port open)")
	for {
		time.Sleep(time.Hour)
	}
}

// scope samples the GPIOX input register straight from /dev/mem in a tight loop
// and prints every change of the watched pins as "microseconds value": a poor
// man's logic analyser for the UART lines (pins keep reading while muxed to the
// UART). Bits: 8 TX (to chip), 9 RX (from chip), 10 CTS (chip's RTS), 11 RTS
// (chip's CTS), 21 bt_rfkill power.
func scope(d time.Duration, mask uint32) error {
	fd, err := unix.Open("/dev/mem", unix.O_RDONLY|unix.O_SYNC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	m, err := unix.Mmap(fd, 0xff634000, 4096, unix.PROT_READ, unix.MAP_SHARED)
	if err != nil {
		return fmt.Errorf("mmap: %w", err)
	}
	reg := (*uint32)(unsafe.Pointer(&m[0x450]))
	type ev struct {
		t time.Duration
		v uint32
	}
	evs := make([]ev, 0, 1<<20)
	start := time.Now()
	last := atomic.LoadUint32(reg) & mask
	evs = append(evs, ev{0, last})
	n := 0
	for {
		v := atomic.LoadUint32(reg) & mask
		n++
		if v != last {
			evs = append(evs, ev{time.Since(start), v})
			last = v
			if len(evs) == cap(evs) {
				break
			}
		} else if n&0xfff == 0 && time.Since(start) > d {
			break
		}
	}
	el := time.Since(start)
	fmt.Printf("# %d samples in %v (%.0f ns/sample), %d changes\n", n, el, float64(el.Nanoseconds())/float64(n), len(evs)-1)
	for _, e := range evs {
		fmt.Printf("%.1f %06x\n", float64(e.t.Nanoseconds())/1000, e.v)
	}
	return nil
}
