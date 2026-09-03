package rfid

import (
	"errors"
	"fmt"
	"time"
)

// ErrNoCard means no card was in the field on this read attempt (the normal
// idle result — not a failure).
var ErrNoCard = errors.New("rfid: no card present")

// Transport is the wire to the MFRC522: a full-duplex SPI transfer plus an
// optional reset line. Real hardware is spiPeriph; tests use a fake.
type Transport interface {
	// Tx sends w and fills r (same length) with what clocks back.
	Tx(w, r []byte) error
	// RST drives the reset pin (true = released/high). No-op if RST isn't wired.
	RST(release bool) error
	Close() error
}

// MFRC522 register addresses (datasheet §9).
const (
	regCommand    = 0x01
	regComIrq     = 0x04
	regDivIrq     = 0x05
	regError      = 0x06
	regFIFOData   = 0x09
	regFIFOLevel  = 0x0A
	regControl    = 0x0C
	regBitFraming = 0x0D
	regColl       = 0x0E
	regMode       = 0x11
	regTxMode     = 0x12
	regRxMode     = 0x13
	regTxControl  = 0x14
	regTxASK      = 0x15
	regCRCResultH = 0x21
	regCRCResultL = 0x22
	regModWidth   = 0x24
	regTMode      = 0x2A
	regTPrescaler = 0x2B
	regTReloadH   = 0x2C
	regTReloadL   = 0x2D
	regVersion    = 0x37
)

// commands
const (
	cmdIdle       = 0x00
	cmdCalcCRC    = 0x03
	cmdTransceive = 0x0C
	cmdSoftReset  = 0x0F
)

// PICC (card) commands
const (
	piccReqA    = 0x26
	piccCascade = 0x88 // "CT" — appears as UID byte 0 for >4-byte UIDs
	piccSelCL1  = 0x93
	piccSelCL2  = 0x95
)

// MFRC522 drives one reader.
type MFRC522 struct {
	t Transport
}

// NewMFRC522 wraps a Transport. Call Init before reading.
func NewMFRC522(t Transport) *MFRC522 { return &MFRC522{t: t} }

func (m *MFRC522) Close() error { return m.t.Close() }

// --- register helpers -------------------------------------------------------

func addrRead(a byte) byte  { return 0x80 | ((a << 1) & 0x7E) }
func addrWrite(a byte) byte { return (a << 1) & 0x7E }

func (m *MFRC522) writeReg(a, v byte) error {
	return m.t.Tx([]byte{addrWrite(a), v}, make([]byte, 2))
}

func (m *MFRC522) readReg(a byte) (byte, error) {
	r := make([]byte, 2)
	if err := m.t.Tx([]byte{addrRead(a), 0x00}, r); err != nil {
		return 0, err
	}
	return r[1], nil
}

func (m *MFRC522) readFIFO(n int) ([]byte, error) {
	if n <= 0 {
		return nil, nil
	}
	w := make([]byte, n+1)
	for i := 0; i < n; i++ {
		w[i] = addrRead(regFIFOData)
	}
	r := make([]byte, n+1)
	if err := m.t.Tx(w, r); err != nil {
		return nil, err
	}
	return r[1:], nil
}

func (m *MFRC522) setBits(a, mask byte) error {
	v, err := m.readReg(a)
	if err != nil {
		return err
	}
	return m.writeReg(a, v|mask)
}

func (m *MFRC522) clearBits(a, mask byte) error {
	v, err := m.readReg(a)
	if err != nil {
		return err
	}
	return m.writeReg(a, v&^mask)
}

// --- lifecycle ------------------------------------------------------------

// Init resets and configures the chip and switches the antenna on.
func (m *MFRC522) Init() error {
	// Hard reset if RST is wired, otherwise soft reset.
	if err := m.t.RST(false); err == nil {
		time.Sleep(2 * time.Millisecond)
		_ = m.t.RST(true)
		time.Sleep(50 * time.Millisecond)
	} else {
		if err := m.writeReg(regCommand, cmdSoftReset); err != nil {
			return err
		}
		for i := 0; i < 20; i++ {
			v, err := m.readReg(regCommand)
			if err != nil {
				return err
			}
			if v&0x10 == 0 { // PowerDown bit cleared
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
	}

	writes := [][2]byte{
		{regTxMode, 0x00}, {regRxMode, 0x00},
		{regModWidth, 0x26},
		{regTMode, 0x80}, {regTPrescaler, 0xA9},
		{regTReloadH, 0x03}, {regTReloadL, 0xE8},
		{regTxASK, 0x40},
		{regMode, 0x3D},
	}
	for _, w := range writes {
		if err := m.writeReg(w[0], w[1]); err != nil {
			return err
		}
	}
	// Antenna on.
	v, err := m.readReg(regTxControl)
	if err != nil {
		return err
	}
	if v&0x03 != 0x03 {
		if err := m.writeReg(regTxControl, v|0x03); err != nil {
			return err
		}
	}
	return nil
}

// Version returns the VersionReg byte (0x91 = v1.0, 0x92 = v2.0; 0x00/0xFF
// usually means the wiring is wrong).
func (m *MFRC522) Version() (byte, error) { return m.readReg(regVersion) }

// SelfTest returns an error if the version register looks implausible.
func (m *MFRC522) SelfTest() error {
	v, err := m.Version()
	if err != nil {
		return err
	}
	if v == 0x00 || v == 0xFF {
		return fmt.Errorf("rfid: implausible VersionReg 0x%02X — check SPI wiring / power / SPI enabled", v)
	}
	return nil
}

// --- card reading -------------------------------------------------------

// ReadUID performs REQA + anti-collision and returns the card UID (4, 7, or 10
// bytes). ErrNoCard when the field is empty.
func (m *MFRC522) ReadUID() ([]byte, error) {
	if err := m.requestA(); err != nil {
		return nil, ErrNoCard
	}
	uid, err := m.anticollisionCascade(piccSelCL1)
	if err != nil {
		return nil, err
	}
	return uid, nil
}

func (m *MFRC522) requestA() error {
	if err := m.writeReg(regBitFraming, 0x07); err != nil { // TxLastBits = 7 (a short frame)
		return err
	}
	back, _, err := m.transceive([]byte{piccReqA}, 0x07)
	if err != nil {
		return err
	}
	if len(back) != 2 { // ATQA is 2 bytes
		return fmt.Errorf("rfid: unexpected ATQA length %d", len(back))
	}
	return nil
}

// anticollisionCascade resolves a full UID, following cascade levels for
// 7-/10-byte UIDs.
func (m *MFRC522) anticollisionCascade(sel byte) ([]byte, error) {
	var uid []byte
	for level := 0; level < 3; level++ {
		cl, err := m.anticollision(sel)
		if err != nil {
			return nil, err
		}
		if cl[0] == piccCascade {
			uid = append(uid, cl[1:4]...) // 3 real UID bytes
			if err := m.selectCL(sel, cl); err != nil {
				return nil, err
			}
			sel += 2 // CL1 (0x93) -> CL2 (0x95) -> CL3 (0x97)
			continue
		}
		uid = append(uid, cl[:4]...)
		return uid, nil
	}
	return uid, nil
}

// anticollision runs one cascade level and returns [uid0..3, BCC].
func (m *MFRC522) anticollision(sel byte) ([]byte, error) {
	if err := m.writeReg(regColl, 0x80); err != nil { // clear received bits after collision
		return nil, err
	}
	if err := m.writeReg(regBitFraming, 0x00); err != nil {
		return nil, err
	}
	back, _, err := m.transceive([]byte{sel, 0x20}, 0x00) // NVB = 0x20
	if err != nil {
		return nil, err
	}
	if len(back) != 5 {
		return nil, fmt.Errorf("rfid: anticollision returned %d bytes, want 5", len(back))
	}
	if back[0]^back[1]^back[2]^back[3] != back[4] {
		return nil, errors.New("rfid: UID BCC check failed")
	}
	return back, nil
}

// selectCL sends a SELECT for one cascade level so the next level can proceed.
func (m *MFRC522) selectCL(sel byte, cl []byte) error {
	frame := []byte{sel, 0x70, cl[0], cl[1], cl[2], cl[3], cl[4]}
	crc, err := m.calcCRC(frame)
	if err != nil {
		return err
	}
	frame = append(frame, crc...)
	if err := m.writeReg(regBitFraming, 0x00); err != nil {
		return err
	}
	back, _, err := m.transceive(frame, 0x00)
	if err != nil {
		return err
	}
	if len(back) < 1 { // SAK
		return errors.New("rfid: empty SAK on SELECT")
	}
	return nil
}

func (m *MFRC522) calcCRC(data []byte) ([]byte, error) {
	if err := m.writeReg(regCommand, cmdIdle); err != nil {
		return nil, err
	}
	if err := m.writeReg(regDivIrq, 0x04); err != nil { // clear CRCIRq
		return nil, err
	}
	if err := m.setBits(regFIFOLevel, 0x80); err != nil { // flush
		return nil, err
	}
	for _, b := range data {
		if err := m.writeReg(regFIFOData, b); err != nil {
			return nil, err
		}
	}
	if err := m.writeReg(regCommand, cmdCalcCRC); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(40 * time.Millisecond)
	for {
		v, err := m.readReg(regDivIrq)
		if err != nil {
			return nil, err
		}
		if v&0x04 != 0 { // CRCIRq
			break
		}
		if time.Now().After(deadline) {
			return nil, errors.New("rfid: CRC calculation timed out")
		}
	}
	_ = m.writeReg(regCommand, cmdIdle)
	lo, err := m.readReg(regCRCResultL)
	if err != nil {
		return nil, err
	}
	hi, err := m.readReg(regCRCResultH)
	if err != nil {
		return nil, err
	}
	return []byte{lo, hi}, nil
}

// transceive runs one Transceive command and returns the received bytes.
func (m *MFRC522) transceive(send []byte, txLastBits byte) ([]byte, byte, error) {
	steps := [][2]byte{
		{regCommand, cmdIdle},
		{regComIrq, 0x7F}, // clear all interrupt bits
	}
	for _, s := range steps {
		if err := m.writeReg(s[0], s[1]); err != nil {
			return nil, 0, err
		}
	}
	if err := m.setBits(regFIFOLevel, 0x80); err != nil { // flush FIFO
		return nil, 0, err
	}
	for _, b := range send {
		if err := m.writeReg(regFIFOData, b); err != nil {
			return nil, 0, err
		}
	}
	if err := m.writeReg(regBitFraming, txLastBits); err != nil {
		return nil, 0, err
	}
	if err := m.writeReg(regCommand, cmdTransceive); err != nil {
		return nil, 0, err
	}
	if err := m.setBits(regBitFraming, 0x80); err != nil { // StartSend
		return nil, 0, err
	}

	deadline := time.Now().Add(40 * time.Millisecond)
	for {
		irq, err := m.readReg(regComIrq)
		if err != nil {
			return nil, 0, err
		}
		if irq&0x30 != 0 { // RxIRq | IdleIRq
			break
		}
		if irq&0x01 != 0 { // TimerIRq
			return nil, 0, ErrNoCard
		}
		if time.Now().After(deadline) {
			return nil, 0, ErrNoCard
		}
	}
	_ = m.clearBits(regBitFraming, 0x80)

	errReg, err := m.readReg(regError)
	if err != nil {
		return nil, 0, err
	}
	if errReg&0x13 != 0 { // BufferOvfl | ParityErr | ProtocolErr
		return nil, 0, fmt.Errorf("rfid: transceive error reg 0x%02X", errReg)
	}

	n, err := m.readReg(regFIFOLevel)
	if err != nil {
		return nil, 0, err
	}
	back, err := m.readFIFO(int(n))
	if err != nil {
		return nil, 0, err
	}
	ctrl, err := m.readReg(regControl)
	if err != nil {
		return nil, 0, err
	}
	return back, ctrl & 0x07, nil
}
