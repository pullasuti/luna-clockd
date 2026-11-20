package dlna

import (
	"encoding/xml"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeLib struct{ root string }

func (l fakeLib) List(dir string) ([]Entry, error) {
	es, err := os.ReadDir(filepath.Join(l.root, dir))
	if err != nil {
		return nil, err
	}
	var out []Entry
	for _, e := range es {
		p := strings.TrimSuffix(dir, "/") + "/" + e.Name()
		if e.IsDir() || mimeOf(p) != "" {
			fi, _ := e.Info()
			out = append(out, Entry{Name: e.Name(), Path: p, Dir: e.IsDir(), Size: fi.Size()})
		}
	}
	return out, nil
}
func (l fakeLib) Open(p string) (*os.File, error) { return os.Open(filepath.Join(l.root, p)) }
func (l fakeLib) Meta(p string) (string, time.Duration) {
	if strings.HasSuffix(p, "song.mp3") {
		return "Artist & Co - Song <1>", 83500 * time.Millisecond
	}
	return "", 0
}
func (l fakeLib) UpdateID() uint32 { return 7 }

type fakePlayer struct {
	mu                    sync.Mutex
	uri                   string
	playing, paused, mute bool
	vol                   int
	calls                 []string
}

func (p *fakePlayer) log(s string) { p.calls = append(p.calls, s) }
func (p *fakePlayer) Play(uri, title string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.uri, p.playing, p.paused = uri, true, false
	p.log("play " + title)
	return nil
}
func (p *fakePlayer) Pause()  { p.mu.Lock(); p.paused = true; p.log("pause"); p.mu.Unlock() }
func (p *fakePlayer) Resume() { p.mu.Lock(); p.paused = false; p.log("resume"); p.mu.Unlock() }
func (p *fakePlayer) Stop()   { p.mu.Lock(); p.playing = false; p.log("stop"); p.mu.Unlock() }
func (p *fakePlayer) Status() (bool, bool, time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.playing, p.paused, 61 * time.Second
}
func (p *fakePlayer) Volume() int     { p.mu.Lock(); defer p.mu.Unlock(); return p.vol }
func (p *fakePlayer) SetVolume(v int) { p.mu.Lock(); p.vol = v; p.mu.Unlock() }
func (p *fakePlayer) Muted() bool     { p.mu.Lock(); defer p.mu.Unlock(); return p.mute }
func (p *fakePlayer) SetMuted(m bool) { p.mu.Lock(); p.mute = m; p.mu.Unlock() }

func soap(t *testing.T, base, path, svcType, act string, args [][2]string) (map[string]string, int) {
	t.Helper()
	var b strings.Builder
	fmt.Fprintf(&b, `<?xml version="1.0"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body><u:%s xmlns:u="%s">`, act, svcType)
	for _, a := range args {
		fmt.Fprintf(&b, "<%s>%s</%s>", a[0], html.EscapeString(a[1]), a[0])
	}
	fmt.Fprintf(&b, "</u:%s></s:Body></s:Envelope>", act)
	req, _ := http.NewRequest("POST", base+path, strings.NewReader(b.String()))
	req.Header.Set("SOAPACTION", `"`+svcType+"#"+act+`"`)
	req.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, out, err := parseSOAP(resp.Body)
	if err != nil && resp.StatusCode == 200 {
		t.Fatalf("%s: %v", act, err)
	}
	return out, resp.StatusCode
}

func get(t *testing.T, url string, hdr ...string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

func wellFormed(t *testing.T, name, s string) {
	t.Helper()
	d := xml.NewDecoder(strings.NewReader(s))
	for {
		if _, err := d.Token(); err == io.EOF {
			return
		} else if err != nil {
			t.Fatalf("%s: not well-formed: %v\n%s", name, err, s)
		}
	}
}

func TestStack(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "Music"), 0o755)
	os.WriteFile(filepath.Join(root, "Music", "song.mp3"), []byte("0123456789"), 0o644)
	os.WriteFile(filepath.Join(root, "notes.txt"), []byte("x"), 0o644)
	s, err := Start(Config{Name: "Test <clock>", Manufacturer: "x", Model: "y", Version: "1", Seed: "aa:bb", Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	base := fmt.Sprintf("http://127.0.0.1:%d", s.Port())
	if r, _ := get(t, base+"/dlna/server/desc.xml"); r.StatusCode != 404 {
		t.Fatal("server answers while off")
	}
	p := &fakePlayer{vol: 40}
	s.SetLibrary(fakeLib{root})
	s.SetPlayer(p)

	for _, u := range []string{"/dlna/server/desc.xml", "/dlna/server/ContentDirectory.xml", "/dlna/server/ConnectionManager.xml",
		"/dlna/renderer/desc.xml", "/dlna/renderer/AVTransport.xml", "/dlna/renderer/RenderingControl.xml", "/dlna/renderer/ConnectionManager.xml"} {
		r, body := get(t, base+u)
		if r.StatusCode != 200 {
			t.Fatalf("%s: %d", u, r.StatusCode)
		}
		wellFormed(t, u, body)
	}
	_, desc := get(t, base+"/dlna/renderer/desc.xml")
	if !strings.Contains(desc, "Test &lt;clock&gt;") || !strings.Contains(desc, "uuid:"+stableUUID("aa:bb:renderer")) {
		t.Errorf("renderer desc: %s", desc)
	}

	// browse the root, then the folder
	cd := "urn:schemas-upnp-org:service:ContentDirectory:1"
	out, code := soap(t, base, "/dlna/server/ContentDirectory/control", cd, "Browse", [][2]string{{"ObjectID", "0"},
		{"BrowseFlag", "BrowseDirectChildren"}, {"Filter", "*"}, {"StartingIndex", "0"}, {"RequestedCount", "0"}, {"SortCriteria", ""}})
	if code != 200 || out["TotalMatches"] != "1" || !strings.Contains(out["Result"], `id="/Music"`) {
		t.Fatalf("root: %d %v", code, out)
	}
	wellFormed(t, "root DIDL", out["Result"])
	out, _ = soap(t, base, "/dlna/server/ContentDirectory/control", cd, "Browse", [][2]string{{"ObjectID", "/Music"},
		{"BrowseFlag", "BrowseDirectChildren"}, {"Filter", "*"}, {"StartingIndex", "0"}, {"RequestedCount", "10"}, {"SortCriteria", ""}})
	didl := out["Result"]
	wellFormed(t, "folder DIDL", didl)
	for _, want := range []string{"<dc:title>Song &lt;1&gt;</dc:title>", "Artist &amp; Co", `duration="0:01:23.500"`, "audio/mpeg", `parentID="/Music"`} {
		if !strings.Contains(didl, want) {
			t.Errorf("DIDL lacks %q: %s", want, didl)
		}
	}
	i := strings.Index(didl, ">http://127.0.0.1") + 1
	mediaURL := html.UnescapeString(didl[i : strings.Index(didl[i:], "<")+i])
	r, body := get(t, mediaURL, "Range", "bytes=2-5")
	if r.StatusCode != 206 || body != "2345" || r.Header.Get("Content-Type") != "audio/mpeg" {
		t.Errorf("media range: %d %q %s", r.StatusCode, body, r.Header.Get("Content-Type"))
	}
	// metadata of the folder, unknown objects, a traversal
	out, _ = soap(t, base, "/dlna/server/ContentDirectory/control", cd, "Browse", [][2]string{{"ObjectID", "/Music"},
		{"BrowseFlag", "BrowseMetadata"}, {"Filter", "*"}, {"StartingIndex", "0"}, {"RequestedCount", "0"}, {"SortCriteria", ""}})
	if out["NumberReturned"] != "1" || !strings.Contains(out["Result"], `parentID="0"`) {
		t.Errorf("metadata: %v", out)
	}
	for _, id := range []string{"/../etc", "Music", "/nope"} {
		if _, code := soap(t, base, "/dlna/server/ContentDirectory/control", cd, "Browse", [][2]string{{"ObjectID", id},
			{"BrowseFlag", "BrowseDirectChildren"}, {"StartingIndex", "0"}, {"RequestedCount", "0"}}); code != 500 {
			t.Errorf("%s: %d", id, code)
		}
	}
	if r, _ := get(t, base+"/dlna/media/"+mediaToken("/notes.txt")); r.StatusCode != 404 {
		t.Error("served a non-audio file")
	}

	// GENA: subscribe to AVTransport, get the initial event and a change
	got := make(chan string, 10)
	cb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- r.Header.Get("SEQ") + " " + html.UnescapeString(string(b))
	}))
	defer cb.Close()
	req, _ := http.NewRequest("SUBSCRIBE", base+"/dlna/renderer/AVTransport/event", nil)
	req.Header.Set("CALLBACK", "<"+cb.URL+"/ev>")
	req.Header.Set("NT", "upnp:event")
	req.Header.Set("TIMEOUT", "Second-300")
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("SID"), "uuid:") || resp.Header.Get("TIMEOUT") != "Second-300" {
		t.Fatalf("subscribe: %v %v", resp, err)
	}
	wait := func(want string) {
		t.Helper()
		select {
		case e := <-got:
			if !strings.Contains(e, want) {
				t.Fatalf("event %q lacks %q", e, want)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("no event with %q", want)
		}
	}
	wait(`0 `)
	// a callback to another host is refused
	req, _ = http.NewRequest("SUBSCRIBE", base+"/dlna/renderer/AVTransport/event", nil)
	req.Header.Set("CALLBACK", "<http://192.0.2.1/x>")
	req.Header.Set("NT", "upnp:event")
	if resp, _ := http.DefaultClient.Do(req); resp.StatusCode != http.StatusPreconditionFailed {
		t.Errorf("foreign callback: %d", resp.StatusCode)
	}

	avt := "urn:schemas-upnp-org:service:AVTransport:1"
	ctl := "/dlna/renderer/AVTransport/control"
	meta := `<DIDL-Lite xmlns="urn:schemas-upnp-org:metadata-1-0/DIDL-Lite/" xmlns:dc="http://purl.org/dc/elements/1.1/"><item><dc:title>Radio &amp; more</dc:title><res duration="0:03:00.000">x</res></item></DIDL-Lite>`
	if _, code := soap(t, base, ctl, avt, "SetAVTransportURI", [][2]string{{"InstanceID", "0"}, {"CurrentURI", "file:///etc/passwd"}, {"CurrentURIMetaData", ""}}); code != 500 {
		t.Error("file:// accepted")
	}
	if _, code := soap(t, base, ctl, avt, "SetAVTransportURI", [][2]string{{"InstanceID", "0"}, {"CurrentURI", "http://192.0.2.1/a.flac"}, {"CurrentURIMetaData", meta}}); code != 200 {
		t.Fatal("SetAVTransportURI")
	}
	wait(`TransportState val="STOPPED"`)
	soap(t, base, ctl, avt, "Play", [][2]string{{"InstanceID", "0"}, {"Speed", "1"}})
	wait(`TransportState val="PLAYING"`)
	out, _ = soap(t, base, ctl, avt, "GetPositionInfo", [][2]string{{"InstanceID", "0"}})
	if out["RelTime"] != "0:01:01" || out["TrackDuration"] != "0:03:00" {
		t.Errorf("position: %v", out)
	}
	soap(t, base, ctl, avt, "Pause", [][2]string{{"InstanceID", "0"}})
	wait(`PAUSED_PLAYBACK`)
	soap(t, base, ctl, avt, "Play", [][2]string{{"InstanceID", "0"}, {"Speed", "1"}})
	wait(`PLAYING`)
	// playback ended on its own: the poll notices
	p.Stop()
	wait(`STOPPED`)
	if got := strings.Join(p.calls, ","); got != "play Radio & more,pause,resume,stop" {
		t.Errorf("player calls: %s", got)
	}
	rcs := "urn:schemas-upnp-org:service:RenderingControl:1"
	soap(t, base, "/dlna/renderer/RenderingControl/control", rcs, "SetVolume", [][2]string{{"InstanceID", "0"}, {"Channel", "Master"}, {"DesiredVolume", "25"}})
	out, _ = soap(t, base, "/dlna/renderer/RenderingControl/control", rcs, "GetVolume", [][2]string{{"InstanceID", "0"}, {"Channel", "Master"}})
	if out["CurrentVolume"] != "25" {
		t.Errorf("volume %v", out)
	}
	if _, code := soap(t, base, "/dlna/renderer/RenderingControl/control", rcs, "SetVolume", [][2]string{{"InstanceID", "0"}, {"Channel", "Master"}, {"DesiredVolume", "250"}}); code != 500 {
		t.Error("volume 250 accepted")
	}
	// off again
	s.SetPlayer(nil)
	if r, _ := get(t, base+"/dlna/renderer/desc.xml"); r.StatusCode != 404 {
		t.Error("renderer still answers")
	}
}

func TestMSearch(t *testing.T) {
	st, mx, ok := parseMSearch([]byte("M-SEARCH * HTTP/1.1\r\nHOST: 239.255.255.250:1900\r\nMAN: \"ssdp:discover\"\r\nMX: 2\r\nST: urn:schemas-upnp-org:device:MediaRenderer:1\r\n\r\n"))
	if !ok || mx != 2 || st != "urn:schemas-upnp-org:device:MediaRenderer:1" {
		t.Errorf("%q %d %v", st, mx, ok)
	}
	for _, bad := range []string{"NOTIFY * HTTP/1.1\r\nNT: x\r\n\r\n", "garbage", "M-SEARCH * HTTP/1.1\r\nST: x\r\n\r\n"} {
		if _, _, ok := parseMSearch([]byte(bad)); ok {
			t.Errorf("accepted %q", bad)
		}
	}
	a := advert{uuid: "u", types: []string{"dev"}}
	if a.usn("uuid:u") != "uuid:u" || a.usn("dev") != "uuid:u::dev" {
		t.Error("usn")
	}
}
