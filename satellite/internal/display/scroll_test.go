package display

import "testing"

func TestStripAndWindow(t *testing.T) {
	s := Strip("Hyvää  päivää!")
	// "hyvaa paivaa" (double space collapsed, "!" blank, trailing blank kept once)
	want := Strip("hyvaa paivaa ")
	if string(s) != string(want) {
		t.Fatalf("strip % x, want % x", s, want)
	}
	if w := Window(s, 0); w != [4]byte{} {
		t.Errorf("step 0 not blank: % x", w)
	}
	if w := Window(s, 4); w != [4]byte{s[0], s[1], s[2], s[3]} {
		t.Errorf("step 4 = % x", w)
	}
	if w := Window(s, 1); w != [4]byte{0, 0, 0, s[0]} {
		t.Errorf("step 1 = % x (text enters from the right)", w)
	}
	if w := Window(s, ScrollSteps(s)); w != [4]byte{} {
		t.Errorf("last step not blank: % x", w)
	}
}
