package rfid

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/dangerweenie/resin-print-portal/internal/fobcode"
)

// Fixed wiring — the MFRC522 always goes on the primary SPI bus with RST on
// GPIO25. Nothing about the reader is configurable on the Pi (see CLAUDE.md on
// why per-Pi runtime config is avoided).
const (
	spiDev = "SPI0.0"
	rstPin = "GPIO25"
)

// Open builds a Reader talking to a real MFRC522 over SPI. Run the returned
// Reader with reader.Run(ctx) in a goroutine.
func Open(log *slog.Logger) (*Reader, error) {
	t, err := OpenSPI(spiDev, rstPin)
	if err != nil {
		return nil, err
	}
	return NewReader(NewMFRC522(t), log), nil
}

// Probe is the wiring diagnostic behind `pi-agent -probe`. It reports the SPI
// link, the chip version, the antenna state, and then, on every poll, exactly
// what happened: "no card in field", "card present but <stage> failed", or the
// decoded UID. Stop the running agent first (`systemctl stop resin-pi-agent`)
// so the two don't fight over the SPI bus.
func Probe(ctx context.Context) error {
	fmt.Printf("rfid probe — SPI %s, RST %s\n", spiDev, rstPin)
	t, err := OpenSPI(spiDev, rstPin)
	if err != nil {
		return err
	}
	defer t.Close()
	dev := NewMFRC522(t)

	// A hard/soft reset settles cheap boards that return 0x00 on a cold first read.
	_ = dev.Init()

	v, err := dev.Version()
	if err != nil {
		return fmt.Errorf("read VersionReg over SPI: %w", err)
	}
	fmt.Printf("VersionReg = 0x%02X (%s)\n", v, versionName(v))
	if v == 0x00 || v == 0xFF {
		return fmt.Errorf("VersionReg 0x%02X — the Pi isn't talking to the chip. Check "+
			"SDA/SS→GPIO8(CE0), SCK→GPIO11, MOSI→GPIO10, MISO→GPIO9, 3.3V (NOT 5V), GND; "+
			"and that SPI is enabled (`dtparam=spi=on` in config.txt, then reboot; `ls /dev/spidev*`)", v)
	}

	for _, reg := range []struct {
		name string
		addr byte
	}{
		{"CommandReg", regCommand}, {"TxControlReg", regTxControl},
		{"ErrorReg", regError}, {"Status1Reg", 0x07}, {"Status2Reg", 0x08},
	} {
		b, _ := dev.ReadReg(reg.addr)
		fmt.Printf("  %-12s 0x%02X\n", reg.name, b)
	}
	if tx, _ := dev.ReadReg(regTxControl); tx&0x03 != 0x03 {
		fmt.Printf("  ! antenna driver is OFF (TxControlReg=0x%02X) — no tag will ever read\n", tx)
	}
	fmt.Println("\nHold a fob to the reader (Ctrl-C to stop).")
	fmt.Println("Note: this reader is 13.56 MHz only. A 125 kHz fob (many older")
	fmt.Println("access systems) will never show up here no matter the wiring.")
	fmt.Println()

	var last string
	tick := time.NewTicker(300 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
			atqa, uid, stage, err := dev.ProbeRead()
			var msg string
			switch {
			case err != nil && (stage == "REQA" || stage == "setup"):
				msg = "no card in field"
			case err != nil:
				msg = fmt.Sprintf("card present (ATQA % X) but %s failed: %v", atqa, stage, err)
			default:
				msg = fmt.Sprintf("UID %d bytes: % X   ATQA % X", len(uid), uid, atqa)
			}
			if msg == last {
				continue
			}
			last = msg
			fmt.Println(msg)
			if err == nil {
				for _, form := range fobcode.Variants(uid) {
					fmt.Printf("    %s\n", form)
				}
			}
		}
	}
}

// versionName interprets the MFRC522 VersionReg. Cheap "RC522" boards are often
// FM17522 clones that report other values and still work.
func versionName(v byte) string {
	switch v {
	case 0x91:
		return "MFRC522 v1.0"
	case 0x92:
		return "MFRC522 v2.0"
	case 0x88:
		return "FM17522 clone"
	case 0xB2:
		return "FM17522E clone"
	case 0x00, 0xFF:
		return "no response — check wiring / power / SPI"
	default:
		return "unrecognised — likely a clone, may still work"
	}
}
