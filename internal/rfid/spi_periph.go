package rfid

import (
	"fmt"

	"periph.io/x/conn/v3/gpio"
	"periph.io/x/conn/v3/gpio/gpioreg"
	"periph.io/x/conn/v3/physic"
	"periph.io/x/conn/v3/spi"
	"periph.io/x/conn/v3/spi/spireg"
	host "periph.io/x/host/v3"
)

type spiPeriph struct {
	port spi.PortCloser
	conn spi.Conn
	rst  gpio.PinIO
}

// OpenSPI opens the MFRC522 on an spidev (e.g. "SPI0.0" or "/dev/spidev0.0")
// with an optional reset GPIO ("GPIO25"; "" to skip and hold RST high in
// hardware). It fails cleanly on a machine without SPI so the caller can carry
// on without a reader.
func OpenSPI(dev, rstPin string) (Transport, error) {
	if _, err := host.Init(); err != nil {
		return nil, fmt.Errorf("periph host init: %w", err)
	}
	port, err := spireg.Open(dev)
	if err != nil {
		return nil, fmt.Errorf("open spi %q (is dtparam=spi=on set and the module wired?): %w", dev, err)
	}
	conn, err := port.Connect(1*physic.MegaHertz, spi.Mode0, 8)
	if err != nil {
		_ = port.Close()
		return nil, fmt.Errorf("spi connect: %w", err)
	}
	t := &spiPeriph{port: port, conn: conn}
	if rstPin != "" {
		if p := gpioreg.ByName(rstPin); p != nil {
			_ = p.Out(gpio.High)
			t.rst = p
		}
	}
	return t, nil
}

func (s *spiPeriph) Tx(w, r []byte) error { return s.conn.Tx(w, r) }

func (s *spiPeriph) RST(release bool) error {
	if s.rst == nil {
		return fmt.Errorf("no RST pin configured")
	}
	lvl := gpio.Low
	if release {
		lvl = gpio.High
	}
	return s.rst.Out(lvl)
}

func (s *spiPeriph) Close() error { return s.port.Close() }
