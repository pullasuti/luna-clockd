// rtlprobe: talk to an RTL2832U over usbfs (no kernel driver) and identify the tuner.
package main

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

type ctrl struct {
	ReqType uint8
	Req     uint8
	Value   uint16
	Index   uint16
	Length  uint16
	Timeout uint32
	_       uint32
	Data    uintptr
}

const usbdevfsControl = 0xC0185500

var fd int

func xfer(reqType uint8, value, index uint16, buf []byte) (int, error) {
	c := ctrl{ReqType: reqType, Value: value, Index: index, Length: uint16(len(buf)), Timeout: 300}
	if len(buf) > 0 {
		c.Data = uintptr(unsafe.Pointer(&buf[0]))
	}
	n, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), usbdevfsControl, uintptr(unsafe.Pointer(&c)))
	if e != 0 {
		return 0, e
	}
	return int(n), nil
}

const (
	demodB = 0
	usbB   = 1
	sysB   = 2
	iicB   = 6
)

func readArr(block uint16, addr uint16, n int) ([]byte, error) {
	b := make([]byte, n)
	_, err := xfer(0xC0, addr, block<<8, b)
	return b, err
}
func writeArr(block uint16, addr uint16, data []byte) error {
	_, err := xfer(0x40, addr, (block<<8)|0x10, data)
	return err
}
func writeReg(block, addr uint16, val uint16, n int) error {
	d := []byte{byte(val >> 8), byte(val)}
	if n == 1 {
		d = d[1:]
	}
	return writeArr(block, addr, d)
}
func demodWrite(page, addr uint16, val uint16, n int) error {
	d := []byte{byte(val >> 8), byte(val)}
	if n == 1 {
		d = d[1:]
	}
	if _, err := xfer(0x40, (addr<<8)|0x20, 0x10|page, d); err != nil {
		return err
	}
	b := make([]byte, 1)
	_, err := xfer(0xC0, (0x01<<8)|0x20, 0x0a, b) // dummy read to sync
	return err
}
func i2cRead(i2c uint16, reg byte) (byte, error) {
	if err := writeArr(iicB, i2c, []byte{reg}); err != nil {
		return 0, err
	}
	b, err := readArr(iicB, i2c, 1)
	if err != nil {
		return 0, err
	}
	return b[0], nil
}

func main() {
	path := "/dev/bus/usb/001/002"
	if len(os.Args) > 1 {
		path = os.Args[1]
	}
	var err error
	fd, err = syscall.Open(path, syscall.O_RDWR, 0)
	if err != nil {
		fmt.Println("open:", err)
		os.Exit(1)
	}
	must := func(what string, err error) {
		if err != nil {
			fmt.Println(what+":", err)
			os.Exit(1)
		}
	}
	// librtlsdr init_baseband (start)
	must("usb sysctl", writeReg(usbB, 0x2000, 0x09, 1))
	must("epa maxpkt", writeReg(usbB, 0x2158, 0x0002, 2))
	must("epa ctl", writeReg(usbB, 0x2148, 0x1002, 2))
	must("demod ctl1", writeReg(sysB, 0x300b, 0x22, 1))
	must("demod ctl", writeReg(sysB, 0x3000, 0xe8, 1))
	must("demod reset1", demodWrite(1, 0x01, 0x14, 1))
	must("demod reset2", demodWrite(1, 0x01, 0x10, 1))
	b, err := readArr(usbB, 0x2000, 1)
	must("read sysctl", err)
	fmt.Printf("RTL2832U answers: USB_SYSCTL=0x%02x\n", b[0])

	must("i2c repeater on", demodWrite(1, 0x01, 0x18, 1))
	tuners := []struct {
		name string
		addr uint16
		reg  byte
		want byte
	}{
		{"R820T/R820T2/R860", 0x34, 0x00, 0x69},
		{"R828D", 0x74, 0x00, 0x69},
		{"E4000", 0xc8, 0x02, 0x40},
		{"FC0013", 0xc6, 0x00, 0xa3},
		{"FC0012", 0xc6, 0x00, 0xa1},
		{"FC2580", 0xac, 0x01, 0x56},
	}
	found := false
	for _, t := range tuners {
		v, err := i2cRead(t.addr, t.reg)
		if err == nil && v == t.want {
			fmt.Printf("tuner: %s (i2c 0x%02x reg%d = 0x%02x)\n", t.name, t.addr, t.reg, v)
			found = true
			break
		}
	}
	if !found {
		fmt.Println("tuner: not identified")
	}
	demodWrite(1, 0x01, 0x10, 1) // repeater off
}
