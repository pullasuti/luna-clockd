package dlna

import (
	"encoding/base64"
	"fmt"
	"html"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"time"
)

// Library is the read-only media the server offers (the USB drive). Paths are
// rooted at "/" and use slashes; the implementation confines them.
type Library interface {
	// List gives the folders and playable files in dir.
	List(dir string) ([]Entry, error)
	// Open opens a playable file.
	Open(p string) (*os.File, error)
	// Meta gives a file's tag name ("Artist - Title" or "") and length (0 = unknown).
	Meta(p string) (name string, length time.Duration)
	// UpdateID changes when the content changes (another drive).
	UpdateID() uint32
}

// Entry is one folder or playable file.
type Entry struct {
	Name string
	Path string
	Dir  bool
	Size int64
}

const dlnaFlags = "DLNA.ORG_OP=01;DLNA.ORG_CI=0;DLNA.ORG_FLAGS=01700000000000000000000000000000"

// mimeOf maps the playable extensions.
func mimeOf(p string) string {
	switch strings.ToLower(path.Ext(p)) {
	case ".mp3":
		return "audio/mpeg"
	case ".flac":
		return "audio/flac"
	case ".wav":
		return "audio/wav"
	}
	return ""
}

type mediaServer struct {
	cfg Config
	lib Library
	dev *device
	cd  *service
	cm  *service
	ev  *eventer
}

func newMediaServer(cfg Config, lib Library) *mediaServer {
	m := &mediaServer{cfg: cfg, lib: lib}
	m.cd = &service{typ: "urn:schemas-upnp-org:service:ContentDirectory:1", id: "urn:upnp-org:serviceId:ContentDirectory", path: "/dlna/server/ContentDirectory",
		actions: []action{
			{"GetSearchCapabilities", []arg{out("SearchCaps", "SearchCapabilities")}},
			{"GetSortCapabilities", []arg{out("SortCaps", "SortCapabilities")}},
			{"GetSystemUpdateID", []arg{out("Id", "SystemUpdateID")}},
			{"Browse", []arg{in("ObjectID", "A_ARG_TYPE_ObjectID"), in("BrowseFlag", "A_ARG_TYPE_BrowseFlag"),
				in("Filter", "A_ARG_TYPE_Filter"), in("StartingIndex", "A_ARG_TYPE_Index"),
				in("RequestedCount", "A_ARG_TYPE_Count"), in("SortCriteria", "A_ARG_TYPE_SortCriteria"),
				out("Result", "A_ARG_TYPE_Result"), out("NumberReturned", "A_ARG_TYPE_Count"),
				out("TotalMatches", "A_ARG_TYPE_Count"), out("UpdateID", "A_ARG_TYPE_UpdateID")}},
		},
		vars: []stateVar{
			{name: "SearchCapabilities", typ: "string"},
			{name: "SortCapabilities", typ: "string"},
			{name: "SystemUpdateID", typ: "ui4", events: true},
			{name: "A_ARG_TYPE_ObjectID", typ: "string"},
			{name: "A_ARG_TYPE_Result", typ: "string"},
			{name: "A_ARG_TYPE_BrowseFlag", typ: "string", allowed: []string{"BrowseMetadata", "BrowseDirectChildren"}},
			{name: "A_ARG_TYPE_Filter", typ: "string"},
			{name: "A_ARG_TYPE_SortCriteria", typ: "string"},
			{name: "A_ARG_TYPE_Index", typ: "ui4"},
			{name: "A_ARG_TYPE_Count", typ: "ui4"},
			{name: "A_ARG_TYPE_UpdateID", typ: "ui4"},
		}}
	m.cm = connectionManager("/dlna/server/ConnectionManager")
	m.dev = &device{uuid: stableUUID(cfg.Seed + ":server"), typ: "urn:schemas-upnp-org:device:MediaServer:1",
		name: cfg.Name + " USB", model: cfg.Model, descPath: "/dlna/server/desc.xml", services: []*service{m.cd, m.cm}}
	m.ev = newEventer(func() kv { return kv{{"SystemUpdateID", strconv.FormatUint(uint64(lib.UpdateID()), 10)}} })
	return m
}

func (m *mediaServer) serve(w http.ResponseWriter, r *http.Request, rest string) {
	serveDevice(w, r, rest, m.dev, m.cfg.Manufacturer, m.control, map[*service]*eventer{m.cd: m.ev})
}

func (m *mediaServer) control(svc *service, act string, args map[string]string, r *http.Request) (kv, error) {
	if svc == m.cm {
		var src []string
		for _, t := range []string{"audio/mpeg", "audio/flac", "audio/wav"} {
			src = append(src, "http-get:*:"+t+":*")
		}
		return connectionManagerControl(act, strings.Join(src, ","), "")
	}
	switch act {
	case "GetSearchCapabilities":
		return kv{{"SearchCaps", ""}}, nil
	case "GetSortCapabilities":
		return kv{{"SortCaps", ""}}, nil
	case "GetSystemUpdateID":
		return kv{{"Id", strconv.FormatUint(uint64(m.lib.UpdateID()), 10)}}, nil
	case "Browse":
		return m.browse(args, r.Host)
	}
	return nil, errInvalidAction
}

// object IDs: "0" is the drive's root, the rest are rooted paths.
func idToPath(id string) (string, bool) {
	if id == "0" {
		return "/", true
	}
	if !strings.HasPrefix(id, "/") {
		return "", false
	}
	p := path.Clean(id)
	return p, p == id || p+"/" == id
}

func pathToID(p string) string {
	if p == "/" {
		return "0"
	}
	return p
}

func parentID(p string) string {
	if p == "/" {
		return "-1"
	}
	return pathToID(path.Dir(p))
}

func (m *mediaServer) browse(args map[string]string, host string) (kv, error) {
	p, ok := idToPath(args["ObjectID"])
	if !ok {
		return nil, &upnpError{701, "No such object"}
	}
	start, _ := strconv.Atoi(args["StartingIndex"])
	count, _ := strconv.Atoi(args["RequestedCount"])
	if start < 0 || count < 0 {
		return nil, errInvalidArgs
	}
	upd := strconv.FormatUint(uint64(m.lib.UpdateID()), 10)
	var items []string
	total := 0
	switch args["BrowseFlag"] {
	case "BrowseMetadata":
		if p == "/" {
			items = append(items, m.container("/", m.cfg.Name+" USB"))
		} else {
			parent, err := m.lib.List(path.Dir(p))
			if err != nil {
				return nil, &upnpError{701, "No such object"}
			}
			found := false
			for _, e := range parent {
				if e.Path == p {
					items, found = append(items, m.object(e, host)), true
					break
				}
			}
			if !found {
				return nil, &upnpError{701, "No such object"}
			}
		}
		total = 1
	case "BrowseDirectChildren":
		list, err := m.lib.List(p)
		if err != nil {
			return nil, &upnpError{701, "No such object"}
		}
		total = len(list)
		end := total
		if count > 0 {
			end = min(start+count, total)
		}
		for i := min(start, total); i < end; i++ {
			items = append(items, m.object(list[i], host))
		}
	default:
		return nil, errInvalidArgs
	}
	didl := `<DIDL-Lite xmlns="urn:schemas-upnp-org:metadata-1-0/DIDL-Lite/" xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:upnp="urn:schemas-upnp-org:metadata-1-0/upnp/" xmlns:dlna="urn:schemas-dlna-org:metadata-1-0/">` +
		strings.Join(items, "") + `</DIDL-Lite>`
	return kv{{"Result", didl}, {"NumberReturned", strconv.Itoa(len(items))}, {"TotalMatches", strconv.Itoa(total)}, {"UpdateID", upd}}, nil
}

func esc(s string) string { return html.EscapeString(s) }

func (m *mediaServer) container(p, title string) string {
	return fmt.Sprintf(`<container id="%s" parentID="%s" restricted="1" searchable="0"><dc:title>%s</dc:title><upnp:class>object.container.storageFolder</upnp:class></container>`,
		esc(pathToID(p)), esc(parentID(p)), esc(title))
}

// duration formats H:MM:SS.mmm (DIDL res@duration).
func duration(d time.Duration) string {
	ms := d.Milliseconds()
	return fmt.Sprintf("%d:%02d:%02d.%03d", ms/3600000, ms/60000%60, ms/1000%60, ms%1000)
}

func mediaToken(p string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(p)) + path.Ext(p)
}

func (m *mediaServer) object(e Entry, host string) string {
	if e.Dir {
		return m.container(e.Path, e.Name)
	}
	mime := mimeOf(e.Path)
	name, length := m.lib.Meta(e.Path)
	title := strings.TrimSuffix(e.Name, path.Ext(e.Name))
	var artist string
	if name != "" {
		title = name
		if a, t, ok := strings.Cut(name, " - "); ok {
			artist, title = a, t
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, `<item id="%s" parentID="%s" restricted="1"><dc:title>%s</dc:title>`, esc(pathToID(e.Path)), esc(parentID(e.Path)), esc(title))
	if artist != "" {
		fmt.Fprintf(&b, `<upnp:artist>%s</upnp:artist><dc:creator>%s</dc:creator>`, esc(artist), esc(artist))
	}
	b.WriteString(`<upnp:class>object.item.audioItem.musicTrack</upnp:class>`)
	fmt.Fprintf(&b, `<res protocolInfo="http-get:*:%s:%s" size="%d"`, mime, dlnaFlags, e.Size)
	if length > 0 {
		fmt.Fprintf(&b, ` duration="%s"`, duration(length))
	}
	fmt.Fprintf(&b, `>http://%s/dlna/media/%s</res></item>`, esc(host), mediaToken(e.Path))
	return b.String()
}

// serveMedia streams one file (ranges supported for seeking).
func (m *mediaServer) serveMedia(w http.ResponseWriter, r *http.Request, token string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	enc, _, _ := strings.Cut(token, ".")
	raw, err := base64.RawURLEncoding.DecodeString(enc)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	p := string(raw)
	if mimeOf(p) == "" {
		http.NotFound(w, r)
		return
	}
	f, err := m.lib.Open(p)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", mimeOf(p))
	w.Header().Set("transferMode.dlna.org", "Streaming")
	w.Header().Set("contentFeatures.dlna.org", dlnaFlags)
	http.ServeContent(w, r, "", fi.ModTime(), f)
}
