package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestInitZone(t *testing.T) {
	defer func(l *time.Location, z string) { time.Local, startZone = l, z }(time.Local, startZone)
	dir := t.TempDir()
	initZone(dir) // nothing saved: UTC
	if time.Local != time.UTC || startZone != "" {
		t.Fatalf("no zone: got %v %q", time.Local, startZone)
	}
	os.WriteFile(filepath.Join(dir, "state.json"), []byte(`{"timezone":"EET-2EEST,M3.5.0/3,M10.5.0/4"}`), 0o600)
	initZone(dir) // HA's POSIX rules
	if startZone == "" || time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC).In(time.Local).Hour() != 15 {
		t.Fatalf("HA zone: got %v %q", time.Local, startZone)
	}
	if effectiveZone("Europe/Stockholm", "EET") != "Europe/Stockholm" || effectiveZone("", "EET") != "EET" {
		t.Fatal("effectiveZone: the setting wins over HA's zone")
	}
}
