package main

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// Files must stay inside the mount whatever the path says, never follow a
// symlinked directory, and list links without traversing them.
func TestUSBFilesConfined(t *testing.T) {
	dir := t.TempDir()
	old := usbMount
	usbMount = dir
	oldM := usbMounted
	usbMounted = func() (string, string, string, bool) { return "sda1", "vfat", "ro", true }
	defer func() { usbMount, usbMounted = old, oldM }()

	os.MkdirAll(filepath.Join(dir, "music", "album"), 0o755)
	os.WriteFile(filepath.Join(dir, "music", "album", "track.flac"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(dir, "zz.txt"), []byte("x"), 0o644)
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "secret"), []byte("s"), 0o644)
	os.Symlink(filepath.Join(outside, "secret"), filepath.Join(dir, "out-link"))
	os.Symlink("music", filepath.Join(dir, "in-link"))

	u := &usbSock{}

	// root: dirs first, then files and links by name
	r, err := u.Files("/")
	if err != nil {
		t.Fatal(err)
	}
	ents := r["entries"].([]ent)
	var got []string
	for _, e := range ents {
		got = append(got, e.Name)
	}
	if strings.Join(got, ",") != "music,in-link,out-link,zz.txt" {
		t.Errorf("root listing: got %v", got)
	}
	for _, e := range ents {
		if e.Name == "music" && (!e.Dir || e.Link) {
			t.Errorf("music should be a plain dir: %+v", e)
		}
		if (e.Name == "out-link" || e.Name == "in-link") && (!e.Link || e.Dir) {
			t.Errorf("link %s should be listed as a non-dir link: %+v", e.Name, e)
		}
	}

	// nested listing has a ".." row first
	r, err = u.Files("/music/album")
	if err != nil {
		t.Fatal(err)
	}
	ents = r["entries"].([]ent)
	if len(ents) != 2 || ents[0].Name != ".." || ents[1].Name != "track.flac" || ents[1].Dir {
		t.Errorf("album listing: %+v", ents)
	}

	// traversal shapes: either refused or confined, never an escape
	for _, p := range []string{"../../etc", "/../../etc/passwd", "music/../../..", "/etc/passwd"} {
		r, err := u.Files(p)
		if err != nil {
			continue
		}
		if !strings.HasPrefix(r["path"].(string), "/") ||
			filepath.Join(dir, r["path"].(string)) != filepath.Join(dir, filepath.Clean("/"+p)) {
			t.Errorf("path %q resolved outside the mount: %v", p, r["path"])
		}
	}

	// symlinked directories are refused, both out of and into the mount
	for _, p := range []string{"/out-link", "/in-link"} {
		if _, err := u.Files(p); err == nil || !strings.Contains(err.Error(), "symbolic") {
			t.Errorf("symlink dir %q: want refusal, got %v", p, err)
		}
	}
}

// without a live mount nothing is listed (not the clock's own mount point)
func TestUSBFilesNeedMount(t *testing.T) {
	oldM := usbMounted
	usbMounted = func() (string, string, string, bool) { return "", "", "", false }
	defer func() { usbMounted = oldM }()
	if _, err := (&usbSock{}).Files("/"); err == nil {
		t.Error("listing without a mounted drive must fail")
	}
}

// the mount is always read-only and hardened; no charset options except
// vfat's utf8 flag — the clock's kernel rejects every non-default NLS table
func TestMountOpts(t *testing.T) {
	if usbMountFlags&syscall.MS_RDONLY == 0 || usbMountFlags&syscall.MS_NOEXEC == 0 ||
		usbMountFlags&syscall.MS_NOSUID == 0 || usbMountFlags&syscall.MS_NODEV == 0 {
		t.Errorf("mount flags: %#x", usbMountFlags)
	}
	for _, fs := range usbFilesystems {
		o := mountData(fs)
		if strings.Contains(o, "iocharset") || strings.Contains(o, "codepage") || hasOpt(o, "rw") {
			t.Errorf("%s data: %s", fs, o)
		}
	}
	if !hasOpt(mountData("vfat"), "utf8") {
		t.Error("vfat needs the utf8 flag for readable names")
	}
	if !hasOpt("ro,nosuid,errors=remount-ro", "ro") || hasOpt("rw,errors=remount-ro", "ro") {
		t.Error("hasOpt must match whole options")
	}
}

// partitions in number order, a disk without a partition table as itself,
// and nothing whose device node is not there yet or that has no medium (a
// card reader without a card: size 0)
func TestStickParts(t *testing.T) {
	sys, dev := t.TempDir(), t.TempDir()
	oldS, oldD := usbSysBlock, usbDevDir
	usbSysBlock, usbDevDir = sys, dev
	defer func() { usbSysBlock, usbDevDir = oldS, oldD }()
	for _, d := range []string{"sda/sda1", "sda/sda2", "sda/sda10", "sda/queue", "sdb", "sdc/sdc1", "sdd", "mmcblk0/mmcblk0p1"} {
		os.MkdirAll(filepath.Join(sys, d), 0o755)
	}
	os.WriteFile(filepath.Join(sys, "sdb", "size"), []byte("7864320\n"), 0o600)
	os.WriteFile(filepath.Join(sys, "sdd", "size"), []byte("0\n"), 0o600)             // empty card reader
	for _, n := range []string{"sda", "sda1", "sda2", "sda10", "sdb", "sdc", "sdd"} { // sdc1's node is missing
		os.WriteFile(filepath.Join(dev, n), nil, 0o600)
	}
	if got := strings.Join(stickParts(), ","); got != "sda1,sda2,sda10,sdb" {
		t.Errorf("stickParts: %s", got)
	}
}

// devices on the bus by name; root hubs and interfaces are not devices
func TestUSBAttached(t *testing.T) {
	bus := t.TempDir()
	old := usbSysBus
	usbSysBus = bus
	defer func() { usbSysBus = old }()
	if got := usbAttached(); len(got) != 0 {
		t.Errorf("empty bus: %v", got)
	}
	for _, d := range []string{"usb1", "usb2", "1-0:1.0", "1-1", "1-1:1.0", "1-1.2"} {
		os.MkdirAll(filepath.Join(bus, d), 0o755)
	}
	os.WriteFile(filepath.Join(bus, "1-1", "manufacturer"), []byte("Wilk\n"), 0o600)
	os.WriteFile(filepath.Join(bus, "1-1", "product"), []byte("USB DISK 3.0\n"), 0o600)
	if got := strings.Join(usbAttached(), "|"); got != "Wilk USB DISK 3.0|USB device" {
		t.Errorf("usbAttached: %s", got)
	}
}

func TestCmdlineHas(t *testing.T) {
	c := "console=ttyS0 androidboot.debuggable=1 androidboot.secure=0"
	if !cmdlineHas(c, "androidboot.debuggable=1") || cmdlineHas("x androidboot.debuggable=0", "androidboot.debuggable=1") ||
		cmdlineHas("androidboot.debuggable=10", "androidboot.debuggable=1") {
		t.Error("cmdlineHas")
	}
}
