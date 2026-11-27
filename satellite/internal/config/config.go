// Package config holds clockd's user settings (/data/local/clockd/config.json).
// Missing fields keep their defaults; the file is rewritten with all fields so
// it doubles as documentation.
package config

import (
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Time sources.
const (
	TimeHomeAssistant = "homeassistant" // GetTimeRequest over the ESPHome API (default)
	TimeNTP           = "ntp"           // SNTP from NTPServer only
	TimeBoth          = "both"          // NTP when reachable, Home Assistant as well
	TimeNone          = "none"          // leave the system clock alone
)

type Config struct {
	// TimeSource selects where the clock gets its time. No source ever
	// contacts the internet unless the user points NTPServer at it.
	TimeSource string `json:"time_source"`
	// NTPServer is host or host:port of a user-chosen NTP server (for example the
	// LAN router or the Home Assistant host). Empty disables NTP.
	NTPServer string `json:"ntp_server"`
	// NTPIntervalMinutes is how often to poll NTPServer.
	NTPIntervalMinutes int `json:"ntp_interval_minutes"`
	// Timezone is an IANA name ("Europe/Helsinki"). Empty uses the one Home
	// Assistant reports (cached in state for offline boots).
	Timezone string `json:"timezone"`
	// WakeWords are the active model ids.
	WakeWords []string `json:"wake_words"`
	// WeatherEntity is a Home Assistant weather entity (e.g. "weather.home")
	// shown on the clock face. Empty = no weather.
	WeatherEntity string `json:"weather_entity"`
	// Display settings for the LED clock face.
	Display Display `json:"display"`
	// Voice satellite behaviour.
	Voice Voice `json:"voice"`
	// LEDs: the four status LEDs above the display.
	LEDs LEDs `json:"leds"`
	// Night is the daily night period.
	Night Night `json:"night"`
	// Chime: the hourly chime.
	Chime Chime `json:"chime"`
	// Sendspin player (Music Assistant multi-room audio).
	Sendspin Sendspin `json:"sendspin"`
	// HA is the optional Home Assistant REST API access (entity pickers). The
	// token itself is stored separately (ha_token, mode 0600).
	HA HAAPI `json:"ha_api"`
	// Bluetooth: the optional BLE proxy for Home Assistant (cmd/clockd/bluetooth.go).
	Bluetooth Bluetooth `json:"bluetooth"`
	// DLNA: the USB drive as a media server, the clock as a renderer
	// (cmd/clockd/dlna.go). Both off by default: UPnP has no authentication.
	DLNA DLNA `json:"dlna"`
	// Radio: the FM radio from an RTL-SDR stick (cmd/clockd/radio.go).
	Radio Radio `json:"radio"`
	// DND: do not disturb (cmd/clockd/dnd.go).
	DND DND `json:"dnd"`
	// UpdateCheck lets clockd ask the project's GitHub releases once a day
	// whether a newer version exists (cmd/clockd/onlineupdate.go). Off by
	// default: it is the only internet access clockd has.
	UpdateCheck bool `json:"update_check"`
	// UpdateInHA shows a newer release found by UpdateCheck in Home Assistant's
	// firmware update entity, which can then install it (cmd/clockd/firmware.go).
	// Off by default: HA's voice setup wizard installs any update it sees.
	UpdateInHA bool `json:"update_ha"`
}

// DLNA switches the two UPnP/DLNA devices.
type DLNA struct {
	Server   bool `json:"server"`
	Renderer bool `json:"renderer"`
}

// DND silences what the clock sounds on its own (the hourly chime, the wake
// sound, Home Assistant's announcements, the startup tune), never alarms or
// timers. Schedule: "none" (only by hand), "night" (while night mode is on)
// or "times" (Start to End, "HH:MM"; "" = none).
type DND struct {
	Schedule string `json:"schedule"`
	Start    string `json:"start"`
	End      string `json:"end"`
}

type Radio struct {
	// Region: "europe" (87.5-108 MHz, 50 us de-emphasis: most of the world),
	// "americas" (87.5-108 MHz, 75 us: also South Korea) or "japan" (76-95
	// MHz, 50 us). "" = europe.
	Region string `json:"region"`
	// Reception: "normal", "weak" (more tuner gain, for far-off stations) or
	// "strong" (less, next to a transmitter). "" = normal.
	Reception string `json:"reception"`
}

// Bluetooth is the BLE advertisement proxy. Default off: the radio shares its
// antenna with Wi-Fi, so it runs only when asked for.
type Bluetooth struct {
	// Proxy forwards nearby BLE advertisements to Home Assistant (passive
	// scanning while HA subscribes).
	Proxy bool `json:"proxy"`
}

type Voice struct {
	WakeSound bool `json:"wake_sound"` // chime after the wake word
	// WakeVote: with several clocks around, the one that heard the wake word
	// best answers (claims over UDP 6054 to the clocks found by mDNS)
	WakeVote bool `json:"wake_vote"`
	StopWord bool `json:"stop_word"` // "stop" ends speech, announcements, media and timers
	// MicGainDB is a digital gain on the microphone (after the vendor AEC),
	// -12..+24 dB. It affects the wake word, the stop word and what HA hears.
	MicGainDB float64 `json:"mic_gain_db"`
	// StopCutoff is the stop word's score threshold (0 = DefaultStopCutoff).
	// The model's own 0.5 missed most real attempts (they peaked at 0.35-0.46).
	StopCutoff float64 `json:"stop_cutoff"`
	// AlarmSound is the timer ringing pattern: "gentle" (default), "classic",
	// "urgent" or "chime".
	AlarmSound string `json:"alarm_sound"`
	// SnoozeMinutes is how long a snoozed alarm waits (0 = 10).
	SnoozeMinutes int `json:"snooze_minutes"`
	// WakeSensitivity: "slight" (the model's cutoff, default), "moderate", "very".
	WakeSensitivity string `json:"wake_sensitivity"`
	// WakeEasierPlaying: one sensitivity step easier while the clock plays sound
	// (music, announcements, a ringing timer): the echo canceller dims the voice then.
	// With music playing it also turns the music down for a few seconds when a
	// wake word score starts to rise (clockd: dipFloor), for the second try.
	WakeEasierPlaying bool `json:"wake_easier_playing"`
	// StartupSound is the tune played once after power-up ("" = none; ids in
	// cmd/clockd/startup.go). Default: our own rising chord.
	StartupSound string `json:"startup_sound"`
	// MicSource: "webrtc" (raw microphones + our build of the open WebRTC audio
	// processing; the default) or "vendor" (iot_audio_capture, as shipped; also
	// the fallback); cmd/clockd/micsource.go. "local" (LocalVQE, disabled in
	// v0.84.3) is read as "webrtc".
	MicSource string `json:"mic_source"`
	// MicNull: the two-mic echo null in front of the echo
	// canceller, while the clock plays (internal/localmic/null.go).
	MicNull bool `json:"mic_null"`

	// Shush: while music plays, the master volume drops the closer a hand
	// comes (the proximity sensor), and restores when it leaves.
	Shush bool `json:"shush"`
	// AttemptClips: save a short clip of what the microphone heard with
	// every wake word / stop word detection and near miss (Recent attempts
	// in the web UI gets a play button; the newest few dozen are kept in
	// <state>/clips).
	AttemptClips bool `json:"attempt_clips"`
}

const DefaultStopCutoff = 0.3

// StopThreshold is the effective stop word threshold.
func (v Voice) StopThreshold() float64 {
	if v.StopCutoff <= 0 {
		return DefaultStopCutoff
	}
	return v.StopCutoff
}

// LEDs colours are "classic" (blue/red/yellow/green), "classic_dim",
// "#rrggbb" (all four LEDs) or "" (off).
type LEDs struct {
	Brightness int    `json:"brightness"` // 0..100 % for all states (0 = LEDs off)
	Animate    bool   `json:"animate"`    // pulse while listening, run while thinking
	Listening  string `json:"listening"`
	Thinking   string `json:"thinking"`   // speech received, waiting for the answer
	Responding string `json:"responding"` // speaking the answer
	Muted      string `json:"muted"`
	Ringing    string `json:"ringing"`
	// SpeakNightLight: the night light pulses with the voice too while the
	// clock speaks (needs Animate).
	SpeakNightLight bool `json:"speak_night_light"`
}

// ValidColour reports whether s is a valid LEDs colour.
func ValidColour(s string) bool {
	switch s {
	case "", "classic", "classic_dim":
		return true
	}
	if len(s) != 7 || s[0] != '#' {
		return false
	}
	for _, c := range s[1:] {
		if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
			return false
		}
	}
	return true
}

type Night struct {
	Enabled bool   `json:"enabled"`
	Start   string `json:"start"` // "HH:MM" local time
	End     string `json:"end"`
	// Schedule: "times" (Start..End) or "sun" (sunset+SunsetOffset ..
	// sunrise+SunriseOffset from Home Assistant's sun.sun; Start..End are the
	// fallback while that is unknown).
	Schedule      string `json:"schedule"`
	SunsetOffset  int    `json:"sunset_offset"`  // minutes, -180..180
	SunriseOffset int    `json:"sunrise_offset"` // minutes, -180..180
	// Display: "normal", "dim" (DisplayBrightness, even above the day's), "off", or
	// "off_dark" (off while the room is dark). Buttons wake it for 10 s.
	Display           string `json:"display"`
	DisplayBrightness int    `json:"display_brightness"` // 1..16
	// DimFollow: "dim" follows the room's light like auto brightness, from the
	// display's BrightnessMin up to DisplayBrightness (the most it gets).
	DimFollow bool `json:"dim_follow"`
	// NightLight: "none", "on", or "dark" (on while the room is dark).
	NightLight      string `json:"night_light"`
	NightLightLevel int    `json:"night_light_level"` // 1..255
	FadeMinutes     int    `json:"fade_minutes"`      // night light and face fade at start/end, 0..60
	// DarkLevel: the room counts as dark below this ambient light % (1..50).
	DarkLevel int `json:"dark_level"`
	// ProximityWake shows a switched-off face when something comes close.
	ProximityWake bool `json:"proximity_wake"`
}

// Chime is the hourly chime (cmd/clockd/chime.go).
type Chime struct {
	// Sound: "" (off) or a chime id from cmd/clockd/chime.go.
	Sound string `json:"sound"`
	// Half also chimes on the half hour, more quietly (ship's bells always do).
	Half bool `json:"half"`
	// Volume is a percentage of the speaker volume, 10..100.
	Volume int `json:"volume"`
	// Quiet: "night" (silent while night mode is on), "times" (silent from
	// QuietStart to QuietEnd) or "none".
	Quiet      string `json:"quiet"`
	QuietStart string `json:"quiet_start"` // "HH:MM"
	QuietEnd   string `json:"quiet_end"`
	// NotWhilePlaying skips a chime while media (music, a sleep sound) plays.
	NotWhilePlaying bool `json:"not_while_playing"`
}

type Sendspin struct {
	Enabled        bool   `json:"enabled"`
	Name           string `json:"name"`             // shown in Music Assistant; empty = the clock's name
	DelayMs        int    `json:"delay_ms"`         // extra delay to line up with other speakers, 0..1000
	ShowNowPlaying bool   `json:"show_now_playing"` // scroll "artist - title" once when a track starts
}

type HAAPI struct {
	URL            string `json:"url"`             // e.g. https://homeassistant.lan:8123
	AllowHTTP      bool   `json:"allow_http"`      // permit plain http (token unencrypted)
	TLSFingerprint string `json:"tls_fingerprint"` // pinned SHA-256 of the server certificate
}

type Display struct {
	Enabled    bool `json:"enabled"`
	Brightness int  `json:"brightness"` // 1..16
	Clock24h   bool `json:"clock_24h"`
	ColonBlink bool `json:"colon_blink"`
	ScrollMs   int  `json:"scroll_ms"` // time per scroll step of text messages
	// AutoBrightness follows the light sensor between BrightnessMin (dark)
	// and Brightness (bright room).
	AutoBrightness bool `json:"auto_brightness"`
	BrightnessMin  int  `json:"brightness_min"` // 1..16
	// Eyes animates a pair of eyes on the big digits while the assistant
	// listens, thinks and speaks.
	Eyes bool `json:"eyes"`
	// StartupAnimation plays the original luna teaser once when clockd starts.
	StartupAnimation bool `json:"startup_animation"`
	// HALostHint shows that Home Assistant is unreachable: "off", "icon"
	// (warning icon), "icon_blink", "small" ("--" instead of the temperature)
	// or "message" (scrolls "no home assistant" once a minute).
	HALostHint string `json:"ha_lost_hint"`
}

func Defaults() Config {
	return Config{
		TimeSource:         TimeHomeAssistant,
		NTPIntervalMinutes: 60,
		WakeWords:          []string{"okay_nabu"},
		Display:            Display{Enabled: true, Brightness: 10, Clock24h: true, ColonBlink: true, AutoBrightness: true, ScrollMs: 250, BrightnessMin: 1, Eyes: false, StartupAnimation: true, HALostHint: "icon"},
		Voice:              Voice{WakeSound: true, WakeVote: true, StopWord: true, StopCutoff: DefaultStopCutoff, AlarmSound: "gentle", SnoozeMinutes: 10, WakeSensitivity: "slight", WakeEasierPlaying: true, StartupSound: "arpeggio", MicSource: "webrtc", AttemptClips: true, Shush: false},
		LEDs:               DefaultLEDs(),
		Sendspin:           Sendspin{Enabled: true, ShowNowPlaying: true},
		Chime:              Chime{Volume: 40, Quiet: "night", QuietStart: "22:00", QuietEnd: "08:00", NotWhilePlaying: true},
		Radio:              Radio{Region: "europe", Reception: "normal"},
		DND:                DND{Schedule: "none", Start: "22:00", End: "07:00"},
		Night: Night{Start: "22:00", End: "07:00", Schedule: "times", Display: "dim", DisplayBrightness: 1,
			NightLight: "dark", NightLightLevel: 40, FadeMinutes: 2, DarkLevel: 5},
	}
}

func DefaultLEDs() LEDs {
	// white (dim while answering) rather than the vendor's classic colours;
	// the night light follows the voice
	return LEDs{Brightness: 100, Animate: true, Listening: "#ffffff", Thinking: "#ffffff", Responding: "#333333", Muted: "", Ringing: "#ffffff", SpeakNightLight: true}
}

// Store loads and saves a Config file.
type Store struct {
	Path string
	mu   sync.Mutex
	cfg  Config
}

// Load reads the file, filling defaults; a missing file is created.
func Load(path string) (*Store, error) {
	s := &Store{Path: path, cfg: Defaults()}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return s, s.save()
	case err != nil:
		return nil, err
	}
	if err := json.Unmarshal(data, &s.cfg); err != nil {
		// a corrupt config (power cut mid-write) must not brick the clock:
		// keep the file for inspection, continue from defaults
		_ = os.Rename(path, path+".corrupt")
		slog.Warn("config was corrupt; starting from defaults", "saved_as", path+".corrupt")
		s.cfg = Defaults()
	}
	if s.cfg.Voice.MicSource == "local" {
		s.cfg.Voice.MicSource = "webrtc"
	}
	if s.cfg.Display.ScrollMs < 80 || s.cfg.Display.ScrollMs > 2000 {
		s.cfg.Display.ScrollMs = 250
	}
	if s.cfg.Display.Brightness < 1 || s.cfg.Display.Brightness > 16 {
		s.cfg.Display.Brightness = 10
	}
	if s.cfg.Display.BrightnessMin < 1 || s.cfg.Display.BrightnessMin > 16 {
		s.cfg.Display.BrightnessMin = 1
	}
	if s.cfg.LEDs.Brightness < 0 || s.cfg.LEDs.Brightness > 100 {
		s.cfg.LEDs.Brightness = 100
	}
	for _, c := range []*string{&s.cfg.LEDs.Listening, &s.cfg.LEDs.Thinking, &s.cfg.LEDs.Responding, &s.cfg.LEDs.Muted, &s.cfg.LEDs.Ringing} {
		*c = strings.Replace(*c, "google", "classic", 1) // v0.9.0 name
		if !ValidColour(*c) {
			*c = ""
		}
	}
	if s.cfg.Night.DarkLevel < 1 || s.cfg.Night.DarkLevel > 50 {
		s.cfg.Night.DarkLevel = 5
	}
	if s.cfg.NTPIntervalMinutes <= 0 {
		s.cfg.NTPIntervalMinutes = 60
	}
	switch s.cfg.TimeSource {
	case TimeHomeAssistant, TimeNTP, TimeBoth, TimeNone:
	default:
		s.cfg.TimeSource = TimeHomeAssistant
	}
	return s, s.save()
}

func (s *Store) Get() Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.cfg
	c.WakeWords = append([]string(nil), s.cfg.WakeWords...)
	return c
}

// Update applies fn and saves.
func (s *Store) Update(fn func(*Config)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.cfg)
	return s.save()
}

func (s *Store) save() error {
	data, err := json.MarshalIndent(s.cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o700); err != nil {
		return err
	}
	tmp := s.Path + ".tmp"
	if err := writeDurable(tmp, append(data, '\n')); err != nil {
		return err
	}
	return os.Rename(tmp, s.Path)
}

// writeDurable writes and fsyncs before the caller renames: without the fsync,
// UBIFS can commit the rename while the data is still in RAM, and a power cut
// leaves a zero-byte file (seen 2026-11-07 after several hard power-cycles).
func writeDurable(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// State is small machine-written data kept across restarts.
type State struct {
	Timezone string `json:"timezone"`  // last timezone Home Assistant reported
	MicMuted bool   `json:"mic_muted"` // software mic mute (survives restarts)
	// Volume is the speaker volume 0..1 (nil = never set: 0.5).
	Volume *float32 `json:"volume,omitempty"`
	// NightLight is the night light level 0..255 last set by hand or by the
	// schedule (the target, not fade steps).
	NightLight int `json:"night_light"`
	// NightLightDay is the level the night light had before the night
	// schedule took it over (NightLightHeld); the schedule gives it back at the
	// end of the night.
	NightLightDay  int  `json:"night_light_day,omitempty"`
	NightLightHeld bool `json:"night_light_held,omitempty"`
	// DeviceName / FriendlyName: chosen once (see clockd names.go).
	DeviceName   string `json:"device_name,omitempty"`
	FriendlyName string `json:"friendly_name,omitempty"`
	// SleepSound / SleepTimer: the last sleep sound and timer length chosen
	// (minutes, 0 = off; nil = never set). A sleep sound started from silence
	// starts the last timer with it.
	SleepSound string `json:"sleep_sound,omitempty"`
	SleepTimer *int   `json:"sleep_timer,omitempty"`
	// FM radio (an RTL-SDR stick on the USB socket): the stations the last
	// search found and the last frequency played, Hz
	RadioStations []RadioStation `json:"radio_stations,omitempty"`
	RadioLast     int            `json:"radio_last,omitempty"`
	// RadioFavs: the favourite stations (Hz, up to six, in the user's order);
	// when set, Home Assistant's FM radio dropdown lists these
	RadioFavs []int `json:"radio_favs,omitempty"`
	// the secret in the radio's stream link (/radio/<token>.wav)
	RadioStreamToken string `json:"radio_stream_token,omitempty"`
}

// RadioStation is one FM station a search found.
type RadioStation struct {
	Hz      int     `json:"hz"`
	PilotDB float64 `json:"pilot_db"`       // stereo pilot over the noise: how well it comes in
	Name    string  `json:"name,omitempty"` // RDS name, learned while it played
}

var stateMu sync.Mutex

// UpdateState loads, modifies and saves the state file atomically with respect
// to other UpdateState calls.
func UpdateState(path string, fn func(*State)) error {
	stateMu.Lock()
	defer stateMu.Unlock()
	st := LoadState(path)
	fn(&st)
	return SaveState(path, st)
}

func LoadState(path string) State {
	var st State
	if data, err := os.ReadFile(path); err == nil {
		json.Unmarshal(data, &st)
	}
	return st
}

func SaveState(path string, st State) error {
	data, _ := json.MarshalIndent(st, "", "  ")
	tmp := path + ".tmp"
	if err := writeDurable(tmp, append(data, '\n')); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
