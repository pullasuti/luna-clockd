package main

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lenovo-satellite/internal/wake"
)

func TestInstallDeleteModel(t *testing.T) {
	man, err1 := os.ReadFile("../../models/hey_jarvis.json")
	tfl, err2 := os.ReadFile("../../models/hey_jarvis.tflite")
	if err1 != nil || err2 != nil {
		t.Skip("no models in ../../models")
	}
	dir := t.TempDir()
	d := &daemon{log: slog.New(slog.DiscardHandler), modelsDir: dir}
	if err := os.WriteFile(filepath.Join(dir, "base.tflite"), tfl, 0o600); err != nil {
		t.Fatal(err)
	}
	d.allModels = []wake.Model{{ID: "base", Phrase: "Base", Cutoff: 0.97, Window: 5, StepMs: 10, Path: filepath.Join(dir, "base.tflite")}}
	if err := d.setActiveWakeWords([]string{"base"}); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct{ id, want string }{
		{"stop", "stop word"}, {"Bad Name", "bad model name"}, {"../x", "bad model name"}, {"base", "already installed"},
	} {
		if _, err := d.installModel(tc.id, man, tfl, ""); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("install %q: %v, want %q", tc.id, err, tc.want)
		}
	}
	if _, err := d.installModel("junk", man, []byte("not a model"), ""); err == nil {
		t.Error("a broken model file was accepted")
	}
	if _, err := d.installModel("junk", []byte(`{"wake_word":"x"}`), tfl, ""); err == nil {
		t.Error("a manifest without a model was accepted")
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "*junk*")); len(left) > 0 {
		t.Errorf("rejected model left files: %v", left)
	}

	m, err := d.installModel("hey_test", man, tfl, "https://example.org/hey_test.json")
	if err != nil {
		t.Fatal(err)
	}
	if m.Phrase != "Hey Jarvis" || len(d.models()) != 2 {
		t.Fatalf("installed %+v, models %d", m, len(d.models()))
	}
	// survives a restart: LoadDir finds the stored manifest
	got, err := wake.LoadDir(dir)
	if err != nil || len(got) != 1 || got[0].ID != "hey_test" || got[0].Source != "https://example.org/hey_test.json" {
		t.Fatalf("reload: %+v %v", got, err)
	}
	if err := d.setActiveWakeWords([]string{"hey_test"}); err != nil {
		t.Fatal(err)
	}
	if err := d.deleteModel("hey_test"); err == nil {
		t.Error("deleted an active wake word")
	}
	if err := d.setActiveWakeWords([]string{"base"}); err != nil {
		t.Fatal(err)
	}
	if err := d.deleteModel("hey_test"); err != nil {
		t.Fatal(err)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "hey_test*")); len(left) > 0 || len(d.models()) != 1 {
		t.Errorf("after delete: files %v, models %d", left, len(d.models()))
	}
}
