// Command pi-agent runs on the Raspberry Pi. It serves the upload page on the
// makerspace LAN, delegates every authorization decision to the central portal,
// and writes approved files onto the USB gadget via usb-refresh.sh.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/dangerweenie/resin-print-portal/internal/buildinfo"
	"github.com/dangerweenie/resin-print-portal/internal/config"
	"github.com/dangerweenie/resin-print-portal/internal/gadget"
	"github.com/dangerweenie/resin-print-portal/internal/piagent"
	"github.com/dangerweenie/resin-print-portal/internal/rfid"
)

func main() {
	probe := flag.Bool("probe", false, "read the RFID reader and print each fob in every code format, then exit (for bring-up)")
	flag.Parse()

	cfg, err := config.LoadPiAgent()
	if err != nil {
		slog.Error("config", "err", err)
		os.Exit(2)
	}
	log := newLogger(cfg.LogLevel)
	log.Info("pi-agent starting", "version", buildinfo.Resolve())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *probe {
		if err := rfid.Probe(ctx); err != nil {
			log.Error("rfid probe failed", "err", err)
			os.Exit(1)
		}
		return
	}

	central := piagent.NewCentralClient(cfg.CentralBaseURL, cfg.PrinterSlug, cfg.PrinterAPIKey)

	// Register (or re-register) with the portal, then keep checking in the
	// background so a wiped/rotated/deleted printer heals itself.
	if err := piagent.MaintainRegistration(ctx, central, cfg, log); err != nil {
		log.Error("registration aborted", "err", err)
		os.Exit(1)
	}

	// Fleet self-update: the portal decides which pi-agent build we run. A
	// successful swap calls stop(), which cancels ctx -> graceful shutdown ->
	// clean exit -> systemd (Restart=always) starts the new binary.
	updater := piagent.NewSelfUpdater(central, cfg.CredsPath, stop, log)
	go updater.Run(ctx)

	// Identity is the fob and only the fob. The reader must be present and
	// working or the agent does not start.
	reader, err := rfid.Open(log)
	if err != nil {
		log.Error("MFRC522 fob reader won't start — check wiring / power / the module", "err", err)
		os.Exit(1)
	}
	go func() {
		if err := reader.Run(ctx); err != nil {
			log.Error("fob reader stopped — the Pi can no longer identify anyone", "err", err)
		}
	}()

	g := gadget.New(cfg.RefreshScript, cfg.GadgetImage)
	agent := piagent.New(central, g, reader, log)
	log.Info("fob reader ready")

	hs := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           agent.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	// We reached a listening state on this binary — clear the crash-loop
	// tracker and confirm any pending self-update as good.
	updater.ConfirmStart()

	errCh := make(chan error, 1)
	go func() {
		log.Info("pi-agent listening", "addr", cfg.ListenAddr, "printer", cfg.PrinterSlug, "central", cfg.CentralBaseURL)
		if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
		log.Info("pi-agent shutting down")
		sc, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = hs.Shutdown(sc)
	case err := <-errCh:
		log.Error("pi-agent failed", "err", err)
		os.Exit(1)
	}
}

func newLogger(level string) *slog.Logger {
	lv := slog.LevelInfo
	switch level {
	case "debug":
		lv = slog.LevelDebug
	case "warn":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	}
	return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: lv}))
}
