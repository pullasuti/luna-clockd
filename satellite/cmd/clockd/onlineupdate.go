package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"lenovo-satellite/internal/config"
	"lenovo-satellite/internal/release"
	"lenovo-satellite/internal/web"
)

// The online update check (System > Update, config update_check, off by
// default) is the only part of clockd that contacts the internet, and only
// after the user switches it on: once a day, and on "Check now", it asks
// GitHub's API for the project's latest release. "Download" fetches that
// release's package into the place an uploaded one goes; from there the usual
// checks (signature, every file) and "Install and restart" take over. Nothing
// installs by itself. Home Assistant is told only with a second switch
// (update_ha, firmware.go): its voice setup wizard installs any update an update
// entity reports.

// projectURL is where releases come from (also the web UI's Source link).
const projectURL = "https://github.com/pullasuti/luna-clockd"

const (
	onlineEvery   = 24 * time.Hour
	onlineTimeout = 30 * time.Second // the API call
	onlineDLLimit = 5 * time.Minute  // the package download
	onlineMaxJSON = 1 << 20
)

// timeFloor: a clock before this has no real time yet (TLS would fail on it).
var timeFloor = web.TimeFloor

type onlineCheck struct {
	log    *slog.Logger
	cfg    *config.Store
	client *http.Client
	api    string // releases/latest URL (tests point it elsewhere)

	mu       sync.Mutex
	checked  time.Time
	latest   string // newest release's version
	asset    string // its package URL ("" = no package for this clock)
	size     int64
	summary  string // the release note's first paragraph (Home Assistant shows it)
	url      string // the release page
	err      string
	checking bool
}

// latestRelease is what the check learns about the newest release.
type latestRelease struct {
	version, asset, summary, url string
	size                         int64
}

func newOnlineCheck(log *slog.Logger, cfg *config.Store) *onlineCheck {
	return &onlineCheck{log: log, cfg: cfg, client: &http.Client{},
		api: "https://api.github.com/repos/" + strings.TrimPrefix(projectURL, "https://github.com/") + "/releases/latest"}
}

func (o *onlineCheck) enabled() bool { return o.cfg.Get().UpdateCheck }

// run checks once a day while the check is on (the first time a few minutes
// after it was switched on or clockd started, once the time is known).
func (o *onlineCheck) run(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	since := time.Now() // switched on (or started): wait a little before the first check
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if !o.enabled() {
			since = time.Now()
			continue
		}
		o.mu.Lock()
		due := time.Since(since) > 2*time.Minute && (o.checked.IsZero() || time.Since(o.checked) > onlineEvery)
		o.mu.Unlock()
		if due && !time.Now().Before(timeFloor) {
			o.check(ctx)
		}
	}
}

// check asks for the latest release and records the outcome.
func (o *onlineCheck) check(ctx context.Context) error {
	o.mu.Lock()
	if o.checking {
		o.mu.Unlock()
		return errors.New("a check is running")
	}
	o.checking = true
	o.mu.Unlock()
	r, err := o.fetch(ctx)
	o.mu.Lock()
	defer o.mu.Unlock()
	o.checking, o.checked = false, time.Now()
	if err != nil {
		o.err = err.Error()
		o.log.Warn("online update check failed", "err", err)
		return err
	}
	o.err, o.latest, o.asset, o.size, o.summary, o.url = "", r.version, r.asset, r.size, r.summary, r.url
	o.log.Info("online update check", "latest", r.version, "running", version, "package", r.asset != "")
	return nil
}

// newer reports the newest release when it is newer than this clockd and has
// a package for this clock.
func (o *onlineCheck) newer() (latestRelease, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	r := latestRelease{version: o.latest, asset: o.asset, summary: o.summary, url: o.url, size: o.size}
	return r, o.enabled() && o.asset != "" && release.CompareVersions(o.latest, version) > 0
}

func (o *onlineCheck) fetch(ctx context.Context) (latestRelease, error) {
	var none latestRelease
	if time.Now().Before(timeFloor) {
		return none, errors.New("the clock's time isn't set yet (it comes from Home Assistant)")
	}
	ctx, cancel := context.WithTimeout(ctx, onlineTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", o.api, nil)
	if err != nil {
		return none, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "clockd/"+version)
	resp, err := o.client.Do(req)
	if err != nil {
		return none, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return none, fmt.Errorf("GitHub answered %s", resp.Status)
	}
	var rel struct {
		Tag    string `json:"tag_name"`
		Body   string `json:"body"`
		URL    string `json:"html_url"`
		Assets []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
			Size int64  `json:"size"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, onlineMaxJSON)).Decode(&rel); err != nil {
		return none, fmt.Errorf("reading the release: %w", err)
	}
	v := strings.TrimPrefix(rel.Tag, "v")
	if _, err := release.ParseVersion(v); err != nil {
		return none, fmt.Errorf("the latest release has no version (%q)", rel.Tag)
	}
	r := latestRelease{version: v, summary: firstParagraph(rel.Body, 250)}
	if strings.HasPrefix(rel.URL, projectURL+"/") {
		r.url = rel.URL
	}
	for _, a := range rel.Assets {
		if a.Name == "clockd-"+v+"-arm64.tar.gz" && strings.HasPrefix(a.URL, "https://") {
			r.asset, r.size = a.URL, a.Size
			break
		}
	}
	return r, nil
}

// firstParagraph: the release note's first paragraph (the CHANGELOG's
// one-sentence summary), at most n bytes.
func firstParagraph(s string, n int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\r", ""))
	if i := strings.Index(s, "\n\n"); i >= 0 {
		s = s[:i]
	}
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		s = strings.ToValidUTF8(s[:n], "") + "…"
	}
	return s
}

// status is the System > Update page's view.
func (o *onlineCheck) status() map[string]any {
	o.mu.Lock()
	defer o.mu.Unlock()
	m := map[string]any{"enabled": o.enabled(), "checking": o.checking, "project": projectURL}
	if !o.checked.IsZero() {
		m["checked"] = o.checked.Format(time.RFC3339)
	}
	if o.err != "" {
		m["error"] = o.err
	}
	if o.latest != "" {
		m["latest"] = o.latest
		m["available"] = o.asset != "" && release.CompareVersions(o.latest, version) > 0
		m["size"] = o.size
	}
	return m
}

// download opens the latest release's package (the caller stores and checks it).
func (o *onlineCheck) download(ctx context.Context) (io.ReadCloser, string, error) {
	o.mu.Lock()
	asset, latest := o.asset, o.latest
	o.mu.Unlock()
	if !o.enabled() {
		return nil, "", errors.New("checking online is off")
	}
	if asset == "" {
		return nil, "", errors.New("no package to download: check first")
	}
	ctx, cancel := context.WithTimeout(ctx, onlineDLLimit)
	req, err := http.NewRequestWithContext(ctx, "GET", asset, nil)
	if err != nil {
		cancel()
		return nil, "", err
	}
	req.Header.Set("User-Agent", "clockd/"+version)
	resp, err := o.client.Do(req)
	if err != nil {
		cancel()
		return nil, "", err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		cancel()
		return nil, "", fmt.Errorf("download: %s", resp.Status)
	}
	o.log.Info("downloading an update", "version", latest, "bytes", resp.ContentLength)
	return &cancelBody{resp.Body, cancel}, latest, nil
}

type cancelBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelBody) Close() error { err := c.ReadCloser.Close(); c.cancel(); return err }

// ---- System > Update (web.Updater) ----

func (b *webBackend) UpdateCheckOnline() (map[string]any, error) {
	if !b.online.enabled() {
		return nil, errors.New("checking online is off")
	}
	if err := b.online.check(context.Background()); err != nil {
		return nil, err
	}
	return b.UpdateStatus(), nil
}

// UpdateDownload fetches the latest release's package and checks it like an
// upload: the page then shows what would change, and Install as usual.
func (b *webBackend) UpdateDownload() (map[string]any, error) {
	body, v, err := b.online.download(context.Background())
	if err != nil {
		return nil, err
	}
	defer body.Close()
	m, err := b.UpdateUpload(io.LimitReader(body, maxDownload))
	if err != nil {
		return nil, fmt.Errorf("clockd %s: %w", v, err)
	}
	return m, nil
}

// maxDownload bounds a downloaded package (a full one is ~9 MB).
const maxDownload = 64 << 20
