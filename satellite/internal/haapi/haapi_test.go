package haapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"lenovo-satellite/internal/config"
)

func setup(t *testing.T, h http.Handler) (*Manager, *httptest.Server, *config.Store) {
	t.Helper()
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	cfg, err := config.Load(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Update(func(c *config.Config) { c.HA.URL = srv.URL })
	m := &Manager{Cfg: cfg, TokenPath: filepath.Join(dir, "ha_token")}
	if err := m.SetToken("tok123"); err != nil {
		t.Fatal(err)
	}
	return m, srv, cfg
}

func fakeHA(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok123" {
			w.WriteHeader(401)
			return
		}
		switch r.URL.Path {
		case "/api/":
			w.Write([]byte(`{"message":"API running."}`))
		case "/api/states":
			w.Write([]byte(`[{"entity_id":"weather.b","state":"sunny","attributes":{"friendly_name":"Beta"}},
			 {"entity_id":"light.x","state":"on","attributes":{}},
			 {"entity_id":"weather.a","state":"rainy","attributes":{"friendly_name":"Alpha"}}]`))
		default:
			w.WriteHeader(404)
		}
	})
}

func TestPinningAndEntities(t *testing.T) {
	m, srv, cfg := setup(t, fakeHA(t))
	ctx := context.Background()

	// self-signed: untrusted until pinned, error carries the fingerprint
	err := m.Test(ctx)
	var ce *CertError
	if !errors.As(err, &ce) {
		t.Fatalf("want CertError, got %v", err)
	}
	sum := sha256.Sum256(srv.Certificate().Raw)
	if ce.Fingerprint != hex.EncodeToString(sum[:]) {
		t.Fatalf("fingerprint %s", ce.Fingerprint)
	}
	cfg.Update(func(c *config.Config) { c.HA.TLSFingerprint = ce.Fingerprint })
	if err := m.Test(ctx); err != nil {
		t.Fatalf("pinned: %v", err)
	}
	list, err := m.Entities(ctx, "weather")
	if err != nil || len(list) != 2 || list[0].ID != "weather.a" || list[1].Name != "Beta" {
		t.Fatalf("entities %v %v", list, err)
	}

	// wrong pin is rejected
	cfg.Update(func(c *config.Config) { c.HA.TLSFingerprint = "00" + ce.Fingerprint[2:] })
	if err := m.Test(ctx); !errors.As(err, &ce) {
		t.Fatalf("wrong pin accepted: %v", err)
	}
	cfg.Update(func(c *config.Config) { c.HA.TLSFingerprint = ce.Fingerprint })

	// bad token
	m.SetToken("nope")
	if err := m.Test(ctx); err == nil {
		t.Fatal("bad token accepted")
	}
	// no token
	m.SetToken("")
	if m.Configured() || m.Test(ctx) == nil {
		t.Fatal("works without token")
	}
}

func TestHTTPPolicy(t *testing.T) {
	if _, err := ValidateURL("http://ha:8123", false); err == nil {
		t.Fatal("plain http accepted without opt-in")
	}
	if _, err := ValidateURL("http://ha:8123", true); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateURL("ftp://ha", true); err == nil {
		t.Fatal("ftp accepted")
	}
	if NormalizeFingerprint("AB:cd") != "" {
		t.Fatal("short fingerprint accepted")
	}
}
