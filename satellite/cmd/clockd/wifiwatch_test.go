package main

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func testDHCP(t *testing.T) (*dhcpClient, *int) {
	t.Helper()
	dir := t.TempDir()
	sent := 0
	d := &dhcpClient{pidFile: filepath.Join(dir, "pid"), stamp: filepath.Join(dir, "lease"), watcher: filepath.Join(dir, "watch"),
		signal:  func(int) error { sent++; return nil },
		hasAddr: func() bool { return true }}
	return d, &sent
}

// A change to another network on the same subnet keeps the old address on
// wlan0 the whole time: only the DHCP answer (the stamp dhcp.sh rewrites)
// says the new network works.
func TestDHCPWaitNeedsAnAnswerNotJustAnAddress(t *testing.T) {
	d, sent := testDHCP(t)
	// "udhcpc" must be in the pid's command line: this test binary's own is not
	os.WriteFile(d.pidFile, []byte(strconv.Itoa(os.Getpid())), 0o644)
	if d.renew() {
		t.Fatal("signalled a process that is not udhcpc")
	}
	// without our udhcpc (use-dhcpcd) an address is all there is to wait for
	if _, err := d.wait(context.Background()); err != nil {
		t.Fatal(err)
	}

	// pretend a renew went out when the stamp was last written
	os.WriteFile(d.stamp, []byte("bound 192.0.2.9\n"), 0o644)
	old := time.Now().Add(-time.Minute)
	os.Chtimes(d.stamp, old, old)
	d.sentAt, d.before = time.Now(), d.stampTime()

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	if _, err := d.wait(ctx); err == nil {
		t.Fatal("an address without an answer from the network counted as connected")
	}
	go func() {
		time.Sleep(150 * time.Millisecond)
		os.WriteFile(d.stamp, []byte("renew 192.0.2.9\n"), 0o644) // same address, new answer
	}()
	ctx2, cancel2 := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel2()
	if _, err := d.wait(ctx2); err != nil {
		t.Fatal("the answer was not noticed:", err)
	}
	if *sent != 0 {
		t.Fatalf("signals sent: %d", *sent)
	}
}

func TestPidOfChecksTheCommand(t *testing.T) {
	f := filepath.Join(t.TempDir(), "pid")
	os.WriteFile(f, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o644)
	if pidOf(f, ".test") != os.Getpid() { // go test binaries are named <pkg>.test
		t.Fatal("own pid not found by command")
	}
	if pidOf(f, "udhcpc") != 0 {
		t.Fatal("a reused pid of another program was accepted")
	}
	os.WriteFile(f, []byte("1"), 0o644)
	if pidOf(f, "") != 0 {
		t.Fatal("pid 1 accepted")
	}
}
