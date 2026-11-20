package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"lenovo-satellite/internal/config"
)

func TestFirmwareOffersOnlyWhenAllowed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"tag_name": "v999.0.0", "html_url": "https://github.com/pullasuti/luna-clockd/releases/tag/v999.0.0",
			"body": "New: a thing.\r\n\r\n- detail one\n- detail two",
			"assets": [{"name": "clockd-999.0.0-arm64.tar.gz", "browser_download_url": "https://example.invalid/p.tar.gz", "size": 9000000}]}`)
	}))
	defer srv.Close()
	cfg, err := config.Load(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	o := newOnlineCheck(log, cfg)
	o.api = srv.URL
	f := newFirmware(log, cfg)
	if st := f.state(); st.LatestVersion != version {
		t.Fatalf("no web backend yet: %+v", st)
	}
	f.wb.Store(&webBackend{online: o})

	cfg.Update(func(c *config.Config) { c.UpdateCheck = true })
	if err := o.check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st := f.state(); st.LatestVersion != version {
		t.Errorf("offered without update_ha: %+v", st)
	}
	cfg.Update(func(c *config.Config) { c.UpdateInHA = true })
	st := f.state()
	if st.LatestVersion != "999.0.0" || st.ReleaseSummary != "New: a thing." || !strings.HasSuffix(st.ReleaseURL, "/v999.0.0") {
		t.Errorf("offered: %+v", st)
	}
	cfg.Update(func(c *config.Config) { c.UpdateCheck = false }) // the check off hides it again
	if st := f.state(); st.LatestVersion != version {
		t.Errorf("offered with the online check off: %+v", st)
	}
}

func TestFirstParagraph(t *testing.T) {
	if got := firstParagraph("One line\nwrapped.\n\nMore.", 100); got != "One line wrapped." {
		t.Errorf("got %q", got)
	}
	if got := firstParagraph(strings.Repeat("ä", 10), 5); got != "ää…" {
		t.Errorf("cut: %q", got)
	}
}

func TestProgressReader(t *testing.T) {
	var got []float32
	p := &progressReader{r: strings.NewReader(strings.Repeat("x", 100)), size: 100, report: func(v float32) { got = append(got, v) }}
	b := make([]byte, 30)
	for {
		if _, err := p.Read(b); err != nil {
			break
		}
	}
	if len(got) == 0 || got[len(got)-1] != 1 {
		t.Errorf("reports %v", got)
	}
}
