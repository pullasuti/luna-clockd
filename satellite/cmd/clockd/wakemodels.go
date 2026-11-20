package main

// Installing and removing wake word models from the web UI. The browser
// downloads a model (the clock itself never goes online for this) and uploads
// the manifest and the .tflite; clockd checks that the model loads and runs in
// the pure-Go engine before it is kept.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"

	esphome "github.com/ygelfand/go-esphome-device"

	"lenovo-satellite/internal/wake"
)

var modelID = regexp.MustCompile(`^[a-z0-9][a-z0-9_]{0,47}$`)

const maxModelBytes = 2 << 20 // the largest community models are ~140 KB

// models returns a copy of the installed wake word models.
func (d *daemon) models() []wake.Model {
	d.modelsMu.Lock()
	defer d.modelsMu.Unlock()
	return slices.Clone(d.allModels)
}

func (d *daemon) availableWakeWords() []esphome.WakeWord {
	var avail []esphome.WakeWord
	for _, m := range d.models() {
		avail = append(avail, esphome.WakeWord{ID: m.ID, Phrase: m.Phrase, TrainedLanguages: m.Languages})
	}
	return avail
}

// modelsChanged tells Home Assistant's side of the satellite about the new
// list. HA reads it when it (re)connects, so a running HA sees the change
// after its next reconnect.
func (d *daemon) modelsChanged() {
	if d.sat != nil {
		d.sat.AvailableWakeWords = d.availableWakeWords()
	}
}

func (d *daemon) activeWakeIDs() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var ids []string
	for _, det := range d.detectors {
		ids = append(ids, det.Model.ID)
	}
	return ids
}

// installModel checks and stores a model under id.
func (d *daemon) installModel(id string, manifestJSON, tflite []byte, source string) (wake.Model, error) {
	if !modelID.MatchString(id) {
		return wake.Model{}, fmt.Errorf("bad model name %q (a-z, 0-9 and _)", id)
	}
	if id == "stop" {
		return wake.Model{}, errors.New(`"stop" is the stop word's model; pick another name`)
	}
	if len(tflite) == 0 || len(tflite) > maxModelBytes {
		return wake.Model{}, fmt.Errorf("the model file is %d bytes; expected up to %d", len(tflite), maxModelBytes)
	}
	for _, m := range d.models() {
		if m.ID == id {
			return wake.Model{}, fmt.Errorf("%q is already installed", id)
		}
	}
	m, err := wake.ParseManifest(manifestJSON, id, d.modelsDir)
	if err != nil {
		return wake.Model{}, err
	}
	m.Source = source
	final := m.Path
	tmp := filepath.Join(d.modelsDir, "."+id+".tflite.new")
	if err := os.WriteFile(tmp, tflite, 0o600); err != nil {
		return wake.Model{}, err
	}
	m.Path = tmp
	if err := wake.Check(m); err != nil {
		os.Remove(tmp)
		return wake.Model{}, err
	}
	// a plain microWakeWord v2 manifest, whatever format came in
	man := map[string]any{
		"type": "micro", "version": 2, "wake_word": m.Phrase, "author": m.Author, "source": source,
		"model": id + ".tflite", "trained_languages": m.Languages,
		"micro": map[string]any{"probability_cutoff": m.Cutoff, "sliding_window_size": m.Window, "feature_step_size": m.StepMs},
	}
	b, _ := json.MarshalIndent(man, "", "  ")
	if err := os.Rename(tmp, final); err != nil {
		os.Remove(tmp)
		return wake.Model{}, err
	}
	if err := os.WriteFile(filepath.Join(d.modelsDir, id+".json"), append(b, '\n'), 0o600); err != nil {
		os.Remove(final)
		return wake.Model{}, err
	}
	m.Path = final
	d.modelsMu.Lock()
	d.allModels = append(d.allModels, m)
	d.modelsMu.Unlock()
	d.modelsChanged()
	d.log.Info("wake word installed", "id", id, "phrase", m.Phrase, "cutoff", m.Cutoff, "source", source)
	return m, nil
}

// deleteModel removes an inactive model.
func (d *daemon) deleteModel(id string) error {
	if slices.Contains(d.activeWakeIDs(), id) {
		return errors.New("this wake word is active; turn it off first")
	}
	d.modelsMu.Lock()
	i := slices.IndexFunc(d.allModels, func(m wake.Model) bool { return m.ID == id })
	if i < 0 {
		d.modelsMu.Unlock()
		return fmt.Errorf("no wake word %q", id)
	}
	m := d.allModels[i]
	d.allModels = slices.Delete(d.allModels, i, i+1)
	d.modelsMu.Unlock()
	os.Remove(filepath.Join(d.modelsDir, id+".json"))
	os.Remove(m.Path)
	d.modelsChanged()
	d.log.Info("wake word removed", "id", id)
	return nil
}
