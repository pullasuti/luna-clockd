package localmic

import (
	"math"
	"math/rand"
	"path/filepath"
	"testing"
)

func noise(n int, r *rand.Rand) []int16 {
	s := make([]int16, n)
	for i := range s {
		s[i] = int16(r.NormFloat64() * 3000)
	}
	return s
}

func rms(s []int16) float64 {
	var e float64
	for _, v := range s {
		e += float64(v) * float64(v)
	}
	return math.Sqrt(e / float64(len(s)))
}

// Not playing: the output is the plain average of the mics, exactly nullN late.
func TestNullPassThrough(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	p := newNullProc()
	l, rr := noise(16000, r), noise(16000, r)
	out := make([]int16, 0, 16000)
	for i := 0; i+160 <= len(l); i += 160 {
		dst := make([]int16, 160)
		p.process(l[i:i+160], rr[i:i+160], false, false, dst)
		out = append(out, dst...)
	}
	for i := nullN + nullHop; i < len(out); i++ { // the first hop has no overlapping frame before it
		want := (float64(l[i-nullN]) + float64(rr[i-nullN])) / 2
		if math.Abs(float64(out[i])-want) > 1.5 {
			t.Fatalf("sample %d: %d, want %.1f", i, out[i], want)
		}
	}
}

// Playing: the speaker reaches mic 1 as 0.7 x mic 2, one sample later. After
// learning, the null removes it; a "voice" with another relative transfer stays.
func TestNullRemovesSpeaker(t *testing.T) {
	r := rand.New(rand.NewSource(2))
	p := newNullProc()
	const n = 16000 * 6
	spk := noise(n+1, r)
	l, rr := make([]int16, n), make([]int16, n)
	for i := range l {
		l[i], rr[i] = int16(0.7*float64(spk[i])), spk[i+1]
	}
	out := make([]int16, 0, n)
	for i := 0; i+256 <= n; i += 256 {
		dst := make([]int16, 256)
		p.process(l[i:i+256], rr[i:i+256], true, true, dst)
		out = append(out, dst...)
	}
	tail := out[len(out)-16000:]
	red := 20 * math.Log10(rms(l[len(l)-16000:])/rms(tail))
	if red < 20 {
		t.Fatalf("speaker reduced by %.1f dB, want >= 20", red)
	}
	if st := p.Stats(); !st.Active || st.Reduction < 20 {
		t.Fatalf("stats %+v", st)
	}
	// a voice reaching both mics alike is mostly kept
	v := noise(16000, r)
	vo := make([]int16, 0, 16000)
	for i := 0; i+256 <= len(v); i += 256 {
		dst := make([]int16, 256)
		p.process(v[i:i+256], v[i:i+256], true, false, dst)
		vo = append(vo, dst...)
	}
	if loss := 20 * math.Log10(rms(v)/rms(vo[nullN:])); loss > 12 {
		t.Fatalf("voice lost %.1f dB", loss)
	}
	// saved and loaded spectra give the same null
	path := filepath.Join(t.TempDir(), "null.bin")
	if err := p.save(path); err != nil {
		t.Fatal(err)
	}
	q := newNullProc()
	if err := q.load(path); err != nil || q.frames != p.frames || q.sab != p.sab || q.sbb != p.sbb {
		t.Fatalf("load: %v", err)
	}
}
