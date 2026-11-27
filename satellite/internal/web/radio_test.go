package web

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeRadio struct {
	Backend // the rest unused
	hz      int
	c       chan []byte
}

func (f *fakeRadio) RadioToken(bool) string { return "secret" }
func (f *fakeRadio) RadioListen(hz int) (<-chan []byte, func(), error) {
	f.hz = hz
	f.c = make(chan []byte, 4)
	f.c <- []byte{1, 2, 3, 4}
	close(f.c)
	return f.c, func() {}, nil
}

// the stream link needs the token, no session; it sends a WAV header, then PCM
func TestRadioStream(t *testing.T) {
	f := &fakeRadio{}
	s := &Server{Backend: f, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	cl := &http.Client{Timeout: 5 * time.Second}
	res, err := cl.Get(srv.URL + "/radio/wrong.wav")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("wrong token: %d, want 404", res.StatusCode)
	}
	res, err = cl.Get(srv.URL + "/radio/secret.wav?mhz=91.2")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || res.Header.Get("Content-Type") != "audio/wav" {
		t.Fatalf("status %d type %q", res.StatusCode, res.Header.Get("Content-Type"))
	}
	if f.hz != 91_200_000 {
		t.Errorf("hz %d, want 91200000", f.hz)
	}
	if len(b) != 48 || !bytes.HasPrefix(b, []byte("RIFF")) || !bytes.Equal(b[44:], []byte{1, 2, 3, 4}) {
		t.Errorf("body %d bytes: % x", len(b), b[:min(len(b), 8)])
	}
	res, err = cl.Get(srv.URL + "/radio/secret.flac")
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || res.Header.Get("Content-Type") != "audio/flac" || !bytes.HasPrefix(b, []byte("fLaC")) {
		t.Errorf("flac: status %d type %q body % x", res.StatusCode, res.Header.Get("Content-Type"), b[:min(len(b), 8)])
	}
	if res, err = cl.Get(srv.URL + "/radio/secret.mp3"); err == nil {
		res.Body.Close()
		if res.StatusCode != http.StatusNotFound {
			t.Errorf("mp3: %d, want 404", res.StatusCode)
		}
	}
}

type endlessRadio struct {
	Backend
	done chan struct{}
}

func (f *endlessRadio) RadioToken(bool) string { return "secret" }
func (f *endlessRadio) RadioListen(int) (<-chan []byte, func(), error) {
	c := make(chan []byte)
	stop := make(chan struct{})
	go func() {
		blk := make([]byte, 64<<10)
		for {
			select {
			case c <- blk:
			case <-stop:
				return
			}
		}
	}()
	return c, func() { close(stop); close(f.done) }, nil
}

// a listener that stops reading is dropped once a write stalls
func TestRadioStreamStall(t *testing.T) {
	old := radioStall
	radioStall = 300 * time.Millisecond
	defer func() { radioStall = old }()
	f := &endlessRadio{done: make(chan struct{})}
	s := &Server{Backend: f, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.(*net.TCPConn).SetReadBuffer(4096)
	io.WriteString(conn, "GET /radio/secret.wav HTTP/1.1\r\nHost: x\r\n\r\n")
	// never read: the socket buffers fill, the write blocks, the deadline ends it
	select {
	case <-f.done:
	case <-time.After(10 * time.Second):
		t.Fatal("a listener that stopped reading was never dropped")
	}
}

type driveFiles struct {
	Backend
	dir string
}

func (f *driveFiles) USBOpen(p string) (*os.File, string, error) {
	g, err := os.Open(filepath.Join(f.dir, filepath.Clean("/"+p)))
	return g, p, err
}

// a drive file goes to the browser with its type and ranges (seeking)
func TestUSBFile(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.flac"), []byte("fLaC0123456789"), 0o644)
	// a session as a logged-in browser has it
	token := strings.Repeat("cd", 32)
	sp := filepath.Join(dir, "sessions.json")
	raw, _ := json.Marshal([]Session{{ID: "s1", Hash: hashToken(token), Created: time.Now(), LastSeen: time.Now(), Method: "button"}})
	os.WriteFile(sp, raw, 0o600)
	s := &Server{Backend: &driveFiles{dir: dir}, Auth: &Auth{Path: sp}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	get := func(p, rg string, login bool) (*http.Response, string) {
		req, _ := http.NewRequest("GET", srv.URL+p, nil)
		if rg != "" {
			req.Header.Set("Range", rg)
		}
		if login {
			req.AddCookie(&http.Cookie{Name: cookieName, Value: token})
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		return res, string(b)
	}
	if res, b := get("/api/usb/file?path=/a.flac", "bytes=4-7", true); res.StatusCode != http.StatusPartialContent || b != "0123" || res.Header.Get("Content-Type") != "audio/flac" {
		t.Errorf("%d %q %q", res.StatusCode, b, res.Header.Get("Content-Type"))
	}
	if res, _ := get("/api/usb/file?path=/a.flac", "", false); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("without a login: %d", res.StatusCode)
	}
	if res, _ := get("/api/usb/file?path=/nope.flac", "", true); res.StatusCode != http.StatusNotFound {
		t.Errorf("missing file: %d", res.StatusCode)
	}
}
