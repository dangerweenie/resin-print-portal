package piagent

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeScanner struct {
	mu       sync.Mutex
	code     string
	lastCode string // the code seq was last bumped for, so set() can detect a fresh presentation
	seq      int
	cleared  bool
}

func (s *fakeScanner) CurrentCode() (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.code, s.code != ""
}
func (s *fakeScanner) CurrentSeq() (string, int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.code, s.seq, s.code != ""
}
func (s *fakeScanner) Clear() {
	s.mu.Lock()
	s.code, s.cleared, s.lastCode = "", true, ""
	s.mu.Unlock()
}

// set changes the tapped code, as if a fresh physical presentation just
// happened (bumping seq) whenever it differs from the last value set.
func (s *fakeScanner) set(c string) {
	s.mu.Lock()
	if c != s.lastCode {
		s.seq++
		s.lastCode = c
	}
	s.code = c
	s.mu.Unlock()
}

// fobCentral serves the Pi-facing API: one known fob (CAFE1234 -> Ada) with a
// print staged and ready to load.
func fobCentral(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/printers/resin/config", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"slug":"resin","display_name":"Resin","approved":true}`))
	})
	mux.HandleFunc("/api/v1/printers/resin/current-job", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"current_job":null}`))
	})
	mux.HandleFunc("/api/v1/printers/resin/check", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"rfid_code":"CAFE1234"`) {
			_, _ = w.Write([]byte(`{"allowed":true,"member_name":"Ada Lovelace","staged_filename":"job.goo","staged_eta":"3h 10m"}`))
			return
		}
		_, _ = w.Write([]byte(`{"allowed":false,"reason":"unknown_fob"}`))
	})
	mux.HandleFunc("/api/v1/printers/resin/claim", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"rfid_code":"CAFE1234"`) {
			_, _ = w.Write([]byte(`{"claimed":false,"reason":"unknown_fob"}`))
			return
		}
		_, _ = w.Write([]byte(`{"claimed":true,"job_id":1,"filename":"job.goo","sha256":"","size":3}`))
	})
	mux.HandleFunc("/api/v1/printers/resin/jobs/1/file", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("GOO"))
	})
	mux.HandleFunc("/api/v1/printers/resin/jobs/1/started", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func fobAgent(t *testing.T, srv *httptest.Server, sc ScanSource) (*Agent, *fakeGadget) {
	t.Helper()
	g := &fakeGadget{}
	return New(NewCentralClient(srv.URL, "resin", "k"), g, sc, slog.New(slog.NewTextHandler(io.Discard, nil))), g
}

func TestPiPageIsTapToLoadOnly(t *testing.T) {
	a, _ := fobAgent(t, fobCentral(t), &fakeScanner{})
	rec := httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	body := rec.Body.String()

	if !strings.Contains(body, "Tap your Tinkermill fob") {
		t.Error("expected the tap prompt")
	}
	if strings.Contains(body, `name="slack_name"`) {
		t.Error("the Pi page has no name input")
	}
	if strings.Contains(body, `type="file"`) {
		t.Error("the Pi page no longer accepts file uploads")
	}
	if strings.Contains(body, `action="/load"`) {
		t.Error("loading is automatic now -- there must be no button/form to click")
	}
}

func TestScanEndpointResolvesMemberAndStagedJob(t *testing.T) {
	sc := &fakeScanner{}
	a, _ := fobAgent(t, fobCentral(t), sc)

	rec := httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/scan", nil))
	if !strings.Contains(rec.Body.String(), `"scanned":false`) {
		t.Errorf("no tap yet: %s", rec.Body.String())
	}

	sc.set("CAFE1234")
	rec = httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/scan", nil))
	b := rec.Body.String()
	if !strings.Contains(b, `"allowed":true`) || !strings.Contains(b, "Ada Lovelace") {
		t.Errorf("/scan after tap = %s", b)
	}
	if !strings.Contains(b, `"staged_filename":"job.goo"`) {
		t.Errorf("/scan should surface the staged file: %s", b)
	}
}

func TestTapAutoLoadsWithoutAnyClick(t *testing.T) {
	sc := &fakeScanner{}
	sc.set("CAFE1234")
	a, g := fobAgent(t, fobCentral(t), sc)

	// The tap alone -- via /scan, the same thing WatchTaps calls -- must be
	// enough. Nothing POSTs a load; there is no such endpoint any more.
	rec := httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/scan", nil))
	if !strings.Contains(rec.Body.String(), `"load_status":"pending"`) {
		t.Fatalf("expected the check to kick off a load, got: %s", rec.Body.String())
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && g.wrote == "" {
		time.Sleep(10 * time.Millisecond)
	}
	if g.wrote == "" {
		t.Fatal("gadget.Write was never called -- the tap alone should have loaded it")
	}
	if !strings.HasSuffix(g.wrote, "job.goo") {
		t.Errorf("gadget file should keep the real name, got %q", g.wrote)
	}
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !sc.cleared {
		time.Sleep(10 * time.Millisecond)
	}
	if !sc.cleared {
		t.Error("scanner should be cleared after a successful auto-load")
	}
}

func TestNoTapMeansNoAutoLoad(t *testing.T) {
	a, g := fobAgent(t, fobCentral(t), &fakeScanner{}) // never tapped

	rec := httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/scan", nil))
	if !strings.Contains(rec.Body.String(), `"scanned":false`) {
		t.Fatalf("expected no scan yet, got: %s", rec.Body.String())
	}
	time.Sleep(50 * time.Millisecond) // nothing async to have kicked off, but be sure
	if g.wrote != "" {
		t.Error("gadget.Write must not run without a tap")
	}
}

// checkCountingCentral serves /check and counts how many times it was hit.
func checkCountingCentral(t *testing.T) (*httptest.Server, *int32) {
	t.Helper()
	var hits int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/printers/resin/check", func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = w.Write([]byte(`{"allowed":true,"member_name":"Ada Lovelace"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &hits
}

func TestCheckFobDedupesBySeqNotWallClock(t *testing.T) {
	srv, hits := checkCountingCentral(t)
	a := New(NewCentralClient(srv.URL, "resin", "k"), &fakeGadget{}, &fakeScanner{}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	// Same seq -- the same one continuous hold -- must not hit the portal twice.
	if _, err := a.checkFob(context.Background(), "CAFE", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := a.checkFob(context.Background(), "CAFE", 1); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(hits); got != 1 {
		t.Fatalf("hits = %d, want 1 for two checks of the same (code, seq)", got)
	}

	// A DIFFERENT seq for the same code -- a genuinely new physical tap --
	// must hit the portal again immediately. A wall-clock cache would have
	// suppressed this for up to 30s; that's exactly what made certify-by-tap
	// need two taps instead of one.
	if _, err := a.checkFob(context.Background(), "CAFE", 2); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(hits); got != 2 {
		t.Fatalf("hits = %d, want 2 after a fresh presentation of the same code", got)
	}
}

func TestWatchTapsChecksWithoutAnyHTTPRequest(t *testing.T) {
	srv, hits := checkCountingCentral(t)
	sc := &fakeScanner{}
	a := New(NewCentralClient(srv.URL, "resin", "k"), &fakeGadget{}, sc, slog.New(slog.NewTextHandler(io.Discard, nil)))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.WatchTaps(ctx)

	sc.set("CAFE1234") // a tap, with nobody ever polling /scan
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && atomic.LoadInt32(hits) == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if atomic.LoadInt32(hits) == 0 {
		t.Fatal("WatchTaps never checked the tap, though nothing ever called /scan")
	}
}
