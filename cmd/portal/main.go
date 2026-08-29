// Command portal is the central service. It multiplexes three subcommands off
// argv[1] so one container image can run the API server, the roster-sync
// worker, or a migration job:
//
//	portal server    # HTTP API + admin UI
//	portal worker    # periodic TinkerAccess roster sync
//	portal migrate [up|down|status]
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/dangerweenie/resin-print-portal/internal/config"
	"github.com/dangerweenie/resin-print-portal/internal/server"
	"github.com/dangerweenie/resin-print-portal/internal/store"
	"github.com/dangerweenie/resin-print-portal/internal/tinkeraccess"
	"github.com/dangerweenie/resin-print-portal/internal/worker"
	"golang.org/x/crypto/bcrypt"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: portal <server|worker|migrate>")
		os.Exit(2)
	}
	sub := os.Args[1]

	cfg, err := config.LoadPortal(sub)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		os.Exit(2)
	}
	log := newLogger(cfg.LogLevel)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch sub {
	case "migrate":
		dir := "up"
		if len(os.Args) > 2 {
			dir = os.Args[2]
		}
		if err := store.Migrate(cfg.DatabaseURL, dir); err != nil {
			log.Error("migrate failed", "err", err)
			os.Exit(1)
		}
		log.Info("migrate complete", "direction", dir)

	case "server":
		if err := runServer(ctx, cfg, log); err != nil {
			log.Error("server exited with error", "err", err)
			os.Exit(1)
		}

	case "worker":
		if err := runWorker(ctx, cfg, log); err != nil {
			log.Error("worker exited with error", "err", err)
			os.Exit(1)
		}

	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n", sub)
		os.Exit(2)
	}
}

func runServer(ctx context.Context, cfg *config.Portal, log *slog.Logger) error {
	st, err := store.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer st.Close()

	if err := st.WaitForSchema(ctx, 2*time.Minute); err != nil {
		return err
	}
	if err := seedAdmin(ctx, st, cfg, log); err != nil {
		return err
	}

	srv, err := server.New(st, cfg, log)
	if err != nil {
		return err
	}

	hs := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           srv.Router(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.ListenAddr)
		if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
		log.Info("shutting down")
		shutCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return hs.Shutdown(shutCtx)
	case err := <-errCh:
		return err
	}
}

func runWorker(ctx context.Context, cfg *config.Portal, log *slog.Logger) error {
	path := cfg.TinkerAccessUsersPath
	if path == "" && cfg.TinkerAccessWASMPath != "" {
		p, err := tinkeraccess.RecoverUsersPath(cfg.TinkerAccessWASMPath)
		if err != nil {
			return err
		}
		log.Info("recovered get_users path from wasm bundle", "path", p)
		path = p
	}

	st, err := store.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer st.Close()

	client := tinkeraccess.New(cfg.TinkerAccessBaseURL, path)
	return worker.New(st, client, cfg.SyncInterval, log).Run(ctx)
}

// seedAdmin creates the first admin account on an empty admins table.
func seedAdmin(ctx context.Context, st *store.Store, cfg *config.Portal, log *slog.Logger) error {
	n, err := st.CountAdmins(ctx)
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	if cfg.AdminPassword == "" {
		log.Warn("admins table is empty and ADMIN_PASSWORD is unset — no one can log in to the admin UI")
		return nil
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(cfg.AdminPassword), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	if err := st.CreateAdmin(ctx, cfg.AdminUsername, string(hash)); err != nil {
		return err
	}
	log.Info("seeded initial admin account", "username", cfg.AdminUsername)
	return nil
}

func newLogger(level string) *slog.Logger {
	var lv slog.Level
	switch level {
	case "debug":
		lv = slog.LevelDebug
	case "warn":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lv}))
}
