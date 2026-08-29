// Command pi-agent runs on the Raspberry Pi. It serves the upload page on the
// makerspace LAN, delegates every authorization decision to the central portal,
// and writes approved files onto the USB gadget via usb-refresh.sh.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/dangerweenie/resin-print-portal/internal/config"
	"github.com/dangerweenie/resin-print-portal/internal/gadget"
	"github.com/dangerweenie/resin-print-portal/internal/piagent"
)

func main() {
	cfg, err := config.LoadPiAgent()
	if err != nil {
		slog.Error("config", "err", err)
		os.Exit(2)
	}
	log := newLogger(cfg.LogLevel)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	central := piagent.NewCentralClient(cfg.CentralBaseURL, cfg.PrinterSlug, cfg.PrinterAPIKey)

	if cfg.NeedsEnrollment() {
		if err := piagent.EnrollUntilSuccess(ctx, central, cfg, log); err != nil {
			log.Error("enrollment aborted", "err", err)
			os.Exit(1)
		}
	}

	g := gadget.New(cfg.RefreshScript, cfg.GadgetImage)
	agent := piagent.New(central, g, log)

	hs := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           agent.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

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
