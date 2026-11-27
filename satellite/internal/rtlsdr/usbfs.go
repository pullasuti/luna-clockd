// Package rtlsdr drives an RTL2832U USB stick with an R820T/R820T2 tuner from
// user space, over Linux usbfs (/dev/bus/usb/BBB/DDD): plain ioctls, no
// libusb, no cgo, no kernel driver (the clock's kernel has none for it).
//
// The RTL2832U side uses the chip's register interface as found on these
// sticks; the R820T side is written from Rafael Micro's "R820T2 Register
// Description" (VER1.3) and keeps every bit that document does not describe
// as the chip powered up with (see r820t.go).
package rtlsdr

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

// usbfs ioctls (linux/usbdevice_fs.h, 64-bit layouts)
const (
	ioctlControl        = 0xC0185500 // USBDEVFS_CONTROL
	ioctlBulk           = 0xC0185502 // USBDEVFS_BULK
	ioctlClaimInterface = 0x8004550F // USBDEVFS_CLAIMINTERFACE
	ioctlRelease        = 0x80045510 // USBDEVFS_RELEASEINTERFACE
	ioctlReset          = 0x00005514 // USBDEVFS_RESET
)

type ctrlTransfer struct {
	ReqType uint8
	Req     uint8
	Value   uint16
	Index   uint16
	Length  uint16
	Timeout uint32 // ms
	_       uint32
	Data    uintptr
}

type bulkTransfer struct {
	Ep      uint32
	Len     uint32
	Timeout uint32 // ms
	_       uint32
	Data    uintptr
}

// usbDev is an open usbfs device node.
type usbDev struct {
	fd int
}

func openUSB(path string) (*usbDev, error) {
	fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	d := &usbDev{fd: fd}
	iface := uint32(0)
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), ioctlClaimInterface, uintptr(unsafe.Pointer(&iface))); e != 0 {
		syscall.Close(fd)
		return nil, fmt.Errorf("claim interface: %w", e)
	}
	return d, nil
}

func (d *usbDev) close() {
	iface := uint32(0)
	syscall.Syscall(syscall.SYS_IOCTL, uintptr(d.fd), ioctlRelease, uintptr(unsafe.Pointer(&iface)))
	syscall.Close(d.fd)
}

func (d *usbDev) control(reqType uint8, value, index uint16, buf []byte) (int, error) {
	c := ctrlTransfer{ReqType: reqType, Value: value, Index: index, Length: uint16(len(buf)), Timeout: 300}
	if len(buf) > 0 {
		c.Data = uintptr(unsafe.Pointer(&buf[0]))
	}
	n, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(d.fd), ioctlControl, uintptr(unsafe.Pointer(&c)))
	if e != 0 {
		return 0, e
	}
	return int(n), nil
}

func (d *usbDev) reset() error {
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(d.fd), ioctlReset, 0); e != 0 {
		return e
	}
	return nil
}

// bulkIn reads up to len(buf) bytes from endpoint ep (0x81 for the samples).
func (d *usbDev) bulkIn(ep uint32, buf []byte, timeoutMs uint32) (int, error) {
	b := bulkTransfer{Ep: ep, Len: uint32(len(buf)), Timeout: timeoutMs, Data: uintptr(unsafe.Pointer(&buf[0]))}
	n, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(d.fd), ioctlBulk, uintptr(unsafe.Pointer(&b)))
	if e != 0 {
		return 0, e
	}
	return int(n), nil
}

// Known sticks (vendor:product). Most RTL-SDR sticks use 0bda:2838.
var knownIDs = map[string]bool{"0bda:2838": true, "0bda:2832": true}

// ErrTuner: an RTL2832U stick with a tuner this driver doesn't know.
var ErrTuner = errors.New("this stick's tuner is not supported yet (only R820T and R820T2)")

// ErrNoStick: no RTL2832U stick on the bus.
var ErrNoStick = errors.New("no RTL-SDR stick plugged in")

// Find returns the usbfs node of the first RTL2832U stick and its product name.
func Find() (node, product string, err error) {
	devs, _ := filepath.Glob("/sys/bus/usb/devices/*")
	for _, d := range devs {
		vid, e1 := os.ReadFile(filepath.Join(d, "idVendor"))
		pid, e2 := os.ReadFile(filepath.Join(d, "idProduct"))
		if e1 != nil || e2 != nil {
			continue
		}
		if !knownIDs[strings.TrimSpace(string(vid))+":"+strings.TrimSpace(string(pid))] {
			continue
		}
		bus, e1 := os.ReadFile(filepath.Join(d, "busnum"))
		dev, e2 := os.ReadFile(filepath.Join(d, "devnum"))
		if e1 != nil || e2 != nil {
			continue
		}
		var b, n int
		fmt.Sscan(strings.TrimSpace(string(bus)), &b)
		fmt.Sscan(strings.TrimSpace(string(dev)), &n)
		p, _ := os.ReadFile(filepath.Join(d, "product"))
		return fmt.Sprintf("/dev/bus/usb/%03d/%03d", b, n), strings.TrimSpace(string(p)), nil
	}
	return "", "", ErrNoStick
}
