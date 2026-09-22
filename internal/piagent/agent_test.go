package piagent

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type fakeGadget struct {
	wrote   string
	cleared bool
	err     error
}

func (f *fakeGadget) Write(_ context.Context, src string) error { f.wrote = src; return f.err }
func (f *fakeGadget) Clear(context.Context) error               { f.cleared = true; return f.err }

// scanCentral serves /check and /claim with independently configurable
// bodies, for exercising the automatic check-then-load path.
func scanCentral(t *testing.T, checkBody, claimBody string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/printers/resin/config", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"slug":"resin","approved":true}`))
	})
	mux.HandleFunc("/api/v1/printers/resin/current-job", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"current_job":null}`))
	})
	mux.HandleFunc("/api/v1/printers/resin/check", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(checkBody))
	})
	mux.HandleFunc("/api/v1/printers/resin/claim", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(claimBody))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestDeniedTapNeverAutoLoads(t *testing.T) {
	srv := scanCentral(t,
		`{"allowed":false,"reason":"not_certified"}`,
		`{"claimed":false,"reason":"not_certified"}`)
	g := &fakeGadget{}
	sc := &fakeScanner{}
	sc.set("DEADBEEF")
	a := New(NewCentralClient(srv.URL, "resin", "k"), g, sc, slog.New(slog.NewTextHandler(io.Discard, nil)))

	rec := httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/scan", nil))
	if !strings.Contains(rec.Body.String(), `"allowed":false`) {
		t.Fatalf("scan response = %s", rec.Body.String())
	}
	time.Sleep(50 * time.Millisecond) // a denied check never launches autoLoad; nothing to wait for
	if g.wrote != "" {
		t.Error("gadget.Write must NOT be called for a denied tap")
	}
}

// TestAutoLoadClaimRaceIsBenign covers the check-said-yes-but-claim-says-no
// race (e.g. an earlier tap of the same held fob already claimed it): it must
// not write the gadget and must not surface it as an error.
func TestAutoLoadClaimRaceIsBenign(t *testing.T) {
	srv := scanCentral(t,
		`{"allowed":true,"member_name":"Ada","staged_filename":"job.goo"}`,
		`{"claimed":false,"reason":"no_staged_job"}`)
	g := &fakeGadget{}
	sc := &fakeScanner{}
	sc.set("CAFE1234")
	code, seq, _ := sc.CurrentSeq()
	a := New(NewCentralClient(srv.URL, "resin", "k"), g, sc, slog.New(slog.NewTextHandler(io.Discard, nil)))

	rec := httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/scan", nil))
	if !strings.Contains(rec.Body.String(), `"load_status":"pending"`) {
		t.Fatalf("expected a pending load right after a staged+allowed tap: %s", rec.Body.String())
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if status, _ := a.getLoadState(code, seq); status != "pending" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if status, msg := a.getLoadState(code, seq); status != "" {
		t.Errorf("claim race should settle back to nothing-to-show, got status=%q msg=%q", status, msg)
	}
	if g.wrote != "" {
		t.Error("gadget.Write must NOT run when the claim finds nothing staged")
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
