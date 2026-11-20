package localmic

import (
	"context"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// The test binary doubles as a canceller helper: hop 160, output = microphone
// minus reference.
func TestMain(m *testing.M) {
	if os.Getenv("LOCALMIC_FAKE_HELPER") == "" {
		os.Exit(m.Run())
	}
	const hop = 160
	os.Stdout.WriteString("ok 160\n")
	in, out := make([]byte, 4*hop), make([]byte, 2*hop)
	for {
		if _, err := io.ReadFull(os.Stdin, in); err != nil {
			os.Exit(0)
		}
		for i := 0; i < hop; i++ {
			v := int16(uint16(in[2*i])|uint16(in[2*i+1])<<8) - int16(uint16(in[2*(hop+i)])|uint16(in[2*(hop+i)+1])<<8)
			out[2*i], out[2*i+1] = byte(v), byte(uint16(v)>>8)
		}
		os.Stdout.Write(out)
	}
}

func replayOnce(t *testing.T, m *Mic, l, r, ref []int16) (in, rf, out []int16) {
	t.Helper()
	err := m.Replay(context.Background(), l, r, ref, func(a, b, c []int16) {
		in, rf, out = append(in, a...), append(rf, b...), append(out, c...)
	})
	if err != nil {
		t.Fatal(err)
	}
	return
}

func TestReplay(t *testing.T) {
	t.Setenv("LOCALMIC_FAKE_HELPER", "1")
	rnd := rand.New(rand.NewSource(1))
	n := 3 * Rate
	l, r, ref := noise(n, rnd), noise(n, rnd), noise(n, rnd)
	for i := range ref {
		ref[i] /= 4
	}

	// no null: the helper gets the plain average and the reference as given
	m := &Mic{Helper: os.Args[0], Always: true}
	in, rf, out := replayOnce(t, m, l, r, ref)
	if len(out) != n/160*160 {
		t.Fatalf("got %d samples of %d", len(out), n)
	}
	for i := range out {
		avg := int16((int(l[i]) + int(r[i])) >> 1)
		if in[i] != avg || rf[i] != ref[i] || out[i] != avg-ref[i] {
			t.Fatalf("sample %d: in %d ref %d out %d, want %d %d %d", i, in[i], rf[i], out[i], avg, ref[i], avg-ref[i])
		}
	}

	// with the null: the reference comes NullDelay late, and two runs from
	// the same start are identical (learning included)
	path := filepath.Join(t.TempDir(), "null.bin")
	run := func() ([]int16, []int16) {
		os.Remove(path)
		m := &Mic{Helper: os.Args[0], Always: true, NullOn: func() bool { return true }, NullPath: path}
		_, rf, out := replayOnce(t, m, l, r, ref)
		return rf, out
	}
	rf1, out1 := run()
	_, out2 := run()
	if !slices.Equal(rf1[NullDelay:], ref[:len(rf1)-NullDelay]) {
		t.Fatal("the reference is not NullDelay late with the null on")
	}
	if !slices.Equal(out1, out2) {
		t.Fatal("two replays of the same input differ")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("the learned null was not saved:", err)
	}
}
