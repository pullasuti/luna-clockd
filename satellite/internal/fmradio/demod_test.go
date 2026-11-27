package fmradio

import (
	"math"
	"testing"
)

// fmIQ makes u8 I/Q of an FM station whose multiplex signal is mpx(t)
// (1 = 75 kHz deviation). The phase integrates mpx over each sample period
// (8 steps), as a real signal's does: taking mpx at the sample instants alone
// hid the discriminator's sinc droop (L-R on 38 kHz 4 % low, crosstalk -34 dB).
func fmIQ(secs float64, mpx func(t float64) float64) []byte {
	n := int(secs * InRate)
	iq := make([]byte, 2*n)
	var ph float64
	for i := 0; i < n; i++ {
		for k := 0; k < 8; k++ {
			t := (float64(i) - 1 + (float64(k)+0.5)/8) / InRate
			ph += 2 * math.Pi * maxDev * mpx(t) / InRate / 8
		}
		iq[2*i] = byte(127.5 + 100*math.Cos(ph))
		iq[2*i+1] = byte(127.5 + 100*math.Sin(ph))
	}
	return iq
}

// toneDB is the level of f in one channel (ch 0 = L, 1 = R) of interleaved stereo.
func toneDB(x []float32, ch int, f float64) float64 {
	var re, im float64
	n := len(x) / 2
	for i := 0; i < n; i++ {
		a := 2 * math.Pi * f * float64(i) / OutRate
		v := float64(x[2*i+ch])
		re += v * math.Cos(a)
		im += v * math.Sin(a)
	}
	return 20 * math.Log10(2*math.Hypot(re, im)/float64(n)+1e-12)
}

const w = 2 * math.Pi * pilotHz

// stereoMPX: the standard multiplex signal for L, R (each -1..1) plus pilot
// and an optional RDS signal (already on its carrier).
func stereoMPX(l, r, rds func(t float64) float64) func(t float64) float64 {
	return func(t float64) float64 {
		v := 0.45*(l(t)+r(t)) + 0.45*(l(t)-r(t))*math.Sin(2*w*t) + 0.09*math.Sin(w*t)
		if rds != nil {
			v += rds(t)
		}
		return v
	}
}

func silent(float64) float64 { return 0 }

// a tone on L only: L gets it, R at least 45 dB less (40 at 10 kHz), no pilot
// in the audio; a mono station (no pilot) plays the tone in both channels
func TestDemodStereo(t *testing.T) {
	d := New(50e-6)
	tone := func(t float64) float64 { return 0.5 * math.Sin(2*math.Pi*1000*t) }
	out := d.Process(fmIQ(1.5, stereoMPX(tone, silent, nil)), nil)
	if len(out) != 2*OutRate*3/2 {
		t.Fatalf("%d values for 1.5 s stereo, want %d", len(out), 2*OutRate*3/2)
	}
	if !d.Stereo() {
		t.Fatal("not in stereo with a clean pilot")
	}
	out = out[OutRate:] // PLL lock and blend (0.5 s)
	if l := toneDB(out, 0, 1000); l < -9 || l > -5 {
		t.Errorf("L %.1f dB, want about -7", l)
	}
	// separation once the blend has settled (its 0.2 s fade alone caps it at
	// ~41 dB one second in)
	sep := func(f float64, onL bool) float64 {
		tone := func(t float64) float64 { return 0.5 * math.Sin(2*math.Pi*f*t) }
		l, r := tone, silent
		if !onL {
			l, r = silent, tone
		}
		o := New(50e-6).Process(fmIQ(2.5, stereoMPX(l, r, nil)), nil)[2*OutRate*3/2:]
		a, b := toneDB(o, 0, f), toneDB(o, 1, f)
		if !onL {
			a, b = b, a
		}
		return a - b
	}
	if s := sep(1000, true); s < 50 {
		t.Errorf("1 kHz on L: %.1f dB of separation, want 50", s)
	}
	if s := sep(10000, false); s < 50 {
		t.Errorf("10 kHz on R: %.1f dB of separation, want 50", s)
	}
	for ch := 0; ch < 2; ch++ {
		if p := toneDB(out, ch, pilotHz); p > -50 {
			t.Errorf("pilot in channel %d at %.1f dB", ch, p)
		}
	}

	m := New(50e-6)
	out = m.Process(fmIQ(1, func(t float64) float64 { return tone(t) }), nil)[OutRate/2:]
	if m.Stereo() {
		t.Error("stereo without a pilot")
	}
	if l, r := toneDB(out, 0, 1000), toneDB(out, 1, 1000); math.Abs(l-r) > 0.5 || l < -8 || l > -5 {
		t.Errorf("mono: L %.1f R %.1f dB, want equal, about -6.5", l, r)
	}
}

// rdsBlock is one 26-bit block: data and its checkword with the offset word.
func rdsBlock(data uint32, off uint32) uint32 {
	reg := data << 10
	for bit := 25; bit >= 10; bit-- {
		if reg&(1<<bit) != 0 {
			reg ^= rdsPoly << (bit - 10)
		}
	}
	return data<<10 | (reg&0x3ff ^ off)
}

// the name and PI arrive from a station sending group 0A
func TestRDSName(t *testing.T) {
	const pi = 0x6201
	name := "TESTFM  "
	var bits []uint32
	for seg := uint32(0); seg < 4; seg++ {
		blocks := []uint32{
			rdsBlock(pi, offA),
			rdsBlock(0<<12|seg, offB),
			rdsBlock(0xE0CD, offC),
			rdsBlock(uint32(name[2*seg])<<8|uint32(name[2*seg+1]), offD),
		}
		for _, b := range blocks {
			for i := 25; i >= 0; i-- {
				bits = append(bits, b>>i&1)
			}
		}
	}
	// differential encoding, then biphase symbols on 57 kHz (16 pilot
	// cycles per bit, the clock locked to the pilot)
	enc := make([]float64, len(bits))
	prev := uint32(0)
	for i, b := range bits {
		prev ^= b
		enc[i] = float64(prev)*2 - 1
	}
	rds := func(t float64) float64 {
		cyc := w * t / (2 * math.Pi) // pilot cycles
		sym := enc[int(cyc/16)%len(enc)]
		if math.Mod(cyc, 16) >= 8 {
			sym = -sym
		}
		return 0.04 * sym * math.Sin(3*w*t)
	}
	d := New(50e-6)
	d.Process(fmIQ(3, stereoMPX(silent, silent, rds)), nil)
	got, gotPI := d.StationName()
	if got != "TESTFM" || gotPI != pi {
		t.Errorf("name %q PI %04x, want TESTFM 6201 (groups %d)", got, gotPI, d.rds.groups)
	}
}

// noise (no station): no pilot, no stereo, no name
func TestDemodNoise(t *testing.T) {
	d := New(50e-6)
	d.MeasurePilot()
	iq := make([]byte, 2*InRate)
	x := uint32(1)
	for i := range iq {
		x ^= x << 13
		x ^= x >> 17
		x ^= x << 5
		iq[i] = byte(x)
	}
	d.Process(iq, nil)
	// two single bins of noise differ by several dB either way; a station
	// counts from radioPilotOK (25 dB)
	if p := d.PilotDB(); p > 15 {
		t.Errorf("pilot %.1f dB in noise, want well below 25", p)
	}
	if n, _ := d.StationName(); d.Stereo() || n != "" {
		t.Errorf("stereo %v name %q in noise", d.Stereo(), n)
	}
}

func TestFastAtan2(t *testing.T) {
	var worst float64
	for a := -math.Pi + 0.001; a < math.Pi; a += 0.001 {
		y, x := math.Sin(a)*3, math.Cos(a)*3
		worst = max(worst, math.Abs(fastAtan2(y, x)-math.Atan2(y, x)))
	}
	if worst > 0.002 {
		t.Errorf("worst error %.4f rad", worst)
	}
}

// A cheap stick off by 10 kHz: the DC is removed (no offset in the audio,
// same tone level) and reported.
func TestDemodCarrierOffset(t *testing.T) {
	tone := func(off float64) func(float64) float64 {
		return func(tt float64) float64 { return off/maxDev + 0.3*math.Sin(2*math.Pi*1000*tt) }
	}
	ref := New(50e-6).Process(fmIQ(1.5, tone(0)), nil)
	d := New(50e-6)
	out := d.Process(fmIQ(1.5, tone(10_000)), nil)
	if off := d.OffsetHz(); math.Abs(off-10_000) > 300 {
		t.Errorf("offset %.0f Hz, want ~10000", off)
	}
	tail := out[len(out)/2:]
	var mean float64
	for _, v := range tail {
		mean += float64(v)
	}
	mean /= float64(len(tail))
	if math.Abs(mean) > 0.005 {
		t.Errorf("DC left in the audio: %.4f", mean)
	}
	a, b := toneDB(ref[len(ref)/2:], 0, 1000), toneDB(tail, 0, 1000)
	if math.Abs(a-b) > 0.3 {
		t.Errorf("1 kHz tone %.2f dB with offset, %.2f dB without", b, a)
	}
	// the start: the DC is gone within a few ms (no thump)
	if v := out[2*int(0.03*OutRate)]; math.Abs(float64(v)) > 0.35 {
		t.Errorf("sample at 30 ms = %.3f: DC still there", v)
	}
}
