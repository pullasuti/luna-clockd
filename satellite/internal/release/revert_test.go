package release

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestNotesFromChangelog(t *testing.T) {
	changelog := `# Changelog

Intro text.

## 0.102.0 (2026-11-15)

Not released yet.

## 0.101.0 (2026-11-14)

MP3 playback from USB drives
and URLs.

Details that stay in the file.

## 0.100.7 (2026-11-14)
- Tin roof rain back to v1 (settled).
- a second bullet

## Unreleased ideas

Not a version.

## 0.87.3 (2026-11-12)

## 0.79.0 (2026-11-09)

Old.`
	got := NotesFromChangelog(changelog, "0.101.0", 4)
	want := []Note{
		{"0.101.0", "MP3 playback from USB drives and URLs"},
		{"0.100.7", "Tin roof rain back to v1 (settled)"},
		{"0.79.0", "Old"}, // 0.87.3 has no text
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%d: %+v, want %+v", i, got[i], want[i])
		}
	}
	if s := NotesSince(got, "0.79.0"); len(s) != 2 || s[1].Version != "0.100.7" {
		t.Errorf("NotesSince = %+v", s)
	}
	if long := NotesFromChangelog("## 1.0.0 (2026-01-01)\n\n"+strings.Repeat("x", 300), "1.0.0", 1); len([]rune(long[0].Text)) != 140 {
		t.Errorf("long title kept %d runes", len([]rune(long[0].Text)))
	}
}

func TestPackNotes(t *testing.T) {
	pub, priv := newKey(t)
	src := t.TempDir()
	writeTree(t, src, map[string]string{"clockd": "x"})
	var b bytes.Buffer
	notes := []Note{{"0.80.0", "new"}, {"0.79.1", "older"}}
	if _, err := Pack(&b, src, "0.80.0", priv, notes); err != nil {
		t.Fatal(err)
	}
	m, err := Verify(bytes.NewReader(b.Bytes()), []ed25519.PublicKey{pub})
	if err != nil || len(m.Notes) != 2 || m.Notes[0] != notes[0] {
		t.Fatalf("Verify = %+v, %v", m, err)
	}
	// the notes are signed: editing them breaks the package
	bad := retar(t, b.Bytes(), func(name string, x []byte) []byte {
		return bytes.Replace(x, []byte(`"new"`), []byte(`"bad"`), 1)
	})
	if _, err := Verify(bytes.NewReader(bad), []ed25519.PublicKey{pub}); err == nil {
		t.Error("edited notes accepted")
	}
}

func TestRevert(t *testing.T) {
	pub, priv := newKey(t)
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"clockd": "old clockd", "start.sh": "old start", "config.json": "{settings}"})
	p := pkg(t, map[string]string{"clockd": "new clockd", "start.sh": "new start", "aec/apmd": "added helper"}, "0.80.0", priv)
	if _, err := Install(bytes.NewReader(p), dir, Options{Keys: []ed25519.PublicKey{pub}, Current: "0.79.1"}); err != nil {
		t.Fatal(err)
	}
	check := func(path string) (string, error) {
		if read(t, path) != "old clockd" {
			return "", errors.New("wrong file")
		}
		return "0.79.1", nil
	}
	// not during the trial
	if _, err := Revert(dir, "0.80.0", check); err == nil {
		t.Fatal("went back during the trial")
	}
	if _, err := Commit(dir); err != nil {
		t.Fatal(err)
	}
	if v, ok := PrevVersion(dir, "0.80.0"); !ok || v != "0.79.1" {
		t.Fatalf("PrevVersion = %q %v after the commit", v, ok)
	}
	// a clockd copied in by adb since (other version): nothing to go back to
	if _, ok := PrevVersion(dir, "0.80.1"); ok {
		t.Error("offered to go back from a version the update didn't install")
	}
	if _, err := Revert(dir, "0.80.0", func(string) (string, error) { return "0.75.0", nil }); err == nil {
		t.Error("went back to a clockd reporting the wrong version")
	}
	to, err := Revert(dir, "0.80.0", check)
	if err != nil || to != "0.79.1" {
		t.Fatalf("Revert = %q, %v", to, err)
	}
	for p, want := range map[string]string{"clockd": "old clockd", "start.sh": "old start", "config.json": "{settings}"} {
		if got := read(t, filepath.Join(dir, p)); got != want {
			t.Errorf("%s = %q, want %q", p, got, want)
		}
	}
	for _, p := range []string{"aec/apmd", "update/prev", "update/added", "update/swapping", "update/trial", "update/failed"} {
		if exists(filepath.Join(dir, p)) {
			t.Errorf("%s exists", p)
		}
	}
	if st := ReadStatus(dir); !strings.HasPrefix(st.Last, "0.80.0 0.79.1 ") || !strings.HasSuffix(st.Last, " back") {
		t.Errorf("last = %q", st.Last)
	}
	if _, ok := PrevVersion(dir, "0.79.1"); ok {
		t.Error("going back twice")
	}
}
