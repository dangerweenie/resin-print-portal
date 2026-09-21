package rfid

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
)

// ErrNoCard means no card was in the field on this read attempt (the normal
// idle result — not a failure).
var ErrNoCard = errors.New("rfid: no card present")

// EM4100 wire format (as emitted by an RDM6300-class 125 kHz reader over
// UART, 9600 8N1): STX, 10 ASCII-hex chars (5 raw data bytes), 2 ASCII-hex
// chars (checksum = XOR of those 5 bytes), ETX. 14 bytes total. None of the
// payload's hex-digit byte values (0x30-0x39, 0x41-0x46) collide with STX/ETX
// (0x02/0x03), so a plain byte-scan for STX is safe framing for this protocol
// — no escaping to worry about.
const (
	em4100STX      = 0x02
	em4100ETX      = 0x03
	em4100FrameLen = 14
)

// SerialPort is the slice of a UART connection the driver needs; a fake
// stands in for it in tests. go.bug.st/serial's Port satisfies this directly.
type SerialPort interface {
	Read(p []byte) (int, error)
	Close() error
}

// EM4100Reader decodes the ASCII frames a 125 kHz EM4100 reader (e.g. the
// RDM6300) streams over UART. Unlike the MFRC522 it replaces, this hardware
// pushes data whenever a tag is in the field rather than being polled for one;
// ReadUID does one bounded Read() from the port (opened with a short read
// timeout — see open.go) and returns whatever frame that turned up, so it
// still fits Reader's existing poll loop unchanged.
type EM4100Reader struct {
	port SerialPort
	buf  []byte // bytes read but not yet resolved into a frame

	sawBytes  bool // sticky: true once any byte has ever arrived (wiring diagnostic)
	badFrames int  // frames shaped right (STX/length/ETX) but failed to decode/checksum
}

// NewEM4100Reader wraps an already-open, already-configured serial port.
func NewEM4100Reader(port SerialPort) *EM4100Reader {
	return &EM4100Reader{port: port}
}

// Init is a no-op: the reader self-initialises on power-up, nothing to
// configure over a plain UART.
func (r *EM4100Reader) Init() error { return nil }

// SelfTest is a no-op: there's no chip register to probe over a plain UART
// (unlike the MFRC522's VersionReg). Probe's own read loop is the real check.
func (r *EM4100Reader) SelfTest() error { return nil }

func (r *EM4100Reader) Close() error { return r.port.Close() }

// ReadUID reads whatever bytes have arrived (bounded by the port's read
// timeout) and returns the most recent validly-checksummed tag ID seen since
// the last call, or ErrNoCard if nothing new decoded.
func (r *EM4100Reader) ReadUID() ([]byte, error) {
	chunk := make([]byte, 256)
	n, err := r.port.Read(chunk)
	if err != nil {
		return nil, err // a real I/O fault, not just "nothing yet"
	}
	if n > 0 {
		r.sawBytes = true
		r.buf = append(r.buf, chunk[:n]...)
		const maxBuf = 4096 // never accumulate unbounded noise if nothing ever frames
		if len(r.buf) > maxBuf {
			r.buf = r.buf[len(r.buf)-maxBuf:]
		}
	}
	id, rest, rejected := parseEM4100Frames(r.buf)
	r.buf = rest
	r.badFrames += rejected
	if id == nil {
		return nil, ErrNoCard
	}
	return id, nil
}

// SawBytes reports whether any bytes have ever arrived on the port. Used by
// the -probe diagnostic to tell "silent line" (wiring/power/enable_uart) from
// "receiving something that never frames".
func (r *EM4100Reader) SawBytes() bool { return r.sawBytes }

// BadFrames is the running count of frames that looked shaped right but
// failed to decode or checksum — a sign of a marginal level shift or a wrong
// baud rate rather than dead wiring.
func (r *EM4100Reader) BadFrames() int { return r.badFrames }

// parseEM4100Frames scans buf for complete EM4100 frames. A held tag
// re-transmits continuously, so it returns the ID of the LAST valid frame
// found (most recent read wins) plus rejected — how many candidate frames
// were the right shape but failed to decode. rest is the unconsumed tail of
// buf: either empty, or a frame still arriving, primed for the next call.
func parseEM4100Frames(buf []byte) (id []byte, rest []byte, rejected int) {
	for {
		start := bytes.IndexByte(buf, em4100STX)
		if start < 0 {
			return id, nil, rejected
		}
		if len(buf)-start < em4100FrameLen {
			return id, buf[start:], rejected
		}
		frame := buf[start : start+em4100FrameLen]
		if got, err := decodeEM4100(frame); err == nil {
			id = got
			buf = buf[start+em4100FrameLen:]
			continue
		}
		rejected++
		buf = buf[start+1:] // not a valid frame here -- resync on the next STX
	}
}

// decodeEM4100 validates and decodes one 14-byte frame into its 5 raw ID
// bytes.
func decodeEM4100(frame []byte) ([]byte, error) {
	if len(frame) != em4100FrameLen || frame[0] != em4100STX || frame[em4100FrameLen-1] != em4100ETX {
		return nil, fmt.Errorf("rfid: malformed EM4100 frame")
	}
	data := make([]byte, 5)
	if _, err := hex.Decode(data, frame[1:11]); err != nil {
		return nil, fmt.Errorf("rfid: bad hex in EM4100 frame: %w", err)
	}
	sum := make([]byte, 1)
	if _, err := hex.Decode(sum, frame[11:13]); err != nil {
		return nil, fmt.Errorf("rfid: bad checksum hex in EM4100 frame: %w", err)
	}
	var xor byte
	for _, b := range data {
		xor ^= b
	}
	if xor != sum[0] {
		return nil, fmt.Errorf("rfid: EM4100 checksum mismatch (got %02X want %02X)", xor, sum[0])
	}
	return data, nil
}
