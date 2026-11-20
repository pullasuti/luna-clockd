package web

import (
	"crypto/subtle"
	"errors"
	"sync"
	"time"
)

// SetupConfirm is the button press that opens the setup page. The setup
// hotspot can be joined by anyone in radio range, so the page may scan and
// send a network only after someone at the clock has held the action button:
// the page asks, the face shows a code (the page shows the same one), the
// button confirms. Like the web login, with its own short-lived state: a
// confirmed page gets no session and nothing outside the setup endpoints.
//
// On a hotspot without a password this does not hide what the page sends; it
// only keeps a stranger from pointing the clock at a network of their own.
type SetupConfirm struct {
	// ShowCode is called with the code to display ("" = clear), under the lock:
	// it must be quick and must not call back into SetupConfirm.
	ShowCode func(code string)

	mu   sync.Mutex
	pend *setupAsk // waiting for the button
	ok   *setupAsk // confirmed
}

type setupAsk struct {
	id, code, ip string
	at           time.Time // asked, then confirmed
}

const (
	setupAskTTL = 60 * time.Second
	setupOKTTL  = 15 * time.Minute // a confirmed page; also ends when the hotspot stops
)

func (c *SetupConfirm) expireLocked(now time.Time) {
	if c.pend != nil && now.Sub(c.pend.at) > setupAskTTL {
		c.pend = nil
		c.show("")
	}
	if c.ok != nil && now.Sub(c.ok.at) > setupOKTTL {
		c.ok = nil
	}
}

// show runs under c.mu, in order: separate goroutines could let a late "show
// the code" land after its "clear" and leave the code on the face.
func (c *SetupConfirm) show(code string) {
	if c.ShowCode != nil {
		c.ShowCode(code)
	}
}

// Ask starts waiting for the button (one page at a time).
func (c *SetupConfirm) Ask(ip string) (id, code string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.expireLocked(time.Now())
	if c.pend != nil {
		return "", "", errors.New("another device is waiting for the button; try again in a minute")
	}
	c.pend = &setupAsk{id: randHex(16), code: newCode(), ip: ip, at: time.Now()}
	c.show(c.pend.code)
	return c.pend.id, c.pend.code, nil
}

// Pending reports whether a page waits for the button.
func (c *SetupConfirm) Pending() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.expireLocked(time.Now())
	return c.pend != nil
}

// Approve is the button: the waiting page is confirmed (and replaces an
// earlier one).
func (c *SetupConfirm) Approve() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.expireLocked(time.Now())
	if c.pend == nil {
		return false
	}
	c.ok, c.pend = c.pend, nil
	c.ok.at = time.Now()
	c.show("")
	return true
}

// Reset forgets everything (the hotspot stopped).
func (c *SetupConfirm) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pend != nil {
		c.show("")
	}
	c.pend, c.ok = nil, nil
}

// State is "confirmed", "waiting" or "" for the page with this id, from ip.
func (c *SetupConfirm) State(id, ip string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.expireLocked(time.Now())
	is := func(a *setupAsk) bool {
		return a != nil && id != "" && a.ip == ip && subtle.ConstantTimeCompare([]byte(a.id), []byte(id)) == 1
	}
	switch {
	case is(c.ok):
		return "confirmed"
	case is(c.pend):
		return "waiting"
	}
	return ""
}
