package alarms

import "testing"

func TestRadioSound(t *testing.T) {
	for s, want := range map[string]int{"fm:91.2": 91_200_000, "fm:87.5": 87_500_000, "fm:107.90": 107_900_000, "fm:76": 76_000_000} {
		if hz, ok := RadioHz(s); !ok || hz != want {
			t.Errorf("%s: %d %v, want %d", s, hz, ok, want)
		}
	}
	for _, s := range []string{"", "gentle", "fm:", "fm:abc", "fm:120", "fm:50"} {
		if _, ok := RadioHz(s); ok {
			t.Errorf("%q taken as a station", s)
		}
	}
	l, err := List{{Time: "07:00", Sound: "fm:91.20"}}.Clean(map[string]bool{"gentle": true})
	if err != nil || l[0].Sound != "fm:91.2" {
		t.Errorf("clean: %v %v", l, err)
	}
	if _, err := (List{{Time: "07:00", Sound: "fm:200"}}).Clean(nil); err == nil {
		t.Error("fm:200 accepted")
	}
}

func TestCleanUSBSound(t *testing.T) {
	sounds := map[string]bool{"gentle": true}
	if l, err := (List{{Time: "07:00", Sound: "usb:/Music/Wake up.mp3"}}).Clean(sounds); err != nil || l[0].Sound != "usb:/Music/Wake up.mp3" {
		t.Errorf("valid usb sound: %v %v", l, err)
	}
	for _, bad := range []string{"usb:", "usb:Music/a.mp3", "usb:/a\x00.mp3"} {
		if _, err := (List{{Time: "07:00", Sound: bad}}).Clean(sounds); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
