package rfid

import (
	"context"
	"encoding/hex"
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

// Probe opens the reader and prints every tapped UID in all string forms until
// ctx is cancelled — a wiring diagnostic (`pi-agent -probe`), not part of
// setup. The portal matches all forms, so there is nothing to configure from
// what this prints.
func Probe(ctx context.Context) error {
	t, err := OpenSPI(spiDev, rstPin)
	if err != nil {
		return err
	}
	defer t.Close()
	dev := NewMFRC522(t)
	if err := dev.Init(); err != nil {
		return fmt.Errorf("init: %w", err)
	}
	if err := dev.SelfTest(); err != nil {
		return err
	}
	v, _ := dev.Version()
	fmt.Printf("MFRC522 VersionReg = 0x%02X — hold a fob to the reader (Ctrl-C to stop)\n\n", v)

	var lastUID string
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
			uid, err := dev.ReadUID()
			if err != nil {
				lastUID = ""
				continue
			}
			key := hex.EncodeToString(uid)
			if key == lastUID {
				continue
			}
			lastUID = key
			fmt.Printf("UID %d bytes: % X\n", len(uid), uid)
			for _, form := range fobcode.Variants(uid) {
				fmt.Printf("  %s\n", form)
			}
			fmt.Println()
		}
	}
}
