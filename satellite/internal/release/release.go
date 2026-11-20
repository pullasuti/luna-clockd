// Package release builds, signs, checks and installs clockd release packages.
//
// A package is a gzipped tar: manifest.json, manifest.sig (Ed25519 over the
// manifest bytes), then the files as "files/<path>". The manifest lists every
// file with its size, SHA-256 and mode, so only the manifest needs signing.
// The private key stays on the build machine (tools/release); clockd carries
// the public keys (keys.go) and refuses anything not signed by one of them.
//
// Install unpacks into <dir>/update/new, keeps the files it replaces in
// <dir>/update/prev and leaves <dir>/update/trial behind. clockd removes the
// trial marker once it has run for a while (Commit); start.sh rolls the files
// back if clockd keeps exiting while the marker is there (see start.sh).
package release

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/sha256"
	"debug/elf"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Model is the only device a package installs on (ro.product.model suffix).
const Model = "CD-4N341Y"

const format = 1

// Manifest describes one release package.
type Manifest struct {
	Format  int    `json:"format"`
	Product string `json:"product"` // "clockd"
	Version string `json:"version"`
	Model   string `json:"model"`
	Created string `json:"created"`
	Files   []File `json:"files"`
	// Notes: one line per version, newest first (what's new; the Update page
	// shows the ones newer than the running version). Signed with the rest;
	// older clockd versions ignore the field.
	Notes []Note `json:"notes,omitempty"`
}

// Note is one version's line of the release notes.
type Note struct {
	Version string `json:"version"`
	Text    string `json:"text"`
}

// File is one file of the package, relative to the clockd directory.
type File struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	Mode   uint32 `json:"mode"`
}

// Allowed reports whether a package may carry this path: only what install.sh
// installs into the clockd directory (never settings, keys or /system).
func Allowed(p string) bool {
	if p == "" || path.Clean(p) != p || path.IsAbs(p) || strings.HasPrefix(p, "../") || strings.Contains(p, "/../") || p == ".." {
		return false
	}
	switch p {
	case "clockd", "start.sh", "restart.sh", "setup-ap.sh":
		return true
	}
	dir, name := path.Split(p)
	if name == "" || strings.HasPrefix(name, ".") {
		return false
	}
	switch dir {
	case "bin/", "aec/", "models/":
		return true
	}
	return false
}

// addOnly paths are installed only when missing: wake word models the user
// already has (or downloaded in the web UI) stay as they are.
func addOnly(p string) bool { return strings.HasPrefix(p, "models/") }

// ---- building (build machine) ----

// Pack signs a package of the allowed files under dir and writes it to w.
func Pack(w io.Writer, dir, version string, key ed25519.PrivateKey, notes []Note) (*Manifest, error) {
	if _, err := ParseVersion(version); err != nil {
		return nil, err
	}
	m := &Manifest{Format: format, Product: "clockd", Version: version, Model: Model,
		Created: time.Now().UTC().Format(time.RFC3339), Notes: notes}
	err := filepath.WalkDir(dir, func(p string, e os.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		rel = filepath.ToSlash(rel)
		if !Allowed(rel) {
			return nil
		}
		fi, err := e.Info()
		if err != nil {
			return err
		}
		sum, err := fileHash(p)
		if err != nil {
			return err
		}
		mode := uint32(0o644)
		if fi.Mode()&0o111 != 0 {
			mode = 0o755
		}
		m.Files = append(m.Files, File{Path: rel, Size: fi.Size(), SHA256: sum, Mode: mode})
		return nil
	})
	if err != nil {
		return nil, err
	}
	if !m.has("clockd") {
		return nil, fmt.Errorf("%s has no clockd", dir)
	}
	sort.Slice(m.Files, func(i, j int) bool { return m.Files[i].Path < m.Files[j].Path })
	mj, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	sig := []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(key, mj)) + "\n")

	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	add := func(name string, mode int64, size int64, r io.Reader) error {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: size, ModTime: time.Now(), Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		_, err := io.Copy(tw, r)
		return err
	}
	if err := add("manifest.json", 0o644, int64(len(mj)), bytes.NewReader(mj)); err != nil {
		return nil, err
	}
	if err := add("manifest.sig", 0o644, int64(len(sig)), bytes.NewReader(sig)); err != nil {
		return nil, err
	}
	for _, f := range m.Files {
		r, err := os.Open(filepath.Join(dir, filepath.FromSlash(f.Path)))
		if err != nil {
			return nil, err
		}
		err = add("files/"+f.Path, int64(f.Mode), f.Size, r)
		r.Close()
		if err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return m, gz.Close()
}

func (m *Manifest) has(p string) bool {
	for _, f := range m.Files {
		if f.Path == p {
			return true
		}
	}
	return false
}

// ---- reading and checking ----

// reader opens a package and returns its verified manifest; the tar reader is
// positioned at the first file.
func reader(r io.Reader, keys []ed25519.PublicKey) (*Manifest, *tar.Reader, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, nil, fmt.Errorf("not a clockd release package: %w", err)
	}
	tr := tar.NewReader(gz)
	next := func(name string, max int64) ([]byte, error) {
		h, err := tr.Next()
		if err != nil {
			return nil, fmt.Errorf("not a clockd release package: %w", err)
		}
		if h.Name != name || h.Size > max {
			return nil, fmt.Errorf("not a clockd release package (found %q)", h.Name)
		}
		return io.ReadAll(tr)
	}
	mj, err := next("manifest.json", 1<<20)
	if err != nil {
		return nil, nil, err
	}
	sigText, err := next("manifest.sig", 4096)
	if err != nil {
		return nil, nil, err
	}
	sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sigText)))
	if err != nil || len(sig) != ed25519.SignatureSize {
		return nil, nil, errors.New("package signature unreadable")
	}
	ok := false
	for _, k := range keys {
		if ed25519.Verify(k, mj, sig) {
			ok = true
			break
		}
	}
	if !ok {
		return nil, nil, errors.New("package not signed by a trusted key")
	}
	var m Manifest
	if err := json.Unmarshal(mj, &m); err != nil {
		return nil, nil, fmt.Errorf("manifest: %w", err)
	}
	if m.Format != format || m.Product != "clockd" {
		return nil, nil, fmt.Errorf("unsupported package (format %d, product %q)", m.Format, m.Product)
	}
	if m.Model != Model {
		return nil, nil, fmt.Errorf("package is for model %q, not %s", m.Model, Model)
	}
	if _, err := ParseVersion(m.Version); err != nil {
		return nil, nil, err
	}
	seen := map[string]bool{}
	for _, f := range m.Files {
		if !Allowed(f.Path) || seen[f.Path] || f.Size < 0 || len(f.SHA256) != 64 {
			return nil, nil, fmt.Errorf("manifest: bad entry %q", f.Path)
		}
		seen[f.Path] = true
	}
	if !seen["clockd"] {
		return nil, nil, errors.New("manifest: no clockd")
	}
	return &m, tr, nil
}

// Verify checks a package completely (signature and every file's hash)
// without installing it.
func Verify(r io.Reader, keys []ed25519.PublicKey) (*Manifest, error) {
	m, tr, err := reader(r, keys)
	if err != nil {
		return nil, err
	}
	err = eachFile(m, tr, func(f File, r io.Reader) error {
		_, err := io.Copy(io.Discard, r)
		return err
	})
	return m, err
}

// eachFile walks the package's files in manifest order and checks each one's
// size and hash after fn has consumed it (fn must read r to the end).
func eachFile(m *Manifest, tr *tar.Reader, fn func(File, io.Reader) error) error {
	for _, f := range m.Files {
		h, err := tr.Next()
		if err != nil {
			return fmt.Errorf("package truncated at %s: %w", f.Path, err)
		}
		if h.Name != "files/"+f.Path || h.Size != f.Size {
			return fmt.Errorf("package doesn't match its manifest at %s", f.Path)
		}
		hs := sha256.New()
		if err := fn(f, io.TeeReader(io.LimitReader(tr, f.Size), hs)); err != nil {
			if errors.Is(err, io.ErrUnexpectedEOF) {
				return fmt.Errorf("package incomplete at %s (upload or copy cut short)", f.Path)
			}
			return err
		}
		if hex.EncodeToString(hs.Sum(nil)) != f.SHA256 {
			return fmt.Errorf("%s: checksum mismatch (damaged or altered package)", f.Path)
		}
	}
	if _, err := tr.Next(); err != io.EOF {
		return errors.New("package has files outside its manifest")
	}
	return nil
}

// Plan is what installing a package would do (Check).
type Plan struct {
	Manifest  *Manifest
	Changed   []string // files that would be written
	Downgrade bool     // older than the running version
}

// Check verifies a package completely (signature, every hash) and works out
// which files it would change in dir, without writing anything.
func Check(r io.Reader, dir string, keys []ed25519.PublicKey, current string) (*Plan, error) {
	m, tr, err := reader(r, keys)
	if err != nil {
		return nil, err
	}
	p := &Plan{Manifest: m, Downgrade: current != "" && CompareVersions(m.Version, current) < 0}
	err = eachFile(m, tr, func(f File, r io.Reader) error {
		if !installed(dir, f) {
			p.Changed = append(p.Changed, f.Path)
		}
		_, err := io.Copy(io.Discard, r)
		return err
	})
	if err != nil {
		return nil, err
	}
	return p, nil
}

// Dismiss clears the note of a rolled-back update (the web UI shows it until then).
func Dismiss(dir string) error {
	err := os.Remove(filepath.Join(dir, failedFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// ParseVersion parses "0.78.0" (digits and dots only).
func ParseVersion(v string) ([]int, error) {
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("bad version %q", v)
	}
	out := make([]int, 3)
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || strconv.Itoa(n) != p {
			return nil, fmt.Errorf("bad version %q", v)
		}
		out[i] = n
	}
	return out, nil
}

// CompareVersions returns -1, 0 or 1 (part by part, numerically).
func CompareVersions(a, b string) int {
	va, ea := ParseVersion(a)
	vb, eb := ParseVersion(b)
	if ea != nil || eb != nil {
		return strings.Compare(a, b)
	}
	for i := range va {
		if va[i] != vb[i] {
			if va[i] < vb[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

// CheckELF checks that p is an arm64 Linux executable.
func CheckELF(p string) error {
	f, err := elf.Open(p)
	if err != nil {
		return fmt.Errorf("%s: not an executable: %w", filepath.Base(p), err)
	}
	defer f.Close()
	if f.Machine != elf.EM_AARCH64 || f.Class != elf.ELFCLASS64 {
		return fmt.Errorf("%s: built for %v, not arm64", filepath.Base(p), f.Machine)
	}
	return nil
}

// ---- installing (on the clock) ----

// ErrDowngrade is returned for an older package without AllowDowngrade.
var ErrDowngrade = errors.New("package is older than the installed clockd")

// Options for Install.
type Options struct {
	Keys           []ed25519.PublicKey
	Current        string // running clockd version
	AllowDowngrade bool
	// CheckClockd runs the staged clockd (e.g. "clockd -version") and must
	// return the version it reports. Nil skips that check (tests).
	CheckClockd func(path string) (string, error)
	Log         func(msg string, args ...any)
}

// Result of an installation.
type Result struct {
	From, To  string
	Changed   []string // files written (replaced or added)
	Unchanged int      // files already identical (or kept: wake word models)
}

// Update directory layout under the clockd directory.
const (
	updDir      = "update"
	stageDir    = "update/new"
	prevDir     = "update/prev"
	addedFile   = "update/added"    // files the update added (removed by a rollback)
	swapMarker  = "update/swapping" // files are being swapped right now
	trialMarker = "update/trial"    // installed, not yet confirmed by a running clockd
	triesFile   = "update/tries"    // start.sh: starts since the update
	failedFile  = "update/failed"   // start.sh: the last update was rolled back
	lastFile    = "update/last"     // the last confirmed update
	lockFile    = "update/.lock"
)

// Install checks the package in r and installs it into dir (the clockd
// directory). Nothing is changed unless the whole package checks out. The
// caller restarts clockd afterwards.
func Install(r io.Reader, dir string, o Options) (*Result, error) {
	logf := o.Log
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if err := os.MkdirAll(filepath.Join(dir, updDir), 0o755); err != nil {
		return nil, err
	}
	lf, err := os.OpenFile(filepath.Join(dir, lockFile), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	defer lf.Close()
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return nil, errors.New("another installation is running")
	}
	defer syscall.Flock(int(lf.Fd()), syscall.LOCK_UN)
	for _, mk := range []string{swapMarker, trialMarker} {
		if exists(filepath.Join(dir, mk)) {
			return nil, errors.New("the previous update is still being tried (wait a minute after the restart)")
		}
	}

	m, tr, err := reader(r, o.Keys)
	if err != nil {
		return nil, err
	}
	res := &Result{From: o.Current, To: m.Version}
	if o.Current != "" && CompareVersions(m.Version, o.Current) < 0 && !o.AllowDowngrade {
		return nil, fmt.Errorf("%w (%s < %s)", ErrDowngrade, m.Version, o.Current)
	}

	// The last update's old files are only kept for start.sh's rollback, which
	// is over once its trial ended (checked above): free their space first.
	stage := filepath.Join(dir, stageDir)
	os.RemoveAll(stage)
	defer os.RemoveAll(stage)
	if err := os.RemoveAll(filepath.Join(dir, prevDir)); err != nil {
		return nil, err
	}
	// Only changed files are written (to update/new); the swap renames, so
	// they are all the extra space needed, plus a little for the markers.
	var need int64
	for _, f := range m.Files {
		if !installed(dir, f) {
			need += f.Size
		}
	}
	if free, err := freeBytes(dir); err == nil && free < need+1<<20 {
		return nil, fmt.Errorf("not enough free space on the clock (%d MB free, %d MB needed)", free>>20, (need+1<<20+1<<20-1)>>20)
	}
	var staged []File
	err = eachFile(m, tr, func(f File, r io.Reader) error {
		if installed(dir, f) {
			res.Unchanged++
			_, err := io.Copy(io.Discard, r)
			return err
		}
		p := filepath.Join(stage, filepath.FromSlash(f.Path))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := writeFile(p, r, os.FileMode(f.Mode&0o777)); err != nil {
			return err
		}
		staged = append(staged, f)
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, f := range staged {
		p := filepath.Join(stage, filepath.FromSlash(f.Path))
		if f.Mode&0o111 != 0 && isELF(p) {
			if err := CheckELF(p); err != nil {
				return nil, err
			}
		}
		if f.Path == "clockd" && o.CheckClockd != nil {
			v, err := o.CheckClockd(p)
			if err != nil {
				return nil, fmt.Errorf("the new clockd doesn't run: %w", err)
			}
			if v != m.Version {
				return nil, fmt.Errorf("the new clockd reports version %q, the package says %s", v, m.Version)
			}
		}
	}
	if len(staged) == 0 {
		logf("update: nothing to install, all files already match", "version", m.Version)
		return res, nil
	}

	// swap: keep each replaced file in update/prev, note the added ones
	prev := filepath.Join(dir, prevDir)
	if err := os.RemoveAll(prev); err != nil {
		return nil, err
	}
	var added []string
	for _, f := range staged {
		if !exists(filepath.Join(dir, filepath.FromSlash(f.Path))) {
			added = append(added, f.Path)
		}
	}
	if err := writeFile(filepath.Join(dir, addedFile), strings.NewReader(lines(added)), 0o644); err != nil {
		return nil, err
	}
	marker := fmt.Sprintf("%s %s\n", o.Current, m.Version)
	for _, f := range []string{triesFile, failedFile} {
		os.Remove(filepath.Join(dir, f))
	}
	if err := writeFile(filepath.Join(dir, swapMarker), strings.NewReader(marker), 0o644); err != nil {
		return nil, err
	}
	for _, f := range staged {
		rel := filepath.FromSlash(f.Path)
		dst := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return nil, swapFailed(err)
		}
		if exists(dst) {
			if err := os.MkdirAll(filepath.Dir(filepath.Join(prev, rel)), 0o755); err != nil {
				return nil, swapFailed(err)
			}
			// rename, not copy: a running binary can be renamed
			if err := os.Rename(dst, filepath.Join(prev, rel)); err != nil {
				return nil, swapFailed(err)
			}
		}
		if err := os.Rename(filepath.Join(stage, rel), dst); err != nil {
			return nil, swapFailed(err)
		}
		res.Changed = append(res.Changed, f.Path)
	}
	syncDirs(dir, staged)
	if err := os.Rename(filepath.Join(dir, swapMarker), filepath.Join(dir, trialMarker)); err != nil {
		return nil, swapFailed(err)
	}
	syncDir(filepath.Join(dir, updDir))
	logf("update installed", "from", o.Current, "to", m.Version, "files", len(res.Changed))
	return res, nil
}

// swapFailed: the swap marker stays, so start.sh restores update/prev when
// clockd restarts.
func swapFailed(err error) error {
	return fmt.Errorf("installing failed half-way, the old version comes back on restart: %w", err)
}

// Status of the last update, for the log and the web UI.
type Status struct {
	Trial  string // "from to" while an update waits for confirmation
	Failed string // set by start.sh when it rolled an update back
	Last   string // the last confirmed update
}

// ReadStatus reads the update markers.
func ReadStatus(dir string) Status {
	rd := func(f string) string {
		b, _ := os.ReadFile(filepath.Join(dir, f))
		return strings.TrimSpace(string(b))
	}
	return Status{Trial: rd(trialMarker), Failed: rd(failedFile), Last: rd(lastFile)}
}

// Commit confirms an update on trial: clockd calls it after running well for a
// while. It returns the confirmed "from to" ("" when nothing was on trial).
func Commit(dir string) (string, error) {
	p := filepath.Join(dir, trialMarker)
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	t := strings.TrimSpace(string(b))
	line := t + " " + time.Now().UTC().Format(time.RFC3339) + "\n"
	if err := writeFile(filepath.Join(dir, lastFile), strings.NewReader(line), 0o644); err != nil {
		return "", err
	}
	os.Remove(filepath.Join(dir, triesFile))
	if err := os.Remove(p); err != nil {
		return "", err
	}
	syncDir(filepath.Join(dir, updDir))
	return t, nil
}

// ---- release notes ----

// changelogEntry matches a CHANGELOG.md version heading: "## 0.107.0 (2026-11-18)".
var changelogEntry = regexp.MustCompile(`^## v?([0-9]+\.[0-9]+\.[0-9]+)\b`)

// NotesFromChangelog builds the release notes from CHANGELOG.md: for each
// version up to version, newest first, at most max, the first paragraph under
// its "## X.Y.Z (date)" heading, wrapped lines joined; in a list, its first
// item. The rest is detail for readers of the file only.
func NotesFromChangelog(changelog, version string, max int) []Note {
	var notes []Note
	var v string
	var para []string
	flush := func() {
		if v != "" && len(para) > 0 {
			text := strings.TrimSuffix(strings.Join(para, " "), ".")
			if r := []rune(text); len(r) > 140 {
				text = string(r[:139]) + "…"
			}
			notes = append(notes, Note{Version: v, Text: text})
		}
		v, para = "", nil
	}
	for _, line := range strings.Split(changelog, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#") {
			flush()
			if len(notes) == max {
				break
			}
			if m := changelogEntry.FindStringSubmatch(line); m != nil && CompareVersions(m[1], version) <= 0 {
				v = m[1]
			}
			continue
		}
		if v == "" {
			continue
		}
		item := strings.HasPrefix(line, "- ")
		if line == "" || item {
			if len(para) > 0 {
				flush() // the first paragraph or list item is the note
				continue
			}
			if line == "" {
				continue
			}
		}
		para = append(para, strings.TrimPrefix(line, "- "))
	}
	if len(notes) < max {
		flush()
	}
	return notes
}

// NotesSince keeps the notes newer than current (all of them when current is "").
func NotesSince(notes []Note, current string) []Note {
	var out []Note
	for _, n := range notes {
		if current == "" || CompareVersions(n.Version, current) > 0 {
			out = append(out, n)
		}
	}
	return out
}

// ---- going back by hand ----

// PrevVersion is the version the last update replaced, when its files are
// still kept (update/prev, until the next install) and the running version
// is the one that update installed (not a clockd copied in by adb since).
func PrevVersion(dir, current string) (string, bool) {
	f := strings.Fields(ReadStatus(dir).Last)
	if len(f) < 2 || f[1] != current || f[0] == "" || f[0] == current {
		return "", false
	}
	if _, err := ParseVersion(f[0]); err != nil {
		return "", false
	}
	if !exists(filepath.Join(dir, prevDir, "clockd")) {
		return "", false
	}
	return f[0], true
}

// Revert puts the previous version's files back (System > Update, "go back").
// The previous version ran here before, so it is not put on trial. If the
// power goes in the middle, the swap marker makes start.sh finish the same
// move (its rollback) at the next start. check runs the old clockd with
// -version first. The caller restarts clockd.
func Revert(dir, current string, check func(string) (string, error)) (string, error) {
	lf, err := os.OpenFile(filepath.Join(dir, lockFile), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return "", err
	}
	defer lf.Close()
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return "", errors.New("an installation is running")
	}
	defer syscall.Flock(int(lf.Fd()), syscall.LOCK_UN)
	for _, mk := range []string{swapMarker, trialMarker} {
		if exists(filepath.Join(dir, mk)) {
			return "", errors.New("the last update is still being tried (wait a minute after the restart)")
		}
	}
	to, ok := PrevVersion(dir, current)
	if !ok {
		return "", errors.New("no previous version kept")
	}
	prev := filepath.Join(dir, prevDir)
	if check != nil {
		v, err := check(filepath.Join(prev, "clockd"))
		if err != nil {
			return "", fmt.Errorf("the previous clockd doesn't run: %w", err)
		}
		if v != to {
			return "", fmt.Errorf("the kept clockd reports version %q, expected %s", v, to)
		}
	}
	var files []string
	err = filepath.WalkDir(prev, func(p string, e os.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(prev, p)
		if !Allowed(filepath.ToSlash(rel)) {
			return fmt.Errorf("unexpected file in update/prev: %s", rel)
		}
		files = append(files, rel)
		return nil
	})
	if err != nil {
		return "", err
	}
	marker := fmt.Sprintf("%s %s\n", current, to)
	if err := writeFile(filepath.Join(dir, swapMarker), strings.NewReader(marker), 0o644); err != nil {
		return "", err
	}
	for _, rel := range files {
		dst := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return "", swapFailed(err)
		}
		if err := os.Rename(filepath.Join(prev, rel), dst); err != nil {
			return "", swapFailed(err)
		}
	}
	// files the update added: the previous version had none
	if b, err := os.ReadFile(filepath.Join(dir, addedFile)); err == nil {
		for _, p := range strings.Split(string(b), "\n") {
			if p = strings.TrimSpace(p); p != "" && Allowed(p) {
				os.Remove(filepath.Join(dir, filepath.FromSlash(p)))
			}
		}
	}
	line := fmt.Sprintf("%s %s %s back\n", current, to, time.Now().UTC().Format(time.RFC3339))
	if err := writeFile(filepath.Join(dir, lastFile), strings.NewReader(line), 0o644); err != nil {
		return "", swapFailed(err)
	}
	os.RemoveAll(prev)
	for _, f := range []string{addedFile, failedFile, triesFile, swapMarker} {
		os.Remove(filepath.Join(dir, f))
	}
	syncDir(dir)
	syncDir(filepath.Join(dir, updDir))
	return to, nil
}

// ---- helpers ----

// installed: f needs no writing (the same file is in place, or it's a wake
// word model the clock already has).
func installed(dir string, f File) bool {
	dst := filepath.Join(dir, filepath.FromSlash(f.Path))
	return exists(dst) && (addOnly(f.Path) || hashIs(dst, f.SHA256))
}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

func lines(s []string) string {
	if len(s) == 0 {
		return ""
	}
	return strings.Join(s, "\n") + "\n"
}

func fileHash(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func hashIs(p, want string) bool { h, err := fileHash(p); return err == nil && h == want }

func isELF(p string) bool {
	f, err := os.Open(p)
	if err != nil {
		return false
	}
	defer f.Close()
	b := make([]byte, 4)
	_, err = io.ReadFull(f, b)
	return err == nil && string(b) == "\x7fELF"
}

// writeFile writes and fsyncs p (a power cut must not leave a truncated file
// under a final name).
func writeFile(p string, r io.Reader, mode os.FileMode) error {
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func syncDir(p string) {
	if d, err := os.Open(p); err == nil {
		d.Sync()
		d.Close()
	}
}

func syncDirs(dir string, files []File) {
	seen := map[string]bool{}
	for _, f := range files {
		d := filepath.Dir(filepath.Join(dir, filepath.FromSlash(f.Path)))
		if !seen[d] {
			seen[d] = true
			syncDir(d)
		}
	}
	syncDir(filepath.Join(dir, prevDir))
}

// FreeBytes is the free space on dir's file system.
func FreeBytes(dir string) (int64, error) { return freeBytes(dir) }

func freeBytes(dir string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}

// ParseKey decodes a base64 Ed25519 public key (keys.go).
func ParseKey(s string) (ed25519.PublicKey, error) {
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("bad public key %q", s)
	}
	return ed25519.PublicKey(b), nil
}

// Fingerprint is a short id of a public key: the first 8 bytes of its SHA-256,
// in hex (shown in the web UI and by tools/release).
func Fingerprint(k ed25519.PublicKey) string {
	h := sha256.Sum256(k)
	return hex.EncodeToString(h[:8])
}

// Trusted returns the public keys compiled into clockd.
func Trusted() []ed25519.PublicKey {
	var out []ed25519.PublicKey
	for _, s := range trustedKeys {
		if k, err := ParseKey(s); err == nil {
			out = append(out, k)
		}
	}
	return out
}
