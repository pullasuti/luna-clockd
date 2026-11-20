package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCastWrapperMatchesDevice(t *testing.T) {
	b, err := os.ReadFile("../../device/system/cast_cli")
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != castCLIWrapper {
		t.Error("castCLIWrapper differs from device/system/cast_cli: update one from the other")
	}
}

func TestFixCastWrapper(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "cast_cli")
	old := "#!/sbin/busybox sh\nif [ -f /tmp/clockd.active ]; then\n    exit 0\nfi\nexec /system/chrome/cast_cli.stock \"$@\"\n"
	os.WriteFile(p, []byte(old), 0o755)
	if err := fixCastWrapper(p); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); string(b) != castCLIWrapper {
		t.Errorf("old wrapper not replaced: %q", b)
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o755 {
		t.Errorf("mode %v", st.Mode())
	}
	stock := "\x7fELF..."
	os.WriteFile(p, []byte(stock), 0o755)
	if err := fixCastWrapper(p); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); string(b) != stock {
		t.Error("the stock binary was touched")
	}
	if fixCastWrapper(filepath.Join(dir, "missing")) == nil {
		t.Error("a missing cast_cli must stop the hand-back")
	}
}
