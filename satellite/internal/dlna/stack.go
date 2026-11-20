package dlna

import (
	"context"
	"crypto/rand"
	"crypto/sha1"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

func randRead(b []byte) { rand.Read(b) }

// stableUUID makes a device UUID from a seed (the clock's MAC and the device
// kind), so control points recognise the device after a restart.
func stableUUID(seed string) string {
	h := sha1.Sum([]byte("clockd-dlna:" + seed))
	h[6] = h[6]&0x0f | 0x50
	h[8] = h[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", h[0:4], h[4:6], h[6:8], h[8:10], h[10:16])
}

// Config describes the clock to the network.
type Config struct {
	Name         string // friendly name, e.g. "Bedroom clock"
	Manufacturer string
	Model        string
	Version      string // clockd's, for the SERVER header
	Seed         string // stable per clock (its MAC)
	Iface        string // where SSDP runs ("wlan0")
	Port         int    // HTTP port for descriptions, control, events and media
	Log          *slog.Logger
}

// Stack is the HTTP server and SSDP for the devices that are switched on.
type Stack struct {
	cfg  Config
	ssdp *ssdp
	ln   net.Listener
	srv  *http.Server

	mu       sync.Mutex
	server   *mediaServer
	renderer *renderer
}

// Start listens on cfg.Port and starts SSDP (no device yet).
func Start(cfg Config) (*Stack, error) {
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	ln, err := net.Listen("tcp4", fmt.Sprintf(":%d", cfg.Port))
	if err != nil {
		return nil, err
	}
	s := &Stack{cfg: cfg, ln: ln}
	if cfg.Port == 0 {
		s.cfg.Port = ln.Addr().(*net.TCPAddr).Port
	}
	s.ssdp = &ssdp{log: cfg.Log, port: s.cfg.Port, server: "Linux UPnP/1.0 clockd/" + cfg.Version}
	s.srv = &http.Server{Handler: http.HandlerFunc(s.serve), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second}
	go s.srv.Serve(ln)
	if cfg.Iface != "" {
		if err := s.ssdp.start(cfg.Iface); err != nil {
			s.srv.Close()
			return nil, err
		}
	}
	return s, nil
}

// Port is the HTTP port in use.
func (s *Stack) Port() int { return s.cfg.Port }

// Close says goodbye on SSDP and stops everything.
func (s *Stack) Close() {
	s.ssdp.close()
	s.mu.Lock()
	if s.renderer != nil {
		s.renderer.close()
	}
	if s.server != nil {
		s.server.ev.closeAll()
	}
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	s.srv.Shutdown(ctx)
}

// SetLibrary switches the media server on (lib != nil) or off.
func (s *Stack) SetLibrary(lib Library) {
	s.mu.Lock()
	if lib == nil {
		if s.server != nil {
			s.server.ev.closeAll()
		}
		s.server = nil
	} else if s.server == nil {
		s.server = newMediaServer(s.cfg, lib)
	}
	s.mu.Unlock()
	s.readvertise()
}

// SetPlayer switches the renderer on (p != nil) or off.
func (s *Stack) SetPlayer(p Player) {
	s.mu.Lock()
	if p == nil {
		if s.renderer != nil {
			s.renderer.close()
		}
		s.renderer = nil
	} else if s.renderer == nil {
		s.renderer = newRenderer(s.cfg, p)
	}
	s.mu.Unlock()
	s.readvertise()
}

// Notify tells the renderer that playback may have changed (stopped by
// something else, volume moved): it checks and sends events.
func (s *Stack) Notify() {
	s.mu.Lock()
	r := s.renderer
	s.mu.Unlock()
	if r != nil {
		r.refresh()
	}
}

func (s *Stack) readvertise() {
	s.mu.Lock()
	var ads []advert
	if s.server != nil {
		ads = append(ads, s.server.dev.advert())
	}
	if s.renderer != nil {
		ads = append(ads, s.renderer.dev.advert())
	}
	s.mu.Unlock()
	s.ssdp.setAdverts(ads)
}

// Status for the web UI.
func (s *Stack) Status() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := map[string]any{"port": s.cfg.Port, "server": s.server != nil, "renderer": s.renderer != nil}
	if s.renderer != nil {
		m["renderer_state"] = s.renderer.transportState()
	}
	return m
}

func (s *Stack) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	srv, ren := s.server, s.renderer
	s.mu.Unlock()
	p := r.URL.Path
	switch {
	case srv != nil && strings.HasPrefix(p, "/dlna/server/"):
		srv.serve(w, r, strings.TrimPrefix(p, "/dlna/server"))
	case srv != nil && strings.HasPrefix(p, "/dlna/media/"):
		srv.serveMedia(w, r, strings.TrimPrefix(p, "/dlna/media/"))
	case ren != nil && strings.HasPrefix(p, "/dlna/renderer/"):
		ren.serve(w, r, strings.TrimPrefix(p, "/dlna/renderer"))
	default:
		http.NotFound(w, r)
	}
}

// serveDevice handles the description, SCPDs, control and event URLs of one
// device. rest is the path below /dlna/<device>.
func serveDevice(w http.ResponseWriter, r *http.Request, rest string, dev *device, manufacturer string,
	control func(svc *service, act string, args map[string]string, r *http.Request) (kv, error), events map[*service]*eventer) {
	if rest == "/desc.xml" {
		w.Header().Set("Content-Type", `text/xml; charset="utf-8"`)
		io.WriteString(w, dev.description(manufacturer))
		return
	}
	for _, svc := range dev.services {
		base := strings.TrimPrefix(svc.path, dev.descPath[:strings.LastIndex(dev.descPath, "/")])
		switch rest {
		case base + ".xml":
			w.Header().Set("Content-Type", `text/xml; charset="utf-8"`)
			io.WriteString(w, svc.scpd())
			return
		case base + "/event":
			if ev := events[svc]; ev != nil {
				ev.handle(w, r)
			} else {
				http.Error(w, "no events", http.StatusNotFound)
			}
			return
		case base + "/control":
			if r.Method != http.MethodPost {
				http.Error(w, "POST only", http.StatusMethodNotAllowed)
				return
			}
			act, args, err := parseSOAP(r.Body)
			if err != nil {
				writeFault(w, errInvalidAction)
				return
			}
			if h := strings.Trim(r.Header.Get("SOAPACTION"), `"`); h != "" && !strings.HasSuffix(h, "#"+act) {
				writeFault(w, errInvalidAction)
				return
			}
			outs, err := control(svc, act, args, r)
			if err != nil {
				writeFault(w, err)
				return
			}
			writeSOAP(w, svc.typ, act, outs)
			return
		}
	}
	http.NotFound(w, r)
}

// ---- ConnectionManager (both devices) ----

func connectionManager(path string) *service {
	return &service{typ: "urn:schemas-upnp-org:service:ConnectionManager:1", id: "urn:upnp-org:serviceId:ConnectionManager", path: path,
		actions: []action{
			{"GetProtocolInfo", []arg{out("Source", "SourceProtocolInfo"), out("Sink", "SinkProtocolInfo")}},
			{"GetCurrentConnectionIDs", []arg{out("ConnectionIDs", "CurrentConnectionIDs")}},
			{"GetCurrentConnectionInfo", []arg{in("ConnectionID", "A_ARG_TYPE_ConnectionID"),
				out("RcsID", "A_ARG_TYPE_RcsID"), out("AVTransportID", "A_ARG_TYPE_AVTransportID"),
				out("ProtocolInfo", "A_ARG_TYPE_ProtocolInfo"), out("PeerConnectionManager", "A_ARG_TYPE_ConnectionManager"),
				out("PeerConnectionID", "A_ARG_TYPE_ConnectionID"), out("Direction", "A_ARG_TYPE_Direction"),
				out("Status", "A_ARG_TYPE_ConnectionStatus")}},
		},
		vars: []stateVar{
			{name: "SourceProtocolInfo", typ: "string", events: true},
			{name: "SinkProtocolInfo", typ: "string", events: true},
			{name: "CurrentConnectionIDs", typ: "string", events: true},
			{name: "A_ARG_TYPE_ConnectionStatus", typ: "string", allowed: []string{"OK", "ContentFormatMismatch", "InsufficientBandwidth", "UnreliableChannel", "Unknown"}},
			{name: "A_ARG_TYPE_ConnectionManager", typ: "string"},
			{name: "A_ARG_TYPE_Direction", typ: "string", allowed: []string{"Input", "Output"}},
			{name: "A_ARG_TYPE_ProtocolInfo", typ: "string"},
			{name: "A_ARG_TYPE_ConnectionID", typ: "i4"},
			{name: "A_ARG_TYPE_AVTransportID", typ: "i4"},
			{name: "A_ARG_TYPE_RcsID", typ: "i4"},
		}}
}

func connectionManagerControl(act, source, sink string) (kv, error) {
	switch act {
	case "GetProtocolInfo":
		return kv{{"Source", source}, {"Sink", sink}}, nil
	case "GetCurrentConnectionIDs":
		return kv{{"ConnectionIDs", "0"}}, nil
	case "GetCurrentConnectionInfo":
		dir := "Output"
		if sink != "" {
			dir = "Input"
		}
		return kv{{"RcsID", "0"}, {"AVTransportID", "0"}, {"ProtocolInfo", ""}, {"PeerConnectionManager", ""},
			{"PeerConnectionID", "-1"}, {"Direction", dir}, {"Status", "OK"}}, nil
	}
	return nil, errInvalidAction
}

var errNotFound = errors.New("not found")
