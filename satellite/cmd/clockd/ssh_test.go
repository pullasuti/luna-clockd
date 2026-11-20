package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func pubLine(t *testing.T, k any, comment string) string {
	t.Helper()
	pk, err := ssh.NewPublicKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pk))) + " " + comment
}

func TestParseNewKey(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	good := pubLine(t, pub, "timo@desk")
	k, err := parseNewKey("  " + good + "  ")
	if err != nil || k.Type != "ssh-ed25519" || k.Comment != "timo@desk" || !strings.HasPrefix(k.Fingerprint, "SHA256:") || k.line != good {
		t.Fatalf("good key: %+v %v", k, err)
	}
	weak, _ := rsa.GenerateKey(rand.Reader, 1024)
	for name, in := range map[string]string{
		"empty":     "",
		"two lines": good + "\n" + good,
		"options":   `command="ls" ` + good,
		"private":   "-----BEGIN OPENSSH PRIVATE KEY-----",
		"garbage":   "ssh-ed25519 notbase64!!",
		"weak rsa":  pubLine(t, &weak.PublicKey, "old"),
		"two keys":  good + " " + good,
	} {
		if _, err := parseNewKey(in); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// control characters leave the comment
	k, err = parseNewKey(strings.TrimSuffix(good, "timo@desk") + "a\x1b[31mb")
	if err != nil || k.Comment != "a[31mb" {
		t.Errorf("comment %q %v", k.Comment, err)
	}
}

func TestIsDropbear(t *testing.T) {
	for exe, want := range map[string]bool{
		"/data/local/clockd/bin/dropbearmulti":           true,
		"/data/local/clockd/bin/dropbearmulti (deleted)": true, // binary replaced by an update, still running
		"/system/bin/sh":                                 false,
		"/data/local/clockd/bin/busybox (deleted)":       false,
		"/data/local/clockd/bin/dropbearmulti.tmp":       false,
	} {
		if got := isDropbear(exe); got != want {
			t.Errorf("%s: %v, want %v", exe, got, want)
		}
	}
}

func TestSSHKeysFile(t *testing.T) {
	dir := t.TempDir()
	s := &sshCtl{dir: dir}
	if keys, err := s.readKeys(); err != nil || len(keys) != 0 {
		t.Fatalf("no file: %v %v", keys, err)
	}
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	k, _ := parseNewKey(pubLine(t, pub, "a"))
	os.MkdirAll(filepath.Join(dir, "ssh"), 0o700)
	// a hand-made file: a comment, a key with options, a broken line
	hand := "# mine\n" + `from="10.0.0.0/8" ` + pubLine(t, pub, "opt") + "\nbroken line\n"
	os.WriteFile(s.keysPath(), []byte(hand), 0o600)
	keys, err := s.readKeys()
	if err != nil || len(keys) != 2 || !keys[0].Options || keys[1].Type != "unreadable" {
		t.Fatalf("hand file: %+v %v", keys, err)
	}
	if err := s.writeKeys(append(keys[1:], k)); err != nil {
		t.Fatal(err)
	}
	keys, _ = s.readKeys()
	if len(keys) != 2 || keys[1].Fingerprint != k.Fingerprint {
		t.Fatalf("after write: %+v", keys)
	}
	if fi, _ := os.Stat(s.keysPath()); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", fi.Mode())
	}
	if !s.enabled() || s.available() {
		t.Error("enabled/available")
	}
}
