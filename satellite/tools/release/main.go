// release: make the signing key and sign clockd release packages (build
// machine only; the private key never goes to a clock or into git).
//
//	go run ./tools/release keygen [-key FILE]
//	go run ./tools/release pack -version 0.78.0 [-key FILE] [-dir build/clockd-device] -o FILE
//	go run ./tools/release verify FILE
//
// keygen prints the public key to add to internal/release/keys.go; pack
// refuses a key clockd doesn't trust. satellite/release.sh does build, stage
// and pack in one go.
package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"lenovo-satellite/internal/release"
)

func defaultKey() string {
	d, err := os.UserConfigDir()
	if err != nil {
		d = "."
	}
	return filepath.Join(d, "clockd-release", "release.key")
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	cmd, args := os.Args[1], os.Args[2:]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	key := fs.String("key", defaultKey(), "private key file")
	switch cmd {
	case "keygen":
		fs.Parse(args)
		check(keygen(*key))
	case "pack":
		version := fs.String("version", "", "clockd version (must match the staged clockd)")
		dir := fs.String("dir", "build/clockd-device", "staged tree (device/stage.sh)")
		out := fs.String("o", "", "package file to write")
		notes := fs.String("notes", "", "CHANGELOG.md: its first paragraph per version becomes the release notes")
		fs.Parse(args)
		if *version == "" || *out == "" {
			usage()
		}
		check(pack(*key, *dir, *version, *out, *notes))
	case "verify":
		fs.Parse(args)
		if fs.NArg() != 1 {
			usage()
		}
		check(verify(fs.Arg(0)))
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: release keygen [-key FILE] | pack -version V -o FILE [-key FILE] [-dir DIR] [-notes CHANGELOG.md] | verify FILE")
	os.Exit(2)
}

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "release:", err)
		os.Exit(1)
	}
}

func keygen(path string) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%s exists; not overwriting a signing key", path)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	seed := base64.StdEncoding.EncodeToString(priv.Seed()) + "\n"
	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		return err
	}
	fmt.Printf("private key: %s (keep it, back it up; never commit it)\n", path)
	fmt.Printf("public key (add to internal/release/keys.go):\n%s\n", base64.StdEncoding.EncodeToString(pub))
	return nil
}

func loadKey(path string) (ed25519.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("signing key: %w (make one with: release keygen)", err)
	}
	seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("%s: not a signing key", path)
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

func pack(keyPath, dir, version, out, notesFrom string) error {
	var notes []release.Note
	if notesFrom != "" {
		b, err := os.ReadFile(notesFrom)
		if err != nil {
			return err
		}
		if notes = release.NotesFromChangelog(string(b), version, 40); len(notes) == 0 || notes[0].Version != version {
			return fmt.Errorf("%s has no entry for clockd %s", notesFrom, version)
		}
	}
	key, err := loadKey(keyPath)
	if err != nil {
		return err
	}
	pub := key.Public().(ed25519.PublicKey)
	trusted := false
	for _, k := range release.Trusted() {
		if bytes.Equal(k, pub) {
			trusted = true
		}
	}
	if !trusted {
		return fmt.Errorf("the key in %s is not in internal/release/keys.go: clocks would refuse the package", keyPath)
	}
	tmp := out + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	m, err := release.Pack(f, dir, version, key, notes)
	if err == nil {
		err = f.Close()
	} else {
		f.Close()
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, out); err != nil {
		return err
	}
	fi, _ := os.Stat(out)
	fmt.Printf("%s: clockd %s, %d files, %.1f MB\n", out, m.Version, len(m.Files), float64(fi.Size())/1e6)
	return nil
}

func verify(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	m, err := release.Verify(f, release.Trusted())
	if err != nil {
		return err
	}
	fmt.Printf("ok: clockd %s for %s, built %s, signed by a trusted key\n", m.Version, m.Model, m.Created)
	for _, x := range m.Files {
		fmt.Printf("  %o %9d %s %s\n", x.Mode, x.Size, x.SHA256[:12], x.Path)
	}
	for _, n := range m.Notes {
		fmt.Printf("  v%s: %s\n", n.Version, n.Text)
	}
	return nil
}
