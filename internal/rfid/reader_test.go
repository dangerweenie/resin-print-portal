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
