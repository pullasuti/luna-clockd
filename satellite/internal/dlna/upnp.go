package dlna

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---- service descriptions (SCPD) ----

type arg struct{ name, dir, rel string }

type action struct {
	name string
	args []arg
}

type stateVar struct {
	name, typ string
	events    bool
	allowed   []string
	rng       [2]int // allowedValueRange min, max (when rng[1] > 0)
}

type service struct {
	typ, id, path string // urn:schemas-upnp-org:service:X:1, urn:upnp-org:serviceId:X, /dlna/<dev>/<X>
	actions       []action
	vars          []stateVar
}

func in(name, rel string) arg  { return arg{name, "in", rel} }
func out(name, rel string) arg { return arg{name, "out", rel} }

func (s *service) scpd() string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="utf-8"?>` + "\n" +
		`<scpd xmlns="urn:schemas-upnp-org:service-1-0"><specVersion><major>1</major><minor>0</minor></specVersion><actionList>`)
	for _, a := range s.actions {
		b.WriteString("<action><name>" + a.name + "</name><argumentList>")
		for _, x := range a.args {
			fmt.Fprintf(&b, "<argument><name>%s</name><direction>%s</direction><relatedStateVariable>%s</relatedStateVariable></argument>", x.name, x.dir, x.rel)
		}
		b.WriteString("</argumentList></action>")
	}
	b.WriteString("</actionList><serviceStateTable>")
	for _, v := range s.vars {
		ev := "no"
		if v.events {
			ev = "yes"
		}
		fmt.Fprintf(&b, `<stateVariable sendEvents="%s"><name>%s</name><dataType>%s</dataType>`, ev, v.name, v.typ)
		if len(v.allowed) > 0 {
			b.WriteString("<allowedValueList>")
			for _, a := range v.allowed {
				b.WriteString("<allowedValue>" + a + "</allowedValue>")
			}
			b.WriteString("</allowedValueList>")
		}
		if v.rng[1] > 0 {
			fmt.Fprintf(&b, "<allowedValueRange><minimum>%d</minimum><maximum>%d</maximum><step>1</step></allowedValueRange>", v.rng[0], v.rng[1])
		}
		b.WriteString("</stateVariable>")
	}
	b.WriteString("</serviceStateTable></scpd>")
	return b.String()
}

// ---- device description ----

type device struct {
	uuid, typ, name, model string
	services               []*service
	descPath               string
}

func (d *device) description(manufacturer string) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="utf-8"?>` + "\n" +
		`<root xmlns="urn:schemas-upnp-org:device-1-0" xmlns:dlna="urn:schemas-dlna-org:device-1-0"><specVersion><major>1</major><minor>0</minor></specVersion><device>`)
	fmt.Fprintf(&b, "<deviceType>%s</deviceType><friendlyName>%s</friendlyName><manufacturer>%s</manufacturer><modelName>%s</modelName><modelNumber>1</modelNumber><UDN>uuid:%s</UDN>",
		d.typ, html.EscapeString(d.name), html.EscapeString(manufacturer), html.EscapeString(d.model), d.uuid)
	if strings.Contains(d.typ, "MediaServer") {
		b.WriteString(`<dlna:X_DLNADOC xmlns:dlna="urn:schemas-dlna-org:device-1-0">DMS-1.50</dlna:X_DLNADOC>`)
	} else {
		b.WriteString(`<dlna:X_DLNADOC xmlns:dlna="urn:schemas-dlna-org:device-1-0">DMR-1.50</dlna:X_DLNADOC>`)
	}
	b.WriteString("<serviceList>")
	for _, s := range d.services {
		fmt.Fprintf(&b, "<service><serviceType>%s</serviceType><serviceId>%s</serviceId><SCPDURL>%s.xml</SCPDURL><controlURL>%s/control</controlURL><eventSubURL>%s/event</eventSubURL></service>",
			s.typ, s.id, s.path, s.path, s.path)
	}
	b.WriteString("</serviceList></device></root>")
	return b.String()
}

func (d *device) advert() advert {
	a := advert{uuid: d.uuid, descPath: d.descPath, types: []string{d.typ}}
	for _, s := range d.services {
		a.types = append(a.types, s.typ)
	}
	return a
}

// ---- SOAP ----

// upnpError is a SOAP fault with a UPnP error code.
type upnpError struct {
	code int
	desc string
}

func (e *upnpError) Error() string { return fmt.Sprintf("UPnP error %d: %s", e.code, e.desc) }

var (
	errInvalidAction = &upnpError{401, "Invalid Action"}
	errInvalidArgs   = &upnpError{402, "Invalid Args"}
)

// parseSOAP reads the action name and its arguments from a SOAP request.
func parseSOAP(r io.Reader) (string, map[string]string, error) {
	dec := xml.NewDecoder(io.LimitReader(r, 256<<10))
	depth, bodyDepth := 0, -1
	var actionName, cur string
	var text strings.Builder
	args := map[string]string{}
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			switch {
			case t.Name.Local == "Body" && bodyDepth < 0:
				bodyDepth = depth
			case bodyDepth > 0 && depth == bodyDepth+1:
				actionName = t.Name.Local
			case bodyDepth > 0 && depth == bodyDepth+2:
				cur = t.Name.Local
				text.Reset()
			}
		case xml.CharData:
			if cur != "" {
				text.Write(t)
			}
		case xml.EndElement:
			if bodyDepth > 0 && depth == bodyDepth+2 && cur != "" {
				args[cur] = text.String()
				cur = ""
			}
			depth--
		}
	}
	if actionName == "" {
		return "", nil, errors.New("no action in the SOAP body")
	}
	return actionName, args, nil
}

// kv keeps the out arguments in order.
type kv [][2]string

func writeSOAP(w http.ResponseWriter, serviceType, act string, outs kv) {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="utf-8"?>` + "\n" +
		`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/"><s:Body>`)
	fmt.Fprintf(&b, `<u:%sResponse xmlns:u="%s">`, act, serviceType)
	for _, o := range outs {
		b.WriteString("<" + o[0] + ">")
		xml.EscapeText(&b, []byte(o[1]))
		b.WriteString("</" + o[0] + ">")
	}
	fmt.Fprintf(&b, "</u:%sResponse></s:Body></s:Envelope>", act)
	w.Header().Set("Content-Type", `text/xml; charset="utf-8"`)
	w.Header().Set("EXT", "")
	io.WriteString(w, b.String())
}

func writeFault(w http.ResponseWriter, err error) {
	ue, ok := err.(*upnpError)
	if !ok {
		ue = &upnpError{501, err.Error()}
	}
	var desc bytes.Buffer
	xml.EscapeText(&desc, []byte(ue.desc))
	w.Header().Set("Content-Type", `text/xml; charset="utf-8"`)
	w.WriteHeader(http.StatusInternalServerError)
	fmt.Fprintf(w, `<?xml version="1.0" encoding="utf-8"?>`+"\n"+
		`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/"><s:Body><s:Fault><faultcode>s:Client</faultcode><faultstring>UPnPError</faultstring><detail><UPnPError xmlns="urn:schemas-upnp-org:control-1-0"><errorCode>%d</errorCode><errorDescription>%s</errorDescription></UPnPError></detail></s:Fault></s:Body></s:Envelope>`,
		ue.code, desc.String())
}

// ---- GENA eventing ----

const (
	maxSubs    = 16
	subTimeout = 1800 * time.Second
)

type subscriber struct {
	sid      string
	callback *url.URL
	seq      uint32
	expires  time.Time
	queue    chan string // property sets to send, in order
}

// eventer keeps one service's subscriptions and sends them its events.
type eventer struct {
	mu   sync.Mutex
	subs map[string]*subscriber
	// initial gives the evented variables' current values (name, value) for
	// a new subscriber's first event
	initial func() kv
	client  *http.Client
}

func newEventer(initial func() kv) *eventer {
	return &eventer{subs: map[string]*subscriber{}, initial: initial,
		client: &http.Client{Timeout: 5 * time.Second}}
}

func propertySet(vars kv) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="utf-8"?>` + "\n" + `<e:propertyset xmlns:e="urn:schemas-upnp-org:event-1-0">`)
	for _, v := range vars {
		b.WriteString("<e:property><" + v[0] + ">")
		xml.EscapeText(&b, []byte(v[1]))
		b.WriteString("</" + v[0] + "></e:property>")
	}
	b.WriteString("</e:propertyset>")
	return b.String()
}

// handle serves SUBSCRIBE and UNSUBSCRIBE on a service's event URL.
func (e *eventer) handle(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "SUBSCRIBE":
		timeout := subTimeout
		if t := strings.TrimPrefix(r.Header.Get("TIMEOUT"), "Second-"); t != "" && t != "infinite" {
			if n, err := strconv.Atoi(t); err == nil && n >= 60 {
				timeout = min(time.Duration(n)*time.Second, subTimeout)
			}
		}
		if sid := r.Header.Get("SID"); sid != "" { // renewal
			e.mu.Lock()
			s := e.subs[sid]
			if s != nil {
				s.expires = time.Now().Add(timeout)
			}
			e.mu.Unlock()
			if s == nil {
				http.Error(w, "no such subscription", http.StatusPreconditionFailed)
				return
			}
			w.Header().Set("SID", sid)
			w.Header().Set("TIMEOUT", fmt.Sprintf("Second-%d", int(timeout.Seconds())))
			return
		}
		if r.Header.Get("NT") != "upnp:event" {
			http.Error(w, "NT must be upnp:event", http.StatusPreconditionFailed)
			return
		}
		cb := parseCallback(r.Header.Get("CALLBACK"))
		// the callback must be the subscriber itself: no relaying to others
		peer, _, _ := net.SplitHostPort(r.RemoteAddr)
		if cb == nil || cb.Hostname() != peer {
			http.Error(w, "callback must be http:// to the subscriber's own address", http.StatusPreconditionFailed)
			return
		}
		s := &subscriber{sid: "uuid:" + newUUID(), callback: cb, expires: time.Now().Add(timeout), queue: make(chan string, 16)}
		e.mu.Lock()
		e.expireLocked()
		if len(e.subs) >= maxSubs {
			e.mu.Unlock()
			http.Error(w, "too many subscriptions", http.StatusServiceUnavailable)
			return
		}
		e.subs[s.sid] = s
		e.mu.Unlock()
		go e.sender(s)
		w.Header().Set("SID", s.sid)
		w.Header().Set("TIMEOUT", fmt.Sprintf("Second-%d", int(timeout.Seconds())))
		w.Header().Set("SERVER", "Linux UPnP/1.0 clockd")
		// the initial event follows the response
		if e.initial != nil {
			s.queue <- propertySet(e.initial())
		}
	case "UNSUBSCRIBE":
		e.mu.Lock()
		s := e.subs[r.Header.Get("SID")]
		if s != nil {
			delete(e.subs, s.sid)
			close(s.queue)
		}
		e.mu.Unlock()
		if s == nil {
			http.Error(w, "no such subscription", http.StatusPreconditionFailed)
		}
	default:
		w.Header().Set("Allow", "SUBSCRIBE, UNSUBSCRIBE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func parseCallback(h string) *url.URL {
	h = strings.TrimSpace(h)
	if !strings.HasPrefix(h, "<") {
		return nil
	}
	end := strings.Index(h, ">")
	if end < 0 {
		return nil
	}
	u, err := url.Parse(h[1:end])
	if err != nil || u.Scheme != "http" || u.Host == "" {
		return nil
	}
	return u
}

func (e *eventer) expireLocked() {
	now := time.Now()
	for id, s := range e.subs {
		if now.After(s.expires) {
			delete(e.subs, id)
			close(s.queue)
		}
	}
}

// send queues an event to every subscriber (dropped for one whose queue is full).
func (e *eventer) send(vars kv) {
	body := propertySet(vars)
	e.mu.Lock()
	defer e.mu.Unlock()
	e.expireLocked()
	for _, s := range e.subs {
		select {
		case s.queue <- body:
		default:
		}
	}
}

func (e *eventer) closeAll() {
	e.mu.Lock()
	defer e.mu.Unlock()
	for id, s := range e.subs {
		delete(e.subs, id)
		close(s.queue)
	}
}

func (e *eventer) sender(s *subscriber) {
	for body := range s.queue {
		req, err := http.NewRequest("NOTIFY", s.callback.String(), strings.NewReader(body))
		if err != nil {
			continue
		}
		req.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
		req.Header.Set("NT", "upnp:event")
		req.Header.Set("NTS", "upnp:propchange")
		req.Header.Set("SID", s.sid)
		req.Header.Set("SEQ", strconv.FormatUint(uint64(s.seq), 10))
		s.seq++
		if resp, err := e.client.Do(req); err == nil {
			io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
		}
	}
}

func newUUID() string {
	var b [16]byte
	randRead(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
