package main

import (
	"encoding/json"
	"log/slog"
	"math"
	"net"
	"sort"
	"sync"
	"time"
)

// Wake word vote: with several clocks in one room, the one that heard the
// wake word best answers. Home Assistant already lets only one run through
// (the first wake-up per phrase within 2 s; the others get
// duplicate_wake_up_detected and end quietly), but "first" is whichever
// clock's request arrived first, not the nearest one.
//
// On a detection each clock sends a claim (how clearly it heard the word)
// by UDP to the other clocks it knows from mDNS, waits voteWindow, and
// starts the turn only if no claim for the same phrase was clearer. Claims
// count from voteBefore before our own detection, so the order in which the
// clocks detect does not matter. Anything lost (a packet, an old clockd, a
// clock with the vote off) falls back to Home Assistant's first-wins rule:
// never two answers, at worst the old choice of clock.
//
// "Clearly" is the speech level over the clock's own background noise
// (wakeLevel): the nearer clock hears the word louder over the same room.
// Both levels pass the same gains, so the mic gain setting cancels out.

const (
	votePort   = 6054                   // UDP, next to the ESPHome API's 6053
	voteWindow = 200 * time.Millisecond // how long a clock waits for the others' claims
	voteBefore = time.Second            // claims this much older than our detection still count
	voteKeep   = 3 * time.Second        // claims kept this long
)

// voteClaim is one clock's report of a detection.
type voteClaim struct {
	V      int     `json:"v"`
	ID     string  `json:"id"`
	Phrase string  `json:"phrase"`
	SNR    float64 `json:"snr"`   // dB, speech over the room's noise
	Score  float64 `json:"score"` // the wake word model's score
	at     time.Time
}

// better reports whether claim a beats b: clearer, then higher score, then
// the smaller id (any fixed order works: every clock decides the same way).
func (a voteClaim) better(b voteClaim) bool {
	if math.Abs(a.SNR-b.SNR) >= 0.5 {
		return a.SNR > b.SNR
	}
	if a.Score != b.Score {
		return a.Score > b.Score
	}
	return a.ID < b.ID
}

type wakeVote struct {
	self  string
	log   *slog.Logger
	peers func() []string // IPv4 addresses of the other clocks seen lately
	conn  *net.UDPConn

	mu     sync.Mutex
	claims []voteClaim
}

func newWakeVote(self string, log *slog.Logger, peers func() []string) *wakeVote {
	return &wakeVote{self: self, log: log, peers: peers}
}

// listen receives the other clocks' claims (only from clocks found by mDNS).
func (w *wakeVote) listen() error {
	c, err := net.ListenUDP("udp4", &net.UDPAddr{Port: votePort})
	if err != nil {
		return err
	}
	w.conn = c
	go func() {
		buf := make([]byte, 512)
		for {
			n, from, err := c.ReadFromUDP(buf)
			if err != nil {
				return
			}
			known := false
			for _, ip := range w.peers() {
				if ip == from.IP.String() {
					known = true
					break
				}
			}
			var cl voteClaim
			if !known || json.Unmarshal(buf[:n], &cl) != nil || cl.V != 1 || cl.ID == "" || cl.ID == w.self {
				continue
			}
			w.add(cl, time.Now())
		}
	}()
	return nil
}

func (w *wakeVote) add(cl voteClaim, now time.Time) {
	cl.at = now
	w.mu.Lock()
	defer w.mu.Unlock()
	keep := w.claims[:0]
	for _, c := range w.claims {
		if now.Sub(c.at) < voteKeep {
			keep = append(keep, c)
		}
	}
	w.claims = append(keep, cl)
}

// beaten returns the clearest claim for phrase received since the given time
// that beats ours.
func (w *wakeVote) beaten(mine voteClaim, since time.Time) (voteClaim, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	var best voteClaim
	found := false
	for _, c := range w.claims {
		if c.Phrase != mine.Phrase || c.at.Before(since) || !c.better(mine) {
			continue
		}
		if !found || c.better(best) {
			best, found = c, true
		}
	}
	return best, found
}

// decide sends our claim and reports whether this clock should answer. It
// blocks for voteWindow when other clocks are around.
func (w *wakeVote) decide(phrase string, snr, score float64) bool {
	now := time.Now()
	mine := voteClaim{V: 1, ID: w.self, Phrase: phrase, SNR: round2(snr), Score: round2(score)}
	peers := w.peers()
	if w.conn == nil || len(peers) == 0 {
		return true
	}
	b, _ := json.Marshal(mine)
	send := func() {
		for _, ip := range peers {
			w.conn.WriteToUDP(b, &net.UDPAddr{IP: net.ParseIP(ip), Port: votePort})
		}
	}
	send()
	time.Sleep(voteWindow / 4)
	send() // UDP may drop one; a duplicate claim changes nothing
	time.Sleep(voteWindow - voteWindow/4)
	if c, lost := w.beaten(mine, now.Add(-voteBefore)); lost {
		w.log.Info("wake vote: another clock heard it better", "phrase", phrase, "snr", mine.SNR,
			"other", c.ID, "other_snr", c.SNR)
		return false
	}
	w.log.Info("wake vote: this clock answers", "phrase", phrase, "snr", mine.SNR, "clocks", len(peers)+1)
	return true
}

// wakeLevel follows the microphone in 50 ms frames to say how clearly the
// last second (the wake word) stood out from the room: the loudest frame of
// the last second over the quiet frames of the last four seconds, in dB.
type wakeLevel struct {
	acc   float64
	n     int
	ring  [80]float64 // frame mean squares, 4 s
	pos   int
	count int
}

const wakeFrame = 800 // samples: 50 ms at 16 kHz

func (l *wakeLevel) feed(s []int16) {
	for _, v := range s {
		f := float64(v)
		l.acc += f * f
		l.n++
		if l.n == wakeFrame {
			l.ring[l.pos] = l.acc / wakeFrame
			l.pos = (l.pos + 1) % len(l.ring)
			l.count = min(l.count+1, len(l.ring))
			l.acc, l.n = 0, 0
		}
	}
}

// snr is the loudest frame of the last second over the room's level (the
// 20th percentile of the last four seconds), in dB; 0 when nothing is known.
func (l *wakeLevel) snr() float64 {
	if l.count < 20 {
		return 0
	}
	frames := make([]float64, 0, l.count)
	var loud float64
	for i := 1; i <= l.count; i++ {
		v := l.ring[(l.pos-i+len(l.ring))%len(l.ring)]
		frames = append(frames, v)
		if i <= 20 {
			loud = max(loud, v)
		}
	}
	sort.Float64s(frames)
	floor := frames[len(frames)/5]
	return 10 * math.Log10((loud+1)/(floor+1))
}

// peerIPs lists the IPv4 addresses of the other clocks seen lately.
func (p *peerList) peerIPs() []string {
	var out []string
	for _, x := range p.list(time.Now()) {
		if x.Online && x.IP != "" {
			out = append(out, x.IP)
		}
	}
	return out
}
