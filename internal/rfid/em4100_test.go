package rfid

import (
	"bytes"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

// encodeEM4100Frame builds a wire-format frame for id, computing its checksum
// the same way a real RDM6300 does (XOR of the 5 data bytes).
func encodeEM4100Frame(id [5]byte) []byte {
	var xor byte
	for _, b := range id {
		xor ^= b
	}
	buf := make([]byte, 0, em4100FrameLen)
	buf = append(buf, em4100STX)
	buf = append(buf, []byte(strings.ToUpper(hex.EncodeToString(id[:])))...)
	buf = append(buf, []byte(strings.ToUpper(hex.EncodeToString([]byte{xor})))...)
	buf = append(buf, em4100ETX)
	return buf
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	id := [5]byte{0x01, 0xA1, 0xB2, 0xC3, 0xD4}
	frame := encodeEM4100Frame(id)
	if len(frame) != em4100FrameLen {
		t.Fatalf("frame len = %d, want %d", len(frame), em4100FrameLen)
	}
	got, err := decodeEM4100(frame)
	if err != nil {
		t.Fatalf("decodeEM4100: %v", err)
	}
	if !bytes.Equal(got, id[:]) {
		t.Fatalf("decoded = % X, want % X", got, id)
	}
}

func TestDecodeRejectsBadChecksum(t *testing.T) {
	frame := encodeEM4100Frame([5]byte{0x01, 0xA1, 0xB2, 0xC3, 0xD4})
	frame[11] = '0' // corrupt the checksum's first hex digit
	frame[12] = '0'
	if _, err := decodeEM4100(frame); err == nil {
		t.Fatal("expected a checksum error")
	}
}

func TestDecodeRejectsBadFraming(t *testing.T) {
	frame := encodeEM4100Frame([5]byte{1, 2, 3, 4, 5})
	frame[em4100FrameLen-1] = 0x99 // not ETX
	if _, err := decodeEM4100(frame); err == nil {
		t.Fatal("expected a framing error when ETX is wrong")
	}
}

func TestParseFramesSkipsLeadingGarbage(t *testing.T) {
	id := [5]byte{0xDE, 0xAD, 0xBE, 0xEF, 0x01}
	buf := append([]byte{0xFF, 0xFE, 0x00}, encodeEM4100Frame(id)...)
	got, rest, rejected := parseEM4100Frames(buf)
	if !bytes.Equal(got, id[:]) {
		t.Fatalf("got = % X, want % X", got, id)
	}
	if len(rest) != 0 || rejected != 0 {
		t.Fatalf("rest = %v rejected = %d, want empty/0", rest, rejected)
	}
}

func TestParseFramesReturnsPartialTail(t *testing.T) {
	id := [5]byte{1, 2, 3, 4, 5}
	frame := encodeEM4100Frame(id)
	got, rest, _ := parseEM4100Frames(frame[:6]) // truncated mid-frame
	if got != nil {
		t.Fatalf("truncated frame should not decode, got % X", got)
	}
	if !bytes.Equal(rest, frame[:6]) {
		t.Fatalf("rest = % X, want the whole partial frame kept", rest)
	}
}

func TestParseFramesLastOfMultipleWins(t *testing.T) {
	a := encodeEM4100Frame([5]byte{1, 1, 1, 1, 1})
	b := encodeEM4100Frame([5]byte{2, 2, 2, 2, 2})
	got, _, rejected := parseEM4100Frames(append(a, b...))
	if !bytes.Equal(got, []byte{2, 2, 2, 2, 2}) {
		t.Fatalf("got = % X, want the second (most recent) frame", got)
	}
	if rejected != 0 {
		t.Fatalf("both frames were valid, rejected should be 0, got %d", rejected)
	}
}

func TestParseFramesCountsRejectedAndResyncs(t *testing.T) {
	bad := encodeEM4100Frame([5]byte{9, 9, 9, 9, 9})
	bad[11], bad[12] = '0', '0' // corrupt checksum
	good := encodeEM4100Frame([5]byte{7, 7, 7, 7, 7})
	got, _, rejected := parseEM4100Frames(append(bad, good...))
	if !bytes.Equal(got, []byte{7, 7, 7, 7, 7}) {
		t.Fatalf("got = % X, want the good frame recovered after the bad one", got)
	}
	if rejected != 1 {
		t.Fatalf("rejected = %d, want 1", rejected)
	}
}

// chunkPort is a fake SerialPort that hands back one buffered chunk per Read
// call, mimicking bytes trickling in from a real UART.
type chunkPort struct {
	chunks [][]byte
	i      int
	err    error
}

func (c *chunkPort) Read(p []byte) (int, error) {
	if c.err != nil {
		return 0, c.err
	}
	if c.i >= len(c.chunks) {
		return 0, nil // like a read-timeout: nothing arrived this round
	}
	n := copy(p, c.chunks[c.i])
	c.i++
	return n, nil
}
func (c *chunkPort) Close() error { return nil }

func TestEM4100ReaderAssemblesFrameAcrossReads(t *testing.T) {
	id := [5]byte{0x01, 0xA1, 0xB2, 0xC3, 0xD4}
	frame := encodeEM4100Frame(id)
	port := &chunkPort{chunks: [][]byte{frame[:6], frame[6:]}}
	r := NewEM4100Reader(port)

	if _, err := r.ReadUID(); !errors.Is(err, ErrNoCard) {
		t.Fatalf("after the first partial chunk: err = %v, want ErrNoCard", err)
	}
	got, err := r.ReadUID()
	if err != nil {
		t.Fatalf("ReadUID: %v", err)
	}
	if !bytes.Equal(got, id[:]) {
		t.Fatalf("uid = % X, want % X", got, id[:])
	}
	if !r.SawBytes() {
		t.Error("SawBytes should be true once bytes have arrived")
	}
	if r.BadFrames() != 0 {
		t.Errorf("BadFrames = %d, want 0", r.BadFrames())
	}
}

func TestEM4100ReaderNoDataYet(t *testing.T) {
	r := NewEM4100Reader(&chunkPort{})
	if _, err := r.ReadUID(); !errors.Is(err, ErrNoCard) {
		t.Fatalf("err = %v, want ErrNoCard", err)
	}
	if r.SawBytes() {
		t.Error("SawBytes should be false when nothing has arrived")
	}
}

func TestEM4100ReaderCountsBadFrames(t *testing.T) {
	bad := encodeEM4100Frame([5]byte{1, 2, 3, 4, 5})
	bad[11], bad[12] = '0', '0'
	r := NewEM4100Reader(&chunkPort{chunks: [][]byte{bad}})
	if _, err := r.ReadUID(); !errors.Is(err, ErrNoCard) {
		t.Fatalf("err = %v, want ErrNoCard", err)
	}
	if r.BadFrames() != 1 {
		t.Errorf("BadFrames = %d, want 1", r.BadFrames())
	}
	if !r.SawBytes() {
		t.Error("SawBytes should be true -- bytes did arrive, they just didn't checksum")
	}
}

func TestEM4100ReaderPropagatesHardIOError(t *testing.T) {
	wantErr := errors.New("boom")
	r := NewEM4100Reader(&chunkPort{err: wantErr})
	if _, err := r.ReadUID(); !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
}
