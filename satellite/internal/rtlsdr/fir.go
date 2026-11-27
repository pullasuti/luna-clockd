package rtlsdr

import "math"

// LowpassFIR designs SetFIR taps: a Hamming-windowed sinc with cut-off cut
// (Hz) at the 28.8 MHz ADC rate, scaled to unity gain.
func LowpassFIR(cut float64) [16]int {
	var h [32]float64
	var sum float64
	for i := range h {
		x := float64(i) - 15.5
		h[i] = math.Sin(2*math.Pi*cut/XtalHz*x) / (math.Pi * x)
		h[i] *= 0.54 - 0.46*math.Cos(2*math.Pi*float64(i)/31)
		sum += h[i]
	}
	var t [16]int
	for i := range t {
		t[i] = int(math.Round(h[i] / sum * 4096))
	}
	return t
}
