package main

import (
	"bufio"
	"bytes"
	"crypto/rsa"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"
)

// SSH keys (System > SSH): our dropbear (key-only root login on port 22,
// started by start.sh when ssh/authorized_keys has a key) managed from the
// web UI: list, add and remove keys, switch it off and on. Off is the flag
// file ssh/disabled (start.sh then doesn't start it either). clockd starts and
// stops dropbear itself so a change applies at once; open sessions stay.

const (
	sshMaxKeys = 20
	sshPidFile = "/tmp/dropbear.pid"
)

type sshCtl struct {
	dir string // clockd's directory
}

func (s *sshCtl) keysPath() string   { return filepath.Join(s.dir, "ssh", "authorized_keys") }
func (s *sshCtl) offPath() string    { return filepath.Join(s.dir, "ssh", "disabled") }
func (s *sshCtl) binary() string     { return filepath.Join(s.dir, "bin", "dropbearmulti") }
func (s *sshCtl) hostKeyDir() string { return filepath.Join(s.dir, "ssh") }

type sshKey struct {
	Type        string `json:"type"`
	Fingerprint string `json:"fingerprint"` // SHA256:...
	Comment     string `json:"comment"`
	Options     bool   `json:"options,omitempty"` // a line with options (added by hand)
	line        string
}

// readKeys parses authorized_keys; lines that don't parse are kept as they are
// (bad lines are shown as "unreadable" and can be removed too).
func (s *sshCtl) readKeys() ([]sshKey, error) {
	b, err := os.ReadFile(s.keysPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var keys []sshKey
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 64<<10), 64<<10)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		pk, comment, opts, _, err := ssh.ParseAuthorizedKey([]byte(line))
		if err != nil {
			keys = append(keys, sshKey{Type: "unreadable", Fingerprint: "line:" + shortHash(line), line: line})
			continue
		}
		keys = append(keys, sshKey{Type: pk.Type(), Fingerprint: ssh.FingerprintSHA256(pk), Comment: comment,
			Options: len(opts) > 0, line: line})
	}
	return keys, sc.Err()
}

func shortHash(s string) string {
	var h uint32 = 2166136261
	for i := 0; i < len(s); i++ {
		h = (h ^ uint32(s[i])) * 16777619
	}
	return fmt.Sprintf("%08x", h)
}

func (s *sshCtl) writeKeys(keys []sshKey) error {
	dir := filepath.Dir(s.keysPath())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k.line)
		b.WriteByte('\n')
	}
	tmp := s.keysPath() + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o600); err != nil {
		return err
	}
	if f, err := os.Open(tmp); err == nil {
		f.Sync()
		f.Close()
	}
	return os.Rename(tmp, s.keysPath())
}

// parseNewKey checks one pasted public key: a single line, no options, a key
// type dropbear takes, RSA at least 2048 bits. It returns the line to store.
func parseNewKey(text string) (sshKey, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return sshKey{}, errors.New("paste a public key (the .pub file's line)")
	}
	if strings.ContainsAny(text, "\r\n") {
		return sshKey{}, errors.New("one key at a time (a single line)")
	}
	if len(text) > 16<<10 {
		return sshKey{}, errors.New("too long for a public key")
	}
	if strings.Contains(text, "PRIVATE KEY") {
		return sshKey{}, errors.New("that is a private key: paste the public one (the .pub file)")
	}
	pk, comment, opts, rest, err := ssh.ParseAuthorizedKey([]byte(text))
	if err != nil {
		return sshKey{}, errors.New("not an OpenSSH public key line (ssh-ed25519 AAAA... comment)")
	}
	if len(opts) > 0 {
		return sshKey{}, errors.New("key options (from=, command=...) are not taken here")
	}
	if len(bytes.TrimSpace(rest)) > 0 {
		return sshKey{}, errors.New("one key at a time")
	}
	for _, f := range strings.Fields(comment) {
		if strings.HasPrefix(f, "AAAA") && len(f) > 40 { // a second key pasted on the same line
			return sshKey{}, errors.New("one key at a time")
		}
	}
	switch pk.Type() {
	case ssh.KeyAlgoED25519, ssh.KeyAlgoECDSA256, ssh.KeyAlgoECDSA384, ssh.KeyAlgoECDSA521,
		ssh.KeyAlgoSKED25519, ssh.KeyAlgoSKECDSA256:
	case ssh.KeyAlgoRSA:
		if ck, ok := pk.(ssh.CryptoPublicKey); ok {
			if r, ok := ck.CryptoPublicKey().(*rsa.PublicKey); ok && r.N.BitLen() < 2048 {
				return sshKey{}, fmt.Errorf("an RSA key of %d bits is too weak (2048 or more)", r.N.BitLen())
			}
		}
	default:
		return sshKey{}, fmt.Errorf("key type %s is not supported (use ed25519)", pk.Type())
	}
	comment = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, comment)
	if len(comment) > 100 {
		comment = comment[:100]
	}
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pk)))
	if comment != "" {
		line += " " + comment
	}
	return sshKey{Type: pk.Type(), Fingerprint: ssh.FingerprintSHA256(pk), Comment: comment, line: line}, nil
}

func (s *sshCtl) pid() int {
	b, err := os.ReadFile(sshPidFile)
	if err != nil {
		return 0
	}
	p, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || p <= 1 {
		return 0
	}
	if syscall.Kill(p, 0) != nil {
		return 0
	}
	// the pid file may be stale and the number reused: check the program
	if exe, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", p)); err != nil || !isDropbear(exe) {
		return 0
	}
	return p
}

// isDropbear tells whether a /proc/<pid>/exe link is our dropbear. An update
// replaces bin/dropbearmulti (copy, then rename) while dropbear keeps running
// from the old file: the link then ends in " (deleted)".
func isDropbear(exe string) bool {
	return filepath.Base(strings.TrimSuffix(exe, " (deleted)")) == "dropbearmulti"
}

func (s *sshCtl) running() bool { return s.pid() != 0 }

func (s *sshCtl) enabled() bool {
	_, err := os.Stat(s.offPath())
	return err != nil
}

func (s *sshCtl) available() bool {
	_, err := os.Stat(s.binary())
	return err == nil
}

// sync starts or stops dropbear to match the setting and the keys.
func (s *sshCtl) sync() error {
	keys, _ := s.readKeys()
	want := s.enabled() && len(keys) > 0 && s.available()
	switch {
	case want && !s.running():
		// as start.sh: host keys made on first start, root only by key
		cmd := exec.Command(s.binary(), "dropbear", "-R", "-s", "-g", "-D", s.hostKeyDir(), "-P", sshPidFile)
		// it forks into the background: its output goes to clockd's log, no pipe to wait on
		cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("dropbear: %v", err)
		}
		for range 20 {
			if s.running() {
				return nil
			}
			time.Sleep(100 * time.Millisecond)
		}
		return errors.New("dropbear did not start")
	case !want && s.running():
		if p := s.pid(); p != 0 {
			syscall.Kill(p, syscall.SIGTERM)
		}
		for range 20 {
			if !s.running() {
				os.Remove(sshPidFile)
				return nil
			}
			time.Sleep(100 * time.Millisecond)
		}
		return errors.New("dropbear did not stop")
	}
	return nil
}

func (s *sshCtl) hostKey() string {
	// dropbear's own key format: dropbearkey -y prints the fingerprint
	out, err := exec.Command(s.binary(), "dropbearkey", "-y", "-f", filepath.Join(s.hostKeyDir(), "dropbear_ed25519_host_key")).Output()
	if err != nil {
		return ""
	}
	for _, l := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(l, "Fingerprint: ") {
			f := strings.TrimPrefix(l, "Fingerprint: ")
			if i := strings.Index(f, "SHA256:"); i >= 0 {
				f = f[i:]
			}
			return strings.TrimSpace(f)
		}
	}
	return ""
}

// ---- web.SSHer ----

func (b *webBackend) SSHStatus() map[string]any {
	s := b.d.ssh
	keys, err := s.readKeys()
	m := map[string]any{"available": s.available(), "enabled": s.enabled(), "running": s.running(),
		"keys": keys, "max": sshMaxKeys}
	if keys == nil {
		m["keys"] = []sshKey{}
	}
	if err != nil {
		m["error"] = err.Error()
	}
	if s.running() {
		m["host_key"] = s.hostKey()
	}
	return m
}

func (b *webBackend) SSHAddKey(text string) (map[string]any, error) {
	s := b.d.ssh
	if !s.available() {
		return nil, errors.New("ssh is not installed on this clock (install-clock.sh puts dropbear on it)")
	}
	k, err := parseNewKey(text)
	if err != nil {
		return nil, err
	}
	b.sshMu.Lock()
	defer b.sshMu.Unlock()
	keys, err := s.readKeys()
	if err != nil {
		return nil, err
	}
	for _, o := range keys {
		if o.Fingerprint == k.Fingerprint {
			return nil, errors.New("that key is already there")
		}
	}
	if len(keys) >= sshMaxKeys {
		return nil, fmt.Errorf("at most %d keys", sshMaxKeys)
	}
	if err := s.writeKeys(append(keys, k)); err != nil {
		return nil, err
	}
	b.d.log.Info("ssh key added", "type", k.Type, "fingerprint", k.Fingerprint, "comment", k.Comment)
	if err := s.sync(); err != nil {
		b.d.log.Warn("ssh", "err", err)
		return nil, err
	}
	return b.SSHStatus(), nil
}

func (b *webBackend) SSHRemoveKey(fp string) (map[string]any, error) {
	s := b.d.ssh
	b.sshMu.Lock()
	defer b.sshMu.Unlock()
	keys, err := s.readKeys()
	if err != nil {
		return nil, err
	}
	kept := keys[:0:0]
	for _, k := range keys {
		if k.Fingerprint != fp {
			kept = append(kept, k)
		}
	}
	if len(kept) == len(keys) {
		return nil, errors.New("no such key")
	}
	if err := s.writeKeys(kept); err != nil {
		return nil, err
	}
	b.d.log.Info("ssh key removed", "fingerprint", fp, "left", len(kept))
	if err := s.sync(); err != nil {
		b.d.log.Warn("ssh", "err", err)
	}
	return b.SSHStatus(), nil
}

func (b *webBackend) SSHEnable(on bool) (map[string]any, error) {
	s := b.d.ssh
	b.sshMu.Lock()
	defer b.sshMu.Unlock()
	if on {
		if err := os.Remove(s.offPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	} else {
		if err := os.MkdirAll(filepath.Dir(s.offPath()), 0o700); err != nil {
			return nil, err
		}
		if err := os.WriteFile(s.offPath(), nil, 0o600); err != nil {
			return nil, err
		}
	}
	b.d.log.Info("ssh", "enabled", on)
	if err := s.sync(); err != nil {
		return nil, err
	}
	return b.SSHStatus(), nil
}
