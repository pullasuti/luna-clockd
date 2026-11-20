package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"lenovo-satellite/internal/release"
)

// commitAfter: an update counts as good once clockd has run this long and
// passes its health check. start.sh rolls it back if clockd exits three times
// before that. A clockd that hangs instead of exiting: it gives up on itself
// after trialGiveUp (exits, so start.sh counts a try), and start.sh kills a
// clockd still on trial after 5 minutes (a whole-process freeze).
const (
	commitAfter = 60 * time.Second
	trialGiveUp = 4 * time.Minute
)

// installCLI is "clockd -install FILE": check a signed release package and
// install it into dir. It doesn't restart anything; restart.sh does.
func installCLI(file, dir string, allowDowngrade bool) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	res, err := release.Install(f, dir, release.Options{
		Keys: release.Trusted(), Current: version, AllowDowngrade: allowDowngrade,
		CheckClockd: runVersion,
	})
	if err != nil {
		return err
	}
	if len(res.Changed) == 0 {
		fmt.Printf("clockd %s: already installed, nothing changed\n", res.To)
		return nil
	}
	fmt.Printf("installed clockd %s -> %s (%s); restart clockd to switch: sh %s/restart.sh\n",
		res.From, res.To, strings.Join(res.Changed, ", "), dir)
	return nil
}

// runVersion runs a staged clockd with -version: it must start on this clock
// and say which version it is.
func runVersion(path string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "-version").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// watchUpdate logs how the last update went and confirms one on trial once
// clockd has run for commitAfter and healthy() passes. A trial still failing
// the check at trialGiveUp exits clockd (exit code 3) for start.sh to retry
// and, the third time, roll back.
func watchUpdate(ctx context.Context, log *slog.Logger, dir string, healthy func() error) {
	os.Remove(uploadPath(dir)) // an upload never installed before the last restart
	st := release.ReadStatus(dir)
	if st.Failed != "" {
		log.Warn("update rolled back by start.sh", "update", st.Failed)
	}
	if st.Trial == "" {
		return
	}
	log.Info("update on trial", "update", st.Trial, "confirm_in", commitAfter)
	start := time.Now()
	wait := commitAfter
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		err := healthy()
		if err == nil {
			break
		}
		if time.Since(start) >= trialGiveUp {
			log.Error("update failed its health check: exiting for start.sh to retry or roll back", "update", st.Trial, "err", err)
			os.Exit(3)
		}
		log.Warn("update not healthy yet", "err", err)
		wait = 15 * time.Second
	}
	if t, err := release.Commit(dir); err != nil {
		log.Warn("update confirm", "err", err)
	} else if t != "" {
		log.Info("update confirmed", "update", t)
	}
}

// ---- System > Update (web.Updater) ----

// uploadPath: where an uploaded package waits for confirmation (on /data:
// /tmp is RAM).
func uploadPath(dir string) string { return filepath.Join(dir, "update", "upload.tar.gz") }

func (b *webBackend) UpdateStatus() map[string]any {
	b.updMu.Lock()
	defer b.updMu.Unlock()
	return b.updateStatus()
}

func (b *webBackend) updateStatus() map[string]any {
	st := release.ReadStatus(b.dir)
	var keys []string
	for _, k := range release.Trusted() {
		keys = append(keys, release.Fingerprint(k))
	}
	m := map[string]any{"version": version, "trial": st.Trial, "failed": st.Failed, "last": st.Last,
		"go": runtime.Version(), "keys": keys}
	if st.Trial != "" {
		m["confirm_in"] = max(0, int((commitAfter - time.Since(b.started)).Seconds()))
	} else if prev, ok := release.PrevVersion(b.dir, version); ok {
		m["prev"] = prev // System > Update offers to go back
	}
	// what the update that installed this version brought (the file names
	// the version it is for: an update rolled back leaves it behind)
	if f := strings.Fields(st.Last); len(f) == 3 && f[1] == version {
		if raw, err := os.ReadFile(notesPath(b.dir)); err == nil {
			var n savedNotes
			if json.Unmarshal(raw, &n) == nil && n.For == version {
				m["notes"] = n.Notes
			}
		}
	}
	if p := b.upd; p != nil {
		pm := map[string]any{"version": p.Manifest.Version, "created": p.Manifest.Created,
			"changed": p.Changed, "downgrade": p.Downgrade}
		if !p.Downgrade {
			pm["notes"] = release.NotesSince(p.Manifest.Notes, version)
		}
		m["pending"] = pm
	}
	if b.online != nil {
		m["online"] = b.online.status()
	}
	return m
}

// notesPath keeps the installed update's release notes (newer than the
// version it replaced) for the page to show after the restart.
func notesPath(dir string) string { return filepath.Join(dir, "update", "notes.json") }

type savedNotes struct {
	For   string         `json:"for"`
	Notes []release.Note `json:"notes"`
}

// uploadReserve is the free space an upload leaves on /data.
const uploadReserve = 16 << 20

func (b *webBackend) UpdateUpload(r io.Reader) (map[string]any, error) {
	b.updMu.Lock()
	defer b.updMu.Unlock()
	b.upd = nil
	p := uploadPath(b.dir)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return nil, err
	}
	f, err := os.Create(p)
	if err != nil {
		return nil, err
	}
	// the package is written before it can be checked: never let it take the
	// last of the flash (config saves and logs need room too)
	room := int64(1) << 62
	if free, ferr := release.FreeBytes(filepath.Dir(p)); ferr == nil {
		room = free - uploadReserve
	}
	n, err := io.Copy(f, io.LimitReader(r, max(room, 0)+1))
	if err == nil && n > room {
		err = errors.New("not enough free space on the clock")
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(p)
		return nil, fmt.Errorf("upload: %w", err)
	}
	f, err = os.Open(p)
	if err != nil {
		return nil, err
	}
	plan, err := release.Check(f, b.dir, release.Trusted(), version)
	f.Close()
	if err != nil {
		os.Remove(p)
		b.d.log.Warn("update package refused", "err", err)
		return nil, err
	}
	b.upd = plan
	b.d.log.Info("update package checked", "version", plan.Manifest.Version, "changes", len(plan.Changed))
	return b.updateStatus(), nil
}

func (b *webBackend) UpdateInstall(allowDowngrade bool) (map[string]any, error) {
	b.updMu.Lock()
	defer b.updMu.Unlock()
	if b.upd == nil {
		return nil, errors.New("upload a package first")
	}
	f, err := os.Open(uploadPath(b.dir))
	if err != nil {
		b.upd = nil
		return nil, err
	}
	notes := release.NotesSince(b.upd.Manifest.Notes, version)
	res, err := release.Install(f, b.dir, release.Options{
		Keys: release.Trusted(), Current: version, AllowDowngrade: allowDowngrade,
		CheckClockd: runVersion, Log: b.d.log.Info,
	})
	f.Close()
	if errors.Is(err, release.ErrDowngrade) {
		return nil, err // keep the upload: the page asks again
	}
	os.Remove(uploadPath(b.dir))
	b.upd = nil
	if err != nil {
		b.d.log.Warn("update install failed", "err", err)
		return nil, err
	}
	if len(res.Changed) > 0 {
		os.Remove(notesPath(b.dir))
		if len(notes) > 0 {
			if raw, err := json.Marshal(savedNotes{res.To, notes}); err == nil {
				os.WriteFile(notesPath(b.dir), raw, 0o644)
			}
		}
	}
	m := b.updateStatus()
	m["changed"] = res.Changed
	m["restarting"] = len(res.Changed) > 0
	if len(res.Changed) > 0 {
		b.d.log.Info("restarting into the update", "to", res.To)
		go func() { time.Sleep(time.Second); b.restart() }()
	}
	return m, nil
}

func (b *webBackend) UpdateCancel() {
	b.updMu.Lock()
	defer b.updMu.Unlock()
	b.upd = nil
	os.Remove(uploadPath(b.dir))
}

func (b *webBackend) UpdateDismiss() error { return release.Dismiss(b.dir) }

func (b *webBackend) UpdateRevert() (map[string]any, error) {
	b.updMu.Lock()
	defer b.updMu.Unlock()
	to, err := release.Revert(b.dir, version, runVersion)
	if err != nil {
		b.d.log.Warn("going back failed", "err", err)
		return nil, err
	}
	os.Remove(notesPath(b.dir))
	b.d.log.Info("going back to the previous version", "to", to)
	m := b.updateStatus()
	m["restarting"], m["to"] = true, to
	go func() { time.Sleep(time.Second); b.restart() }()
	return m, nil
}
