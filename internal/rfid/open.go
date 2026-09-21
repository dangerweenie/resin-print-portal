package rfid

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/dangerweenie/resin-print-portal/internal/fobcode"
	"go.bug.st/serial"
)

// Fixed wiring — the RDM6300 always goes on the Pi's primary UART (the pins
// freed from Bluetooth by dtoverlay=disable-bt in provisioning). Nothing about
// the reader is configurable on the Pi (see CLAUDE.md on why per-Pi runtime
// config is avoided): 5V -> reader VCC, GND -> reader GND, reader TX -> a
// voltage divider (reader TX is 5V logic, the Pi's GPIO is not 5V-tolerant) ->
// GPIO15/RXD (physical pin 10).
const (
	serialDev  = "/dev/serial0"
	baudRate   = 9600
	readTimout = 300 * time.Millisecond
)

func openPort() (serial.Port, error) {
	port, err := serial.Open(serialDev, &serial.Mode{
		BaudRate: baudRate, DataBits: 8, Parity: serial.NoParity, StopBits: serial.OneStopBit,
	})
	if err != nil {
		return nil, fmt.Errorf("open serial %q (is enable_uart=1 set, dtoverlay=disable-bt applied, "+
			"and the reader wired to GPIO14/15? does /dev/serial0 exist?): %w", serialDev, err)
	}
	if err := port.SetReadTimeout(readTimout); err != nil {
		_ = port.Close()
		return nil, fmt.Errorf("set read timeout: %w", err)
	}
	return port, nil
}

// Open builds a Reader talking to a real RDM6300-class 125 kHz EM4100 reader
// over UART. Run the returned Reader with reader.Run(ctx) in a goroutine.
func Open(log *slog.Logger) (*Reader, error) {
	port, err := openPort()
	if err != nil {
		return nil, err
	}
	return NewReader(NewEM4100Reader(port), log), nil
}

// Probe opens the reader and reports every tapped tag in every code format
// until ctx is cancelled — a wiring diagnostic (`pi-agent -probe`), not part
// of setup. The portal matches all forms, so there is nothing to configure
// from what this prints. Stop the running agent first (`systemctl stop
// resin-pi-agent`) so the two don't fight over the serial port.
func Probe(ctx context.Context) error {
	port, err := openPort()
	if err != nil {
		return err
	}
	defer port.Close()
	dev := NewEM4100Reader(port)

	fmt.Printf("EM4100 probe — UART %s @ %d baud\n", serialDev, baudRate)
	fmt.Println("Hold a fob to the reader (Ctrl-C to stop).")
	fmt.Println("Note: this is a 125 kHz reader. A 13.56 MHz card/fob will never show up here.")
	fmt.Println()

	var lastUID string
	warnedSilent := false
	silentDeadline := time.Now().Add(5 * time.Second)
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
			uid, err := dev.ReadUID()
			switch {
			case err == nil:
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
			case errors.Is(err, ErrNoCard):
				lastUID = ""
				if warnedSilent || time.Now().Before(silentDeadline) {
					continue
				}
				warnedSilent = true
				switch {
				case !dev.SawBytes():
					fmt.Println("... no bytes received in 5s. Check: 5V + GND wired, TX -> divider -> " +
						"Pi RXD (GPIO15 / physical pin 10), enable_uart=1 + dtoverlay=disable-bt in " +
						"config.txt (then reboot), and `ls /dev/serial0` on the Pi.")
				case dev.BadFrames() > 0:
					fmt.Printf("... receiving bytes but %d frame(s) failed to decode/checksum — check the "+
						"baud rate (9600), that TX/RX aren't swapped, and that the voltage divider isn't "+
						"corrupting bits.\n", dev.BadFrames())
				default:
					fmt.Println("... receiving bytes but no complete frame yet — hold the fob closer/steadier.")
				}
			default:
				fmt.Printf("read error: %v\n", err)
			}
		}
	}
}
