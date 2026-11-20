package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"log/slog"
)

// The USB-A socket: device mode (adb over a cable) by default, host mode by
// one PHY register write. u2p_r0 (0xffe09020) bit 13 is the ID pull-up: with
// no ID pin wired, clearing it makes ID read low = host, and the vendor PHY
// driver does the whole switch itself (Engineering Notes 2026-11-10 "USB
// host mode at run time"). A reboot always returns to device mode; nothing
// persists, so adb (and burn mode) can never be locked out by a setting.
// A locked clock (no adb: adbOn) is switched to host mode when clockd starts,
// since device mode has no use there; burn mode is the boot ROM's, before any
// of this runs.
//
// The kernel is the only truth here: what is mounted comes from
// /proc/mounts, whether the drive is still plugged in from /sys/class/block.
// clockd keeps no copy of either. (v0.91.0 remembered a device name; a
// mount whose drive had gone was then served as if alive — the "Directory
// bread failed" listings.)
//
// We do NOT use the vendor mountd (/sbin/mount_usb_drive.sh). Its first line
// is `rm -r /data/usb` and it mounts read-write: started while a drive is
// mounted there, it deletes everything on the drive (how the first test
// stick was emptied). It is stopped before anything here touches the mount
// and is never started. clockd mounts the drive itself, read-only with
// noexec,nosuid,nodev. Nothing on the clock ever executes from the drive.
//
// xHCI and the port: flipping the port back to device mode with a drive
// still attached makes xHCI see a glitch connect, enumerate the drive
// degraded (full-speed) and die ("HC died", dmesg). So the controller is
// unbound BEFORE the port leaves host mode (a clean disconnect) and bound
// again when host mode is entered.

const (
	usbRegAddr = 0xffe09020
	usbIDBit   = 0x2000 // u2p_r0 bit 13, idpullup
	xhciDriver = "/sys/bus/platform/drivers/xhci-hcd/"
	xhciDevice = "xhci-hcd.0.auto"

	usbMountFlags = syscall.MS_RDONLY | syscall.MS_NOEXEC | syscall.MS_NOSUID | syscall.MS_NODEV
)

var (
	usbMount    = "/data/usb"
	usbSysBlock = "/sys/block"       // disks: sda, sdb, ... (only USB storage is sd* here)
	usbSysClass = "/sys/class/block" // disks and partitions; an entry goes when the drive is pulled
	usbDevDir   = "/dev/block"
	usbSysBus   = "/sys/bus/usb/devices" // what is on the bus: 1-1, 1-1.2, ... (plus hubs and interfaces)
)

// the filesystems tried on a drive, in order (exFAT and NTFS are not in this kernel)
var usbFilesystems = []string{"vfat", "ext4", "ext3", "ext2"}

type usbSock struct {
	dir string // clockd's directory (busybox at dir/bin/busybox)
	log *slog.Logger

	op sync.Mutex // one switch or watcher pass at a time (held for seconds during a switch)

	mu        sync.Mutex    // the fields below; never held across a slow call
	stop      chan struct{} // stops the watcher (nil = not running)
	skip      string        // a drive nothing mounted: skipped until the drives change
	mountErr  string        // why that drive failed (shown in the panel)
	gen       int           // counts our mounts: the panel resets its folder when it changes
	switching bool          // a mode switch is running

	// plugged is told about a drive the watcher found after its first pass
	// (true: mounted, false: nothing could mount it); not about one that was
	// already in when the watcher started (boot, a mode switch)
	plugged func(mounted bool)
}

func newUSBSock(dir string, log *slog.Logger, plugged func(mounted bool)) *usbSock {
	u := &usbSock{dir: dir, log: log, plugged: plugged}
	// After a clockd restart in host mode the watcher takes over mounting.
	// It runs only while the port is host.
	if m, _, err := u.mode(); err == nil && m == "host" {
		u.startWatch()
	}
	return u
}

func (u *usbSock) busybox() string { return filepath.Join(u.dir, "bin", "busybox") }

func (u *usbSock) devmem(addr uint32) (uint32, error) {
	out, err := exec.Command(u.busybox(), "devmem", fmt.Sprintf("0x%08x", addr), "32").Output()
	if err != nil {
		return 0, fmt.Errorf("devmem: %w", err)
	}
	v, err := strconv.ParseUint(strings.TrimSpace(string(out)), 0, 64)
	if err != nil {
		return 0, fmt.Errorf("devmem read %q: %w", out, err)
	}
	return uint32(v), nil
}

func (u *usbSock) writeReg(addr, val uint32) error {
	if _, err := exec.Command(u.busybox(), "devmem",
		fmt.Sprintf("0x%08x", addr), "32", fmt.Sprintf("0x%08x", val)).Output(); err != nil {
		return fmt.Errorf("devmem write: %w", err)
	}
	return nil
}

// mode reports "adb" (device mode, the default) or "host", plus the raw
// register. On an error nothing may be written back: the register value is
// unknown.
func (u *usbSock) mode() (string, uint32, error) {
	reg, err := u.devmem(usbRegAddr)
	if err != nil {
		return "adb", 0, err
	}
	if reg&usbIDBit == 0 {
		return "host", reg, nil
	}
	return "adb", reg, nil
}

// usbMounted reports the live mount at usbMount: ok only when something is
// mounted AND its block device still exists (a pulled drive leaves a dead
// mount behind until the watcher clears it).
var usbMounted = func() (dev, fs, opts string, ok bool) {
	mdev, fs, opts, ok := mountInfo(usbMount)
	if !ok || !blockPresent(filepath.Base(mdev)) {
		return "", "", "", false
	}
	return filepath.Base(mdev), fs, opts, true
}

func blockPresent(name string) bool {
	_, err := os.Stat(filepath.Join(usbSysClass, name))
	return err == nil
}

// usbAttached names what is plugged into the socket (host mode; with the
// controller unbound or dead the bus is empty). Root hubs (usbN) and
// interfaces (1-1:1.0) are not devices.
func usbAttached() []string {
	devs, _ := filepath.Glob(filepath.Join(usbSysBus, "[0-9]*-[0-9]*"))
	sort.Strings(devs)
	out := []string{}
	for _, d := range devs {
		if strings.Contains(filepath.Base(d), ":") {
			continue
		}
		name := ""
		for _, f := range []string{"manufacturer", "product"} {
			if b, err := os.ReadFile(filepath.Join(d, f)); err == nil {
				name = strings.TrimSpace(name + " " + strings.TrimSpace(string(b)))
			}
		}
		if name == "" {
			name = "USB device"
		}
		out = append(out, name)
	}
	return out
}

func hasOpt(opts, want string) bool {
	for _, o := range strings.Split(opts, ",") {
		if o == want {
			return true
		}
	}
	return false
}

// Status is the System > USB panel's data. It only reads.
// adbOn: this boot runs adb (an unlocked clock: the bootloader passes
// androidboot.debuggable=1). Relocked, there is no adb over USB or Wi-Fi and
// device mode leaves the socket unused.
func adbOn() bool {
	b, err := os.ReadFile("/proc/cmdline")
	return err == nil && cmdlineHas(string(b), "androidboot.debuggable=1")
}

// logMode names device mode for the log: "adb", or "off" on a locked clock.
func logMode(mode string) string {
	if mode == "adb" && !adbOn() {
		return "off"
	}
	return mode
}

func cmdlineHas(cmdline, want string) bool {
	for _, f := range strings.Fields(cmdline) {
		if f == want {
			return true
		}
	}
	return false
}

func (u *usbSock) Status() map[string]any {
	mode, _, err := u.mode()
	u.mu.Lock()
	mErr, gen, switching := u.mountErr, u.gen, u.switching
	u.mu.Unlock()
	st := map[string]any{"mode": mode, "device": "", "mounted": false, "switching": switching, "adb": adbOn()}
	if err != nil {
		u.log.Warn("usb: register read failed", "err", err)
		st["error"] = "cannot read the socket's state: " + err.Error()
		return st
	}
	if mode == "host" {
		// the panel keeps "adb" disabled while this is not empty
		st["attached"] = usbAttached()
	}
	if dev, fs, opts, ok := usbMounted(); ok && mode == "host" {
		st["device"], st["fs"], st["options"], st["mounted"] = dev, fs, opts, true
		st["drive"] = dev + "#" + strconv.Itoa(gen)
		var sf syscall.Statfs_t
		if err := syscall.Statfs(usbMount, &sf); err == nil {
			st["size_bytes"] = sf.Blocks * uint64(sf.Bsize)
			st["free_bytes"] = sf.Bavail * uint64(sf.Bsize)
		}
	} else if mErr != "" && mode == "host" {
		st["error"] = mErr
	}
	return st
}

// SetHost switches the socket: host=true powers a USB drive, host=false
// returns to adb over a cable (also what every boot starts as).
func (u *usbSock) SetHost(host bool) error {
	u.op.Lock()
	defer u.op.Unlock()
	u.mu.Lock()
	u.switching = true
	u.mu.Unlock()
	defer func() {
		u.mu.Lock()
		u.switching = false
		u.mu.Unlock()
	}()

	mode, reg, err := u.mode()
	if err != nil {
		return err
	}
	if host {
		u.stopMountd()
		if mode != "host" {
			u.mu.Lock()
			u.skip, u.mountErr = "", ""
			u.mu.Unlock()
			if err := u.writeReg(usbRegAddr, reg&^usbIDBit); err != nil {
				return err
			}
			if err := bindXHCI(); err != nil {
				u.log.Warn("usb: xHCI bind", "err", err)
			}
			// a drive enumerates ~1.5 s after the bind; one that arrives
			// later is picked up by the watcher
			for i := 0; i < 16 && len(stickParts()) == 0; i++ {
				time.Sleep(250 * time.Millisecond)
			}
		}
		u.startWatch()
		u.syncMount()
		dev, _, _, _ := usbMounted()
		u.log.Info("usb: host mode", "was", logMode(mode), "device", dev)
		return nil
	}
	// back to adb: not with something still plugged in. No cable fits while
	// it is there, and the socket would stop being a host under it.
	if mode == "host" {
		if att := usbAttached(); len(att) > 0 {
			return fmt.Errorf("%s is still plugged in: pull it out first", strings.Join(att, ", "))
		}
	}
	// stop the watcher (it would mount again), unmount, take the controller
	// away cleanly, and only then flip the port
	u.stopWatch()
	if err := u.unmount(); err != nil {
		if mode == "host" {
			u.startWatch()
		}
		return err
	}
	if mode == "host" {
		unbindXHCI()
		if err := u.writeReg(usbRegAddr, reg|usbIDBit); err != nil {
			return err
		}
	}
	if adbOn() {
		u.log.Info("usb: adb mode", "was", logMode(mode))
	} else {
		u.log.Info("usb: socket off (locked clock, no adb)", "was", logMode(mode))
	}
	return nil
}

// unbindXHCI detaches the host controller driver: every USB device goes
// through a clean disconnect. Not bound (already unbound, or a build
// without it) is fine.
func unbindXHCI() {
	os.WriteFile(xhciDriver+"unbind", []byte(xhciDevice), 0)
	time.Sleep(300 * time.Millisecond)
}

// bindXHCI (re)starts the host controller driver. Unbinding first also
// revives a controller that died (a boot with a drive in the socket ends
// that way: the port is in device mode then).
func bindXHCI() error {
	unbindXHCI()
	err := os.WriteFile(xhciDriver+"bind", []byte(xhciDevice), 0)
	time.Sleep(300 * time.Millisecond)
	return err
}

// stickParts lists what could hold a filesystem, in mount order: each
// disk's partitions by number, or the disk itself when it has no partition
// table. Only names whose device node exists yet are returned.
func stickParts() []string {
	disks, _ := filepath.Glob(filepath.Join(usbSysBlock, "sd*"))
	sort.Strings(disks)
	var out []string
	for _, d := range disks {
		disk := filepath.Base(d)
		// size 0: a card reader with no card (or one the kernel is still
		// reading); nothing to mount yet, and not an unreadable drive
		if b, err := os.ReadFile(filepath.Join(d, "size")); err == nil && strings.TrimSpace(string(b)) == "0" {
			continue
		}
		parts, _ := filepath.Glob(filepath.Join(d, disk+"[0-9]*"))
		names := []string{}
		for _, p := range parts {
			names = append(names, filepath.Base(p))
		}
		sort.Slice(names, func(i, j int) bool {
			a, _ := strconv.Atoi(names[i][len(disk):])
			b, _ := strconv.Atoi(names[j][len(disk):])
			return a < b
		})
		if len(names) == 0 {
			names = []string{disk}
		}
		for _, n := range names {
			if _, err := os.Stat(filepath.Join(usbDevDir, n)); err == nil {
				out = append(out, n)
			}
		}
	}
	return out
}

// mountData are the filesystem's own mount options (the flags are
// usbMountFlags for all). vfat `utf8` gives proper UTF-8 file names; the
// kernel has no NLS tables beyond the default, so iocharset= and codepage=
// are EINVAL and must not be passed.
func mountData(fs string) string {
	if fs == "vfat" {
		return "shortname=mixed,utf8"
	}
	return ""
}

// syncMount brings the mount in line with what is plugged in (caller holds
// op): clears a mount whose drive is gone, takes over a writable one, and
// mounts a present drive when nothing is mounted. A drive no filesystem
// fits is skipped until the drives change or the user switches modes again.
func (u *usbSock) syncMount() {
	if mdev, _, opts, ok := mountInfo(usbMount); ok {
		if blockPresent(filepath.Base(mdev)) && hasOpt(opts, "ro") {
			return // ours and alive
		}
		u.stopMountd()
		if err := u.unmount(); err != nil {
			u.log.Warn("usb: cannot take over the mount", "err", err)
			return
		}
	}
	parts := stickParts()
	key := strings.Join(parts, ",")
	u.mu.Lock()
	if key == "" {
		u.skip, u.mountErr = "", ""
	}
	skipped := key == u.skip
	u.mu.Unlock()
	if key == "" || skipped {
		return
	}
	os.MkdirAll(usbMount, 0o755)
	for _, part := range parts {
		for _, fs := range usbFilesystems {
			err := syscall.Mount(filepath.Join(usbDevDir, part), usbMount, fs, usbMountFlags, mountData(fs))
			if err == nil {
				u.mu.Lock()
				u.skip, u.mountErr = "", ""
				u.gen++
				u.mu.Unlock()
				u.log.Info("usb: drive mounted", "device", part, "fs", fs)
				return
			}
		}
	}
	u.mu.Lock()
	u.skip = key
	u.mountErr = "no FAT or ext2-4 filesystem on it (exFAT and NTFS are not supported)"
	u.mu.Unlock()
	u.log.Warn("usb: drive not mountable", "devices", key)
}

// unmount clears usbMount (caller holds op). A read-only mount that is in
// use, or one whose drive is gone, is detached (nothing to flush; whoever
// has files open sees them fail). A writable mount in use is refused:
// detaching it could lose data that was still being written.
func (u *usbSock) unmount() error {
	mdev, _, opts, ok := mountInfo(usbMount)
	if !ok {
		return nil
	}
	gone := !blockPresent(filepath.Base(mdev))
	ro := hasOpt(opts, "ro")
	if !ro && !gone {
		syscall.Sync()
	}
	err := syscall.Unmount(usbMount, 0)
	if err == nil {
		u.log.Info("usb: drive unmounted", "device", filepath.Base(mdev), "gone", gone)
		return nil
	}
	if !ro && !gone {
		return fmt.Errorf("the drive is mounted writable by something else and in use: %w", err)
	}
	if err2 := syscall.Unmount(usbMount, syscall.MNT_DETACH); err2 != nil {
		return fmt.Errorf("unmount: %w", errors.Join(err, err2))
	}
	u.log.Info("usb: drive detached", "device", filepath.Base(mdev), "gone", gone, "why", err)
	return nil
}

// stopMountd stops the vendor mountd (see the note at the top). start.sh
// stops it at boot already; this covers a clock with the use-mountd flag.
func (u *usbSock) stopMountd() {
	if err := exec.Command("stop", "mountd").Run(); err != nil {
		u.log.Info("usb: stop mountd", "err", err)
	}
}

// The watcher: while in host mode, mount a drive that arrives and clear the
// mount of a pulled one (3 s polling is fine; no interrupts to hook). It
// runs only in host mode, so an adb-mode clock pays nothing.
func (u *usbSock) startWatch() {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.stop != nil {
		return
	}
	stop := make(chan struct{})
	u.stop = stop
	go func() {
		u.stopMountd()
		t := time.NewTicker(3 * time.Second)
		defer t.Stop()
		first := true
		for {
			u.op.Lock()
			select {
			case <-stop: // stopped while we waited for a switch to finish
				u.op.Unlock()
				return
			default:
			}
			u.mu.Lock()
			gen, skip := u.gen, u.skip
			u.mu.Unlock()
			u.syncMount()
			u.mu.Lock()
			mounted, failed := u.gen != gen, u.skip != "" && u.skip != skip
			u.mu.Unlock()
			u.op.Unlock()
			if !first && (mounted || failed) && u.plugged != nil {
				u.plugged(mounted)
			}
			first = false
			select {
			case <-stop:
				return
			case <-t.C:
			}
		}
	}()
}

func (u *usbSock) stopWatch() {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.stop != nil {
		close(u.stop)
		u.stop = nil
	}
}

type ent struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	Dir   bool   `json:"dir"`
	Link  bool   `json:"link"`
	Size  int64  `json:"size"`
	MTime int64  `json:"mtime"`
}

// Files lists one directory of the mounted drive, confined to the mount:
// the path is cleaned and rooted, symlinked directories are refused (they
// could point out of the mount), symlink entries are listed but never
// followed. Read-only: this is a browser, not a file manager.
func (u *usbSock) Files(dir string) (map[string]any, error) {
	if _, _, _, ok := usbMounted(); !ok {
		// without this the clock's own (empty) mount point would be listed
		return nil, fmt.Errorf("no drive mounted")
	}
	p := filepath.Clean("/" + strings.TrimPrefix(dir, usbMount))
	full := filepath.Join(usbMount, p)
	if !strings.HasPrefix(full, usbMount+string(filepath.Separator)) && full != usbMount {
		return nil, fmt.Errorf("path outside the drive")
	}
	fi, err := os.Lstat(full)
	if err != nil {
		return nil, fmt.Errorf("no such directory")
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("symbolic links are not followed")
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("not a directory")
	}
	entries, err := os.ReadDir(full)
	if err != nil {
		return nil, err
	}
	list := []ent{}
	for _, e := range entries {
		info, err := e.Info() // lstat: does not follow
		if err != nil {
			continue
		}
		link := info.Mode()&os.ModeSymlink != 0
		list = append(list, ent{Name: e.Name(),
			Path: filepath.ToSlash(filepath.Join(p, e.Name())),
			Dir:  !link && info.IsDir(), Link: link,
			Size: info.Size(), MTime: info.ModTime().Unix()})
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].Dir != list[j].Dir {
			return list[i].Dir
		}
		return strings.ToLower(list[i].Name) < strings.ToLower(list[j].Name)
	})
	if p != "/" {
		list = append([]ent{{Name: "..", Path: filepath.ToSlash(filepath.Dir(p)), Dir: true}}, list...)
	}
	return map[string]any{"path": p, "entries": list}, nil
}

// OpenAudio opens one audio file on the mounted drive for playback, with the
// same confinement as Files: rooted under the mount, no symlink following,
// .flac/.wav/.mp3 only. The caller closes the file.
func (u *usbSock) OpenAudio(dir string) (*os.File, string, error) {
	if _, _, _, ok := usbMounted(); !ok {
		return nil, "", fmt.Errorf("no drive mounted")
	}
	p := filepath.Clean("/" + strings.TrimPrefix(dir, usbMount))
	if !audioExt(p) {
		return nil, "", fmt.Errorf("not a .flac, .mp3 or .wav file")
	}
	full := filepath.Join(usbMount, p)
	if !strings.HasPrefix(full, usbMount+string(filepath.Separator)) {
		return nil, "", fmt.Errorf("path outside the drive")
	}
	fi, err := os.Lstat(full)
	if err != nil || fi.Mode()&os.ModeSymlink != 0 || !fi.Mode().IsRegular() {
		return nil, "", fmt.Errorf("no such file")
	}
	f, err := os.Open(full)
	if err != nil {
		return nil, "", err
	}
	return f, p, nil
}

// mountInfo reports the device, filesystem and options when target is mounted.
func mountInfo(target string) (dev, fs, opts string, ok bool) {
	b, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return "", "", "", false
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) >= 4 && f[1] == target {
			return f[0], f[2], f[3], true
		}
	}
	return "", "", "", false
}
