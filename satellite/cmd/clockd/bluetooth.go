// Bluetooth proxy: forwards BLE advertisements to Home Assistant.
//
// The radio is the AP6256 module's Bluetooth side (BCM4345C5) on /dev/ttyS1.
// bringUp loads the vendor .hcd over the UART (Broadcom patchram, as verified
// with tools/btinit 2026-11-12), hands the port to the kernel's HCI line
// discipline (hci0), then talks to the kernel through an HCI raw socket:
// passive LE scanning while Home Assistant is subscribed. Advertisements are
// forwarded through the library's BluetoothProxy (raw advertisements mode).
//
// Everything is behind the bluetooth.proxy setting (default off) and starts or
// stops with it at runtime. Connections/GATT are not offered: HA gets an
// advertisement-only, passive scanner (what it needs for Ruuvi-style sensors).
package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
	"unsafe"

	esphome "github.com/ygelfand/go-esphome-device"
	"github.com/ygelfand/go-esphome-device/api"
	"golang.org/x/sys/unix"

	"lenovo-satellite/internal/config"
)

const (
	btUART       = "/dev/ttyS1"
	btBaud       = 115200 // the only well-supported rate (8 MHz / integer divider)
	btHCIName    = "/sys/class/bluetooth/hci0"
	btFlushEvery = 300 * time.Millisecond
	btMaxBatch   = 32
	btRetryDelay = 30 * time.Second
	btMaxRetries = 3
)

// HCI bits the kernel interface needs (linux/hci.h, hci_uart.h).
const (
	btHCIDEVUP        = 0x400448c9 // _IOW('H', 201, int)
	btHCIUARTSETPROTO = 0x400455c8 // _IOW('U', 200, int)
	btN_HCI           = 15         // N_HCI line discipline
	btHCI_UART_H4     = 0
	btSOLHCI          = 0 // socket level for the event filter
	btHCI_FILTER      = 2
	btEvtCommandCmpl  = 0x0e
	btEvtLEMeta       = 0x3e
	btEvtVendor       = 0xff
	btOpReset         = 0x0c03
	btOpReadBDADDR    = 0x1009
	btOpLEScanParams  = 0x200b
	btOpLEScanEnable  = 0x200c
	btOpDownloadMini  = 0xfc2e
	btEvtLEAdvReport  = 0x02 // LE meta subevent
	btAdvRSSIUnknown  = 127
)

// bluetoothCtl owns the proxy lifecycle: one session (radio up + scanning) at
// a time, started and stopped by the config, scanned while HA subscribes.
type bluetoothCtl struct {
	d     *daemon
	proxy *esphome.BluetoothProxy

	mu       sync.Mutex
	want     bool // the setting
	session  *btSession
	state    string // "off"|"starting"|"waiting"|"scanning"|"failed"
	err      string
	addr     string // the radio's BD_ADDR once known
	advTotal uint64
	advTimes []time.Time
}

// btSession is one radio bring-up: the UART fd holding the HCI line
// discipline, the HCI socket, and the goroutine that owns it all. The socket
// has a single goroutine (run/readLoop): everything that wants a scan change
// (HA subscribing, mode changes) sends its wish over ctrl instead of writing
// to the socket itself, so reads never race.
type btSession struct {
	b      *bluetoothCtl
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	ctrl   chan btDesire

	ctrlMu sync.Mutex // guards coalescing into ctrl
}

// btDesire is the wanted scanner state.
type btDesire struct {
	scan   bool
	active bool
}

func newBluetooth(d *daemon) *bluetoothCtl {
	b := &bluetoothCtl{d: d, state: "off"}
	b.proxy = &esphome.BluetoothProxy{
		OnSubscribed: b.subscribedChanged,
		OnMode:       b.modeChanged,
	}
	return b
}

// setConfig starts or stops the session to match the setting (no-op when it
// already matches: every settings save runs ApplyConfig in full).
func (b *bluetoothCtl) setConfig(on bool) {
	b.mu.Lock()
	b.want = on
	if on && b.session == nil {
		b.state, b.err = "starting", ""
		b.mu.Unlock()
		b.d.log.Info("bluetooth: bringing up the radio")
		ctx, cancel := context.WithCancel(context.Background())
		s := &btSession{b: b, ctx: ctx, cancel: cancel, done: make(chan struct{}), ctrl: make(chan btDesire, 1)}
		b.mu.Lock()
		b.session = s
		b.mu.Unlock()
		go s.run()
		return
	}
	if !on && b.session != nil {
		s := b.session
		b.mu.Unlock()
		s.stop()
		b.mu.Lock()
		b.session, b.state, b.err = nil, "off", ""
		b.mu.Unlock()
		b.d.log.Info("bluetooth: proxy stopped")
		return
	}
	b.mu.Unlock()
}

// subscribedChanged: Home Assistant started or stopped wanting advertisements.
func (b *bluetoothCtl) subscribedChanged(want bool) {
	word := "unsubscribed"
	if want {
		word = "subscribed"
	}
	b.d.log.Info("bluetooth: Home Assistant " + word)
	b.mu.Lock()
	s := b.session
	b.mu.Unlock()
	if s != nil {
		s.desire(btDesire{scan: want, active: b.proxy.Active()})
	}
}

// modeChanged: HA asked for active or passive scanning. Passive is the
// default; active additionally sends scan requests (the radio transmits).
func (b *bluetoothCtl) modeChanged(active bool) {
	b.d.log.Info("bluetooth: scan mode requested", "active", active)
	b.mu.Lock()
	s := b.session
	b.mu.Unlock()
	if s != nil {
		s.desire(btDesire{scan: b.proxy.Subscribed(), active: active})
	}
}

type btStatus struct {
	State, Err, Addr string
	AdvTotal         uint64
	PerMin           float64
	Scanning         bool
}

func (b *bluetoothCtl) status() btStatus {
	b.mu.Lock()
	defer b.mu.Unlock()
	st := btStatus{State: b.state, Err: b.err, Addr: b.addr, AdvTotal: b.advTotal}
	now := time.Now()
	n := 0
	for i := len(b.advTimes) - 1; i >= 0; i-- { // newest last
		if now.Sub(b.advTimes[i]) <= time.Minute {
			n++
		} else {
			break
		}
	}
	st.PerMin = float64(n)
	st.Scanning = b.state == "scanning"
	return st
}

func (b *bluetoothCtl) countAds(n int) {
	b.mu.Lock()
	b.advTotal += uint64(n)
	now := time.Now()
	for i := 0; i < n; i++ {
		b.advTimes = append(b.advTimes, now)
	}
	if len(b.advTimes) > 4096 {
		b.advTimes = b.advTimes[len(b.advTimes)-4096:]
	}
	b.mu.Unlock()
}

func (b *bluetoothCtl) setState(state, err string) {
	b.mu.Lock()
	b.state, b.err = state, err
	b.mu.Unlock()
	b.proxy.Report(btScannerState(state))
}

// btScannerState maps our state onto the ESPHome scanner state HA shows.
func btScannerState(state string) esphome.ScannerState {
	switch state {
	case "starting":
		return esphome.ScannerStarting
	case "scanning":
		return esphome.ScannerRunning
	case "failed":
		return esphome.ScannerFailed
	}
	return esphome.ScannerStopped
}

// stop tears the session down (setting off, clockd exiting).
func (s *btSession) stop() {
	s.cancel()
	<-s.done
}

// desire sends the wanted scanner state to the session loop (latest wins).
func (s *btSession) desire(d btDesire) {
	s.ctrlMu.Lock()
	for {
		select {
		case <-s.ctrl:
			continue
		default:
		}
		break
	}
	select {
	case s.ctrl <- d:
	default:
	}
	s.ctrlMu.Unlock()
}

func (s *btSession) run() {
	defer close(s.done)
	defer func() {
		if r := recover(); r != nil {
			s.b.d.log.Error("bluetooth: session panic", "panic", r)
			s.b.setState("failed", fmt.Sprintf("internal error: %v", r))
		}
	}()
	for try := 0; try < btMaxRetries; try++ {
		err := s.bringUp()
		if err == nil || s.ctx.Err() != nil {
			return
		}
		s.b.setState("failed", err.Error())
		s.b.d.log.Warn("bluetooth: bring-up failed", "err", err, "try", try+1)
		select {
		case <-s.ctx.Done():
			return
		case <-time.After(btRetryDelay):
		}
	}
}

// bringUp runs one full bring-up and then the scanning loop. Returns nil when
// the session was stopped cleanly.
func (s *btSession) bringUp() error {
	d := s.b.d
	s.b.setState("starting", "")

	if err := btPowerCycle(); err != nil {
		return err
	}
	tty, err := btOpenUART()
	if err != nil {
		return err
	}
	// late-bound: tty is closed and reopened below before the line discipline
	defer func() { tty.Close() }()
	if err := btLoadFirmware(tty); err != nil {
		return fmt.Errorf("firmware: %w", err)
	}
	addr, err := btReadBDAddr(tty)
	if err != nil {
		return err
	}
	s.b.mu.Lock()
	old := s.b.addr
	s.b.addr = addr
	s.b.mu.Unlock()
	if old != addr {
		d.log.Info("bluetooth: radio up", "bd_addr", addr)
	}

	// Hand the port to the kernel: close our raw-UART fd and attach the line
	// discipline from a FRESH open, the way the verified tools/btinit flow does
	// (load in one process, attach from another). Attaching on the fd the load
	// ran on left the kernel's init commands unanswered (tx timeouts, then
	// "hci0 up: no such device"); a fresh open clears whatever tty state the
	// raw phase leaves behind. The discipline lives as long as this new fd
	// stays open; hci0 follows it.
	// Closing the last fd makes the tty HANG UP (async). Reopen only after
	// that has run its course, and prove the port is alive again with a Reset,
	// exactly like the proven btinit flow (load in one process, attach in
	// another, seconds apart): a reopen that races the hangup registers hci0
	// and the deferred hangup then unregisters it ("hci0 up: no such device").
	tty.Close()
	tty = nil
	var alive bool
	for try := 0; try < 6 && !alive; try++ {
		time.Sleep(500 * time.Millisecond)
		tty, err = btOpenUART()
		if err != nil {
			return err
		}
		btDrain(tty)
		if _, err = btH4Command(tty, btOpReset, nil); err == nil {
			alive = true
			break
		}
		d.log.Info("bluetooth: port not answering yet after the reopen", "try", try+1)
	}
	if !alive {
		return errors.New("ttyS1 never answered again after the reopen")
	}
	if err := unix.IoctlSetPointerInt(int(tty.Fd()), unix.TIOCSETD, btN_HCI); err != nil {
		return fmt.Errorf("TIOCSETD N_HCI: %w", err)
	}
	had := btHCIIDs() // the attach registers the device; find the new index below
	if err := unix.IoctlSetInt(int(tty.Fd()), btHCIUARTSETPROTO, btHCI_UART_H4); err != nil {
		return fmt.Errorf("HCIUARTSETPROTO: %w", err)
	}

	id, err := btWaitHCI(had, 3*time.Second)
	if err != nil {
		return err
	}

	sock, err := btHCISocketRetry(s.b.d.log, id)
	if err != nil {
		return err
	}
	hf := os.NewFile(uintptr(sock), "hci0") // hf owns the fd from here
	defer hf.Close()

	s.b.setState("waiting", "")
	d.log.Info("bluetooth: proxy ready", "subscribed", s.b.proxy.Subscribed())

	return s.loop(hf)
}

// loop is the single owner of the HCI socket: it applies the wanted scanner
// state, reads events, and forwards advertising reports in batches.
func (s *btSession) loop(hf *os.File) error {
	var batch []*api.BluetoothLERawAdvertisement
	buf := make([]byte, 1024)
	var stream []byte
	have := btDesire{} // what the radio is doing now
	flush := time.NewTicker(btFlushEvery)
	defer flush.Stop()
	for {
		// newest wish wins; apply its diff
		var want *btDesire
		for {
			select {
			case w := <-s.ctrl:
				want = &w
				continue
			default:
			}
			break
		}
		if want == nil && s.b.proxy.Subscribed() {
			w := btDesire{scan: true, active: s.b.proxy.Active()}
			want = &w
		}
		if want != nil {
			switch {
			case want.scan && !have.scan, want.scan && want.active != have.active:
				typ := byte(0) // passive: listen only
				if want.active {
					typ = 1
				}
				if err := btScanParams(hf, typ); err != nil {
					s.b.setState("failed", err.Error())
					return err
				}
				if err := btScanEnable(hf, true); err != nil {
					s.b.setState("failed", err.Error())
					return err
				}
				have = *want
				s.b.setState("scanning", "")
				d := "passive"
				if have.active {
					d = "active"
				}
				s.b.d.log.Info("bluetooth: scan started", "mode", d)
			case !want.scan && have.scan:
				if err := btScanEnable(hf, false); err != nil {
					s.b.d.log.Warn("bluetooth: scan stop failed", "err", err)
				}
				have = *want
				s.b.setState("waiting", "")
				s.b.d.log.Info("bluetooth: scan stopped (Home Assistant unsubscribed)")
			}
		}

		select {
		case <-s.ctx.Done():
			if have.scan {
				btScanEnable(hf, false) // best effort
			}
			return nil
		default:
		}

		hf.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		n, err := hf.Read(buf)
		if n > 0 {
			stream = append(stream, buf[:n]...)
			for {
				ev, rest, more := btNextEvent(stream)
				if !more {
					break
				}
				stream = rest
				if ads := btAdvReports(ev); len(ads) > 0 {
					batch = append(batch, ads...)
					s.b.countAds(len(ads))
				}
			}
		}
		select { // flush on the ticker or a full batch
		case <-flush.C:
			if len(batch) > 0 {
				s.sendBatch(batch)
				batch = batch[:0]
			}
		default:
			if len(batch) >= btMaxBatch {
				s.sendBatch(batch)
				batch = batch[:0]
			}
		}
		if err != nil {
			if os.IsTimeout(err) {
				continue
			}
			if s.ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("hci socket: %w", err)
		}
	}
}

func (s *btSession) sendBatch(batch []*api.BluetoothLERawAdvertisement) {
	if err := s.b.proxy.Advertise(batch); err != nil && !errors.Is(err, esphome.ErrNoSubscriber) {
		s.b.d.log.Warn("bluetooth: forwarding failed", "err", err, "ads", len(batch))
	}
}

// ---- radio bring-up over the raw UART (verified in tools/btinit) ----

// btPowerCycle resets the module through its rfkill switch so the chip is in
// its ROM state (deterministic, no matter what ran before).
func btPowerCycle() error {
	ents, _ := os.ReadDir("/sys/class/rfkill")
	var found string
	for _, e := range ents {
		p := "/sys/class/rfkill/" + e.Name()
		if b, err := os.ReadFile(p + "/type"); err == nil && strings.TrimSpace(string(b)) == "bluetooth" {
			found = p
			break
		}
	}
	if found == "" {
		return errors.New("no bluetooth rfkill switch")
	}
	if err := os.WriteFile(found+"/state", []byte("0"), 0); err != nil {
		return err
	}
	time.Sleep(250 * time.Millisecond)
	if err := os.WriteFile(found+"/state", []byte("1"), 0); err != nil {
		return err
	}
	time.Sleep(300 * time.Millisecond)
	return nil
}

// btOpenUART opens ttyS1 at 115200 8N1 with RTS/CTS flow control (the chip
// only transmits while the host's RTS is asserted) and VMIN 0 / VTIME 1 reads
// (a read waits up to 100 ms and then returns what arrived, possibly nothing).
func btOpenUART() (*os.File, error) {
	f, err := os.OpenFile(btUART, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	t, err := unix.IoctlGetTermios(int(f.Fd()), unix.TCGETS)
	if err != nil {
		f.Close()
		return nil, err
	}
	t.Iflag = unix.IGNPAR
	t.Oflag = 0
	t.Lflag = 0
	t.Cflag = unix.CS8 | unix.CLOCAL | unix.CREAD | unix.CRTSCTS | unix.B115200
	t.Cc[unix.VMIN], t.Cc[unix.VTIME] = 0, 1
	if err := unix.IoctlSetTermios(int(f.Fd()), unix.TCSETS, t); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// btUARTRead does ONE bounded read and returns whatever arrived (nil when
// nothing did): with the port in blocking mode the termios VTIME (100 ms)
// caps the wait, and the answer comes back the moment it arrives. It goes
// around os.File.Read on purpose: Go turns a 0-byte read into io.EOF, and
// treating that as a closed port is what made the chip look dead for a week
// (Engineering Notes 2026-11-12). An earlier version looped here until its
// own deadline; that turned every firmware record into a full 2 s wait and a
// bring-up into ~10 minutes.
func btUARTRead(f *os.File, d time.Duration) []byte {
	var buf [512]byte
	n, _ := unix.Read(int(f.Fd()), buf[:])
	if n > 0 {
		return buf[:n:n]
	}
	// nothing: the VTIME window expired, or the fd runs non-blocking under
	// Go's poller and read reports EAGAIN at once - pause a beat either way so
	// polling callers do not spin hot
	if d > 2*time.Millisecond {
		d = 2 * time.Millisecond
	}
	time.Sleep(d)
	return nil
}

// btDrain throws away everything pending for the next 200 ms (a chip reset
// answers for a while).
func btDrain(f *os.File) {
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		btUARTRead(f, 100*time.Millisecond)
	}
}

// btHCDRecords splits a Broadcom .hcd into its HCI command records
// [opcode LE 2][plen 1][payload], without the H4 type byte.
func btHCDRecords(data []byte) [][]byte {
	var pkts [][]byte
	for i := 0; i+4 <= len(data); {
		pl := int(data[i+2])
		if i+3+pl > len(data) {
			break
		}
		pkts = append(pkts, data[i:i+3+pl])
		i += 3 + pl
	}
	return pkts
}

func btFindHCD() (string, error) {
	const p = "/system/etc/bluetooth"
	ents, err := os.ReadDir(p)
	if err != nil {
		return "", err
	}
	var names []string
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".hcd") {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		return "", errors.New("no .hcd firmware in " + p)
	}
	sort.Strings(names) // one file on both builds; stable if more appear
	return p + "/" + names[0], nil
}

// btLoadFirmware downloads the vendor patchram and resets the chip into it.
func btLoadFirmware(f *os.File) error {
	path, err := btFindHCD()
	if err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	pkts := btHCDRecords(data)
	if len(pkts) == 0 {
		return errors.New("empty firmware file")
	}
	btDrain(f)
	if _, err := btH4Command(f, btOpReset, nil); err != nil {
		return fmt.Errorf("reset: %w", err)
	}
	if _, err := btH4Command(f, btOpDownloadMini, nil); err != nil {
		return fmt.Errorf("download minidriver: %w", err)
	}

	time.Sleep(50 * time.Millisecond)
	for i, p := range pkts {
		if err := btH4CommandRaw(f, p); err != nil {
			return fmt.Errorf("record %d/%d (opcode %#06x): %w", i+1, len(pkts), binary.LittleEndian.Uint16(p), err)
		}
	}
	time.Sleep(400 * time.Millisecond) // the chip restarts into the firmware
	btDrain(f)
	_, err = btH4Command(f, btOpReset, nil)
	return err
}

// btReadBDAddr asks the chip for its address (the WLAN MAC + 1).
func btReadBDAddr(f *os.File) (string, error) {
	ev, err := btH4Command(f, btOpReadBDADDR, nil)
	if err != nil {
		return "", err
	}
	if len(ev) < 12 {
		return "", errors.New("short BD_ADDR answer")
	}
	a := ev[6:12] // little-endian on the wire; print the usual way
	return fmt.Sprintf("%02x:%02x:%02x:%02x:%02x:%02x", a[5], a[4], a[3], a[2], a[1], a[0]), nil
}

// btH4CommandRaw sends one .hcd record and waits for its completion
// (a Command Complete for its opcode, or a vendor event).
func btH4CommandRaw(f *os.File, rec []byte) error {
	op := binary.LittleEndian.Uint16(rec)
	if _, err := f.Write(append([]byte{0x01}, rec...)); err != nil {
		return err
	}
	if btWaitCC(f, op, 2*time.Second) == nil {
		return fmt.Errorf("no answer to record %#06x", op)
	}
	return nil
}

// btH4Command sends one HCI command over H4 and waits for its Command
// Complete, returning [event][plen][num][opcode LE2][status][...] or an error
// when nothing answered.
func btH4Command(f *os.File, op uint16, params []byte) ([]byte, error) {
	pkt := make([]byte, 4+len(params))
	pkt[0] = 0x01
	binary.LittleEndian.PutUint16(pkt[1:], op)
	pkt[3] = byte(len(params))
	copy(pkt[4:], params)
	if _, err := f.Write(pkt); err != nil {
		return nil, err
	}
	ev := btWaitCC(f, op, 2*time.Second)
	if ev == nil {
		return nil, fmt.Errorf("no answer to command %#06x", op)
	}
	if ev[0] == btEvtCommandCmpl && len(ev) >= 6 && ev[5] != 0 {
		return nil, fmt.Errorf("command %#06x: status %#x", op, ev[5])
	}
	return ev, nil
}

// btWaitCC reads H4 events until the Command Complete for op (vendor events
// count as the answer: some patchram records ack that way). nil on timeout.
func btWaitCC(f *os.File, op uint16, d time.Duration) []byte {
	deadline := time.Now().Add(d)
	var pending []byte
	for time.Now().Before(deadline) {
		pending = append(pending, btUARTRead(f, time.Until(deadline))...)
		for len(pending) >= 3 {
			// [type][event][plen][event params...]
			if pending[0] != 0x04 {
				pending = pending[1:] // resync on stray bytes
				continue
			}
			need := 3 + int(pending[2])
			if len(pending) < need {
				break
			}
			ev := pending[:need]
			pending = pending[need:]
			if btCCFor(ev, op) != nil {
				out := make([]byte, len(ev)-1)
				copy(out, ev[1:]) // [event][plen][num][opcode][status][...]
				return out
			}
			if ev[1] == btEvtVendor {
				out := make([]byte, len(ev)-1)
				copy(out, ev[1:])
				return out
			}

		}
	}
	return nil
}

// btCCFor reports the Command Complete event for op from a raw H4 event
// [type][code][plen][params], or nil. Some patchram records answer with a
// vendor event instead; those count for any opcode (btWaitCC handles them).
func btCCFor(ev []byte, op uint16) []byte {
	if len(ev) < 6 || ev[0] != 0x04 || ev[1] != btEvtCommandCmpl {
		return nil
	}
	if binary.LittleEndian.Uint16(ev[4:6]) != op {
		return nil
	}
	return ev
}

// ---- kernel HCI socket ----

// btHCIIDs lists the registered HCI device indices from sysfs.
func btHCIIDs() map[int]bool {
	ids := map[int]bool{}
	ents, _ := os.ReadDir("/sys/class/bluetooth")
	for _, e := range ents {
		var n int
		if strings.HasPrefix(e.Name(), "hci") && strings.Count(e.Name(), "hci") == 1 {
			if _, err := fmt.Sscanf(e.Name(), "hci%d", &n); err == nil {
				ids[n] = true
			}
		}
	}
	return ids
}

// btWaitHCI waits for a NEW hci device to appear after the line discipline
// attach and returns its index (hciconfig resolves the name the same way; the
// index is not guaranteed to be 0 - a leftover or racing registration can take it).
func btWaitHCI(before map[int]bool, d time.Duration) (int, error) {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		for id := range btHCIIDs() {
			if !before[id] {
				return id, nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return 0, errors.New("no new hci device after attaching the line discipline")
}

// btHCISocketRetry brings the device up, tolerating the registration race:
// the sysfs entry can show up before the kernel's device list does. A retry
// re-discovers the current indices instead of trusting the first one.
func btHCISocketRetry(log interface{ Info(string, ...any) }, id int) (int, error) {
	var err error
	for try := 0; try < 6; try++ {
		time.Sleep(500 * time.Millisecond)
		sock, e := btHCISocket(id)
		if e == nil {
			return sock, nil
		}
		err = e
		ids := []int{}
		for i := range btHCIIDs() {
			ids = append(ids, i)
		}
		log.Info("bluetooth: hci up failed, will retry", "try", try+1, "want", id, "have", ids, "err", e.Error())
		// if a different index appeared, try it next
		for _, i := range ids {
			if i != id {
				id = i
				break
			}
		}
	}
	return 0, err
}

// btHCISocket brings the device up and returns an event socket bound to it.
// hciconfig's order: socket, ioctl UP on the UNBOUND socket, then bind (a
// bind before the up ioctl returned ENODEV on the clock).
func btHCISocket(id int) (int, error) {
	sock, err := unix.Socket(unix.AF_BLUETOOTH, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.BTPROTO_HCI)
	if err != nil {
		return 0, err
	}
	// Bring the device up. The kernel's HCIDEVUP handler takes the device id
	// BY VALUE in the ioctl argument (hci_dev_get(arg)), like hciconfig - a
	// pointer here makes it look up a bogus id and answer ENODEV forever.
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(sock), btHCIDEVUP, uintptr(id)); errno != 0 && errno != unix.EALREADY {
		unix.Close(sock)
		return 0, fmt.Errorf("hci%d up: %w", id, errno)
	}
	if err := unix.Bind(sock, &unix.SockaddrHCI{Dev: uint16(id), Channel: unix.HCI_CHANNEL_RAW}); err != nil {
		unix.Close(sock)
		return 0, fmt.Errorf("bind hci%d: %w", id, err)
	}
	// Event filter: accept everything (type = every packet kind, all events).
	// A hand-computed bitmap (which bit means which event code differs between
	// headers) silently dropped the LE reports; the loop skips what it does
	// not need anyway.
	var filt [14]byte
	binary.LittleEndian.PutUint32(filt[0:4], 0xffffffff)
	binary.LittleEndian.PutUint32(filt[4:8], 0xffffffff)
	binary.LittleEndian.PutUint32(filt[8:12], 0xffffffff)
	if _, _, errno := unix.Syscall6(unix.SYS_SETSOCKOPT, uintptr(sock), btSOLHCI, btHCI_FILTER, uintptr(unsafe.Pointer(&filt[0])), 14, 0); errno != 0 {
		unix.Close(sock)
		return 0, fmt.Errorf("hci filter: %w", errno)
	}
	return sock, nil
}

// btScanParams sets the LE scan parameters: interval 60 ms, window 30 ms
// (x 0.625 ms), public own address, no address filtering.
func btScanParams(f *os.File, typ byte) error {
	return btHCICommand(f, btOpLEScanParams, []byte{typ, 0x60, 0x00, 0x30, 0x00, 0x00, 0x00}, 2*time.Second)
}

func btScanEnable(f *os.File, on bool) error {
	enable := byte(0x00)
	if on {
		enable = 0x01
	}
	return btHCICommand(f, btOpLEScanEnable, []byte{enable, 0x00}, 2*time.Second)
}

// btHCICommand sends a command over the raw HCI socket and waits for its
// Command Complete, skipping the kernel's own traffic. The first byte of a
// raw-socket write is the H4 packet type (0x01 command); the kernel strips
// it and queues the rest. Only the session loop goroutine may call this.
func btHCICommand(f *os.File, op uint16, params []byte, wait time.Duration) error {
	pkt := make([]byte, 4+len(params))
	pkt[0] = 0x01 // H4: command packet
	binary.LittleEndian.PutUint16(pkt[1:], op)
	pkt[3] = byte(len(params))
	copy(pkt[4:], params)
	if _, err := f.Write(pkt); err != nil {
		return err
	}
	deadline := time.Now().Add(wait)
	buf := make([]byte, 1024)
	for time.Now().Before(deadline) {
		f.SetReadDeadline(time.Now().Add(time.Until(deadline)))
		n, err := f.Read(buf)
		if n > 0 {
			if ev, ok := btCommandComplete(buf[:n], op); ok {
				if len(ev) >= 4 && ev[3] != 0 {
					return fmt.Errorf("command %#06x failed: status %#x", op, ev[3])
				}
				return nil
			}
		}
		if err != nil && !os.IsTimeout(err) {
			return err
		}
	}
	return fmt.Errorf("no answer to command %#06x", op)
}

// btCommandComplete checks one socket read for a Command Complete of op.
// The socket delivers events WITH the 0x04 type byte; ev (without type,
// plen) is [num][opcode LE2][status][...], so status sits at ev[3].
func btCommandComplete(b []byte, op uint16) ([]byte, bool) {
	if len(b) < 7 || b[0] != 0x04 || b[1] != btEvtCommandCmpl {
		return nil, false
	}
	need := 3 + int(b[2])
	if len(b) < need {
		return nil, false
	}
	ev := b[3:need]
	if binary.LittleEndian.Uint16(ev[1:3]) != op {
		return nil, false
	}
	return ev, true
}

// ---- event stream parsing ----

// btNextEvent splits one HCI event [0x04][code][plen][params] off the front
// of the socket stream.
func btNextEvent(b []byte) (ev []byte, rest []byte, more bool) {
	for len(b) >= 3 {
		if b[0] != 0x04 {
			b = b[1:]
			continue
		}
		need := 3 + int(b[2])
		if len(b) < need {
			return nil, b, false
		}
		return b[:need], b[need:], true
	}
	return nil, b, false
}

// btAdvReports parses a LE Advertising Report meta event into raw
// advertisements for Home Assistant: the address as a little-endian uint64
// (HCI order), the address type (0 public / 1 random), everything the device
// broadcast, and the RSSI. Reports without an RSSI are dropped.
func btAdvReports(ev []byte) []*api.BluetoothLERawAdvertisement {
	// ev: [0x04][0x3e][plen][subevent 0x02][num][reports...]
	if len(ev) < 5 || ev[1] != btEvtLEMeta || ev[3] != btEvtLEAdvReport {
		return nil
	}
	num := int(ev[4])
	b := ev[5:]
	var ads []*api.BluetoothLERawAdvertisement
	for i := 0; i < num; i++ {
		// report header: type (1), address type (1), address (6), data length (1)
		if len(b) < 9 {
			break
		}
		dlen := int(b[8])
		if len(b) < 9+dlen+1 {
			break
		}
		rssi := int8(b[9+dlen])
		if rssi != btAdvRSSIUnknown {
			var a [8]byte
			copy(a[:6], b[2:8])
			ads = append(ads, &api.BluetoothLERawAdvertisement{
				Address:     binary.LittleEndian.Uint64(a[:]),
				Rssi:        int32(rssi),
				AddressType: uint32(b[1]),
				Data:        append([]byte(nil), b[9:9+dlen]...),
			})
		}
		b = b[9+dlen+1:]
	}
	return ads
}

// applyBluetooth starts or stops the proxy from the config (boot and every
// settings save; a no-op when unchanged).
func (d *daemon) applyBluetooth(c config.Config) {
	if d.bt != nil {
		d.bt.setConfig(c.Bluetooth.Proxy)
	}
}
