package release

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

func writeTree(t *testing.T, dir string, files map[string]string) {
	for p, c := range files {
		fp := filepath.Join(dir, filepath.FromSlash(p))
		os.MkdirAll(filepath.Dir(fp), 0o755)
		mode := os.FileMode(0o644)
		if p == "clockd" || strings.HasSuffix(p, "apmd") {
			mode = 0o755
		}
		if err := os.WriteFile(fp, []byte(c), mode); err != nil {
			t.Fatal(err)
		}
	}
}

func read(t *testing.T, p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func pkg(t *testing.T, files map[string]string, version string, key ed25519.PrivateKey) []byte {
	src := t.TempDir()
	writeTree(t, src, files)
	var b bytes.Buffer
	if _, err := Pack(&b, src, version, key, nil); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// retar rewrites a package entry by entry (simulates tampering).
func retar(t *testing.T, p []byte, edit func(name string, b []byte) []byte) []byte {
	gr, err := gzip.NewReader(bytes.NewReader(p))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gr)
	var out bytes.Buffer
	gw := gzip.NewWriter(&out)
	tw := tar.NewWriter(gw)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(tr)
		b = edit(h.Name, b)
		h.Size = int64(len(b))
		tw.WriteHeader(h)
		tw.Write(b)
	}
	tw.Close()
	gw.Close()
	return out.Bytes()
}

func TestAllowed(t *testing.T) {
	for p, want := range map[string]bool{
		"clockd": true, "start.sh": true, "aec/apmd": true, "models/okay_nabu.json": true,
		"config.json": false, "api.key": false, "../clockd": false, "/clockd": false,
		"models/../api.key": false, "aec/x/y": false, "system/cast_cli": false, "models/.hidden": false,
		"install.sh": false,
	} {
		if Allowed(p) != want {
			t.Errorf("Allowed(%q) = %v", p, !want)
		}
	}
}

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{{"0.10.0", "0.9.9", 1}, {"0.78.0", "0.78.0", 0}, {"0.77.1", "0.78.0", -1}, {"1.0.0", "0.99.99", 1}}
	for _, c := range cases {
		if got := CompareVersions(c.a, c.b); got != c.want {
			t.Errorf("Compare(%s, %s) = %d", c.a, c.b, got)
		}
	}
	for _, v := range []string{"0.78", "0.78.0-dev", "v0.78.0", "0.078.0"} {
		if _, err := ParseVersion(v); err == nil {
			t.Errorf("ParseVersion(%q) accepted", v)
		}
	}
}

func TestInstallUpdateAndCommit(t *testing.T) {
	pub, priv := newKey(t)
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"clockd": "old clockd", "start.sh": "same", "config.json": "{settings}",
		"models/a.json": "user's own a",
	})
	p := pkg(t, map[string]string{
		"clockd": "new clockd", "start.sh": "same", "aec/apmd": "new helper",
		"models/a.json": "shipped a", "models/b.json": "shipped b",
		"install.sh": "not shipped", "api.key": "never",
	}, "0.78.0", priv)

	var checked string
	pl, err := Check(bytes.NewReader(p), dir, []ed25519.PublicKey{pub}, "0.79.0")
	if err != nil || strings.Join(pl.Changed, ",") != "aec/apmd,clockd,models/b.json" || !pl.Downgrade || pl.Manifest.Version != "0.78.0" {
		t.Fatalf("Check = %+v, %v", pl, err)
	}
	if exists(filepath.Join(dir, "update/new")) || read(t, filepath.Join(dir, "clockd")) != "old clockd" {
		t.Fatal("Check wrote something")
	}
	res, err := Install(bytes.NewReader(p), dir, Options{Keys: []ed25519.PublicKey{pub}, Current: "0.77.1",
		CheckClockd: func(path string) (string, error) { checked = read(t, path); return "0.78.0", nil }})
	if err != nil {
		t.Fatal(err)
	}
	if checked != "new clockd" {
		t.Errorf("checked %q", checked)
	}
	if got := strings.Join(res.Changed, ","); got != "aec/apmd,clockd,models/b.json" {
		t.Errorf("changed = %s", got)
	}
	if res.Unchanged != 2 {
		t.Errorf("unchanged = %d", res.Unchanged)
	}
	for p, want := range map[string]string{
		"clockd": "new clockd", "aec/apmd": "new helper", "models/a.json": "user's own a",
		"models/b.json": "shipped b", "config.json": "{settings}", "update/prev/clockd": "old clockd",
		"update/added": "aec/apmd\nmodels/b.json\n", "update/trial": "0.77.1 0.78.0\n",
	} {
		if got := read(t, filepath.Join(dir, p)); got != want {
			t.Errorf("%s = %q, want %q", p, got, want)
		}
	}
	for _, p := range []string{"install.sh", "api.key", "update/new", "update/swapping", "update/prev/start.sh"} {
		if exists(filepath.Join(dir, p)) {
			t.Errorf("%s exists", p)
		}
	}
	if fi, _ := os.Stat(filepath.Join(dir, "clockd")); fi.Mode()&0o111 == 0 {
		t.Error("clockd not executable")
	}

	// a second install waits for the trial to end
	if _, err := Install(bytes.NewReader(p), dir, Options{Keys: []ed25519.PublicKey{pub}}); err == nil {
		t.Error("installed during a trial")
	}
	if got, err := Commit(dir); err != nil || got != "0.77.1 0.78.0" {
		t.Fatalf("Commit = %q, %v", got, err)
	}
	st := ReadStatus(dir)
	if st.Trial != "" || !strings.HasPrefix(st.Last, "0.77.1 0.78.0 ") {
		t.Errorf("status %+v", st)
	}
	if got, _ := Commit(dir); got != "" {
		t.Errorf("second Commit = %q", got)
	}
	// the same package again changes nothing
	if pl, err := Check(bytes.NewReader(p), dir, []ed25519.PublicKey{pub}, "0.78.0"); err != nil || len(pl.Changed) != 0 || pl.Downgrade {
		t.Errorf("Check after install: %+v %v", pl, err)
	}
	res, err = Install(bytes.NewReader(p), dir, Options{Keys: []ed25519.PublicKey{pub}, Current: "0.78.0"})
	if err != nil || len(res.Changed) != 0 || exists(filepath.Join(dir, "update/trial")) {
		t.Errorf("reinstall: %+v %v", res, err)
	}
	if exists(filepath.Join(dir, "update/prev")) {
		t.Error("the confirmed update's old files are still kept")
	}
}

func TestInstallRefusals(t *testing.T) {
	pub, priv := newKey(t)
	_, other := newKey(t)
	files := map[string]string{"clockd": "new clockd"}
	keys := []ed25519.PublicKey{pub}
	try := func(name string, p []byte, o Options) error {
		t.Helper()
		dir := t.TempDir()
		writeTree(t, dir, map[string]string{"clockd": "old clockd"})
		o.Keys = keys
		_, err := Install(bytes.NewReader(p), dir, o)
		if err == nil {
			t.Errorf("%s: installed", name)
		} else if got := read(t, filepath.Join(dir, "clockd")); got != "old clockd" || exists(filepath.Join(dir, "update/trial")) {
			t.Errorf("%s: changed the clock (%q)", name, got)
		}
		return err
	}

	try("unknown key", pkg(t, files, "0.78.0", other), Options{})
	err := try("downgrade", pkg(t, files, "0.77.0", priv), Options{Current: "0.77.1"})
	if !errors.Is(err, ErrDowngrade) {
		t.Errorf("downgrade: %v", err)
	}
	try("wrong version", pkg(t, files, "0.78.0", priv), Options{CheckClockd: func(string) (string, error) { return "0.77.9", nil }})
	try("clockd fails", pkg(t, files, "0.78.0", priv), Options{CheckClockd: func(string) (string, error) { return "", errors.New("exit 1") }})
	try("garbage", []byte("hello"), Options{})

	// one flipped byte in a file: the hash check catches it
	good := pkg(t, map[string]string{"clockd": strings.Repeat("x", 4000)}, "0.78.0", priv)
	try("tampered file", retar(t, good, func(name string, b []byte) []byte {
		if name == "files/clockd" {
			b[100] = 'y'
		}
		return b
	}), Options{})
	// a changed manifest: the signature check catches it
	try("tampered manifest", retar(t, good, func(name string, b []byte) []byte {
		if name == "manifest.json" {
			b = bytes.Replace(b, []byte("0.78.0"), []byte("0.79.0"), 1)
		}
		return b
	}), Options{})

	// an allowed downgrade goes through
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"clockd": "old"})
	if _, err := Install(bytes.NewReader(pkg(t, files, "0.70.0", priv)), dir, Options{Keys: keys, Current: "0.77.1", AllowDowngrade: true}); err != nil {
		t.Errorf("allowed downgrade: %v", err)
	}
}

func TestVerify(t *testing.T) {
	pub, priv := newKey(t)
	p := pkg(t, map[string]string{"clockd": "c", "aec/vqed": "v"}, "1.0.0", priv)
	m, err := Verify(bytes.NewReader(p), []ed25519.PublicKey{pub})
	if err != nil || m.Version != "1.0.0" || len(m.Files) != 2 {
		t.Fatalf("Verify = %+v, %v", m, err)
	}
}

func TestTrustedKeys(t *testing.T) {
	if len(Trusted()) != len(trustedKeys) || len(trustedKeys) == 0 {
		t.Errorf("keys.go: %d of %d keys parse", len(Trusted()), len(trustedKeys))
	}
}
