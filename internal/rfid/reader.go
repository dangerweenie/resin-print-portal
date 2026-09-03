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

// cardReader is the slice of *MFRC522 the poll loop needs; tests supply a fake.
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

// Reader polls an MFRC522 and remembers the last fob tapped, for a short window.
type Reader struct {
	dev  cardReader
	poll time.Duration
	ttl  time.Duration
	log  *slog.Logger

	mu   sync.Mutex
	last *Scan
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
				continue // TTL handles expiry; no card is normal
			}
			if err != nil {
				r.log.Debug("rfid read error", "err", err)
				continue
			}
			r.mu.Lock()
			r.last = &Scan{UID: uid, Code: strings.ToUpper(hex.EncodeToString(uid)), At: time.Now()}
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

// Clear forgets the current tap — call it after a submit so the next member
// starts fresh.
func (r *Reader) Clear() {
	r.mu.Lock()
	r.last = nil
	r.mu.Unlock()
}
