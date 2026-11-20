package dlna

import (
	"encoding/xml"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Player is what the renderer drives (clockd's media playback).
type Player interface {
	// Play starts uri from the beginning (title for the clock's displays).
	Play(uri, title string) error
	Pause()
	Resume()
	Stop()
	// Status: whether the renderer's playback is still the clock's media
	// (false once it ended or something else replaced it), paused, and the
	// position played.
	Status() (active, paused bool, pos time.Duration)
	Volume() int // 0..100
	SetVolume(v int)
	Muted() bool
	SetMuted(m bool)
}

// the renderer accepts what clockd decodes
var sinkProtocols = []string{"audio/flac", "audio/x-flac", "audio/mpeg", "audio/mp3", "audio/wav", "audio/x-wav", "audio/wave"}

const (
	stNoMedia = "NO_MEDIA_PRESENT"
	stStopped = "STOPPED"
	stPlaying = "PLAYING"
	stPaused  = "PAUSED_PLAYBACK"
)

type renderer struct {
	cfg Config
	p   Player
	dev *device
	avt *service
	rcs *service
	cm  *service
	avE *eventer
	rcE *eventer

	mu       sync.Mutex
	state    string
	uri      string
	meta     string
	title    string
	length   time.Duration
	lastAV   string // last LastChange sent, to send only changes
	lastRC   string
	stopPoll chan struct{}
}

func newRenderer(cfg Config, p Player) *renderer {
	r := &renderer{cfg: cfg, p: p, state: stNoMedia, stopPoll: make(chan struct{})}
	iid := in("InstanceID", "A_ARG_TYPE_InstanceID")
	r.avt = &service{typ: "urn:schemas-upnp-org:service:AVTransport:1", id: "urn:upnp-org:serviceId:AVTransport", path: "/dlna/renderer/AVTransport",
		actions: []action{
			{"SetAVTransportURI", []arg{iid, in("CurrentURI", "AVTransportURI"), in("CurrentURIMetaData", "AVTransportURIMetaData")}},
			{"GetMediaInfo", []arg{iid, out("NrTracks", "NumberOfTracks"), out("MediaDuration", "CurrentMediaDuration"),
				out("CurrentURI", "AVTransportURI"), out("CurrentURIMetaData", "AVTransportURIMetaData"),
				out("NextURI", "NextAVTransportURI"), out("NextURIMetaData", "NextAVTransportURIMetaData"),
				out("PlayMedium", "PlaybackStorageMedium"), out("RecordMedium", "RecordStorageMedium"),
				out("WriteStatus", "RecordMediumWriteStatus")}},
			{"GetTransportInfo", []arg{iid, out("CurrentTransportState", "TransportState"),
				out("CurrentTransportStatus", "TransportStatus"), out("CurrentSpeed", "TransportPlaySpeed")}},
			{"GetPositionInfo", []arg{iid, out("Track", "CurrentTrack"), out("TrackDuration", "CurrentTrackDuration"),
				out("TrackMetaData", "CurrentTrackMetaData"), out("TrackURI", "CurrentTrackURI"),
				out("RelTime", "RelativeTimePosition"), out("AbsTime", "AbsoluteTimePosition"),
				out("RelCount", "RelativeCounterPosition"), out("AbsCount", "AbsoluteCounterPosition")}},
			{"GetDeviceCapabilities", []arg{iid, out("PlayMedia", "PossiblePlaybackStorageMedia"),
				out("RecMedia", "PossibleRecordStorageMedia"), out("RecQualityModes", "PossibleRecordQualityModes")}},
			{"GetTransportSettings", []arg{iid, out("PlayMode", "CurrentPlayMode"), out("RecQualityMode", "CurrentRecordQualityMode")}},
			{"Stop", []arg{iid}},
			{"Play", []arg{iid, in("Speed", "TransportPlaySpeed")}},
			{"Pause", []arg{iid}},
			{"Seek", []arg{iid, in("Unit", "A_ARG_TYPE_SeekMode"), in("Target", "A_ARG_TYPE_SeekTarget")}},
			{"Next", []arg{iid}},
			{"Previous", []arg{iid}},
			{"GetCurrentTransportActions", []arg{iid, out("Actions", "CurrentTransportActions")}},
		},
		vars: []stateVar{
			{name: "TransportState", typ: "string", allowed: []string{stStopped, stPlaying, stPaused, "TRANSITIONING", stNoMedia}},
			{name: "TransportStatus", typ: "string", allowed: []string{"OK", "ERROR_OCCURRED"}},
			{name: "PlaybackStorageMedium", typ: "string", allowed: []string{"NONE", "NETWORK"}},
			{name: "RecordStorageMedium", typ: "string", allowed: []string{"NOT_IMPLEMENTED"}},
			{name: "PossiblePlaybackStorageMedia", typ: "string"},
			{name: "PossibleRecordStorageMedia", typ: "string"},
			{name: "CurrentPlayMode", typ: "string", allowed: []string{"NORMAL"}},
			{name: "TransportPlaySpeed", typ: "string", allowed: []string{"1"}},
			{name: "RecordMediumWriteStatus", typ: "string", allowed: []string{"NOT_IMPLEMENTED"}},
			{name: "CurrentRecordQualityMode", typ: "string", allowed: []string{"NOT_IMPLEMENTED"}},
			{name: "PossibleRecordQualityModes", typ: "string"},
			{name: "NumberOfTracks", typ: "ui4"},
			{name: "CurrentTrack", typ: "ui4"},
			{name: "CurrentTrackDuration", typ: "string"},
			{name: "CurrentMediaDuration", typ: "string"},
			{name: "CurrentTrackMetaData", typ: "string"},
			{name: "CurrentTrackURI", typ: "string"},
			{name: "AVTransportURI", typ: "string"},
			{name: "AVTransportURIMetaData", typ: "string"},
			{name: "NextAVTransportURI", typ: "string"},
			{name: "NextAVTransportURIMetaData", typ: "string"},
			{name: "RelativeTimePosition", typ: "string"},
			{name: "AbsoluteTimePosition", typ: "string"},
			{name: "RelativeCounterPosition", typ: "i4"},
			{name: "AbsoluteCounterPosition", typ: "i4"},
			{name: "CurrentTransportActions", typ: "string"},
			{name: "LastChange", typ: "string", events: true},
			{name: "A_ARG_TYPE_SeekMode", typ: "string", allowed: []string{"REL_TIME", "TRACK_NR"}},
			{name: "A_ARG_TYPE_SeekTarget", typ: "string"},
			{name: "A_ARG_TYPE_InstanceID", typ: "ui4"},
		}}
	ch := in("Channel", "A_ARG_TYPE_Channel")
	r.rcs = &service{typ: "urn:schemas-upnp-org:service:RenderingControl:1", id: "urn:upnp-org:serviceId:RenderingControl", path: "/dlna/renderer/RenderingControl",
		actions: []action{
			{"ListPresets", []arg{iid, out("CurrentPresetNameList", "PresetNameList")}},
			{"SelectPreset", []arg{iid, in("PresetName", "A_ARG_TYPE_PresetName")}},
			{"GetMute", []arg{iid, ch, out("CurrentMute", "Mute")}},
			{"SetMute", []arg{iid, ch, in("DesiredMute", "Mute")}},
			{"GetVolume", []arg{iid, ch, out("CurrentVolume", "Volume")}},
			{"SetVolume", []arg{iid, ch, in("DesiredVolume", "Volume")}},
		},
		vars: []stateVar{
			{name: "PresetNameList", typ: "string"},
			{name: "LastChange", typ: "string", events: true},
			{name: "Mute", typ: "boolean"},
			{name: "Volume", typ: "ui2", rng: [2]int{0, 100}},
			{name: "A_ARG_TYPE_Channel", typ: "string", allowed: []string{"Master"}},
			{name: "A_ARG_TYPE_InstanceID", typ: "ui4"},
			{name: "A_ARG_TYPE_PresetName", typ: "string", allowed: []string{"FactoryDefaults"}},
		}}
	r.cm = connectionManager("/dlna/renderer/ConnectionManager")
	r.dev = &device{uuid: stableUUID(cfg.Seed + ":renderer"), typ: "urn:schemas-upnp-org:device:MediaRenderer:1",
		name: cfg.Name, model: cfg.Model, descPath: "/dlna/renderer/desc.xml", services: []*service{r.avt, r.rcs, r.cm}}
	r.avE = newEventer(func() kv { return kv{{"LastChange", r.avLastChange()}} })
	r.rcE = newEventer(func() kv { return kv{{"LastChange", r.rcLastChange()}} })
	go r.poll()
	return r
}

func (r *renderer) close() {
	close(r.stopPoll)
	r.avE.closeAll()
	r.rcE.closeAll()
	r.mu.Lock()
	active := r.state == stPlaying || r.state == stPaused
	r.mu.Unlock()
	if active {
		r.p.Stop()
	}
}

func (r *renderer) serve(w http.ResponseWriter, req *http.Request, rest string) {
	serveDevice(w, req, rest, r.dev, r.cfg.Manufacturer, r.control, map[*service]*eventer{r.avt: r.avE, r.rcs: r.rcE})
}

func (r *renderer) transportState() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state
}

// poll notices playback that ended or was replaced, and volume changes made
// elsewhere, and sends the events.
func (r *renderer) poll() {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-r.stopPoll:
			return
		case <-t.C:
			r.refresh()
		}
	}
}

func (r *renderer) refresh() {
	r.mu.Lock()
	if r.state == stPlaying || r.state == stPaused {
		if active, _, _ := r.p.Status(); !active {
			r.state = stStopped
		}
	}
	r.mu.Unlock()
	r.events()
}

func (r *renderer) events() {
	av, rc := r.avLastChange(), r.rcLastChange()
	r.mu.Lock()
	sendAV, sendRC := av != r.lastAV, rc != r.lastRC
	r.lastAV, r.lastRC = av, rc
	r.mu.Unlock()
	if sendAV {
		r.avE.send(kv{{"LastChange", av}})
	}
	if sendRC {
		r.rcE.send(kv{{"LastChange", rc}})
	}
}

func attrEsc(s string) string {
	var b strings.Builder
	xml.EscapeText(&b, []byte(s))
	return strings.ReplaceAll(b.String(), `"`, "&quot;")
}

func (r *renderer) actions() string {
	switch r.state {
	case stPlaying:
		return "Pause,Stop"
	case stPaused:
		return "Play,Stop"
	case stStopped:
		return "Play"
	}
	return ""
}

func (r *renderer) avLastChange() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	dur := "00:00:00"
	if r.length > 0 {
		dur = hms(r.length)
	}
	tracks := "0"
	if r.uri != "" {
		tracks = "1"
	}
	return `<Event xmlns="urn:schemas-upnp-org:metadata-1-0/AVT/"><InstanceID val="0">` +
		`<TransportState val="` + r.state + `"/><TransportStatus val="OK"/><TransportPlaySpeed val="1"/>` +
		`<NumberOfTracks val="` + tracks + `"/><CurrentTrack val="` + tracks + `"/>` +
		`<CurrentTrackDuration val="` + dur + `"/><CurrentMediaDuration val="` + dur + `"/>` +
		`<AVTransportURI val="` + attrEsc(r.uri) + `"/><CurrentTrackURI val="` + attrEsc(r.uri) + `"/>` +
		`<AVTransportURIMetaData val="` + attrEsc(r.meta) + `"/><CurrentTrackMetaData val="` + attrEsc(r.meta) + `"/>` +
		`<CurrentTransportActions val="` + r.actions() + `"/><PlaybackStorageMedium val="NETWORK"/>` +
		`</InstanceID></Event>`
}

func (r *renderer) rcLastChange() string {
	mute := "0"
	if r.p.Muted() {
		mute = "1"
	}
	return `<Event xmlns="urn:schemas-upnp-org:metadata-1-0/RCS/"><InstanceID val="0">` +
		`<Volume channel="Master" val="` + strconv.Itoa(r.p.Volume()) + `"/><Mute channel="Master" val="` + mute + `"/>` +
		`</InstanceID></Event>`
}

// hms formats H:MM:SS (AVTransport times).
func hms(d time.Duration) string {
	s := int(d.Seconds())
	return fmt.Sprintf("%d:%02d:%02d", s/3600, s/60%60, s%60)
}

// parseHMS reads H+:MM:SS[.fff] (DIDL res@duration).
func parseHMS(s string) time.Duration {
	p := strings.Split(strings.TrimSpace(s), ":")
	if len(p) != 3 {
		return 0
	}
	h, e1 := strconv.Atoi(p[0])
	m, e2 := strconv.Atoi(p[1])
	sec, e3 := strconv.ParseFloat(p[2], 64)
	if e1 != nil || e2 != nil || e3 != nil {
		return 0
	}
	return time.Duration(h)*time.Hour + time.Duration(m)*time.Minute + time.Duration(sec*float64(time.Second))
}

// didlInfo reads the title and duration from DIDL-Lite metadata.
func didlInfo(meta string) (title string, length time.Duration) {
	dec := xml.NewDecoder(strings.NewReader(meta))
	var cur string
	for {
		tok, err := dec.Token()
		if err != nil {
			return
		}
		switch t := tok.(type) {
		case xml.StartElement:
			cur = t.Name.Local
			if cur == "res" && length == 0 {
				for _, a := range t.Attr {
					if a.Name.Local == "duration" {
						length = parseHMS(a.Value)
					}
				}
			}
		case xml.CharData:
			if cur == "title" && title == "" {
				title = strings.TrimSpace(string(t))
			}
		case xml.EndElement:
			cur = ""
		}
	}
}

func (r *renderer) control(svc *service, act string, args map[string]string, _ *http.Request) (kv, error) {
	if svc == r.cm {
		var sink []string
		for _, t := range sinkProtocols {
			sink = append(sink, "http-get:*:"+t+":*")
		}
		return connectionManagerControl(act, "", strings.Join(sink, ","))
	}
	if id, ok := args["InstanceID"]; ok && id != "0" {
		return nil, &upnpError{718, "Invalid InstanceID"}
	}
	if svc == r.rcs {
		return r.rcsControl(act, args)
	}
	out, err := r.avtControl(act, args)
	if err == nil {
		r.events()
	}
	return out, err
}

func (r *renderer) rcsControl(act string, args map[string]string) (kv, error) {
	if ch, ok := args["Channel"]; ok && ch != "Master" {
		return nil, errInvalidArgs
	}
	switch act {
	case "ListPresets":
		return kv{{"CurrentPresetNameList", "FactoryDefaults"}}, nil
	case "SelectPreset":
		return nil, nil
	case "GetVolume":
		return kv{{"CurrentVolume", strconv.Itoa(r.p.Volume())}}, nil
	case "SetVolume":
		v, err := strconv.Atoi(args["DesiredVolume"])
		if err != nil || v < 0 || v > 100 {
			return nil, errInvalidArgs
		}
		r.p.SetVolume(v)
		r.events()
		return nil, nil
	case "GetMute":
		m := "0"
		if r.p.Muted() {
			m = "1"
		}
		return kv{{"CurrentMute", m}}, nil
	case "SetMute":
		switch strings.ToLower(args["DesiredMute"]) {
		case "1", "true", "yes":
			r.p.SetMuted(true)
		case "0", "false", "no":
			r.p.SetMuted(false)
		default:
			return nil, errInvalidArgs
		}
		r.events()
		return nil, nil
	}
	return nil, errInvalidAction
}

func (r *renderer) avtControl(act string, args map[string]string) (kv, error) {
	switch act {
	case "SetAVTransportURI":
		u, err := url.Parse(strings.TrimSpace(args["CurrentURI"]))
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, &upnpError{716, "Resource not found (http only)"}
		}
		title, length := didlInfo(args["CurrentURIMetaData"])
		r.mu.Lock()
		wasPlaying := r.state == stPlaying
		r.uri, r.meta, r.title, r.length = u.String(), args["CurrentURIMetaData"], title, length
		r.state = stStopped
		r.mu.Unlock()
		if wasPlaying {
			// a new track while one plays: play it at once (what control points expect)
			if err := r.p.Play(u.String(), title); err != nil {
				return nil, &upnpError{716, err.Error()}
			}
			r.mu.Lock()
			r.state = stPlaying
			r.mu.Unlock()
		}
		return nil, nil
	case "Play":
		r.mu.Lock()
		state, uri, title := r.state, r.uri, r.title
		r.mu.Unlock()
		switch state {
		case stNoMedia:
			return nil, &upnpError{701, "Transition not available (no media)"}
		case stPaused:
			r.p.Resume()
		case stPlaying:
			return nil, nil
		default:
			if err := r.p.Play(uri, title); err != nil {
				return nil, &upnpError{716, err.Error()}
			}
		}
		r.mu.Lock()
		r.state = stPlaying
		r.mu.Unlock()
		return nil, nil
	case "Pause":
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.state != stPlaying {
			return nil, &upnpError{701, "Transition not available"}
		}
		r.p.Pause()
		r.state = stPaused
		return nil, nil
	case "Stop":
		r.mu.Lock()
		active := r.state == stPlaying || r.state == stPaused
		if r.state != stNoMedia {
			r.state = stStopped
		}
		r.mu.Unlock()
		if active {
			r.p.Stop()
		}
		return nil, nil
	case "Seek", "Next", "Previous":
		return nil, &upnpError{701, "Transition not available"}
	case "GetTransportInfo":
		return kv{{"CurrentTransportState", r.transportState()}, {"CurrentTransportStatus", "OK"}, {"CurrentSpeed", "1"}}, nil
	case "GetMediaInfo":
		r.mu.Lock()
		defer r.mu.Unlock()
		n, dur := "0", "00:00:00"
		if r.uri != "" {
			n = "1"
		}
		if r.length > 0 {
			dur = hms(r.length)
		}
		return kv{{"NrTracks", n}, {"MediaDuration", dur}, {"CurrentURI", r.uri}, {"CurrentURIMetaData", r.meta},
			{"NextURI", ""}, {"NextURIMetaData", ""}, {"PlayMedium", "NETWORK"}, {"RecordMedium", "NOT_IMPLEMENTED"},
			{"WriteStatus", "NOT_IMPLEMENTED"}}, nil
	case "GetPositionInfo":
		r.mu.Lock()
		state, uri, meta, length := r.state, r.uri, r.meta, r.length
		r.mu.Unlock()
		pos := time.Duration(0)
		if state == stPlaying || state == stPaused {
			_, _, pos = r.p.Status()
		}
		dur, track := "00:00:00", "0"
		if length > 0 {
			dur = hms(length)
		}
		if uri != "" {
			track = "1"
		}
		return kv{{"Track", track}, {"TrackDuration", dur}, {"TrackMetaData", meta}, {"TrackURI", uri},
			{"RelTime", hms(pos)}, {"AbsTime", hms(pos)}, {"RelCount", "2147483647"}, {"AbsCount", "2147483647"}}, nil
	case "GetDeviceCapabilities":
		return kv{{"PlayMedia", "NETWORK"}, {"RecMedia", "NOT_IMPLEMENTED"}, {"RecQualityModes", "NOT_IMPLEMENTED"}}, nil
	case "GetTransportSettings":
		return kv{{"PlayMode", "NORMAL"}, {"RecQualityMode", "NOT_IMPLEMENTED"}}, nil
	case "GetCurrentTransportActions":
		r.mu.Lock()
		defer r.mu.Unlock()
		return kv{{"Actions", r.actions()}}, nil
	}
	return nil, errInvalidAction
}
