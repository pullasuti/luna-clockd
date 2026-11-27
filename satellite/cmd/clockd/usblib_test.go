package main

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLibGuess(t *testing.T) {
	for _, tc := range []struct {
		in   libTrack
		want libTrack
	}{
		{libTrack{Path: "/Detektivbyrån - Hemvägen EP (2006)/01. E18.flac"},
			libTrack{Title: "E18", Artist: "Detektivbyrån", Album: "Hemvägen EP", Year: "2006", Track: 1, Guess: true}},
		{libTrack{Path: "/Band - Record/CD2/1-03 Song.mp3"},
			libTrack{Title: "Song", Artist: "Band", Album: "Record", Track: 3, Disc: 2, Guess: true}},
		{libTrack{Path: "/Mix/Band - Song.mp3", Artist: "Band"},
			libTrack{Title: "Song", Artist: "Band", Album: "Mix", Guess: true}},
		{libTrack{Path: "/x/a.flac", Title: "Tagged", Album: "Tagged album", Track: 4},
			libTrack{Title: "Tagged", Album: "Tagged album", Track: 4}},
		{libTrack{Path: "/top.flac"}, libTrack{Title: "top", Guess: true}},
		{libTrack{Path: "/A/2024.flac"}, libTrack{Title: "2024", Album: "A", Guess: true}},
	} {
		got := libGuess(tc.in)
		tc.want.Path = tc.in.Path
		if got != tc.want {
			t.Errorf("%s:\n got %+v\nwant %+v", tc.in.Path, got, tc.want)
		}
	}
}

// a scan finds the music (not hidden folders or macOS ._ files), keeps an
// index file, and a rescan reads only changed files
func TestLibScan(t *testing.T) {
	root, state := t.TempDir(), t.TempDir()
	for _, f := range []string{"Band - Record (1999)/01 One.flac", "Band - Record (1999)/02 Two.mp3",
		"Band - Record (1999)/._01 One.flac", "Band - Record (1999)/cover.jpg", ".Trashes/x.mp3", "loose.wav"} {
		p := filepath.Join(root, f)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte("not really audio"), 0o644)
	}
	l := &musicLib{root: root, cache: filepath.Join(state, "library.json"), log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	wait := func(gen int) {
		for i := 0; i < 200; i++ {
			if l.status(false)["gen"].(int) > gen {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("scan did not finish")
	}
	l.ensure("sda1#1")
	wait(0)
	st := l.status(true)
	tr := st["tracks"].([]libTrack)
	if len(tr) != 3 {
		t.Fatalf("%d tracks: %+v", len(tr), tr)
	}
	if tr[0].Album != "Record" || tr[0].Title != "One" || tr[0].Year != "1999" || tr[2].Path != "/loose.wav" {
		t.Errorf("%+v", tr)
	}
	if x, ok := l.lookup("/Band - Record (1999)/02 Two.mp3"); !ok || x.Track != 2 {
		t.Errorf("lookup: %+v %v", x, ok)
	}
	if _, err := os.Stat(l.cache); err != nil {
		t.Error("no index file")
	}
	// the drive goes, comes back: from the file; a changed file is read again
	l.ensure("")
	os.WriteFile(filepath.Join(root, "loose.wav"), []byte("changed and longer"), 0o644)
	g := l.status(false)["gen"].(int)
	l.ensure("sda1#2")
	wait(g)
	if n := l.status(false)["count"].(int); n != 3 {
		t.Errorf("rescan: %d tracks", n)
	}
}

func TestQueueFromPaths(t *testing.T) {
	q := queueFromPaths([]string{"/a/1.flac", "/a/2.flac", "/a/cover.jpg", "/a/1.flac", "/b/3.mp3"}, "/a/2.flac")
	if len(q.tracks) != 3 || q.oi != 1 || q.track().path != "/a/2.flac" {
		t.Errorf("%+v", q)
	}
	q = queueFromPaths([]string{"/a/1.flac"}, "/z.flac")
	if len(q.tracks) != 2 || q.track().path != "/z.flac" {
		t.Errorf("missing chosen: %+v", q)
	}
}
