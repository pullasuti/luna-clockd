package localmic

import "testing"

func benchNull(b *testing.B, active bool) {
	p := newNullProc()
	l, r, dst := make([]int16, 256), make([]int16, 256), make([]int16, 256)
	for i := range l {
		l[i], r[i] = int16(i*37%2000), int16(i*91%2000)
	}
	p.frames = 1000
	b.ResetTimer()
	for i := 0; i < b.N; i++ { // one iteration = 16 ms of audio
		p.process(l, r, active, active, dst)
	}
}

func BenchmarkNullIdle(b *testing.B)    { benchNull(b, false) }
func BenchmarkNullPlaying(b *testing.B) { benchNull(b, true) }
