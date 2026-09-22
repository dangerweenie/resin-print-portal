package rfid

import (
	"context"
	"encoding/hex"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// cardReader is the slice of the reader hardware (*EM4100Reader in
// production) the poll loop needs; tests supply a fake.
type cardReader interface {
	Init() error
	SelfTest() error
	ReadUID() ([]byte, error)
	Close() error
}

// Scan is the most recent fob tap. Code is the UID as canonical uppercase hex;
// the portal expands it to every format and matches against members.code.
type Scan struct {
	UID  []byte
	Code string
	At   time.Time
}

// Reader polls the fob reader and remembers the last tap, for a short window.
type Reader struct {
	dev  cardReader
	poll time.Duration
	ttl  time.Duration
	log  *slog.Logger

	mu        sync.Mutex
	last      *Scan
	warnedRd  bool // first read error is logged loudly; the rest are Debug
	hwPresent bool // true only while THIS instant's hardware read succeeded — no TTL grace
	seq       int  // increments each time a genuinely new presentation begins (see CurrentSeq)
}

// NewReader builds a Reader. A tapped fob stays "current" for ttl so a member
// can tap, step back, and submit.
func NewReader(dev cardReader, log *slog.Logger) *Reader {
	return &Reader{
		dev:  dev,
		poll: 250 * time.Millisecond,
		ttl:  10 * time.Second,
		log:  log,
	}
}

// Run initialises the reader and polls until ctx is cancelled. It returns an
// error only if the hardware never came up; transient read errors are logged
// and retried.
func (r *Reader) Run(ctx context.Context) error {
	if err := r.dev.Init(); err != nil {
		return err
	}
	if err := r.dev.SelfTest(); err != nil {
		return err
	}
	r.log.Info("rfid reader started")
	defer r.dev.Close()

	t := time.NewTicker(r.poll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			uid, err := r.dev.ReadUID()
			if errors.Is(err, ErrNoCard) {
				r.mu.Lock()
				r.hwPresent = false // real-time absence, no TTL grace (see CurrentSeq)
				r.mu.Unlock()
				continue // TTL handles UI-facing expiry; no card is normal
			}
			if err != nil {
				// A real protocol error (not just "no card"): surface the first
				// one at Warn so a mis-wired or clone board is visible without
				// LOG_LEVEL=debug. `pi-agent -probe` is the full diagnostic.
				if !r.warnedRd {
					r.warnedRd = true
					r.log.Warn("rfid read error — a card was in range but couldn't be read; run `pi-agent -probe`", "err", err)
				} else {
					r.log.Debug("rfid read error", "err", err)
				}
				continue
			}
			r.warnedRd = false
			code := strings.ToUpper(hex.EncodeToString(uid))
			r.mu.Lock()
			// A "fresh presentation" is either the fob just arriving after a
			// real absence, or a different fob replacing it outright — not
			// merely the same fob still sitting there since the last poll.
			if !r.hwPresent || r.last == nil || r.last.Code != code {
				r.seq++
			}
			r.hwPresent = true
			r.last = &Scan{UID: uid, Code: code, At: time.Now()}
			r.mu.Unlock()
		}
	}
}

// Current returns the most recent tap if it is still within the TTL.
func (r *Reader) Current() (Scan, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.last == nil || time.Since(r.last.At) > r.ttl {
		return Scan{}, false
	}
	return *r.last, true
}

// CurrentCode returns just the canonical-hex code of the current tap.
func (r *Reader) CurrentCode() (string, bool) {
	s, ok := r.Current()
	return s.Code, ok
}

// CurrentSeq returns the current tap's code (same TTL-based rule as
// CurrentCode) plus a sequence number that only advances when a genuinely new
// physical presentation begins. It exists so a caller can dedupe "checked
// this already" correctly: the TTL above deliberately keeps a tap "current"
// for a few seconds after the fob is physically removed (so a member can tap,
// step back, and act on it), so wall-clock time alone can't tell "still the
// one continuous hold" from "removed and tapped again" — the seq can, because
// it only moves when the hardware itself reports a real gap or a different
// fob, never merely with the passage of time.
func (r *Reader) CurrentSeq() (code string, seq int, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.last == nil || time.Since(r.last.At) > r.ttl {
		return "", r.seq, false
	}
	return r.last.Code, r.seq, true
}

// Clear forgets the current tap — call it after a submit so the next member
// starts fresh. Also guarantees the next presentation (even of the same
// fob, still resting on the reader) is treated as new by CurrentSeq.
func (r *Reader) Clear() {
	r.mu.Lock()
	r.last = nil
	r.mu.Unlock()
}
