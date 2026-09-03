package worker_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"log/slog"

	"github.com/dangerweenie/resin-print-portal/internal/store"
	"github.com/dangerweenie/resin-print-portal/internal/tinkeraccess"
	"github.com/dangerweenie/resin-print-portal/internal/worker"
)

// Needs a real Postgres, same convention as the store tests.
func testStore(t *testing.T) *store.Store {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL to run worker integration tests")
	}
	_ = store.Migrate(dsn, "reset")
	if err := store.Migrate(dsn, "up"); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	st, err := store.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(st.Close)
	return st
}

func TestWorkerSyncsThenKeepsLastOn404(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	roster := `[{"id":1,"name":"Jane Doe","code":"1","status":"A"},
	            {"id":2,"name":"John Roe","code":"2","status":"I"}]`
	serve := roster
	code := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if code != http.StatusOK {
			w.WriteHeader(code)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(serve))
	}))
	defer srv.Close()

	w := worker.New(st, tinkeraccess.New(srv.URL, "/api/get_users123"), time.Minute,
		slog.New(slog.NewTextHandler(discard{}, nil)))

	res, err := w.SyncOnce(ctx)
	if err != nil {
		t.Fatalf("first sync: %v", err)
	}
	if res.Received != 2 {
		t.Fatalf("received = %d", res.Received)
	}
	m, _, err := st.ResolveSlackName(ctx, "jane doe")
	if err != nil || !m.Active {
		t.Fatalf("jane should be active after sync: %+v %v", m, err)
	}

	// Endpoint starts 404ing: SyncOnce must error and NOT wipe the roster.
	code = http.StatusNotFound
	if _, err := w.SyncOnce(ctx); err == nil {
		t.Fatal("expected an error when the endpoint 404s")
	}
	m, _, err = st.ResolveSlackName(ctx, "jane doe")
	if err != nil || !m.Active {
		t.Fatalf("jane must still be active after a failed fetch: %+v %v", m, err)
	}
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }
