package wake

import (
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Reset from another goroutine while Process runs used to panic inside
// microwakeword (slice bounds out of range). Needs the model files in ../../models.
func TestConcurrentResetDoesNotPanic(t *testing.T) {
	models, err := LoadDir("../../models")
	if err != nil || len(models) == 0 {
		t.Skip("no models in ../../models")
	}
	det, err := New(models[0], 0)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]int16, 160)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				det.Reset()
			}
		}
	}()
	for i := 0; i < 3000; i++ {
		for j := range buf {
			buf[j] = int16(rand.Intn(2000) - 1000)
		}
		det.Process(buf)
	}
	close(stop)
	wg.Wait()
}

func TestPhrase(t *testing.T) {
	for in, want := range map[string]string{"hey_glados": "Hey Glados", "Okay Nabu": "Okay Nabu", "": "Poopy Face", "alexa": "Alexa", "hey beebee": "Hey Beebee", "hey_R2": "hey_R2"} {
		if got := phrase(in, "poopy_face"); got != want {
			t.Errorf("phrase(%q) = %q, want %q", in, got, want)
		}
	}
}

// WAKE_TRY_DIR=dir go test -run TestCheckDir ./internal/wake checks every
// manifest in dir (e.g. a sample of community models).
func TestCheckDir(t *testing.T) {
	dir := os.Getenv("WAKE_TRY_DIR")
	if dir == "" {
		t.Skip("WAKE_TRY_DIR not set")
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	ok := 0
	for _, f := range files {
		data, _ := os.ReadFile(f)
		id := strings.TrimSuffix(filepath.Base(f), ".json")
		m, err := ParseManifest(data, id, dir)
		if err == nil {
			err = Check(m)
		}
		if err != nil {
			t.Logf("FAIL %s: %v", id, err)
			continue
		}
		ok++
		t.Logf("ok   %s (%q, cutoff %.2f, window %d)", id, m.Phrase, m.Cutoff, m.Window)
	}
	t.Logf("%d/%d load and run", ok, len(files))
}
