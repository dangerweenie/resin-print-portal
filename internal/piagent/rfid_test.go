package piagent

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type fakeScanner struct {
	mu      sync.Mutex
	code    string
	cleared bool
}

func (s *fakeScanner) CurrentCode() (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.code, s.code != ""
}
func (s *fakeScanner) Clear() {
	s.mu.Lock()
	s.code, s.cleared = "", true
	s.mu.Unlock()
}
func (s *fakeScanner) set(c string) { s.mu.Lock(); s.code = c; s.mu.Unlock() }

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
	if !strings.Contains(body, `action="/load"`) {
		t.Error("expected the load form")
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

func TestLoadUsesTappedFobAndWritesGadget(t *testing.T) {
	sc := &fakeScanner{}
	sc.set("CAFE1234")
	a, g := fobAgent(t, fobCentral(t), sc)

	req := httptest.NewRequest(http.MethodPost, "/load", nil)
	rec := httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if g.wrote == "" {
		t.Error("gadget.Write not called after a successful load")
	}
	if !strings.HasSuffix(g.wrote, "job.goo") {
		t.Errorf("gadget file should keep the real name, got %q", g.wrote)
	}
	if !sc.cleared {
		t.Error("scanner should be cleared after a successful load")
	}
}

func TestLoadRequiresTap(t *testing.T) {
	a, _ := fobAgent(t, fobCentral(t), &fakeScanner{}) // no tap

	rec := httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/load", nil))

	if !strings.Contains(rec.Body.String(), "Tap your Tinkermill fob") {
		t.Errorf("expected a tap-first error, got: %s", rec.Body.String())
	}
}
