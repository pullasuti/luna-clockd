// micbench: the offline test bench for the microphone path. It runs saved rig
// captures (tools/wwtest/run.py --record) through clockd's own chain
// (localmic.Replay: two-mic null, the helper, the gain) and the wake word
// models, with no clock and no sound in the room. The same input gives the
// same output every time, so a change to the chain is a change in the table.
//
//	micbench [flags] run.json... ["name=helper args"...]
//
// For every capture of every run it prints one line for what the clock
// produced live (the capture's output channel) and one per variant: the peak
// score of each clip's word, how many passed the thresholds, the highest score
// away from the clips (false alarms) and a hash of the output.
//
//	-stage apm    feed the canceller input as recorded (after the clock's
//	              null); tests the helper and what follows (default)
//	-stage full   feed the two raw microphones: the null runs here too
//	              (-null FILE: start from a learned null.bin, it is not
//	              changed; -warm: learn on the capture first; else from zero)
//	-stage raw    feed one raw microphone, no null (what a canceller gets
//	              without the two-mic null in front)
//
// -prime runs each capture through once before the scored pass, so the helper
// starts adapted to the room instead of from nothing (the live chain had been
// running for a while when the capture started).
//
// -starts N repeats each variant from N starting points (each drops another
// 63 ms from the front of the capture) and adds a line with the range: the
// echo canceller's result depends heavily on where it starts, and a variant is
// only better when it is better across them.
//
// -save FILE writes the scores, -base FILE compares with a saved run.
// What it can't replay: the state the live chain was in when the capture
// started, and anything the clock did to the room (the music dip is in the
// capture as it happened).
package main

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"

	"lenovo-satellite/internal/localmic"
	"lenovo-satellite/internal/wake"
)

const rate = localmic.Rate

type clip struct {
	Word       string
	Start, Dur float64
}

type runFile struct {
	Results []struct {
		Source  string
		Music   bool
		Offset  float64 `json:"offset_s"`
		Capture string
		TrackS  float64 `json:"capture_track_s"`
	}
}

// result is one variant on one capture.
type result struct {
	Scores    []float64          `json:"scores"`    // per clip: peak score of its word
	Elsewhere map[string]float64 `json:"elsewhere"` // per model: peak away from every clip
	OutDB     float64            `json:"out_db"`
	SHA       string             `json:"sha"`
}

func fatal(a ...any) {
	fmt.Fprintln(os.Stderr, append([]any{"micbench:"}, a...)...)
	os.Exit(1)
}

// readCapture returns the channels of a 5-channel 16-bit capture.
func readCapture(path string) (ch [5][]int16, err error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return ch, err
	}
	for i := 12; i+8 <= len(b); {
		id, n := string(b[i:i+4]), int(binary.LittleEndian.Uint32(b[i+4:]))
		if id == "fmt " && i+12 <= len(b) && binary.LittleEndian.Uint16(b[i+10:]) != 5 {
			return ch, fmt.Errorf("%s: not a 5-channel capture", path)
		}
		if id == "data" {
			d := b[i+8 : min(len(b), i+8+n)]
			fr := len(d) / 10
			for c := range ch {
				ch[c] = make([]int16, fr)
				for k := 0; k < fr; k++ {
					ch[c][k] = int16(binary.LittleEndian.Uint16(d[10*k+2*c:]))
				}
			}
			return ch, nil
		}
		i += 8 + n + n%2
	}
	return ch, fmt.Errorf("%s: no data chunk", path)
}

func writeWAV(path string, s []int16) error {
	b := make([]byte, 44+2*len(s))
	copy(b, "RIFF")
	binary.LittleEndian.PutUint32(b[4:], uint32(36+2*len(s)))
	copy(b[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(b[16:], 16)
	binary.LittleEndian.PutUint16(b[20:], 1)
	binary.LittleEndian.PutUint16(b[22:], 1)
	binary.LittleEndian.PutUint32(b[24:], rate)
	binary.LittleEndian.PutUint32(b[28:], rate*2)
	binary.LittleEndian.PutUint16(b[32:], 2)
	binary.LittleEndian.PutUint16(b[34:], 16)
	copy(b[36:], "data")
	binary.LittleEndian.PutUint32(b[40:], uint32(2*len(s)))
	for i, v := range s {
		binary.LittleEndian.PutUint16(b[44+2*i:], uint16(v))
	}
	return os.WriteFile(path, b, 0o644)
}

// scorer runs every model over a stream and keeps the peaks per clip.
type scorer struct {
	models []wake.Model
	dets   []*wake.Detector
	clips  []clip
	t0     float64
	n      int // samples so far
	res    result
	sq     float64
	hash   io.Writer
	sum    func() string
}

func newScorer(models []wake.Model, clips []clip, t0 float64) *scorer {
	s := &scorer{models: models, clips: clips, t0: t0}
	for _, m := range models {
		d, err := wake.New(m, m.Cutoff)
		if err != nil {
			fatal(err)
		}
		s.dets = append(s.dets, d)
	}
	s.res.Scores = make([]float64, len(clips))
	s.res.Elsewhere = map[string]float64{}
	h := sha256.New()
	s.hash, s.sum = h, func() string { return fmt.Sprintf("%x", h.Sum(nil))[:12] }
	return s
}

// feed takes the next block, in the sizes clockd gets them.
func (s *scorer) feed(blk []int16) {
	binary.Write(s.hash, binary.LittleEndian, blk)
	for _, v := range blk {
		s.sq += float64(v) * float64(v)
	}
	s.n += len(blk)
	t := float64(s.n)/rate - s.t0 // on the clip track
	for i, d := range s.dets {
		d.Process(blk)
		sc := d.Score()
		away := true
		for k, c := range s.clips {
			// a word scores up to ~1.5 s after it ends (the sliding window)
			if t >= c.Start-0.5 && t <= c.Start+c.Dur+1.5 {
				away = false
				if c.Word == s.models[i].ID {
					s.res.Scores[k] = max(s.res.Scores[k], sc)
				}
			}
		}
		if away {
			s.res.Elsewhere[s.models[i].ID] = max(s.res.Elsewhere[s.models[i].ID], sc)
		}
	}
}

func (s *scorer) done() result {
	s.res.OutDB = 10*math.Log10(s.sq/float64(max(1, s.n))+1e-9) - 90.3
	s.res.SHA = s.sum()
	return s.res
}

// spread sums up one variant's results from several starting points.
func spread(set []result, clips []clip) string {
	rng := func(word string, th float64) string {
		lo, hi, sum, of := math.MaxInt, 0, 0, 0
		for _, r := range set {
			n := 0
			of = 0
			for i, c := range clips {
				if c.Word == word {
					of++
					if r.Scores[i] >= th {
						n++
					}
				}
			}
			lo, hi, sum = min(lo, n), max(hi, n), sum+n
		}
		return fmt.Sprintf("%d-%d (mean %.1f) of %d", lo, hi, float64(sum)/float64(len(set)), of)
	}
	mean, n, else_ := 0.0, 0, 0.0
	for _, r := range set {
		for i, c := range clips {
			if c.Word == "okay_nabu" {
				mean, n = mean+r.Scores[i], n+1
			}
		}
		for _, v := range r.Elsewhere {
			else_ = max(else_, v)
		}
	}
	return fmt.Sprintf("wake >=.9 %s, >=.8 %s, >=.5 %s, mean peak %.2f | stop >=.5 %s | elsewhere %.2f",
		rng("okay_nabu", 0.9), rng("okay_nabu", 0.8), rng("okay_nabu", 0.5), mean/float64(max(1, n)), rng("stop", 0.5), else_)
}

func main() {
	modelsDir := flag.String("models", os.Getenv("MODELS"), "wake word models directory (env MODELS)")
	helper := flag.String("helper", os.Getenv("APMD"), "canceller helper built for this machine (env APMD)")
	model := flag.String("model", "", "the helper's model file (vqed)")
	stage := flag.String("stage", "apm", "apm: from the recorded canceller input; full: from the raw microphones (null here); raw: one raw microphone, no null")
	nullFile := flag.String("null", "", "full: start from this null.bin (not changed)")
	warm := flag.Bool("warm", false, "full: learn the null on the capture first")
	refDelay := flag.Int("refdelay", localmic.NullDelay, "full: samples the recorded reference is late (the null that was on in the capture)")
	post := flag.Float64("post", 0, "PostGainDB (webrtc 0, local 18)")
	agc := flag.Float64("agc", 0, "AGCMaxDB (webrtc 0, local 12)")
	prime := flag.Bool("prime", false, "run the capture through once before the scored pass (adapted helper)")
	starts := flag.Int("starts", 1, "repeat each variant from this many starting points and show the range")
	only := flag.String("only", "", "music or quiet: only those captures")
	wavDir := flag.String("wav", "", "write each variant's output here")
	save := flag.String("save", "", "write the scores to this file")
	base := flag.String("base", "", "compare with scores saved earlier")
	flag.Parse()
	if *modelsDir == "" || *helper == "" || flag.NArg() == 0 || (*stage != "apm" && *stage != "full" && *stage != "raw") {
		fmt.Fprintln(os.Stderr, "usage: micbench -models DIR -helper APMD [flags] run.json... [\"name=helper args\"...]")
		flag.PrintDefaults()
		os.Exit(2)
	}
	models, err := wake.LoadDir(*modelsDir)
	if err != nil {
		fatal(err)
	}
	type variant struct {
		name string
		args []string
	}
	var runs []string
	var variants []variant
	for _, a := range flag.Args() {
		if name, args, ok := strings.Cut(a, "="); ok && !strings.HasSuffix(a, ".json") {
			variants = append(variants, variant{name, strings.Fields(args)})
		} else {
			runs = append(runs, a)
		}
	}
	if len(variants) == 0 {
		variants = []variant{{"bench", nil}}
	}
	old := map[string]map[string]result{}
	if *base != "" {
		b, err := os.ReadFile(*base)
		if err == nil {
			err = json.Unmarshal(b, &old)
		}
		if err != nil {
			fatal(err)
		}
	}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	tmp, err := os.MkdirTemp("", "micbench")
	if err != nil {
		fatal(err)
	}
	defer os.RemoveAll(tmp)

	all := map[string]map[string]result{}
	changed := 0
	for _, run := range runs {
		dir := filepath.Dir(run)
		var rf runFile
		var track struct{ Clips []clip }
		for path, v := range map[string]any{run: &rf, filepath.Join(dir, "clips", "track.json"): &track} {
			b, err := os.ReadFile(path)
			if err == nil {
				err = json.Unmarshal(b, v)
			}
			if err != nil {
				fatal(path+":", err)
			}
		}
		// the clips' place in a capture: the track's start in it plus the
		// player's delay, which the quiet part of the run measured
		off, haveOff := 0.0, false
		for _, r := range rf.Results {
			if !r.Music && !haveOff {
				off, haveOff = r.Offset, true
			}
		}
		for _, r := range rf.Results {
			if r.Capture == "" || (*only == "music" && !r.Music) || (*only == "quiet" && r.Music) {
				continue
			}
			o := off
			if !haveOff {
				o = r.Offset
			}
			t0 := r.TrackS + o
			ch, err := readCapture(filepath.Join(dir, r.Capture))
			if err != nil {
				fatal(err)
			}
			fmt.Printf("== %s (%.0f s, %s), clips at %+.2f s, stage %s\n", r.Capture, float64(len(ch[0]))/rate,
				map[bool]string{true: "music", false: "quiet"}[r.Music], t0, *stage)
			all[r.Capture] = map[string]result{}
			report := func(name string, res result) {
				all[r.Capture][name] = res
				cnt := func(word string, th float64) (n, of int) {
					for i, c := range track.Clips {
						if c.Word == word {
							of++
							if res.Scores[i] >= th {
								n++
							}
						}
					}
					return
				}
				w9, wn := cnt("okay_nabu", 0.9)
				w8, _ := cnt("okay_nabu", 0.8)
				w5, _ := cnt("okay_nabu", 0.5)
				s5, sn := cnt("stop", 0.5)
				else_ := 0.0
				for _, v := range res.Elsewhere {
					else_ = max(else_, v)
				}
				var sc []string
				for _, v := range res.Scores {
					sc = append(sc, fmt.Sprintf("%.2f", v))
				}
				note := ""
				if o, ok := old[r.Capture][name]; ok {
					if o.SHA == res.SHA {
						note = "  = base"
					} else {
						d := 0.0
						for i := range res.Scores {
							if i < len(o.Scores) {
								d += res.Scores[i] - o.Scores[i]
							}
						}
						note = fmt.Sprintf("  CHANGED: scores %+.2f in sum", d)
						changed++
					}
				}
				fmt.Printf("   %-14s wake %d/%d >=.9  %d >=.8  %d >=.5 | stop %d/%d >=.5 | elsewhere %.2f | %6.1f dBFS | %s | %s%s\n",
					name, w9, wn, w8, w5, s5, sn, else_, res.OutDB, res.SHA, strings.Join(sc, " "), note)
			}

			live := newScorer(models, track.Clips, t0)
			for i := 0; i+160 <= len(ch[4]); i += 160 {
				live.feed(ch[4][i : i+160])
			}
			report("live", live.done())

			for _, v := range variants {
				m := &localmic.Mic{Helper: *helper, Model: *model, Args: v.args, Always: *model == "",
					PostGainDB: *post, AGCMaxDB: *agc, Log: quiet}
				left, right, ref := ch[0], ch[0], ch[3]
				if *stage != "apm" {
					left, right = ch[1], ch[2]
					d := min(max(*refDelay, 0), len(ref))
					ref = append(append([]int16{}, ref[d:]...), make([]int16, d)...)
				}
				if *stage == "raw" {
					right = left
				}
				if *stage == "full" {
					m.NullOn = func() bool { return true }
					m.NullPath = filepath.Join(tmp, "null.bin")
					os.Remove(m.NullPath)
					if *nullFile != "" {
						b, err := os.ReadFile(*nullFile)
						if err == nil {
							err = os.WriteFile(m.NullPath, b, 0o600)
						}
						if err != nil {
							fatal(err)
						}
					}
					if *warm {
						if err := m.Replay(context.Background(), left, right, ref, func(_, _, _ []int16) {}); err != nil {
							fatal(v.name+":", err)
						}
					}
				}
				var set []result
				for k := 0; k < max(1, *starts); k++ {
					name := v.name
					if k > 0 {
						name = fmt.Sprintf("%s#%d", v.name, k)
					}
					const startStep = 1009 // samples (63 ms; a prime, so block edges move too)
					cut := min(k*startStep, len(ref))
					left, right, ref := left[cut:], right[cut:], ref[cut:]
					sc := newScorer(models, track.Clips, t0-float64(cut)/rate)
					var out []int16
					skip := 0
					if *prime {
						skip = len(ref)
						left, right, ref = append(left[:skip:skip], left...), append(right[:skip:skip], right...), append(ref[:skip:skip], ref...)
					}
					err := m.Replay(context.Background(), left, right, ref, func(_, _, o []int16) {
						if skip > 0 {
							skip -= len(o)
							return
						}
						sc.feed(o)
						if *wavDir != "" {
							out = append(out, o...)
						}
					})
					if err != nil {
						fatal(name+":", err)
					}
					res := sc.done()
					set = append(set, res)
					report(name, res)
					if *wavDir != "" {
						p := filepath.Join(*wavDir, strings.TrimSuffix(filepath.Base(r.Capture), ".wav")+"-"+name+".wav")
						if err := writeWAV(p, out); err != nil {
							fatal(err)
						}
					}
				}
				if len(set) > 1 {
					fmt.Printf("   %-14s %s\n", v.name+" range", spread(set, track.Clips))
				}
			}
		}
	}
	if *save != "" {
		b, _ := json.MarshalIndent(all, "", " ")
		if err := os.WriteFile(*save, b, 0o644); err != nil {
			fatal(err)
		}
	}
	if *base != "" {
		fmt.Printf("%d result(s) differ from %s\n", changed, *base)
	}
}
