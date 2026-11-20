// Package haapi is an optional, read-only client for Home Assistant's REST
// API, used only to list entities for pickers in the web UI. The main link to
// Home Assistant stays the (Noise-encrypted) ESPHome API.
//
// Security: the long-lived access token lives in its own 0600 file and is never
// returned by any API. HTTPS is required unless the user explicitly allows plain
// HTTP. Publicly signed certificates verify against the system roots (the
// firmware ships /etc/ssl/certs/ca-certificates.crt, 2021). A self-signed one
// can be pinned by its SHA-256 fingerprint (trust on first use from the web UI).
package haapi

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"lenovo-satellite/internal/config"
)

// Entity is one item for a picker.
type Entity struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	State string `json:"state"`
}

// CertError means the server certificate is not trusted yet. Fingerprint is
// what the server presented, for the user to compare and pin.
type CertError struct {
	Fingerprint string
	Subject     string
	Err         error
}

func (e *CertError) Error() string {
	return fmt.Sprintf("untrusted server certificate (%s, sha256 %s): %v", e.Subject, e.Fingerprint, e.Err)
}

// NormalizeFingerprint returns lowercase hex without separators ("" if invalid).
func NormalizeFingerprint(s string) string {
	s = strings.ToLower(strings.NewReplacer(":", "", " ", "", "-", "").Replace(strings.TrimSpace(s)))
	if b, err := hex.DecodeString(s); err != nil || len(b) != sha256.Size {
		return ""
	}
	return s
}

type Manager struct {
	Cfg       *config.Store
	TokenPath string

	mu    sync.Mutex
	cache map[string]cached
}

type cached struct {
	at   time.Time
	list []Entity
}

func (m *Manager) Token() string {
	b, err := os.ReadFile(m.TokenPath)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// SetToken stores the token ("" removes it).
func (m *Manager) SetToken(t string) error {
	t = strings.TrimSpace(t)
	m.flush()
	if t == "" {
		if err := os.Remove(m.TokenPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	if strings.ContainsAny(t, " \r\n\t") || len(t) > 4096 {
		return errors.New("that doesn't look like an access token")
	}
	tmp := m.TokenPath + ".tmp"
	if err := os.WriteFile(tmp, []byte(t+"\n"), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, m.TokenPath)
}

func (m *Manager) flush() {
	m.mu.Lock()
	m.cache = nil
	m.mu.Unlock()
}

// Configured reports whether a URL and a token are set.
func (m *Manager) Configured() bool { return m.Cfg.Get().HA.URL != "" && m.Token() != "" }

// ValidateURL checks a base URL against the HTTPS policy.
func ValidateURL(raw string, allowHTTP bool) (*url.URL, error) {
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(raw), "/"))
	if err != nil || u.Host == "" {
		return nil, errors.New("URL must look like https://homeassistant.local:8123")
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !allowHTTP {
			return nil, errors.New("plain http would send the token unencrypted; use https or allow http explicitly")
		}
	default:
		return nil, errors.New("URL must start with https:// (or http://)")
	}
	return u, nil
}

func (m *Manager) client() (*http.Client, *url.URL, string, error) {
	c := m.Cfg.Get().HA
	tok := m.Token()
	if c.URL == "" || tok == "" {
		return nil, nil, "", errors.New("Home Assistant API not configured (URL and token)")
	}
	u, err := ValidateURL(c.URL, c.AllowHTTP)
	if err != nil {
		return nil, nil, "", err
	}
	pin := NormalizeFingerprint(c.TLSFingerprint)
	host := u.Hostname()
	tc := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: true, // verification is done below (pin or system roots)
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) == 0 {
				return errors.New("no certificate")
			}
			sum := sha256.Sum256(raw[0])
			fp := hex.EncodeToString(sum[:])
			if pin != "" {
				if fp == pin {
					return nil
				}
				return &CertError{Fingerprint: fp, Subject: subject(raw[0]), Err: errors.New("does not match the pinned fingerprint")}
			}
			certs := make([]*x509.Certificate, 0, len(raw))
			for _, r := range raw {
				cert, err := x509.ParseCertificate(r)
				if err != nil {
					return err
				}
				certs = append(certs, cert)
			}
			inter := x509.NewCertPool()
			for _, c := range certs[1:] {
				inter.AddCert(c)
			}
			roots, _ := x509.SystemCertPool()
			if _, err := certs[0].Verify(x509.VerifyOptions{DNSName: host, Intermediates: inter, Roots: roots}); err != nil {
				return &CertError{Fingerprint: fp, Subject: subject(raw[0]), Err: err}
			}
			return nil
		},
	}
	hc := &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{TLSClientConfig: tc, Proxy: nil},
		// never follow redirects: they could carry the token elsewhere
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return hc, u, tok, nil
}

func subject(der []byte) string {
	if c, err := x509.ParseCertificate(der); err == nil {
		return c.Subject.String()
	}
	return "?"
}

func (m *Manager) get(ctx context.Context, path string, out any) error {
	hc, u, tok, err := m.client()
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String()+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := hc.Do(req)
	if err != nil {
		var ce *CertError
		if errors.As(err, &ce) {
			return ce
		}
		return err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return errors.New("Home Assistant rejected the token (401)")
	case resp.StatusCode >= 300:
		return fmt.Errorf("Home Assistant answered %s", resp.Status)
	}
	return json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 16<<20)).Decode(out)
}

// Test checks URL, certificate and token.
func (m *Manager) Test(ctx context.Context) error {
	var r struct {
		Message string `json:"message"`
	}
	return m.get(ctx, "/api/", &r)
}

// Entities lists entities of one domain ("weather"), sorted by name. Results
// are cached for 30 s.
func (m *Manager) Entities(ctx context.Context, domain string) ([]Entity, error) {
	m.mu.Lock()
	if c, ok := m.cache[domain]; ok && time.Since(c.at) < 30*time.Second {
		m.mu.Unlock()
		return c.list, nil
	}
	m.mu.Unlock()
	var states []struct {
		EntityID   string         `json:"entity_id"`
		State      string         `json:"state"`
		Attributes map[string]any `json:"attributes"`
	}
	if err := m.get(ctx, "/api/states", &states); err != nil {
		return nil, err
	}
	list := []Entity{}
	for _, s := range states {
		if domain != "" && !strings.HasPrefix(s.EntityID, domain+".") {
			continue
		}
		name, _ := s.Attributes["friendly_name"].(string)
		if name == "" {
			name = s.EntityID
		}
		list = append(list, Entity{ID: s.EntityID, Name: name, State: s.State})
	}
	sort.Slice(list, func(i, j int) bool { return strings.ToLower(list[i].Name) < strings.ToLower(list[j].Name) })
	m.mu.Lock()
	if m.cache == nil {
		m.cache = map[string]cached{}
	}
	m.cache[domain] = cached{time.Now(), list}
	m.mu.Unlock()
	return list, nil
}
