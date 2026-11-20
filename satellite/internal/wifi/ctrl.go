package wifi

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// CtrlSocket is where the vendor wpa_supplicant listens on these clocks. The
// config says ctrl_interface=/data/wifi, but this is an Android build: it takes
// the socket init made for the service ("socket wpa_wlan0 dgram 660 wifi wifi"),
// so nothing is ever created under /data/wifi. Same on retail 1.52 and eng 1.56.
const CtrlSocket = "/dev/socket/wpa_wlan0"

// Event is an unsolicited message from wpa_supplicant ("<3>CTRL-EVENT-...").
// Attached is our own: the client (re)attached, events before it may be missed.
type Event struct {
	Level int
	Name  string // first word, e.g. CTRL-EVENT-CONNECTED
	Text  string // whole message without the level
}

const Attached = "CLOCKD-ATTACHED"

// Arg returns the value of a key=value word in the event text ("" if absent);
// brackets and quotes around it are dropped.
func (e Event) Arg(key string) string {
	for _, w := range strings.Fields(e.Text) {
		w = strings.Trim(w, "[]")
		if v, ok := strings.CutPrefix(w, key+"="); ok {
			return strings.Trim(v, `"`)
		}
	}
	return ""
}

func parseEvent(s string) Event {
	e := Event{}
	if strings.HasPrefix(s, "IFNAME=") { // global interface form: "IFNAME=wlan0 <3>..."
		if _, rest, ok := strings.Cut(s, " "); ok {
			s = rest
		}
	}
	if strings.HasPrefix(s, "<") {
		if i := strings.IndexByte(s, '>'); i > 0 {
			e.Level, _ = strconv.Atoi(s[1:i])
			s = s[i+1:]
		}
	}
	e.Text = strings.TrimSpace(s)
	e.Name, _, _ = strings.Cut(e.Text, " ")
	return e
}

// Ctrl is a client for wpa_supplicant's control interface: a unix datagram
// socket taking text commands (what wpa_cli speaks). After ATTACH the same
// socket also receives the events; replies and events are told apart by the
// leading '<'. It dials on first use and again after an error, so a
// restarted wpa_supplicant is picked up by the next request or Watch round.
type Ctrl struct {
	Server string // default CtrlSocket
	// Local is the address our end binds to: wpa_supplicant answers to it, as
	// the wifi user. Empty: a name in the abstract namespace (no file, so no
	// permissions to set and nothing left behind). Tests use a path.
	Local   string
	Timeout time.Duration // per request; default 5 s

	mu      sync.Mutex // one request at a time
	conn    *net.UnixConn
	replies chan string

	subMu sync.Mutex
	subs  map[chan Event]struct{}
}

var ctrlSeq atomic.Int32

func (c *Ctrl) dial() error {
	server := c.Server
	if server == "" {
		server = CtrlSocket
	}
	local := c.Local
	if local == "" {
		local = fmt.Sprintf("@clockd-wpa-%d-%d", os.Getpid(), ctrlSeq.Add(1))
	} else {
		os.Remove(local)
	}
	conn, err := net.DialUnix("unixgram", &net.UnixAddr{Name: local, Net: "unixgram"}, &net.UnixAddr{Name: server, Net: "unixgram"})
	if err != nil {
		return fmt.Errorf("wpa_supplicant control socket: %w", err)
	}
	replies := make(chan string, 1)
	c.conn, c.replies = conn, replies
	go c.read(conn, replies)
	if out, err := c.roundTrip("ATTACH"); err != nil || out != "OK" {
		c.drop()
		if err == nil {
			err = fmt.Errorf("ATTACH: %s", out)
		}
		return fmt.Errorf("wpa_supplicant control socket: %w", err)
	}
	c.publish(Event{Name: Attached, Text: Attached})
	return nil
}

func (c *Ctrl) read(conn *net.UnixConn, replies chan string) {
	buf := make([]byte, 64<<10)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			close(replies)
			return
		}
		s := string(buf[:n])
		if strings.HasPrefix(s, "<") || strings.HasPrefix(s, "IFNAME=") {
			c.publish(parseEvent(s))
			continue
		}
		select {
		case replies <- s:
		default: // a late reply to a request that already timed out
		}
	}
}

func (c *Ctrl) drop() {
	if c.conn != nil {
		c.conn.Close()
		if c.Local != "" {
			os.Remove(c.Local)
		}
		c.conn = nil
	}
}

func (c *Ctrl) roundTrip(cmd string) (string, error) {
	select { // a late reply left over from before
	case _, ok := <-c.replies:
		if !ok {
			return "", errors.New("connection closed")
		}
	default:
	}
	if _, err := c.conn.Write([]byte(cmd)); err != nil {
		return "", err
	}
	to := c.Timeout
	if to == 0 {
		to = 5 * time.Second
	}
	select {
	case out, ok := <-c.replies:
		if !ok {
			return "", errors.New("connection closed")
		}
		return strings.TrimSpace(out), nil
	case <-time.After(to):
		return "", errors.New("no reply")
	}
}

// Request sends one command (e.g. "STATUS", "SET_NETWORK 1 ssid 686f6d65")
// and returns the reply. A "FAIL..." reply is not an error here.
func (c *Ctrl) Request(cmd string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		if err := c.dial(); err != nil {
			return "", err
		}
	}
	out, err := c.roundTrip(cmd)
	if err != nil {
		c.drop() // dial again next time
		return "", fmt.Errorf("wpa_supplicant %s: %w", firstWord(cmd), err)
	}
	return out, nil
}

// Close detaches and closes the socket.
func (c *Ctrl) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		c.roundTrip("DETACH")
		c.drop()
	}
}

// Subscribe returns a channel of events and a function that ends it. A
// subscriber that falls behind loses events rather than blocking the reader.
func (c *Ctrl) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 64)
	c.subMu.Lock()
	if c.subs == nil {
		c.subs = map[chan Event]struct{}{}
	}
	c.subs[ch] = struct{}{}
	c.subMu.Unlock()
	return ch, func() {
		c.subMu.Lock()
		delete(c.subs, ch)
		c.subMu.Unlock()
	}
}

func (c *Ctrl) publish(e Event) {
	c.subMu.Lock()
	for ch := range c.subs {
		select {
		case ch <- e:
		default:
		}
	}
	c.subMu.Unlock()
}

// Watch keeps the event subscription alive until stop closes: events only
// arrive while attached, so it pings every interval, which dials (and
// attaches) again after wpa_supplicant restarted or wasn't up yet.
func (c *Ctrl) Watch(stop <-chan struct{}, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		c.Request("PING")
		select {
		case <-stop:
			return
		case <-t.C:
		}
	}
}

func firstWord(s string) string {
	w, _, _ := strings.Cut(s, " ")
	return w
}
