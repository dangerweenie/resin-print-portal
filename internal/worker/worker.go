// Package worker runs the periodic TinkerAccess roster sync: pull the member
// list from the get_users endpoint and reconcile it into Postgres so the portal
// always knows who is a currently-active member.
package worker

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/dangerweenie/resin-print-portal/internal/store"
	"github.com/dangerweenie/resin-print-portal/internal/tinkeraccess"
)

// Worker reconciles the members table with TinkerAccess on an interval.
type Worker struct {
	st       *store.Store
	client   *tinkeraccess.Client
	interval time.Duration
	log      *slog.Logger
}

// New builds a Worker.
func New(st *store.Store, client *tinkeraccess.Client, interval time.Duration, log *slog.Logger) *Worker {
	return &Worker{st: st, client: client, interval: interval, log: log}
}

// Run syncs once immediately, then every interval, until ctx is cancelled.
func (w *Worker) Run(ctx context.Context) error {
	w.log.Info("roster sync worker started", "interval", w.interval.String())
	t := time.NewTicker(w.interval)
	defer t.Stop()

	w.runOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			w.log.Info("roster sync worker stopping")
			return nil
		case <-t.C:
			w.runOnce(ctx)
		}
	}
}

func (w *Worker) runOnce(ctx context.Context) {
	start := time.Now()
	res, err := w.SyncOnce(ctx)
	if err != nil {
		if errors.Is(err, tinkeraccess.ErrEndpointNotFound) {
			w.log.Error("get_users returned 404 — the Leptos hash suffix likely changed; "+
				"recover it per docs/GET_MEMBERS_ENDPOINT.md (wasm strings grep) and update "+
				"TINKERACCESS_GET_USERS_PATH. Keeping the last-known roster.", "err", err)
		} else {
			w.log.Error("roster sync failed — keeping the last-known roster", "err", err)
		}
		return
	}
	w.log.Info("roster synced",
		"received", res.Received, "deactivated_missing", res.Deactivated,
		"dur_ms", time.Since(start).Milliseconds())
}

// SyncOnce performs a single fetch + reconcile. It is exported for tests and
// for a one-shot CLI mode.
func (w *Worker) SyncOnce(ctx context.Context) (store.SyncResult, error) {
	users, err := w.client.FetchUsers(ctx)
	if err != nil {
		return store.SyncResult{}, err
	}
	roster := make([]store.RosterEntry, 0, len(users))
	for _, u := range users {
		e := store.RosterEntry{ID: u.ID, Status: u.Status}
		if u.Name != nil {
			e.Name = *u.Name
		}
		if u.Code != nil {
			e.Code = *u.Code
		}
		roster = append(roster, e)
	}
	return w.st.SyncRoster(ctx, roster)
}
