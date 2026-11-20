package main

import (
	"encoding/binary"
	"testing"

	esphome "github.com/ygelfand/go-esphome-device"
	"github.com/ygelfand/go-esphome-device/api"
)

// The HCI constants are ioctl/socket numbers the kernel checks exactly.
func TestHCIConstants(t *testing.T) {
	cases := []struct {
		name string
		got  uint64
		want uint64
	}{
		// _IOW('H', 201, int): dir 1, size 4, type 0x48, nr 201
		{"HCIDEVUP", btHCIDEVUP, 1<<30 | 4<<16 | 0x48<<8 | 201},
		// _IOW('U', 200, int)
		{"HCIUARTSETPROTO", btHCIUARTSETPROTO, 1<<30 | 4<<16 | 0x55<<8 | 200},
		{"N_HCI", btN_HCI, 15},
		{"SOL_HCI", btSOLHCI, 0},
		{"HCI_FILTER", btHCI_FILTER, 2},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %#x, want %#x", c.name, c.got, c.want)
		}
	}
}

func TestHCDRecords(t *testing.T) {
	// two records (op, plen, payload) then a truncated tail
	rec := func(op uint16, payload ...byte) []byte {
		b := []byte{byte(op), byte(op >> 8), byte(len(payload))}
		return append(b, payload...)
	}
	data := append(append(rec(0xfc4c, []byte("BRCmcfgS")...), rec(0xfc4e, 1, 2, 3)...), 0xaa, 0xbb)
	pkts := btHCDRecords(data)
	if len(pkts) != 2 {
		t.Fatalf("got %d records, want 2", len(pkts))
	}
	if binary.LittleEndian.Uint16(pkts[0]) != 0xfc4c || string(pkts[0][3:]) != "BRCmcfgS" {
		t.Errorf("record 0 = %x", pkts[0])
	}
	if len(pkts[1]) != 6 || pkts[1][2] != 3 {
		t.Errorf("record 1 = %x", pkts[1])
	}
	if pkts := btHCDRecords(nil); len(pkts) != 0 {
		t.Errorf("empty input gave %d records", len(pkts))
	}
}

// A Command Complete as it comes off the wire: 04 0e plen num opcodeLE status.
func TestCCParsing(t *testing.T) {
	// HCI Reset accepted: 04 0e 04 01 03 0c 00
	ev := []byte{0x04, 0x0e, 0x04, 0x01, 0x03, 0x0c, 0x00}
	if cc := btCCFor(ev, 0x0c03); cc == nil {
		t.Fatal("Reset completion not matched")
	}
	if cc := btCCFor(ev, 0x1009); cc != nil {
		t.Error("matched the wrong opcode")
	}
	// wrong event codes must not match
	for _, bad := range [][]byte{
		{0x04, 0x0f, 0x04, 0x00, 0x03, 0x0c, 0x00}, // command status
		{0x04, 0x0e, 0x02, 0x01},                   // too short
		{0x01, 0x0e, 0x04, 0x01, 0x03, 0x0c, 0x00}, // not an event
	} {
		if cc := btCCFor(bad, 0x0c03); cc != nil {
			t.Errorf("matched %#x", bad)
		}
	}
	// socket side: [num][opcode LE2][status] without type/plen, status at ev[3]
	sock := []byte{0x04, 0x0e, 0x04, 0x01, 0x03, 0x0c, 0x02}
	ev2, ok := btCommandComplete(sock, 0x0c03)
	if !ok || len(ev2) < 4 || ev2[3] != 0x02 {
		t.Fatalf("btCommandComplete = %v %v, want status 2 at ev[3]", ev2, ok)
	}
	if _, ok := btCommandComplete(sock, 0x1009); ok {
		t.Error("socket match for wrong opcode")
	}
}

// One LE Advertising Report meta event with two reports; the second carries
// no RSSI (127) and must be dropped. Data shaped like a Ruuvi tag
// (service data 0x9904).
func TestAdvReports(t *testing.T) {
	// report 1: connectable, random address FE:08:CE:9D:B0:37 (little-endian
	// on the wire), flags + service data 0x9904, RSSI -62 (0xc2 as int8)
	r1 := append([]byte{0x00, 0x01, 0x37, 0xb0, 0x9d, 0xce, 0x08, 0xfe, 11},
		0x02, 0x01, 0x06, // flags
		0x08, 0x16, 0x99, 0x04, 0x00, 0x03, 0xe2, 0xb8, // service data 0x9904 + payload
		0xc2) // rssi -62
	// report 2: no RSSI (127)
	r2 := []byte{0x00, 0x00, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0, 0x7f}

	params := append([]byte{btEvtLEAdvReport, 0x02}, r1...)
	params = append(params, r2...)
	ev := append([]byte{0x04, btEvtLEMeta, byte(len(params))}, params...)

	ads := btAdvReports(ev)
	if len(ads) != 1 {
		t.Fatalf("got %d ads, want 1 (the RSSI-less one is dropped)", len(ads))
	}
	a := ads[0]
	if want := uint64(0xfe08ce9db037); a.Address != want { // little-endian value of the 6 bytes
		t.Errorf("address = %#x, want %#x", a.Address, want)
	}
	if a.AddressType != 1 {
		t.Errorf("address type = %d, want 1 (random)", a.AddressType)
	}
	if a.Rssi != -62 {
		t.Errorf("rssi = %d, want -62", a.Rssi)
	}
	wantData := []byte{0x02, 0x01, 0x06, 0x08, 0x16, 0x99, 0x04, 0x00, 0x03, 0xe2, 0xb8}
	if len(a.Data) != len(wantData) {
		t.Fatalf("data = %x, want %x", a.Data, wantData)
	}
	for i := range wantData {
		if a.Data[i] != wantData[i] {
			t.Fatalf("data = %x, want %x", a.Data, wantData)
		}
	}

	// non-advertising events give nothing
	if ads := btAdvReports([]byte{0x04, 0x3e, 3, 0x01, 0x01, 0x00}); len(ads) != 0 {
		t.Errorf("subevent 0x01 parsed as advertisements: %v", ads)
	}
	// truncated report stream gives nothing (must not panic or half-parse)
	bad := []byte{0x04, btEvtLEMeta, 5, btEvtLEAdvReport, 0x01, 0x00, 0x01, 0xaa}
	if ads := btAdvReports(bad); len(ads) != 0 {
		t.Errorf("truncated report parsed: %v", ads)
	}
}

func TestNextEvent(t *testing.T) {
	ev1 := []byte{0x04, 0x0e, 0x02, 0x01, 0x00}
	ev2 := []byte{0x04, 0x3e, 0x00}
	stream := append(append([]byte{0xff, 0x00}, ev1...), ev2...)
	ev, rest, ok := btNextEvent(stream)
	if !ok || string(ev) != string(ev1) {
		t.Fatalf("first event = %v ok=%v", ev, ok)
	}
	ev, rest, ok = btNextEvent(rest)
	if !ok || string(ev) != string(ev2) {
		t.Fatalf("second event = %v ok=%v", ev, ok)
	}
	if _, _, ok := btNextEvent(rest); ok {
		t.Error("tail reported as event")
	}
	// a partial event waits for more
	part := []byte{0x04, 0x0e, 0x10, 0x01}
	if _, _, ok := btNextEvent(part); ok {
		t.Error("partial event reported as complete")
	}
}

func TestScannerStateMapping(t *testing.T) {
	for st, want := range map[string]api.BluetoothScannerState{
		"starting":  esphome.ScannerStarting,
		"scanning":  esphome.ScannerRunning,
		"failed":    esphome.ScannerFailed,
		"off":       esphome.ScannerStopped,
		"waiting":   esphome.ScannerStopped,
		"anything ": esphome.ScannerStopped,
	} {
		if got := btScannerState(st); got != want {
			t.Errorf("state %q -> %v, want %v", st, got, want)
		}
	}
}
