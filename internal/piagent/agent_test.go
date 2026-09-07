package piagent

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeGadget struct {
	wrote   string
	cleared bool
	err     error
}

func (f *fakeGadget) Write(_ context.Context, src string) error { f.wrote = src; return f.err }
func (f *fakeGadget) Clear(context.Context) error               { f.cleared = true; return f.err }

// deniedClaimCentral answers /check and /claim with a denial.
func deniedClaimCentral(t *testing.T, reason string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/printers/resin/config", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"slug":"resin","approved":true}`))
	})
	mux.HandleFunc("/api/v1/printers/resin/current-job", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"current_job":null}`))
	})
	mux.HandleFunc("/api/v1/printers/resin/check", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"allowed":false,"reason":"` + reason + `"}`))
	})
	mux.HandleFunc("/api/v1/printers/resin/claim", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"claimed":false,"reason":"` + reason + `"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestLoadDeniedDoesNotWriteGadget(t *testing.T) {
	srv := deniedClaimCentral(t, "not_certified")
	g := &fakeGadget{}
	sc := &fakeScanner{}
	sc.set("DEADBEEF")
	a := New(NewCentralClient(srv.URL, "resin", "k"), g, sc, slog.New(slog.NewTextHandler(io.Discard, nil)))

	rec := httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/load", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (re-rendered page)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "not certified") {
		t.Errorf("expected a friendly not-certified message, got: %s", rec.Body.String())
	}
	if g.wrote != "" {
		t.Error("gadget.Write must NOT be called on a denied claim")
	}
}

func TestLoadWithNoStagedJob(t *testing.T) {
	srv := deniedClaimCentral(t, "no_staged_job")
	g := &fakeGadget{}
	sc := &fakeScanner{}
	sc.set("CAFE1234")
	a := New(NewCentralClient(srv.URL, "resin", "k"), g, sc, slog.New(slog.NewTextHandler(io.Discard, nil)))

	rec := httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/load", nil))
	if g.wrote != "" {
		t.Error("nothing to load — gadget.Write must not run")
	}
	if !strings.Contains(rec.Body.String(), "No queued print") {
		t.Errorf("expected the no-queued-print message, got: %s", rec.Body.String())
	}
}

func TestDenyMessageKnownReasons(t *testing.T) {
	for _, r := range []string{
		"unknown_fob", "not_certified", "membership_inactive",
		"extension_not_allowed", "checklist_incomplete", "printer_pending_approval",
		"no_staged_job",
	} {
		if m := denyMessage(r); m == "" || strings.Contains(m, "(") {
			t.Errorf("denyMessage(%q) = %q, want a plain-English sentence", r, m)
		}
	}
	if m := denyMessage("weird_new_reason"); !strings.Contains(m, "weird_new_reason") {
		t.Errorf("unknown reason should fall through with the code, got %q", m)
	}
}
