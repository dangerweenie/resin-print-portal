package rfid

import (
	"bytes"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

// fakeCard is a minimal MFRC522 + one PICC, enough to exercise the driver's
// REQA / anti-collision / cascade logic without hardware.
type fakeCard struct {
	regs    [64]byte
	sendBuf []byte // bytes written to the FIFO since the last flush
	fifoOut []byte // staged response for readFIFO
	uid     []byte // 4 or 7 bytes
	noCard  bool
}

func (f *fakeCard) RST(bool) error { return errors.New("no RST wired") } // force soft-reset path
func (f *fakeCard) Close() error   { return nil }

func (f *fakeCard) Tx(w, r []byte) error {
	if len(w) != len(r) {
		panic("Tx: w/r length mismatch")
	}
	// Burst FIFO read: first byte repeated == read(FIFOData).
	if len(w) > 2 && w[0] == addrRead(regFIFOData) {
		copy(r[1:], f.fifoOut)
		return nil
	}
	a := (w[0] >> 1) & 0x3F
	if w[0]&0x80 != 0 { // read
		r[1] = f.read(a)
		return nil
	}
	f.write(a, w[1]) // write
	return nil
}

func (f *fakeCard) read(a byte) byte {
	switch a {
	case regCommand:
		return 0 // power-down bit already clear
	case regDivIrq:
		return 0x04 // CRCIRq always ready
	case regCRCResultL, regCRCResultH:
		return 0
	default:
		return f.regs[a]
	}
}

func (f *fakeCard) write(a, v byte) {
	switch a {
	case regFIFOLevel:
		if v&0x80 != 0 {
			f.sendBuf = nil // flush
		}
	case regFIFOData:
		f.sendBuf = append(f.sendBuf, v)
	case regBitFraming:
		f.regs[a] = v
		if v&0x80 != 0 && f.regs[regCommand] == cmdTransceive {
			f.execute()
		}
	default:
		f.regs[a] = v
	}
}

func (f *fakeCard) execute() {
	f.regs[regError] = 0
	resp := f.respond(f.sendBuf)
	f.sendBuf = nil
	if resp == nil {
		f.regs[regComIrq] = 0x01 // TimerIRq -> ErrNoCard
		f.regs[regFIFOLevel] = 0
		f.fifoOut = nil
		return
	}
	f.fifoOut = resp
	f.regs[regFIFOLevel] = byte(len(resp))
	f.regs[regComIrq] = 0x30 // RxIRq | IdleIRq
}

func (f *fakeCard) respond(sent []byte) []byte {
	if f.noCard {
		return nil
	}
	switch {
	case len(sent) == 1 && sent[0] == piccReqA:
		return []byte{0x04, 0x00} // ATQA
	case len(sent) == 2 && sent[0] == piccSelCL1 && sent[1] == 0x20:
		return f.anticollBytes(0)
	case len(sent) >= 2 && sent[0] == piccSelCL1 && sent[1] == 0x70:
		return []byte{0x08, 0x00, 0x00} // SAK + CRC
	case len(sent) == 2 && sent[0] == piccSelCL2 && sent[1] == 0x20:
		return f.anticollBytes(1)
	case len(sent) >= 2 && sent[0] == piccSelCL2 && sent[1] == 0x70:
		return []byte{0x00, 0x00, 0x00}
	default:
		return nil
	}
}

// anticollBytes returns [b0,b1,b2,b3,BCC] for cascade level (0 or 1).
func (f *fakeCard) anticollBytes(level int) []byte {
	var b [4]byte
	if len(f.uid) <= 4 {
		copy(b[:], f.uid)
	} else if level == 0 {
		b[0] = piccCascade
		copy(b[1:], f.uid[0:3])
	} else {
		copy(b[:], f.uid[3:7])
	}
	return []byte{b[0], b[1], b[2], b[3], b[0] ^ b[1] ^ b[2] ^ b[3]}
}

func TestReadUID4Byte(t *testing.T) {
	f := &fakeCard{uid: []byte{0xDE, 0xAD, 0xBE, 0xEF}}
	m := NewMFRC522(f)
	if err := m.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	uid, err := m.ReadUID()
	if err != nil {
		t.Fatalf("ReadUID: %v", err)
	}
	if !bytes.Equal(uid, f.uid) {
		t.Fatalf("uid = %X, want %X", uid, f.uid)
	}
	if got := strings.ToUpper(hex.EncodeToString(uid)); got != "DEADBEEF" {
		t.Errorf("hex = %q", got)
	}
}

func TestReadUID7Byte(t *testing.T) {
	f := &fakeCard{uid: []byte{0x04, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66}}
	m := NewMFRC522(f)
	if err := m.Init(); err != nil {
		t.Fatal(err)
	}
	uid, err := m.ReadUID()
	if err != nil {
		t.Fatalf("ReadUID: %v", err)
	}
	if !bytes.Equal(uid, f.uid) {
		t.Fatalf("uid = %X, want %X", uid, f.uid)
	}
}

func TestReadUIDNoCard(t *testing.T) {
	f := &fakeCard{uid: []byte{1, 2, 3, 4}, noCard: true}
	m := NewMFRC522(f)
	_ = m.Init()
	if _, err := m.ReadUID(); !errors.Is(err, ErrNoCard) {
		t.Fatalf("err = %v, want ErrNoCard", err)
	}
}

func TestSelfTestRejectsBadVersion(t *testing.T) {
	f := &fakeCard{}
	f.regs[regVersion] = 0x00
	m := NewMFRC522(f)
	if err := m.SelfTest(); err == nil {
		t.Fatal("SelfTest should reject VersionReg 0x00")
	}
	f.regs[regVersion] = 0x92
	if err := m.SelfTest(); err != nil {
		t.Fatalf("SelfTest v2.0: %v", err)
	}
}
