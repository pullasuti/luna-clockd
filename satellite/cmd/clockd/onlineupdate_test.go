package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"lenovo-satellite/internal/config"
)

func TestOnlineCheck(t *testing.T) {
	var tag, assetName string
	status := http.StatusOK
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/latest":
			if r.Header.Get("User-Agent") != "clockd/"+version {
				t.Errorf("User-Agent %q", r.Header.Get("User-Agent"))
			}
			w.WriteHeader(status)
			fmt.Fprintf(w, `{"tag_name": %q, "body": "x", "assets": [{"name": "SHA256SUMS", "browser_download_url": "https://x/SHA256SUMS", "size": 94},
				{"name": %q, "browser_download_url": "https://example.invalid/pkg.tar.gz", "size": 9000000}]}`, tag, assetName)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	cfg, err := config.Load(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	o := newOnlineCheck(slog.New(slog.NewTextHandler(io.Discard, nil)), cfg)
	o.api = srv.URL + "/latest"
	ctx := context.Background()

	if st := o.status(); st["enabled"] != false {
		t.Fatalf("on by default: %v", st)
	}
	if _, _, err := o.download(ctx); err == nil {
		t.Error("download while off")
	}
	cfg.Update(func(c *config.Config) { c.UpdateCheck = true })

	tag, assetName = "v999.0.0", "clockd-999.0.0-arm64.tar.gz"
	if err := o.check(ctx); err != nil {
		t.Fatal(err)
	}
	if st := o.status(); st["latest"] != "999.0.0" || st["available"] != true || st["size"] != int64(9000000) || st["checked"] == nil {
		t.Errorf("newer: %v", st)
	}
	tag, assetName = "v"+version, "clockd-"+version+"-arm64.tar.gz"
	o.check(ctx)
	if st := o.status(); st["available"] != false {
		t.Errorf("same version: %v", st)
	}
	tag, assetName = "v999.0.0", "something-else.tar.gz" // a release without a package for this clock
	o.check(ctx)
	if st := o.status(); st["available"] != false {
		t.Errorf("no package: %v", st)
	}
	if _, _, err := o.download(ctx); err == nil {
		t.Error("download without a package")
	}
	status = http.StatusForbidden // e.g. rate limited
	if err := o.check(ctx); err == nil {
		t.Error("403 accepted")
	}
	if st := o.status(); st["error"] == nil || st["latest"] != "999.0.0" {
		t.Errorf("error kept the last result? %v", st)
	}
	status, tag = http.StatusOK, "nightly"
	if err := o.check(ctx); err == nil {
		t.Error("a tag without a version accepted")
	}
}
