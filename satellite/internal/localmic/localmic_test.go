package localmic

import (
	"math"
	"testing"
)

func TestDecimator(t *testing.T) {
	var d decimator
	total := 0
	var last []float32
	for p := 0; p < 50; p++ { // 50 periods of 10 ms, a 1 kHz tone
		mix := make([]float32, 480*2)
		for i := 0; i < 480; i++ {
			v := float32(0.5 * math.Sin(2*math.Pi*1000*float64(p*480+i)/48000))
			mix[2*i], mix[2*i+1] = v, v
		}
		last = d.process(mix)
		total += len(last)
	}
	if want := 50 * 160; total < want-20 || total > want {
		t.Fatalf("got %d samples, want ~%d", total, want)
	}
	var peak float32
	for _, v := range last {
		peak = max(peak, v)
	}
	if peak < 0.45 || peak > 0.52 {
		t.Errorf("1 kHz passes at %.3f, want ~0.5", peak)
	}
	// 12 kHz must be filtered out (it would alias at 16 kHz)
	var d2 decimator
	var hi float32
	for p := 0; p < 10; p++ {
		mix := make([]float32, 480*2)
		for i := 0; i < 480; i++ {
			v := float32(0.5 * math.Sin(2*math.Pi*12000*float64(p*480+i)/48000))
			mix[2*i], mix[2*i+1] = v, v
		}
		for _, v := range d2.process(mix) {
			if p > 1 {
				hi = max(hi, float32(math.Abs(float64(v))))
			}
		}
	}
	if hi > 0.01 {
		t.Errorf("12 kHz leaks at %.3f", hi)
	}
}

func TestRefQueue(t *testing.T) {
	m := &Mic{}
	m.onMix(make([]float32, 480*2*20)) // 200 ms at once: trimmed
	m.mu.Lock()
	n := len(m.ref)
	m.mu.Unlock()
	if n > refMax {
		t.Fatalf("queue %d > %d", n, refMax)
	}
	dst := make([]int16, vqeHop)
	for i := 0; i < 20; i++ {
		m.takeRef(dst) // runs dry: zeros, no panic
	}
}

func TestAGC(t *testing.T) {
	a := newAGC(12)
	quiet := make([]int16, vqeHop)
	for i := range quiet {
		quiet[i] = int16(100 * math.Sin(float64(i)))
	}
	for i := 0; i < 400; i++ { // ~6.4 s: fully risen
		b := append([]int16(nil), quiet...)
		a.apply(b, 1)
	}
	if a.gainDB != 12 {
		t.Fatalf("gain %.1f dB after quiet input, want 12", a.gainDB)
	}
	loud := make([]int16, vqeHop)
	for i := range loud {
		loud[i] = int16(20000 * math.Sin(float64(i)))
	}
	a.apply(loud, 1)
	var peak int16
	for _, v := range loud {
		peak = max(peak, v)
	}
	if a.gainDB != 0 || peak < 19000 || peak == 32767 {
		t.Fatalf("loud block: gain %.1f dB, peak %d (want 0 dB, unclipped)", a.gainDB, peak)
	}
}
