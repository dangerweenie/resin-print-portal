package rfid

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

type fakeDev struct {
	mu   sync.Mutex
	uid  []byte // nil => ErrNoCard
	fail error
}

func (d *fakeDev) Init() error     { return d.fail }
func (d *fakeDev) SelfTest() error { return d.fail }
func (d *fakeDev) Close() error    { return nil }
func (d *fakeDev) ReadUID() ([]byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.uid == nil {
		return nil, ErrNoCard
	}
	return append([]byte(nil), d.uid...), nil
}
func (d *fakeDev) put(uid []byte) { d.mu.Lock(); d.uid = uid; d.mu.Unlock() }

func newTestReader(dev cardReader) *Reader {
	r := NewReader(dev, slog.New(slog.NewTextHandler(io.Discard, nil)))
	r.poll = 5 * time.Millisecond
	r.ttl = 80 * time.Millisecond
	return r
}

func TestReaderTracksTapsAndExpires(t *testing.T) {
	dev := &fakeDev{}
	r := newTestReader(dev)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = r.Run(ctx) }()

	if _, ok := r.Current(); ok {
		t.Fatal("no tap yet, Current should be empty")
	}

	dev.put([]byte{0x01, 0x02, 0x03, 0x04})
	waitFor(t, 500*time.Millisecond, func() bool {
		s, ok := r.Current()
		return ok && s.Code == "01020304"
	}, "tap to register")

	// Fob removed — the last scan should linger for the TTL, then expire.
	dev.put(nil)
	waitFor(t, 500*time.Millisecond, func() bool {
		_, ok := r.Current()
		return !ok
	}, "tap to expire after TTL")
}

func TestReaderClear(t *testing.T) {
	dev := &fakeDev{uid: []byte{9, 9, 9, 9}}
	r := newTestReader(dev)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = r.Run(ctx) }()

	waitFor(t, 500*time.Millisecond, func() bool { _, ok := r.Current(); return ok }, "initial tap")
	r.Clear()
	if _, ok := r.Current(); ok {
		t.Fatal("Current should be empty right after Clear")
	}
}

func TestCurrentSeqStableWhileContinuouslyHeld(t *testing.T) {
	dev := &fakeDev{uid: []byte{1, 2, 3, 4}}
	r := newTestReader(dev)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = r.Run(ctx) }()

	waitFor(t, 500*time.Millisecond, func() bool { _, _, ok := r.CurrentSeq(); return ok }, "initial tap")
	_, seq1, _ := r.CurrentSeq()

	// Several more poll cycles of the SAME fob sitting there must not bump seq
	// -- this is exactly what stops a held fob from being re-checked (and
	// re-logged, and re-consuming a certify-by-tap capture) on every poll.
	time.Sleep(40 * time.Millisecond)
	_, seq2, ok := r.CurrentSeq()
	if !ok {
		t.Fatal("fob should still read as current")
	}
	if seq2 != seq1 {
		t.Errorf("seq changed from %d to %d while the same fob was continuously held", seq1, seq2)
	}
}

func TestCurrentSeqAdvancesOnRemovalAndRetap(t *testing.T) {
	dev := &fakeDev{}
	r := newTestReader(dev)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = r.Run(ctx) }()

	dev.put([]byte{5, 5, 5, 5})
	waitFor(t, 500*time.Millisecond, func() bool { _, _, ok := r.CurrentSeq(); return ok }, "first tap")
	_, seq1, _ := r.CurrentSeq()

	// Physically removed, then the SAME fob is presented again. Even though
	// CurrentCode/Current would still say "current" throughout (the TTL
	// deliberately lingers well past this), CurrentSeq must treat this as a
	// brand new presentation -- this is the bug that let certify-by-tap (and
	// any other consumer keyed on "already handled this code") silently reuse
	// a stale result across two distinct taps.
	dev.put(nil)
	waitFor(t, 200*time.Millisecond, func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return !r.hwPresent
	}, "hardware to register the fob as gone")
	dev.put([]byte{5, 5, 5, 5})

	waitFor(t, 500*time.Millisecond, func() bool {
		_, seq, ok := r.CurrentSeq()
		return ok && seq != seq1
	}, "seq to advance on the second physical presentation")
}

func TestReaderRunFailsWhenHardwareDown(t *testing.T) {
	r := newTestReader(&fakeDev{fail: io.ErrUnexpectedEOF})
	if err := r.Run(context.Background()); err == nil {
		t.Fatal("Run should return the init error")
	}
}

func waitFor(t *testing.T, d time.Duration, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
