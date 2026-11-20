package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValidColour(t *testing.T) {
	for _, s := range []string{"", "classic", "classic_dim", "#00ff7F"} {
		if !ValidColour(s) {
			t.Errorf("%q rejected", s)
		}
	}
	for _, s := range []string{"red", "#fff", "#gg0000", "google "} {
		if ValidColour(s) {
			t.Errorf("%q accepted", s)
		}
	}
}

func TestOldConfigGetsNewDefaults(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(p, []byte(`{"time_source":"homeassistant","leds":{"brightness":40,"listening":"nope","responding":"google_dim"}}`), 0o600)
	s, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	c := s.Get()
	if !c.Voice.WakeSound || !c.Voice.StopWord {
		t.Errorf("voice defaults lost: %+v", c.Voice)
	}
	if c.LEDs.Brightness != 40 || c.LEDs.Listening != "" || c.LEDs.Ringing != "#ffffff" ||
		c.LEDs.Responding != "classic_dim" || c.LEDs.Thinking != "#ffffff" || !c.LEDs.Animate || !c.LEDs.SpeakNightLight {
		t.Errorf("leds %+v", c.LEDs)
	}
}

func TestUpdateStateKeepsFields(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state.json")
	UpdateState(p, func(s *State) { s.Timezone = "Europe/Helsinki" })
	UpdateState(p, func(s *State) { s.MicMuted = true })
	if st := LoadState(p); st.Timezone != "Europe/Helsinki" || !st.MicMuted {
		t.Fatalf("%+v", st)
	}
}

func TestStateVolumeAndNightLight(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state.json")
	if st := LoadState(p); st.Volume != nil || st.NightLight != 0 {
		t.Fatalf("empty state: %+v", st)
	}
	v := float32(0.35)
	if err := UpdateState(p, func(s *State) { s.Volume = &v; s.NightLight = 13 }); err != nil {
		t.Fatal(err)
	}
	if err := UpdateState(p, func(s *State) { s.MicMuted = true }); err != nil { // other fields keep their values
		t.Fatal(err)
	}
	st := LoadState(p)
	if st.Volume == nil || *st.Volume != 0.35 || st.NightLight != 13 || !st.MicMuted {
		t.Fatalf("got %+v", st)
	}
}
