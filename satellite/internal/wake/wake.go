// Package wake wraps the pure-Go microWakeWord detector.
package wake

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"

	mww "github.com/zserge/microwakeword"
)

// Model is one microWakeWord v2 model with its manifest
// (the .json next to the .tflite in esphome/micro-wake-word-models).
type Model struct {
	ID        string
	Phrase    string
	Languages []string
	Cutoff    float64
	Window    int
	StepMs    int
	Path      string
	Author    string
	Source    string // where it was downloaded from (added in the web UI)
}

type manifest struct {
	WakeWord  string   `json:"wake_word"`
	Author    string   `json:"author"`
	Source    string   `json:"source"`
	Model     string   `json:"model"`
	Languages []string `json:"trained_languages"`
	Micro     struct {
		Cutoff float64 `json:"probability_cutoff"`
		Step   int     `json:"feature_step_size"`
		Window int     `json:"sliding_window_size"`
	} `json:"micro"`
}

// ParseManifest reads a microWakeWord v2 manifest (.json). id names the model;
// the .tflite is expected at dir/<id>.tflite (the manifest's own "model" file
// name is ignored, so a download can't point elsewhere).
func ParseManifest(data []byte, id, dir string) (Model, error) {
	var m manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return Model{}, fmt.Errorf("not a wake word manifest: %w", err)
	}
	// Tater "wake word bundles" carry the microWakeWord manifest inside
	var bundle struct {
		Micro *manifest `json:"micro_wake_word"`
	}
	if m.Model == "" || m.Micro.Cutoff == 0 {
		if json.Unmarshal(data, &bundle) == nil && bundle.Micro != nil {
			m = *bundle.Micro
		}
	}
	if m.Model == "" || m.Micro.Cutoff <= 0 || m.Micro.Cutoff > 1 {
		return Model{}, fmt.Errorf("not a microWakeWord model (no model file or probability_cutoff)")
	}
	return Model{
		ID:        id,
		Phrase:    phrase(m.WakeWord, id),
		Languages: m.Languages,
		Cutoff:    m.Micro.Cutoff,
		Window:    m.Micro.Window,
		StepMs:    m.Micro.Step,
		Path:      filepath.Join(dir, id+".tflite"),
		Author:    m.Author,
	}, nil
}

// phrase makes "hey_glados" read as "Hey Glados" (community manifests often
// carry the file name instead of the phrase).
func phrase(p, id string) string {
	if p == "" {
		p = id
	}
	if p != strings.ToLower(p) {
		return p // has its own capitals: "Okay Nabu"
	}
	w := strings.Fields(strings.ReplaceAll(p, "_", " "))
	for i, x := range w {
		if x != "" {
			w[i] = strings.ToUpper(x[:1]) + x[1:]
		}
	}
	return strings.Join(w, " ")
}

// Check loads the model and runs a second of silence and of noise through it,
// so a model the pure-Go engine can't run is refused before it is installed.
func Check(m Model) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("the model does not run here: %v", r)
		}
	}()
	d, err := New(m, 1)
	if err != nil {
		return fmt.Errorf("the model does not load: %w", err)
	}
	buf := make([]int16, 160)
	seed := uint32(1)
	for i := 0; i < 200; i++ {
		for j := range buf {
			if i >= 100 {
				seed = seed*1664525 + 1013904223
				buf[j] = int16(seed>>16) / 8
			}
		}
		d.Process(buf)
	}
	if s := d.Score(); s != s || s < 0 || s > 1 {
		return fmt.Errorf("the model gives nonsense scores (%v)", s)
	}
	return nil
}

// LoadDir reads every *.json manifest in dir.
func LoadDir(dir string) ([]Model, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	var out []Model
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		var m manifest
		if err := json.Unmarshal(data, &m); err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		if m.Model == "" || m.Micro.Cutoff == 0 {
			continue // not a micro model
		}
		out = append(out, Model{
			ID:        strings.TrimSuffix(filepath.Base(f), ".json"),
			Phrase:    phrase(m.WakeWord, strings.TrimSuffix(filepath.Base(f), ".json")),
			Languages: m.Languages,
			Cutoff:    m.Micro.Cutoff,
			Window:    m.Micro.Window,
			StepMs:    m.Micro.Step,
			Path:      filepath.Join(dir, m.Model),
			Author:    m.Author,
			Source:    m.Source,
		})
	}
	return out, nil
}

// Detector runs one model over a 16 kHz mono stream. It is safe for
// concurrent use: the mic goroutine calls Process while turn handling calls
// Reset (the underlying detector is not, and panics if reset mid-Process).
type Detector struct {
	Model Model
	mu    sync.Mutex
	d     *mww.Detector
	panic string // last caught interpreter panic with its stack (TakePanic)
}

// TakePanic returns (and clears) the last caught interpreter panic, "" if none.
func (d *Detector) TakePanic() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	p := d.panic
	d.panic = ""
	return p
}

// New creates a detector. cutoff overrides the manifest's probability cutoff when > 0.
func New(m Model, cutoff float64) (*Detector, error) {
	if cutoff <= 0 {
		cutoff = m.Cutoff
	}
	d, err := mww.NewDetector(mww.Config{
		ModelPath:         m.Path,
		ProbabilityCutoff: cutoff,
		SlidingWindowSize: m.Window,
		FeaturesStepMs:    m.StepMs,
	})
	if err != nil {
		return nil, err
	}
	return &Detector{Model: m, d: d}, nil
}

// Process feeds samples and reports whether the wake word fired.
//
// A panic inside the model interpreter (seen twice on the clock, "index out of
// range" in a convolution, not reproduced on the PC) is caught: the detector
// is reset and TakePanic reports it, so one bad inference can't stop clockd.
func (d *Detector) Process(samples []int16) (fired bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	defer func() {
		if e := recover(); e != nil {
			d.panic = fmt.Sprint(e) + "\n" + string(debug.Stack())
			d.d.Reset()
			fired = false
		}
	}()
	return d.d.ProcessAudio(samples)
}

// Reset clears the detector state (after a detection, or when audio was interrupted).
func (d *Detector) Reset() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.d.Reset()
}

// Score is the current sliding-window average probability (0-1).
func (d *Detector) Score() float64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.d.SlidingAverage()
}
