// clockd: Home Assistant voice satellite for the Lenovo Smart Clock.
//
// It speaks the ESPHome native API (Home Assistant discovers it like an
// ESPHome device), detects wake words on the device with microWakeWord,
// streams the vendor echo-cancelled microphone (iot_audio_capture) to the
// Assist pipeline and plays responses, announcements, timers and media on
// the speaker. Nothing here talks to the internet; it only serves Home
// Assistant on the LAN and fetches media URLs Home Assistant hands it.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/pprof"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"

	esphome "github.com/ygelfand/go-esphome-device"
	"github.com/ygelfand/go-esphome-device/mdns"

	"lenovo-satellite/internal/config"
	"lenovo-satellite/internal/haapi"
	"lenovo-satellite/internal/output"
	"lenovo-satellite/internal/sendspin"
	"lenovo-satellite/internal/wake"
	"lenovo-satellite/internal/web"
	"lenovo-satellite/internal/wifi"
)

const version = "1.7.0"

type state int

const (
	stIdle       state = iota
	stListening        // streaming mic audio to Home Assistant
	stResponding       // waiting for / playing the response
)

func (s state) String() string {
	return [...]string{"idle", "listening", "responding"}[s]
}

// stopGrace: the stop word is ignored this long after a sound starts.
const stopGrace = 1200 * time.Millisecond

// Listening closer: while music plays, a wake word score that only starts to
// rise (dipFloor) turns the music down (dipGain) for dipHold, so the rest of
// the word, or the second try, reaches the microphones over far less echo.
// Rig, 2026-11-10 (music at 38 %): music alone never scored above 0.05 in
// ~350 s of captures, the missed words peaked at 0.1-0.77.
const (
	dipFloor = 0.15
	dipGain  = 0.12
	dipHold  = 4 * time.Second
)

type daemon struct {
	freqs     *freqStats // CPU speed shares for the web UI
	vol       float32    // the set master volume (0-1); shush ducks below it
	shushLvl  float32    // current shush multiplier (1 = not ducking)
	shushOn   bool       // a shush level is applied (restore once when it ends)
	restart   func()     // restart clockd (start.sh starts it again)
	peers     *peerList  // other clocks on the network (web UI picker)
	vote      *wakeVote  // which clock answers a wake word several heard
	radio     radioState // FM radio from an RTL-SDR stick
	esp       *esphome.Server
	ents      *esphome.Entities
	voting    atomic.Bool  // a vote is running: no new detections meanwhile
	wakeLvl   wakeLevel    // how clearly the wake word stood out (mic goroutine)
	testUntil atomic.Int64 // wake word test run until (unix ns): log hits, start no turns
	mic       *micSource
	cfg       *config.Store
	log       *slog.Logger
	out       *output.Output
	sat       *esphome.VoiceSatellite
	player    *esphome.MediaPlayer

	mu          sync.Mutex
	st          state
	turnStarted time.Time
	detectors   []*wake.Detector
	modelsMu    sync.Mutex
	allModels   []wake.Model // wake words (not the stop word); see models()
	modelsDir   string
	continueReq bool
	ttsVoice    *output.Voice
	alarm       *output.Voice
	alarmID     string          // the clock alarm ringing in alarm ("" = a timer)
	alarms      *alarmCtl       // the clock's own alarms
	alarmUI     *alarmUI        // setting them on the clock (alarmset.go)
	sleep       *sleepCtl       // sleep sounds + sleep timer (sleep.go)
	ssh         *sshCtl         // dropbear and its keys (ssh.go)
	dlna        *dlnaCtl        // DLNA media server and renderer (dlna.go)
	chimeSel    *esphome.Select // the "Hourly chime" dropdown (chime.go)
	media       *output.Voice
	mediaName   string     // what d.media is (URL or drive path), for the status page
	mediaSrc    string     // where d.media comes from: fm, usb, dlna, url (Home Assistant), sleep
	ssNow       string     // Music Assistant's "artist - title" (Sendspin metadata)
	usbQ        *usbQueue  // the USB drive folder being played through, if any
	usbPaused   *usbPaused // that folder paused, with where it stopped
	usbShuffle  bool       // player options, remembered across folders (d.mu)
	usbRepeat   repeatMode // d.mu
	auth        *web.Auth
	sendspin    *sendspin.Player
	bt          *bluetoothCtl
	micGain     micGain
	amb         *ambient
	night       *nightCtl
	dnd         *dndCtl
	fw          *firmware      // the Firmware update entity in Home Assistant
	playDown    time.Time      // action button press time (long press approves a web login)
	playUsed    bool           // that press was consumed by the long-press action
	haSeen      time.Time      // last time Home Assistant was connected (or clockd start)
	volStop     chan struct{}  // stops volume-button auto-repeat
	volSave     *time.Timer    // debounced save of the volume
	speaking    bool           // the response audio has started (thinking before that)
	stopDet     *wake.Detector // "stop" word, active while something is sounding
	soundSince  time.Time      // mic goroutine: when the current sound started (zero: silent)
	dipUntil    time.Time      // mic goroutine: music is turned down to listen closer until then
	nearMisses  map[*wake.Detector]*nearMiss
	stopNM      nearMiss      // stop word near misses (mic goroutine only)
	wakeHist    wakeHistory   // recent attempts for the web UI
	clips       *attemptClips // a short clip per attempt (mic goroutine + its writers)
	echo        echoMeter     // leftover-echo log line after each sound (mic goroutine)
	hwMuted     bool          // hardware mic switch
	softMuted   bool          // web UI / Home Assistant mute
	announcing  bool
	annCancel   bool
	statePath   string
	mediaBuf    *output.Buffer
	ttsPCM      []byte
	muted       bool
	face        *face
	setup       *setupMode
	micMu       sync.Mutex  // serializes onMic
	resetDets   atomic.Bool // endTurn: reset the detectors on the mic goroutine
	music       mediaLevel  // the music's level: turn, announcement, mute and dip ask here
	turn        uint64      // counts turns (d.mu): a finished run only ends its own turn
	annN        int         // announcements playing (announcing = annN > 0)
	nullHold    atomic.Bool // the music is turned down for a voice: the two-mic null must not learn
	// both volume buttons (setup hotspot combination)
	volUpDown, volDnDown, comboOn bool
	comboAt                       time.Time
	// alarm + volume down held (back to stock, stockreset.go)
	stockAlarm, stockVolDn bool
	stockAt                time.Time
	stateDir               string
	name                   string // device name ("lenovo-clock-1a2b")
	friendly               string
	wx                     *weather
	hw                     peripherals

	chimeWake, chimeError, chimeTick []float32
	// the wake chime of the current turn (d.mu) and, mic goroutine only, the
	// chime the gate is timing and when it was seen finished (chimeGate)
	turnChime    *output.Voice
	gateChime    *output.Voice
	gateChimeEnd time.Time
}

func main() {
	var (
		name      = flag.String("name", "", "device name (hostname Home Assistant sees; default: chosen once, see names.go)")
		friendly  = flag.String("friendly", "", "friendly name (default: chosen once)")
		stateDir  = flag.String("state", "/data/local/clockd", "state directory (key, models)")
		addr      = flag.String("addr", ":6053", "ESPHome API listen address")
		wakeIDs   = flag.String("wake", "", "active wake word model ids, comma separated (overrides config)")
		verbose   = flag.Bool("v", false, "debug logging")
		webAddr   = flag.String("web", ":80", "web UI listen address (empty = off)")
		netMgrCmd = flag.String("netmgr", "", "send one command to the vendor net_mgr, print the reply and exit (setup-ap.sh)")
		wpaCmd    = flag.String("wpa", "", "send one command to wpa_supplicant (e.g. STATUS), print the reply and exit")
		wpaWatch  = flag.Duration("wpa-watch", 0, "print wpa_supplicant's events for this long (after -wpa, if given) and exit")
		showVer   = flag.Bool("version", false, "print the version and exit")
		install   = flag.String("install", "", "install a signed release package (.tar.gz) and exit; restart.sh switches to it")
		downgrade = flag.Bool("allow-downgrade", false, "with -install: accept an older version")
		rescueRun = flag.Bool("rescue", false, "rescue mode (start.sh, after a crash loop): only the face and the back-to-stock hold")
	)
	flag.Parse()
	if *showVer {
		fmt.Println(version)
		return
	}
	if *rescueRun {
		os.Exit(rescue(*stateDir))
	}
	if *install != "" {
		if err := installCLI(*install, *stateDir, *downgrade); err != nil {
			fatal(err)
		}
		return
	}
	initZone(*stateDir) // before anything logs or reads the time
	if *wpaCmd != "" || *wpaWatch > 0 {
		os.Exit(wpaTool(*wpaCmd, *wpaWatch))
	}
	if *netMgrCmd != "" {
		code, text, err := netMgr(*netMgrCmd)
		fmt.Println(code, text)
		if err != nil {
			fmt.Fprintln(os.Stderr, "clockd -netmgr:", err)
			os.Exit(1)
		}
		return
	}

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	logRing := web.NewRing(2000)
	log := slog.New(slog.NewTextHandler(io.MultiWriter(os.Stderr, logRing), &slog.HandlerOptions{Level: level}))
	slog.SetDefault(log) // the log package (libraries) goes through it too: same format

	cfg, err := config.Load(filepath.Join(*stateDir, "config.json"))
	if err != nil {
		fatal(err)
	}
	if *wakeIDs != "" {
		cfg.Update(func(c *config.Config) { c.WakeWords = strings.Split(*wakeIDs, ",") })
	}
	tk := newTimeKeeper(log, cfg, filepath.Join(*stateDir, "state.json"))

	_, keyErr := os.Stat(filepath.Join(*stateDir, "api.key"))
	mac := macAddress()
	{
		st := config.LoadState(tk.statePath)
		n, f, changed := chooseNames(st, keyErr == nil, mac)
		if changed {
			if err := config.UpdateState(tk.statePath, func(s *config.State) { s.DeviceName, s.FriendlyName = n, f }); err != nil {
				log.Warn("save device name", "err", err)
			}
			log.Info("device name chosen", "name", n, "friendly", f)
		}
		if *name == "" {
			*name = n
		}
		if *friendly == "" {
			*friendly = f
		}
	}

	psk, err := loadOrCreateKey(filepath.Join(*stateDir, "api.key"), log)
	if err != nil {
		fatal(err)
	}
	models, err := wake.LoadDir(filepath.Join(*stateDir, "models"))
	if err != nil {
		fatal(err)
	}
	var stopModel *wake.Model
	for i := 0; i < len(models); i++ {
		if models[i].ID == "stop" {
			m := models[i]
			stopModel = &m
			models = append(models[:i], models[i+1:]...)
			i--
		}
	}
	if len(models) == 0 {
		fatal(fmt.Errorf("no wake word models in %s/models", *stateDir))
	}

	d := &daemon{
		log:        log,
		out:        output.New(log),
		allModels:  models,
		modelsDir:  filepath.Join(*stateDir, "models"),
		stateDir:   *stateDir,
		chimeWake:  output.Tone(-14, [2]float64{880, 70}, [2]float64{1320, 90}),
		chimeError: output.Tone(-14, [2]float64{440, 120}, [2]float64{330, 180}),
		chimeTick:  output.Tone(-20, [2]float64{1500, 25}),
	}
	d.music.apply = func(g float32) { d.out.SetGroupGain("media", g) }
	d.cfg = cfg
	d.statePath = tk.statePath
	d.clips = newAttemptClips(log, filepath.Join(*stateDir, "clips"))
	d.softMuted = config.LoadState(d.statePath).MicMuted
	d.muted = d.softMuted
	if err := d.setActiveWakeWords(cfg.Get().WakeWords); err != nil {
		fatal(err)
	}
	if stopModel != nil {
		// The detector never fires by itself (cutoff 1); onMic compares its
		// score with the configured threshold so it can change at runtime.
		if d.stopDet, err = wake.New(*stopModel, 1); err != nil {
			fatal(err)
		}
	} else {
		log.Warn("no stop word model (models/stop.json); stop word disabled")
	}

	srv := &esphome.Server{
		Addr: *addr,
		Info: esphome.Info{
			Name:         *name,
			FriendlyName: *friendly,
			MACAddress:   mac,
			Manufacturer: "Lenovo",
			Model:        "Smart Clock (clockd)",
			ProjectName:  "lenovo.smart_clock",
			Version:      version,
			// No FeatureSpeaker: that flag means "send me the answer as audio
			// over the API", and Home Assistant then forces the TTS output to
			// 16 kHz mono WAV whatever the media player lists. Without it HA
			// takes the media player's first announcement format (flac) for
			// voice answers, the way a Voice PE is advertised. onTTSAudio
			// stays for API audio but HA has no reason to send any now.
			VoiceFeatures: esphome.DefaultVoiceFeatures |
				esphome.FeatureTimers |
				esphome.FeatureAnnounce |
				esphome.FeatureStartConversation,
			// Advertisement-only BLE proxy (cmd/clockd/bluetooth.go): passive
			// scanning + raw advertisements. No connections/GATT.
			BluetoothFeatures: esphome.BluetoothPassiveScan | esphome.BluetoothRawAdvertisements,
		},
		PSK:    &psk,
		Logger: log,
	}

	d.player = &esphome.MediaPlayer{
		Base:          esphome.Base{ObjectID: "speaker", Name: "Speaker"},
		SupportsPause: false,
		Features: esphome.MediaPlayerFeaturePlayMedia | esphome.MediaPlayerFeatureBrowseMedia |
			esphome.MediaPlayerFeatureVolumeSet | esphome.MediaPlayerFeatureVolumeStep |
			esphome.MediaPlayerFeatureVolumeMute | esphome.MediaPlayerFeatureStop |
			esphome.MediaPlayerFeatureAnnounce,
		// Home Assistant transcodes to the FIRST listed default (and first
		// announcement) format with ffmpeg, so flac leads: same decode path,
		// a fraction of the WAV bytes on the LAN. WAV stays listed as the
		// fallback for anything that can't produce flac.
		SupportedFormats: []esphome.MediaFormat{
			{Format: "flac", SampleRate: 48000, Channels: 2, SampleBytes: 2},
			// The first announcement entry is also the format HA converts
			// voice answers to. For mono answers and announcements set
			// Channels: 1 here: the decoder plays mono on both channels and
			// the speaker sums L+R anyway. Tried in v0.93.3 and it works, but
			// the flac comes out the same size (two identical channels cost
			// almost nothing), so stereo stays.
			{Format: "flac", SampleRate: 48000, Channels: 2, SampleBytes: 2, Announcement: true},
			{Format: "wav", SampleRate: 48000, Channels: 2, SampleBytes: 2},
			{Format: "wav", SampleRate: 48000, Channels: 2, SampleBytes: 2, Announcement: true},
		},
		OnCommand: d.onMediaCommand,
	}
	vol := float32(0.5)
	d.vol = 0.5
	d.shushLvl = 1
	if v := config.LoadState(d.statePath).Volume; v != nil {
		vol = max(0, min(*v, 1))
	}
	d.vol = vol
	d.player.SetVolume(vol)
	d.out.SetVolume(vol)

	ents := esphome.NewEntities()
	haMgr := &haapi.Manager{Cfg: cfg, TokenPath: filepath.Join(*stateDir, "ha_token")}
	wx := &weather{log: log, cfg: cfg, ha: haMgr}
	d.wx = wx
	d.amb = &ambient{d: d}
	d.night = newNightCtl(d)
	d.dnd = newDNDCtl(d)
	sun := &sunInfo{log: log}
	d.night.sun = sun
	d.face = &face{log: log, cfg: cfg, tk: tk, wx: wx, amb: d.amb, nt: d.night, eyes: newEyes()}
	d.fw = newFirmware(log, cfg)
	d.alarms = newAlarmCtl(d, filepath.Join(*stateDir, "alarms.json"))
	d.alarmUI = newAlarmUI(d)
	d.sleep = newSleepCtl(d)
	d.ssh = &sshCtl{dir: *stateDir}
	// switched off in the web UI: also after a restart (start.sh may have started it)
	if !d.ssh.enabled() {
		if err := d.ssh.sync(); err != nil {
			log.Warn("ssh", "err", err)
		}
	}
	d.face.mood = d.mood
	d.face.haDown = d.haDown
	d.face.sleepLeft = d.sleep.left
	d.esp, d.ents = srv, ents
	if err := ents.Add(append([]esphome.Entity{d.player, wx.entitySelect(), d.amb.entity(), d.night.entity(), d.dnd.entity(), d.fw.ent}, append(append(d.buildPeripherals(), d.sleep.entities()...), d.chimeEntity())...)...); err != nil {
		fatal(err)
	}
	if len(config.LoadState(d.statePath).RadioStations) > 0 {
		d.radioAddEntity() // only on clocks that found stations (have had a stick)
	}
	if err := ents.AddActions(&esphome.Action{
		Name: "show_text",
		Args: []esphome.Arg{{Name: "text", Type: esphome.ArgString}, {Name: "repeat", Type: esphome.ArgInt}},
		Run: func(c esphome.Call) (any, error) {
			d.face.showText(c.String("text"), c.Int("repeat"))
			return nil, nil
		},
	}, &esphome.Action{
		Name: "clear_text",
		Run:  func(esphome.Call) (any, error) { d.face.clearText(); return nil, nil },
	}, &esphome.Action{
		// a colour on the status LEDs (washer done, door open...): color = a
		// name or #rrggbb, seconds (0 = 10), style = pulse, blink or solid
		Name: "led_notify",
		Args: []esphome.Arg{{Name: "color", Type: esphome.ArgString}, {Name: "seconds", Type: esphome.ArgInt}, {Name: "style", Type: esphome.ArgString}},
		Run: func(c esphome.Call) (any, error) {
			err := d.notifyLEDs(c.String("color"), c.Int("seconds"), c.String("style"))
			if err != nil { // HA doesn't show an action's error: the log does
				d.log.Warn("led_notify", "err", err)
			}
			return nil, err
		},
	}, &esphome.Action{
		Name: "led_clear",
		Run: func(esphome.Call) (any, error) {
			if d.clearLEDNotify() {
				d.log.Info("LED notification cleared")
			}
			return nil, nil
		},
	}); err != nil {
		fatal(err)
	}
	if err := ents.AddActions(d.alarmActions()...); err != nil {
		fatal(err)
	}
	d.sat = d.buildSatellite()
	d.bt = newBluetooth(d)
	srv.Handler = esphome.Chain(ents, d.sat, tk, wx, sun, d.bt.proxy)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		// Run only returns early if the mixer-service socket cannot be bound.
		if err := d.out.Run(ctx); err != nil && ctx.Err() == nil {
			log.Error("audio output failed; continuing without speaker", "err", err)
		}
	}()
	d.mic = &micSource{d: d, aecDir: filepath.Join(*stateDir, "aec")}
	micDone := make(chan struct{})
	go func() { d.mic.run(ctx); close(micDone) }()
	go d.watchdog(ctx)
	go tk.Run(ctx)
	d.applyBluetooth(cfg.Get()) // starts the proxy when the setting is on
	d.dlna = &dlnaCtl{d: d}
	d.night.tick() // night state before the face's first frame (no time flash at night)
	faceDone := make(chan struct{})
	go func() { d.face.run(ctx); close(faceDone) }()
	go d.runLEDs(ctx)
	go d.alarms.run(ctx)
	go d.amb.run(ctx)
	go d.night.run(ctx)
	go d.dnd.run(ctx)
	go d.startupTune(ctx)
	go d.chimeLoop(ctx)
	go d.runButtons(ctx)
	d.updateLEDs()
	defer func() { stop(); <-faceDone; <-micDone }() // blank the display, give iot_audio back
	go func() {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGUSR1)
		for range ch {
			pprof.Lookup("goroutine").WriteTo(os.Stderr, 2)
		}
	}()

	d.name, d.friendly = *name, *friendly
	d.dlna.apply(cfg.Get().DLNA) // needs the friendly name
	// Wi-Fi: wpa_supplicant's control socket; its events drive the DHCP renew
	// and the mDNS advertiser. Back to the vendor wpa_cli:
	// touch /data/local/clockd/use-wpa_cli
	wm := wifi.New()
	wm.Log = log
	if _, err := os.Stat(filepath.Join(*stateDir, "use-wpa_cli")); err == nil {
		wm.Exec = wifi.WpaCli // the old route, without events
	}
	dhcp := newDHCPClient()
	wm.WaitIP = dhcp.waitIP
	mdnsKick := make(chan bool, 1)
	go (&wifiWatch{log: log, wm: wm, dhcp: dhcp, mdns: mdnsKick}).run(ctx)
	d.setup = newSetupMode(d, wm, filepath.Join(*stateDir, "setup-ap.sh"), hotspotName(mac))
	go d.setup.run(ctx)

	if *webAddr != "" {
		d.restart = stop
		tk.restart = stop
		wb := &webBackend{d: d, tk: tk, started: time.Now(), restart: stop, dir: *stateDir,
			name: *name, friendly: *friendly, apiAddr: *addr, apiKey: psk.String()}
		// a drive plugged in while the socket is in drive mode: "usb" on the
		// face, then "err" when it has no filesystem the clock can read
		wb.lib = &musicLib{cache: filepath.Join(*stateDir, "library.json"), log: log}
		wb.usb = newUSBSock(*stateDir, log, func(mounted bool) {
			d.face.flash("usb", 2*time.Second)
			if !mounted {
				time.AfterFunc(2*time.Second, func() { d.face.flash("err", 2*time.Second) })
			}
		})
		go func() { // index a mounted drive's music without waiting for the page
			for {
				wb.lib.ensure(wb.usb.Drive())
				select {
				case <-ctx.Done():
					return
				case <-time.After(5 * time.Second):
				}
			}
		}()
		if !adbOn() { // a locked clock has no adb: the socket starts on the USB drive
			go func() {
				if err := wb.usb.SetHost(true); err != nil {
					log.Warn("usb: drive mode at start", "err", err)
				}
			}()
		}
		wb.online = newOnlineCheck(log, cfg)
		go wb.online.run(ctx)
		d.fw.wb.Store(wb)
		go d.fw.run(ctx)
		d.auth = &web.Auth{Path: filepath.Join(*stateDir, "sessions.json"), PassPath: filepath.Join(*stateDir, "password.json"),
			ShowCode: d.face.setCode}
		d.auth.OnPendingChange = func() {
			if p, ok := d.auth.PendingInfo(); ok {
				d.hw.loginReq.Set(fmt.Sprintf("code %s: %s (%s)", p.Code, p.Browser, p.IP))
			} else {
				d.hw.loginReq.Set("none")
			}
		}
		d.auth.OnApprove = func(method string) {
			if method != "button" { // the button path already confirms
				d.out.Play(output.NewSamples(d.chimeWake, 0), 1, "alert")
				d.face.flash("good", 1500*time.Millisecond)
			}
		}
		ws := &web.Server{Addr: *webAddr, Backend: wb, Logs: logRing, Log: log, Auth: d.auth, Wifi: wm, Setup: d.setup, SetupConfirm: d.setup.confirm,
			HA: haMgr}
		go func() {
			if err := ws.Run(); err != nil {
				log.Error("web UI stopped", "err", err)
			}
		}()
	}

	var extra []extraService
	if sc := cfg.Get().Sendspin; sc.Enabled {
		ssName := sc.Name
		if ssName == "" {
			ssName = *friendly
		}
		d.sendspin = &sendspin.Player{
			Addr: ":8928", Name: ssName, Version: version, Out: d.out, Log: log,
			ClientID:    "clockd-" + strings.ReplaceAll(strings.ToLower(mac), ":", ""),
			StaticDelay: time.Duration(max(0, min(sc.DelayMs, 1000))) * time.Millisecond,
			OnMetadata: func(np string) {
				d.mu.Lock()
				d.ssNow = np
				d.mu.Unlock()
				if np != "" && d.cfg.Get().Sendspin.ShowNowPlaying {
					d.face.showText(np, 1)
				}
			},
		}
		go func() {
			if err := d.sendspin.Run(ctx); err != nil {
				log.Error("sendspin player stopped", "err", err)
			}
		}()
		extra = append(extra, extraService{Instance: *name, Service: sendspin.Service, Port: 8928,
			TXT: []string{"path=" + sendspin.Path, "name=" + ssName}})
	}
	d.freqs = newFreqStats()
	nw := newNetWatch(d)
	go nw.run(ctx.Done())
	d.peers = newPeers(*name)
	d.vote = newWakeVote(*name, log, d.peers.peerIPs)
	if err := d.vote.listen(); err != nil {
		log.Warn("wake vote: cannot listen, this clock answers every wake word it hears", "err", err)
	}
	// Wi-Fi: signal strength and network name to HA (every 30 s, or on change)
	go func() {
		lastN, lastR, at := "", -999, time.Time{}
		for t := time.NewTicker(30 * time.Second); ; {
			<-t.C
			if r, ok := wifiSignal(); ok && (r < lastR-2 || r > lastR+2 || time.Since(at) >= 5*time.Minute) && d.hw.wifiRSSI != nil {
				d.hw.wifiRSSI.Set(float32(r))
				lastR, at = r, time.Now()
			}
			if n := wm.Status().SSID; n != "" && n != lastN && d.hw.wifiNet != nil {
				d.hw.wifiNet.Set(n)
				lastN = n
			}
		}
	}()
	// Proximity (raw, uncalibrated) once a second: idles ~5, a hand ~80-100, covered 300+
	if d.amb != nil {
		go func() {
			last := float32(-1)
			for t := time.NewTicker(time.Second); ; {
				<-t.C
				p, ok := d.amb.Proximity()
				if ok && d.hw.proximity != nil && (p < last-3 || p > last+3) {
					d.hw.proximity.Set(p)
					last = p
				}
			}
		}()
	}
	// SoC temperature: reported to HA at most every 5 minutes, and only on a 1 C
	// change, so the value's wobble doesn't flood Home Assistant's recorder
	go func() {
		last, at := float32(-99), time.Time{}
		for t := time.NewTicker(10 * time.Second); ; {
			<-t.C
			if _, err := os.Stat("/sys/class/thermal/thermal_zone0/temp"); err != nil {
				continue
			}
			b, err := os.ReadFile("/sys/class/thermal/thermal_zone0/temp")
			if err != nil {
				continue
			}
			v, err := strconv.ParseFloat(strings.TrimSpace(string(b)), 64)
			if err != nil {
				continue
			}
			c := float32(v) / 1000
			if d.hw.socTemp == nil {
				continue
			}
			if time.Since(at) >= 5*time.Minute || c < last-1 || c > last+1 {
				d.hw.socTemp.Set(c)
				last, at = c, time.Now()
			}
		}
	}()
	if *webAddr != "" {
		port := 80
		if _, p, err := net.SplitHostPort(*webAddr); err == nil {
			if n, err := strconv.Atoi(p); err == nil {
				port = n
			}
		}
		extra = append(extra, extraService{Instance: *friendly, Service: peerService, Port: port,
			TXT: peerTXT(*name, *friendly)})
		go d.peers.run(ctx, log)
	}
	go runMDNS(ctx, log, mdnsKick, mdns.Config{
		Name:         *name,
		FriendlyName: *friendly,
		Port:         6053,
		MACAddress:   mac,
		Version:      version,
		Platform:     "host",
		Network:      "wifi",
		Encrypted:    true,
	}, extra...)

	c := cfg.Get()
	log.Info("clockd running", "version", version, "name", *name, "mac", mac,
		"wake", c.WakeWords, "models", len(models), "time_source", c.TimeSource,
		"ntp_server", c.NTPServer, "local_time", tk.Now().Format("2006-01-02 15:04:05 MST"))
	go watchUpdate(ctx, log, *stateDir, func() error { return d.healthy(*webAddr) })
	if err := srv.ListenAndServe(ctx); err != nil && ctx.Err() == nil {
		fatal(err)
	}
}

// ---- wake words and turns ----

func (d *daemon) setActiveWakeWords(ids []string) error {
	// every saved setting comes through here: keep the running detectors when
	// the wake words didn't change (rebuilding them while the mic thread runs
	// them coincided with both interpreter crashes seen on the clock)
	d.mu.Lock()
	var cur []string
	for _, det := range d.detectors {
		cur = append(cur, det.Model.ID)
	}
	d.mu.Unlock()
	if len(cur) > 0 && slices.Equal(cur, trimmed(ids)) {
		return nil
	}
	var dets []*wake.Detector
	for _, id := range ids {
		id = strings.TrimSpace(id)
		for _, m := range d.models() {
			if m.ID == id {
				// never fires by itself: onMic compares Score() with the
				// sensitivity setting (wakeThreshold), so it applies at once
				det, err := wake.New(m, 1)
				if err != nil {
					return err
				}
				dets = append(dets, det)
			}
		}
	}
	if len(dets) == 0 {
		return fmt.Errorf("none of the wake words %v are installed", ids)
	}
	nms := map[*wake.Detector]*nearMiss{}
	for _, det := range dets {
		nms[det] = &nearMiss{}
	}
	d.mu.Lock()
	d.detectors, d.nearMisses = dets, nms
	d.mu.Unlock()
	return nil
}

func trimmed(ids []string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, strings.TrimSpace(id))
	}
	return out
}

// reportPanics logs interpreter panics a detector caught (wake.Detector.Process).
func (d *daemon) reportPanics(dets ...*wake.Detector) {
	for _, det := range dets {
		if det == nil {
			continue
		}
		if p := det.TakePanic(); p != "" {
			d.log.Error("wake word model panicked; detector reset", "model", det.Model.ID, "panic", p)
		}
	}
}

// onMic runs on the capture goroutine for every 10 ms packet.
func (d *daemon) onMic(s []int16) {
	// one caller at a time (a microphone source switch can briefly overlap two)
	d.micMu.Lock()
	defer d.micMu.Unlock()
	d.mu.Lock()
	st := d.st
	dets := d.detectors
	nms := d.nearMisses
	muted := d.muted
	stopDet := d.stopDet
	chime := d.turnChime
	sounding := st == stResponding || d.alarm != nil || d.media != nil || d.announcing ||
		(d.sendspin != nil && d.sendspin.Playing())
	// music only: a reply, an announcement or a timer is not turned down
	announcing := d.announcing
	music := (d.media != nil || (d.sendspin != nil && d.sendspin.Playing())) &&
		st == stIdle && d.alarm == nil && !announcing
	d.mu.Unlock()
	if d.resetDets.Swap(false) {
		for _, x := range dets {
			x.Reset()
		}
		if stopDet != nil {
			stopDet.Reset()
		}
	}
	if muted {
		return
	}
	d.micGain.apply(s, d.cfg.Get().Voice.MicGainDB)
	d.wakeLvl.feed(s)
	lvl := d.micGain.level().PeakDB
	testing := time.Now().UnixNano() < d.testUntil.Load()
	// attempt clips (the buffer behind every Recent attempts row) and the
	// leftover-echo line after each sound — both mic goroutine only
	d.clips.feed(s)
	if r, ok := d.echo.update(sounding, s); ok {
		d.log.Info("sound ended", "mic_dbfs", round2(r.DuringDB), "leftover_dbfs", round2(r.TailDB),
			"seconds", int(r.Seconds+0.5))
	}
	// Stop word grace: nobody says "stop" in the first second of a sound, and
	// that is when the echo canceller (switched on by apmd at the first sound)
	// is weakest; the clock's own voice once triggered it there.
	if !sounding {
		d.soundSince = time.Time{}
	} else if d.soundSince.IsZero() {
		d.soundSince = time.Now()
		if stopDet != nil {
			stopDet.Reset()
		}
	}
	grace := sounding && !testing && time.Since(d.soundSince) < stopGrace
	// a test run scores the stop word in a quiet room too
	if v := d.cfg.Get().Voice; stopDet != nil && (sounding || testing) && st != stListening && v.StopWord && !grace {
		stopDet.Process(s)
		d.reportPanics(stopDet)
		sc, th := stopDet.Score(), v.StopThreshold()
		if sc >= th {
			d.reportWake("stop", false)
			d.wakeHist.add(wakeEvent{At: time.Now(), Kind: "detected", Word: "Stop", Stop: true,
				Score: round2(sc), Threshold: th, LevelDB: d.stopNM.level(lvl), Playing: sounding, Test: testing,
				Clip: d.clips.arm(v.AttemptClips)})
			d.stopNM.reset()
			stopDet.Reset()
			if !testing {
				go d.onStopWord()
			}
			return
		}
		if p, l, ok := d.stopNM.track(sc, stopNearMissFloor(th), lvl); ok {
			d.reportWake("stop", true)
			d.log.Info("stop word near miss", "peak", round2(p), "threshold", th)
			d.wakeHist.add(wakeEvent{At: time.Now(), Kind: "near_miss", Word: "Stop", Stop: true,
				Score: round2(p), Threshold: th, LevelDB: l, Playing: sounding, Test: testing,
				Clip: d.clips.arm(v.AttemptClips)})
		}
	}

	if !d.dipUntil.IsZero() && time.Now().After(d.dipUntil) {
		d.dipUntil = time.Time{}
		d.music.clear(lvlDip)
	}

	d.nullHold.Store(st == stListening || !d.dipUntil.IsZero())

	switch st {
	case stIdle:
		if d.voting.Load() { // the clocks are deciding who answers
			break
		}
		v := d.cfg.Get().Voice
		easier := v.WakeEasierPlaying && sounding
		now := time.Now()
		for _, det := range dets {
			nm := nms[det]
			det.Process(s)
			d.reportPanics(det)
			sc, th := det.Score(), wakeThreshold(det.Model, v.WakeSensitivity)
			if easier {
				th = wakeThresholdPlaying(det.Model, v.WakeSensitivity)
			}
			if sc >= th {
				d.wakeHist.add(wakeEvent{At: now, Kind: "detected", Word: det.Model.Phrase,
					Score: round2(sc), Threshold: th, LevelDB: nm.level(lvl), Playing: sounding, Test: testing,
					Clip: d.clips.arm(v.AttemptClips)})
				for _, x := range dets {
					x.Reset()
					nms[x].reset()
				}
				d.reportWake(det.Model.ID, false)
				d.log.Info("wake word detected", "phrase", det.Model.Phrase, "score", round2(sc), "threshold", th, "playing", sounding, "test", testing,
					"music_down", !d.dipUntil.IsZero())
				if !testing {
					// the turn takes over the music level (and when no turn
					// starts, Home Assistant away, the music comes back)
					d.dipUntil = time.Time{}
					d.music.clear(lvlDip)
					d.voting.Store(true)
					go d.voteTurn(det.Model.Phrase, d.wakeLvl.snr(), sc)
				}
				return
			}
			if easier && music && sc >= dipFloor && !d.player.Muted() {
				if d.dipUntil.IsZero() {
					d.log.Info("wake word maybe: music down to listen closer", "phrase", det.Model.Phrase, "score", round2(sc))
					d.music.set(lvlDip, dipGain)
				}
				d.dipUntil = now.Add(dipHold)
			}
			if p, l, ok := nm.track(sc, wakeNearMissFloor, lvl); ok {
				d.reportWake(det.Model.ID, true)
				d.log.Info("wake word near miss", "phrase", det.Model.Phrase, "peak", round2(p), "threshold", th)
				d.wakeHist.add(wakeEvent{At: now, Kind: "near_miss", Word: det.Model.Phrase,
					Score: round2(p), Threshold: th, LevelDB: l, Playing: sounding, Test: testing,
					Clip: d.clips.arm(v.AttemptClips)})
			}
		}
	case stListening:
		buf := make([]byte, len(s)*2)
		if !d.chimeGate(chime) { // gated: silence, the stream keeps its timing
			for i, v := range s {
				binary.LittleEndian.PutUint16(buf[i*2:], uint16(v))
			}
		}
		if err := d.sat.SendAudio(buf); err != nil {
			d.log.Debug("send audio", "err", err)
		}
	}
}

// voteTurn lets the clocks that heard the wake word decide which one
// answers (wakevote.go), then starts the turn here if it is this one. A
// ringing alarm or timer is silenced here at once, and a clock that can't
// answer (muted, Home Assistant away) doesn't take part.
func (d *daemon) voteTurn(phrase string, snr, score float64) {
	defer d.voting.Store(false)
	d.mu.Lock()
	vote := d.vote != nil && d.cfg.Get().Voice.WakeVote && d.alarm == nil && !d.muted
	d.mu.Unlock()
	if vote && d.sat.Subscribed() && !d.vote.decide(phrase, snr, score) {
		return
	}
	d.startTurn(phrase, true)
}

func (d *daemon) startTurn(phrase string, chime bool) {
	if d.stopAlarm() { // the wake word silences a ringing timer
		return
	}
	d.mu.Lock()
	if d.muted {
		d.mu.Unlock()
		return
	}
	if d.st != stIdle {
		d.mu.Unlock()
		return
	}
	if !d.sat.Subscribed() {
		d.mu.Unlock()
		d.log.Warn("wake word ignored: Home Assistant is not connected")
		d.out.Play(output.NewSamples(d.chimeError, 0), 1, "alert")
		d.flashError()
		return
	}
	d.st = stListening
	d.turn++
	d.turnStarted = time.Now()
	d.continueReq = false
	d.mu.Unlock()
	d.updateLEDs()

	d.music.set(lvlTurn, duckGain) // duck music during the turn
	var cv *output.Voice
	if chime && d.cfg.Get().Voice.WakeSound && !d.dnd.On() { // do not disturb: the LEDs show it
		cv = d.out.Play(output.NewSamples(d.chimeWake, 0), 1, "alert")
	}
	d.mu.Lock()
	d.turnChime = cv
	d.mu.Unlock()
	if err := d.sat.StartTurn(phrase, nil); err != nil {
		d.log.Warn("start turn", "err", err)
		d.endTurn()
	}
}

// chimeTail: how long the wake chime is still heard after its voice is done:
// the output buffer (~0.25 s) plus room echo the canceller has not learned yet.
const chimeTail = 450 * time.Millisecond

// chimeGate reports whether the microphone audio of a turn is replaced with
// silence: while the turn's wake chime plays and chimeTail after. The
// canceller switches on with the first sound after an idle spell and does
// not cancel that first chime; Home Assistant's speech detection took it
// for speech ~1 s into every turn (clock #2), so a user who paused after the
// wake word got "no text recognized", or a made-up phrase. Mic goroutine.
func (d *daemon) chimeGate(v *output.Voice) bool {
	if v == nil {
		return false
	}
	if v != d.gateChime {
		d.gateChime, d.gateChimeEnd = v, time.Time{}
	}
	select {
	case <-v.Done():
	default:
		return true
	}
	now := time.Now()
	if d.gateChimeEnd.IsZero() {
		d.gateChimeEnd = now
	}
	return now.Sub(d.gateChimeEnd) < chimeTail
}

func (d *daemon) endTurn() {
	d.mu.Lock()
	d.st = stIdle
	d.speaking = false
	d.mu.Unlock()
	// onMic resets them on its own goroutine: resetting here raced with a
	// detector running on the microphone thread (clockd crashed with "index
	// out of range" in the wake word model at the end of a test run)
	d.resetDets.Store(true)
	d.music.clear(lvlTurn)
	d.updateLEDs()
}

// healthy is the update trial's check: the daemon's lock is free (no
// deadlock), the clock face is ticking and the web UI answers (the way back
// to an older version is through it).
func (d *daemon) healthy(webAddr string) error {
	locked := make(chan struct{})
	go func() { d.mu.Lock(); d.mu.Unlock(); close(locked) }()
	select {
	case <-locked:
	case <-time.After(5 * time.Second):
		return fmt.Errorf("daemon lock held for 5 s")
	}
	if d.face != nil {
		if t := d.face.lastTick(); !t.IsZero() && time.Since(t) > 10*time.Second {
			return fmt.Errorf("clock face stalled for %s", time.Since(t).Round(time.Second))
		}
	}
	if webAddr == "" {
		return nil
	}
	_, port, err := net.SplitHostPort(webAddr)
	if err != nil {
		return nil
	}
	cl := http.Client{Timeout: 5 * time.Second}
	resp, err := cl.Get("http://127.0.0.1:" + port + "/")
	if err != nil {
		return fmt.Errorf("web UI: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode >= 500 {
		return fmt.Errorf("web UI: %s", resp.Status)
	}
	return nil
}

// watchdog ends turns Home Assistant never finished.
func (d *daemon) watchdog(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		d.mu.Lock()
		// a long answer still being spoken is not stuck (finishRun ends it,
		// or this does once the voice is done)
		talking := false
		if d.st == stResponding && d.ttsVoice != nil {
			select {
			case <-d.ttsVoice.Done():
			default:
				talking = true
			}
		}
		stuck := d.st != stIdle && !talking && time.Since(d.turnStarted) > 60*time.Second
		listeningTooLong := d.st == stListening && time.Since(d.turnStarted) > 20*time.Second
		d.mu.Unlock()
		if listeningTooLong || stuck {
			d.log.Warn("turn timed out")
			d.sat.StopTurn()
			d.endTurn()
		}
	}
}

func (d *daemon) buildSatellite() *esphome.VoiceSatellite {
	avail := d.availableWakeWords()
	var active []string
	for _, det := range d.detectors {
		active = append(active, det.Model.ID)
	}
	v := &esphome.VoiceSatellite{
		AvailableWakeWords: avail,
		ActiveWakeWords:    active,
		MaxActiveWakeWords: 2,
	}
	v.OnSubscribed = func(on bool) { d.log.Info("Home Assistant voice subscription", "subscribed", on) }
	v.OnSetActiveWakeWords = func(ids []string) {
		if err := d.setActiveWakeWords(ids); err != nil {
			d.log.Warn("set wake words", "err", err)
		} else {
			d.log.Info("active wake words", "ids", ids)
			d.cfg.Update(func(c *config.Config) { c.WakeWords = ids })
		}
	}
	v.OnStartRequestAccepted = func(port uint32, isErr bool) {
		if isErr {
			d.log.Warn("Home Assistant refused the turn")
			d.endTurn()
		}
	}
	v.OnPipelineEvent = d.onPipelineEvent
	v.OnTTSAudio = d.onTTSAudio
	v.OnAnnounce = d.onAnnounce
	v.OnTimer = d.onTimer
	return v
}

func (d *daemon) onPipelineEvent(e esphome.PipelineEvent) {
	if t := e.Data["text"]; e.Type.String() == "VOICE_ASSISTANT_TTS_START" && t != "" {
		// the same text INTENT_END just logged as "speech": log its size only
		d.log.Info("pipeline", "event", "TTS_START", "chars", utf8.RuneCountInString(t))
	} else {
		d.log.Info("pipeline", "event", strings.TrimPrefix(e.Type.String(), "VOICE_ASSISTANT_"), "data", e.Data)
	}
	switch e.Type.String() {
	case "VOICE_ASSISTANT_STT_VAD_END", "VOICE_ASSISTANT_STT_END":
		d.mu.Lock()
		if d.st == stListening {
			d.st = stResponding
		}
		d.mu.Unlock()
		d.updateLEDs()
	case "VOICE_ASSISTANT_INTENT_END":
		d.mu.Lock()
		d.continueReq = e.Data["continue_conversation"] == "1"
		d.mu.Unlock()
	case "VOICE_ASSISTANT_TTS_END":
		if u := e.Data["url"]; u != "" {
			d.playTTS(u)
		}
	case "VOICE_ASSISTANT_RUN_END":
		go d.finishRun(d.turnNow())
	case "VOICE_ASSISTANT_ERROR":
		// another satellite heard the same wake word first (HA keeps the
		// first run per phrase within 2 s): not a failure here, end quietly
		// like the Voice PE firmware does
		if e.Data["code"] != "duplicate_wake_up_detected" {
			d.out.Play(output.NewSamples(d.chimeError, 0), 1, "alert")
			d.flashError()
		}
		go d.finishRun(d.turnNow())
	}
}

func (d *daemon) turnNow() uint64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.turn
}

// playTTS starts the answer. Not after the turn was cancelled (button, stop
// word, mute): the answer of a cancelled turn used to play anyway when its
// URL arrived late.
func (d *daemon) playTTS(u string) {
	buf := output.NewBuffer(5)
	d.mu.Lock()
	if d.st == stIdle {
		d.mu.Unlock()
		d.log.Info("answer dropped: the turn has ended")
		return
	}
	// registered before this returns, so the run's end (the next message
	// from Home Assistant) finds the voice to wait for
	v := d.out.Play(buf, 1, "voice")
	d.ttsVoice = v
	d.speaking = true
	d.mu.Unlock()
	go func() {
		if err := fetchAudio(d.log, u, buf, 2*time.Minute); err != nil {
			d.log.Warn("TTS playback", "err", err)
		}
	}()
}

// finishRun waits for the response audio, then returns to idle or continues
// the conversation. turn is the turn the run belonged to: when that one has
// been ended meanwhile (button, stop word, timeout), or Home Assistant sent
// both an error and a run end, there is nothing left to end, and ending
// again would cut a turn started since or continue a cancelled conversation.
func (d *daemon) finishRun(turn uint64) {
	time.Sleep(300 * time.Millisecond) // answers sent over the API: the last audio packet
	d.mu.Lock()
	v := d.ttsVoice
	d.mu.Unlock()
	if v != nil {
		select {
		case <-v.Done():
		case <-time.After(2 * time.Minute):
		}
	}
	d.mu.Lock()
	if d.ttsVoice == v {
		d.ttsVoice = nil
	}
	cont := d.continueReq
	stale := d.turn != turn || d.st == stIdle
	d.mu.Unlock()
	if stale {
		return
	}
	d.endTurn()
	if cont {
		d.log.Info("continuing conversation")
		d.startTurn("", false)
	}
}

// onTTSAudio handles responses sent over the API instead of as a URL.
func (d *daemon) onTTSAudio(data []byte, end bool) {
	d.mu.Lock()
	d.ttsPCM = append(d.ttsPCM, data...)
	pcm := d.ttsPCM
	if end {
		d.ttsPCM = nil
	}
	d.mu.Unlock()
	if !end || len(pcm) == 0 {
		return
	}
	buf := output.NewBuffer(600)
	go func() {
		if len(pcm) > 4 && string(pcm[:4]) == "RIFF" {
			output.DecodeWAV(strings.NewReader(string(pcm)), buf)
			return
		}
		// raw 16 kHz mono s16 (ESPHome API audio)
		output.DecodeWAV(strings.NewReader(string(wavHeader(16000, 1, len(pcm)))+string(pcm)), buf)
	}()
	v := d.out.Play(buf, 1, "voice")
	d.mu.Lock()
	d.ttsVoice = v
	d.speaking = true
	d.mu.Unlock()
}

func (d *daemon) onAnnounce(a esphome.Announce) {
	d.log.Info("announce", "text", a.Text, "media", shortURL(a.MediaID),
		"preannounce", shortURL(a.PreannounceMediaID), "start_conversation", a.StartConversation)
	if d.dnd.On() {
		d.log.Info("announcement skipped: do not disturb", "text", a.Text)
		d.sat.AnnounceFinished(true) // HA waits for this
		return
	}
	go func() {
		// counted: a second announcement can arrive while one plays
		d.mu.Lock()
		if d.annN++; d.annN == 1 {
			d.announcing, d.annCancel = true, false
		}
		d.mu.Unlock()
		d.music.set(lvlAnnounce, duckGain)
		ok := true
		for _, u := range []string{a.PreannounceMediaID, a.MediaID} {
			d.mu.Lock()
			cancelled := d.annCancel
			d.mu.Unlock()
			if u == "" || cancelled {
				continue
			}
			buf := output.NewBuffer(5)
			v := d.out.Play(buf, 1, "voice")
			if err := fetchAudio(d.log, u, buf, time.Minute); err != nil {
				d.log.Warn("announcement", "err", err)
				ok = false
			}
			select {
			case <-v.Done():
			case <-time.After(30 * time.Second):
				d.log.Warn("announcement playback did not finish; stopping it")
				buf.Abort()
				d.out.Stop(v)
			}
		}
		d.mu.Lock()
		cancelled := d.annCancel
		if d.annN--; d.annN == 0 {
			d.announcing, d.annCancel = false, false
			d.music.clear(lvlAnnounce)
		}
		d.mu.Unlock()
		d.sat.AnnounceFinished(ok)
		if a.StartConversation && !cancelled {
			d.startTurn("", false)
		}
	}()
}

func (d *daemon) onTimer(t esphome.TimerEvent) {
	d.log.Info("timer", "event", strings.TrimPrefix(t.Type.String(), "VOICE_ASSISTANT_TIMER_"),
		"name", t.Name, "left", t.SecondsLeft)
	if t.Type.String() != "VOICE_ASSISTANT_TIMER_FINISHED" {
		return
	}
	d.ring("", "", 0) // at most 5 minutes; the wake word, "stop" or a button stops it
}

// ---- media player ----

func (d *daemon) onMediaCommand(c esphome.MediaCommand) {
	if c.HasVolume {
		d.setVolume(c.Volume)
	}
	if c.HasCommand {
		switch c.Command {
		case esphome.MediaPlayerStop:
			d.stopMedia()
		case esphome.MediaPlayerMute:
			d.player.SetMuted(true)
			d.music.set(lvlMute, 0)
		case esphome.MediaPlayerUnmute:
			d.player.SetMuted(false)
			d.music.clear(lvlMute)
		case esphome.MediaPlayerVolumeUp:
			d.setVolume(min(d.player.Volume()+0.05, 1))
		case esphome.MediaPlayerVolumeDown:
			d.setVolume(max(d.player.Volume()-0.05, 0))
		}
		if c.Command.String() == "MEDIA_PLAYER_COMMAND_PAUSE" {
			d.stopMedia() // no pause support yet: stop instead
		}
	}
	if c.HasMediaURL {
		d.playMediaURL(c.MediaURL, c.HasAnnouncement && c.Announcement)
	}
}

// playMediaURL plays a WAV, FLAC or MP3 URL as media (or as an announcement in the voice group).
func (d *daemon) playMediaURL(url string, announce bool) {
	group := "media"
	if announce {
		group = "voice"
	}
	d.log.Info("play media", "url", shortURL(url), "group", group)
	if mhz, ok := strings.CutPrefix(url, "fm:"); ok && group == "media" {
		// Home Assistant: media_content_id "fm:91.2" plays that FM station
		var f float64
		if _, err := fmt.Sscan(mhz, &f); err == nil {
			if err := d.radioPlay(int(math.Round(f*10)) * fmStep); err != nil {
				d.log.Warn("radio from Home Assistant", "err", err)
			}
		}
		return
	}
	if group == "media" {
		d.stopMedia()
		d.startMedia("url", shortURL(url), func(buf *output.Buffer) error {
			return fetchAudio(d.log, url, buf, 0)
		}, nil)
		return
	}
	if d.dnd.On() {
		d.log.Info("announcement skipped: do not disturb", "url", shortURL(url))
		return
	}
	buf := output.NewBuffer(5)
	v := d.out.Play(buf, 1, group)
	go func() {
		if err := fetchAudio(d.log, url, buf, 0); err != nil {
			d.log.Warn("media playback", "err", err)
		}
		<-v.Done()
	}()
}

// startMedia plays buf in the media group (from: the source for the status
// page, see nowPlaying) and tracks it as the current media
// (one at a time). fill decodes into buf from its own goroutine; playback ends
// when fill returns and the buffer drains (or stopMedia aborts it). onEnd runs
// when playback ended on its own — not when it was stopped or replaced — which
// is what lets the USB folder queue continue with the next file.
func (d *daemon) startMedia(from, name string, fill func(buf *output.Buffer) error, onEnd func()) {
	d.startMediaWith(from, name, fill, onEnd, nil)
}

// startMediaWith is startMedia with the buffer wrapped by wrap (nil: played
// as it is), e.g. the DLNA renderer's pausable source. It returns the voice.
func (d *daemon) startMediaWith(from, name string, fill func(buf *output.Buffer) error, onEnd func(), wrap func(*output.Buffer) output.Source) *output.Voice {
	buf := output.NewBuffer(5)
	var src output.Source = buf
	if wrap != nil {
		src = wrap(buf)
	}
	v := d.out.Play(src, 1, "media")
	d.mu.Lock()
	d.media, d.mediaBuf, d.mediaName, d.mediaSrc = v, buf, name, from
	d.mu.Unlock()
	d.player.SetState(esphome.MediaPlayerPlaying)
	go func() {
		if err := fill(buf); err != nil {
			d.log.Warn("media playback", "err", err)
		}
		<-v.Done()
		d.mu.Lock()
		current := d.media == v
		if current {
			d.media, d.mediaBuf, d.mediaName, d.mediaSrc = nil, nil, "", ""
		}
		d.mu.Unlock()
		if current {
			d.player.SetState(esphome.MediaPlayerIdle)
			if onEnd != nil {
				onEnd()
			}
		}
	}()
	return v
}

// stopMedia ends playback of any media, including a USB folder queue (which
// must not continue to its next file after this) and its pause state.
// stopPlaying ends whatever plays: the clock's media, the radio (also for
// stream listeners) and Music Assistant (paused there, as a spoken stop does).
func (d *daemon) stopPlaying() {
	d.radioStop()
	d.stopMedia()
	if d.sendspin != nil && d.sendspin.Playing() {
		d.sendspin.Pause()
	}
	d.log.Info("playback stopped", "by", "web UI")
}

func (d *daemon) stopMedia() {
	d.mu.Lock()
	v, b := d.media, d.mediaBuf
	d.media, d.mediaBuf, d.mediaName, d.mediaSrc, d.usbQ, d.usbPaused = nil, nil, "", "", nil, nil
	d.mu.Unlock()
	if b != nil {
		b.Abort()
	}
	d.out.Stop(v)
	d.player.SetState(esphome.MediaPlayerIdle)
}

// ---- helpers ----

var httpClient = &http.Client{Timeout: 0, Transport: &http.Transport{ResponseHeaderTimeout: 15 * time.Second}}

// fetchAudio streams a WAV, FLAC or MP3 URL into buf. maxDur > 0 caps the whole transfer.
func fetchAudio(log *slog.Logger, u string, buf *output.Buffer, maxDur time.Duration) error {
	ctx := context.Background()
	if maxDur > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, maxDur)
		defer cancel()
	}
	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		buf.Close(err)
		return err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		buf.Close(err)
		return err
	}
	defer resp.Body.Close()
	log.Info("fetching audio", "url", shortURL(u), "status", resp.StatusCode,
		"type", resp.Header.Get("Content-Type"), "length", resp.ContentLength)
	if resp.StatusCode != http.StatusOK {
		err := fmt.Errorf("%s: %s", resp.Status, shortURL(u))
		buf.Close(err)
		return err
	}
	cr := &countingReader{r: resp.Body}
	err = output.Decode(cr, buf)
	// the stream plays while it arrives: this marks its end, "took" is how
	// long the sender spent producing and sending it
	log.Info("audio stream ended", "url", shortURL(u), "bytes", cr.n, "took", time.Since(start).Round(time.Millisecond), "err", err)
	return err
}

type countingReader struct {
	r interface{ Read([]byte) (int, error) }
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func wavHeader(rate, ch, dataLen int) []byte {
	h := make([]byte, 44)
	copy(h[0:], "RIFF")
	binary.LittleEndian.PutUint32(h[4:], uint32(36+dataLen))
	copy(h[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(h[16:], 16)
	binary.LittleEndian.PutUint16(h[20:], 1)
	binary.LittleEndian.PutUint16(h[22:], uint16(ch))
	binary.LittleEndian.PutUint32(h[24:], uint32(rate))
	binary.LittleEndian.PutUint32(h[28:], uint32(rate*ch*2))
	binary.LittleEndian.PutUint16(h[32:], uint16(ch*2))
	binary.LittleEndian.PutUint16(h[34:], 16)
	copy(h[36:], "data")
	binary.LittleEndian.PutUint32(h[40:], uint32(dataLen))
	return h
}

func shortURL(raw string) string {
	if u, err := url.Parse(raw); err == nil {
		return u.Scheme + "://" + u.Host + u.Path
	}
	return raw
}

func loadOrCreateKey(path string, log *slog.Logger) (esphome.PSK, error) {
	if b, err := os.ReadFile(path); err == nil {
		return esphome.ParsePSK(strings.TrimSpace(string(b)))
	}
	k, err := esphome.GeneratePSK()
	if err != nil {
		return k, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return k, err
	}
	if err := os.WriteFile(path, []byte(k.String()+"\n"), 0o600); err != nil {
		return k, err
	}
	log.Info("generated new API encryption key", "path", path)
	return k, nil
}

func macAddress() string {
	for _, ifc := range []string{"wlan0", "eth0"} {
		if b, err := os.ReadFile("/sys/class/net/" + ifc + "/address"); err == nil {
			if m := strings.TrimSpace(string(b)); m != "" && m != "00:00:00:00:00:00" {
				return strings.ToUpper(m)
			}
		}
	}
	h, _ := os.Hostname()
	sum := sha256.Sum256([]byte(h))
	return fmt.Sprintf("02:%02X:%02X:%02X:%02X:%02X", sum[0], sum[1], sum[2], sum[3], sum[4])
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "clockd:", err)
	os.Exit(1)
}

// Timer and alarm sounds, all made here (beeps by output.Tone, tunes by
// output.Melody; no recordings). While a sound plays, the echo canceller
// removes the user's voice along with the echo (the speaker is ~28 dB louder at
// the mics), so "stop" is only heard in the pauses: every sound ends in one
// (TestAlarmSounds), and the long ones are easier to stop by voice. The
// buttons always work. The web UI lists the same ids (index.html, two selects).
type alarmSound struct {
	beeps  [][2]float64 // a beep pattern (Hz, ms), or
	timbre output.Timbre
	notes  [][2]float64 // a tune
	pause  int          // ms of silence after the tune
}

var alarmSounds = map[string]alarmSound{
	"gentle":  {beeps: [][2]float64{{1000, 150}, {0, 100}, {1000, 150}, {0, 1600}}},                                           // 2 s
	"classic": {beeps: [][2]float64{{1000, 150}, {0, 100}, {1000, 150}, {0, 600}}},                                            // 1 s
	"urgent":  {beeps: [][2]float64{{2000, 100}, {0, 50}, {2000, 100}, {0, 50}, {2000, 100}, {0, 50}, {2000, 100}, {0, 650}}}, // 1.2 s
	"chime":   {beeps: [][2]float64{{880, 350}, {660, 450}, {0, 1200}}},                                                       // 2 s
	// tunes: public-domain melodies or our own
	"morning": {timbre: output.Legato, pause: 1500, // Grieg, Morning Mood (1875)
		notes: seq("G5", 300, "E5", 300, "D5", 300, "C5", 300, "D5", 300, "E5", 300, "G5", 300, "E5", 300,
			"D5", 300, "C5", 300, "D5", 300, "E5", 300, "D5", 300, "E5", 300, "G5", 300, "E5", 300, "G5", 300, "A5", 300, "E5", 300, "A5", 300, "G5", 300, "E5", 300, "D5", 300, "C5", 900)},
	"birds": {timbre: output.Flute, pause: 1600, // our own: two chirps and a trill
		notes: seq("G6", 60, "B6", 60, "D7", 110, "-", 250, "A6", 60, "C#7", 60, "E7", 130, "-", 500,
			"E7", 50, "C#7", 50, "E7", 50, "C#7", 50, "E7", 50, "C#7", 50, "A6", 160)},
	"bells": {timbre: output.Bell, pause: 1500, // Westminster Quarters (1793)
		notes: seq("G#4", 400, "E4", 400, "F#4", 400, "B3", 800, "B3", 400, "F#4", 400, "G#4", 400, "E4", 900)},
	"reveille": {timbre: output.Brass, pause: 1500, // bugle call
		notes: seq("G4", 140, "C5", 280, "E5", 140, "C5", 140, "G4", 140, "C5", 280, "E5", 140, "C5", 140, "G4", 140,
			"C5", 280, "E5", 140, "C5", 140, "E5", 140, "G5", 700)},
	"rise": {timbre: output.Bell, pause: 1200, // our own: a chord climbing two octaves
		notes: seq("C5", 180, "E5", 180, "G5", 180, "C6", 180, "E6", 180, "G6", 180, "C7", 900)},
	"cuckoo": {timbre: output.Flute, pause: 1500, // cuckoo clock
		notes: seq("E5", 220, "C5", 500, "E5", 220, "C5", 500, "E5", 220, "C5", 900)},
}

const alarmMs = 5 * 60 * 1000 // ring for at most 5 minutes

func alarmTone(name string) []float32 {
	s, ok := alarmSounds[name]
	if !ok {
		s = alarmSounds["gentle"]
	}
	if s.beeps != nil {
		return output.Tone(-10, s.beeps...)
	}
	pcm := output.Melody(-10, s.timbre, s.notes...)
	return append(pcm, make([]float32, s.pause*output.Rate/1000*output.Channels)...)
}

// alarmRepeats is the extra loops for about 5 minutes of ringing.
func alarmRepeats(pcm []float32) int {
	ms := len(pcm) / output.Channels * 1000 / output.Rate
	return max(0, alarmMs/ms-1)
}
