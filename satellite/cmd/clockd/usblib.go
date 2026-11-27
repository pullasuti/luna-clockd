package main

// The music library of the USB drive: every audio file's tags, read in the
// background when a drive is mounted and kept in a file, so a rescan only
// reads what is new or changed (same path, size and time = the same file).
// The browser gets the whole index once and builds albums, artists, songs and
// the filter from it; nothing per click comes back to the clock.

import (
	"encoding/json"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"lenovo-satellite/internal/output"
)

// libTrack is one file in the index (the short JSON names keep a big
// library's index small). Album, Title and Artist are filled from the folder
// and file names when the tags lack them (Guess).
type libTrack struct {
	Path        string `json:"p"`
	Title       string `json:"t"`
	Artist      string `json:"a,omitempty"`
	AlbumArtist string `json:"aa,omitempty"`
	Album       string `json:"al,omitempty"`
	Year        string `json:"y,omitempty"`
	Track       int    `json:"n,omitempty"`
	Disc        int    `json:"d,omitempty"`
	Secs        int    `json:"s,omitempty"`
	Guess       bool   `json:"g,omitempty"`
}

// libEntry is a cached file: its tags as read (no guesses) and what tells a
// changed file.
type libEntry struct {
	T     libTrack `json:"t"`
	Size  int64    `json:"z"`
	MTime int64    `json:"m"`
}

// libMax: files indexed at most (a bigger drive lists the first ones).
const libMax = 50_000

type musicLib struct {
	cache string // the index file
	root  string // the drive's mount ("" = usbMount; tests)
	log   interface {
		Info(msg string, args ...any)
		Warn(msg string, args ...any)
	}

	mu          sync.Mutex
	drive       string // the drive the index is for ("" = none)
	scanning    bool
	done, total int
	tracks      []libTrack          // the index with guesses, sorted
	byPath      map[string]libTrack // the same, by path
	gen         int                 // counts finished scans: the browser refetches when it changes
}

// ensure starts indexing drive when it is not the one indexed (cheap to call
// often: from the status poll).
func (l *musicLib) ensure(drive string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if drive == l.drive {
		return
	}
	l.drive = drive
	if drive == "" {
		l.tracks, l.byPath, l.scanning = nil, nil, false
		l.gen++
		return
	}
	l.tracks, l.byPath = nil, nil // another drive's (a remount reads back from the index file fast)
	l.scanning, l.done, l.total = true, 0, 0
	go l.scan(drive)
}

// current reports whether drive is still the one to index.
func (l *musicLib) current(drive string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.drive == drive
}

func (l *musicLib) scan(drive string) {
	start := time.Now()
	root := l.root
	if root == "" {
		root = usbMount
	}
	old := map[string]libEntry{}
	if b, err := os.ReadFile(l.cache); err == nil {
		var list []libEntry
		if json.Unmarshal(b, &list) == nil {
			for _, e := range list {
				old[e.T.Path] = e
			}
		}
	}
	type file struct {
		rel         string
		size, mtime int64
	}
	var files []file
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // an unreadable folder: skip it, keep going
		}
		name := d.Name()
		if d.IsDir() {
			if p != root && libSkipDir(name) {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || strings.HasPrefix(name, "._") || !audioExt(name) || len(files) >= libMax {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		files = append(files, file{"/" + filepath.ToSlash(strings.TrimPrefix(p, root+"/")), fi.Size(), fi.ModTime().Unix()})
		return nil
	})
	l.mu.Lock()
	l.total = len(files)
	l.mu.Unlock()
	entries := make([]libEntry, 0, len(files))
	read := 0
	for i, f := range files {
		if i%50 == 0 {
			if !l.current(drive) {
				return // pulled or swapped: the new drive's scan takes over
			}
			l.mu.Lock()
			l.done = i
			l.mu.Unlock()
		}
		if e, ok := old[f.rel]; ok && e.Size == f.size && e.MTime == f.mtime {
			entries = append(entries, e)
			continue
		}
		e := libEntry{T: libTrack{Path: f.rel}, Size: f.size, MTime: f.mtime}
		if fh, err := os.Open(filepath.Join(root, f.rel)); err == nil {
			if info, err := output.TrackMeta(fh); err == nil {
				e.T.Title, e.T.Artist, e.T.Album = info.Title, info.Artist, info.Album
				e.T.AlbumArtist, e.T.Year, e.T.Track, e.T.Disc = info.AlbumArtist, info.Year, info.Track, info.Disc
				e.T.Secs = int(info.Duration.Round(time.Second).Seconds())
			}
			fh.Close()
		}
		entries = append(entries, e)
		read++
	}
	// the cache keeps this drive's files (another drive's are read again later)
	if b, err := json.Marshal(entries); err == nil {
		tmp := l.cache + ".tmp"
		if os.WriteFile(tmp, b, 0o600) == nil {
			os.Rename(tmp, l.cache)
		}
	}
	tracks := make([]libTrack, len(entries))
	for i, e := range entries {
		tracks[i] = libGuess(e.T)
	}
	sort.SliceStable(tracks, func(i, j int) bool { return tracks[i].Path < tracks[j].Path })
	by := make(map[string]libTrack, len(tracks))
	for _, t := range tracks {
		by[t.Path] = t
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.drive != drive {
		return
	}
	l.tracks, l.byPath, l.scanning, l.done = tracks, by, false, len(files)
	l.gen++
	l.log.Info("usb: library", "tracks", len(tracks), "read", read, "took", time.Since(start).Round(time.Millisecond))
}

// libSkipDir: system and hidden folders that hold no music.
func libSkipDir(name string) bool {
	if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "$") {
		return true
	}
	switch strings.ToLower(name) {
	case "system volume information", "lost+found", "recycler":
		return true
	}
	return false
}

var (
	// "01. Song", "01 - Song", "1-03 Song", "03_Song"
	reTrackNo = regexp.MustCompile(`^(?:\d{1,2}[-.])?(\d{1,3})(?:\s*[-._]\s*|\s+)`)
	// "Artist - Album (2006)", "Artist - Album [2006]"
	reYear = regexp.MustCompile(`\s*[(\[]((?:19|20)\d\d)[)\]]\s*$`)
	// a disc folder inside an album: "CD1", "Disc 2", "disk_3"
	reDiscDir = regexp.MustCompile(`(?i)^(?:cd|disc|disk)[\s_-]*(\d{1,2})$`)
)

// libGuess fills what the tags lack from the names: the album (and artist,
// year) from the folder ("Artist - Album (Year)"), the title and track
// number from the file name.
func libGuess(t libTrack) libTrack {
	dir := path.Dir(t.Path)
	base := path.Base(dir)
	if m := reDiscDir.FindStringSubmatch(base); m != nil && dir != "/" {
		if t.Disc == 0 {
			t.Disc, _ = strconv.Atoi(m[1])
		}
		base = path.Base(path.Dir(dir))
	}
	if t.Album == "" && base != "/" && base != "." {
		t.Guess = true
		al := base
		if m := reYear.FindStringSubmatch(al); m != nil {
			if t.Year == "" {
				t.Year = m[1]
			}
			al = al[:len(al)-len(m[0])]
		}
		if a, b, ok := strings.Cut(al, " - "); ok && strings.TrimSpace(b) != "" {
			if t.Artist == "" {
				t.Artist = strings.TrimSpace(a)
			}
			al = b
		}
		t.Album = strings.TrimSpace(al)
	}
	if t.Title == "" {
		t.Guess = true
		name := strings.TrimSuffix(path.Base(t.Path), path.Ext(t.Path))
		if m := reTrackNo.FindStringSubmatch(name); m != nil && len(m[0]) < len(name) {
			if t.Track == 0 {
				t.Track, _ = strconv.Atoi(m[1])
			}
			name = name[len(m[0]):]
		}
		if t.Artist != "" {
			name = strings.TrimPrefix(name, t.Artist+" - ")
		}
		t.Title = strings.TrimSpace(name)
	}
	return t
}

// status is the scan's state for the page; full adds the index.
func (l *musicLib) status(full bool) map[string]any {
	if l == nil {
		return map[string]any{"drive": "", "scanning": false, "count": 0, "tracks": []libTrack{}}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	m := map[string]any{"drive": l.drive, "scanning": l.scanning, "done": l.done, "total": l.total,
		"gen": l.gen, "count": len(l.tracks)}
	if full {
		t := l.tracks
		if t == nil {
			t = []libTrack{}
		}
		m["tracks"] = t
	}
	return m
}

// lookup: a file's index entry, if indexed.
func (l *musicLib) lookup(p string) (libTrack, bool) {
	if l == nil {
		return libTrack{}, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	t, ok := l.byPath[p]
	return t, ok
}
