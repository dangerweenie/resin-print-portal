package piagent

import (
	"bytes"
	"io"
	"log/slog"
	"mime/multipart"
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

// fobCentral serves the Pi-facing API, resolving one known fob to "Ada".
func fobCentral(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/printers/resin/config", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"slug":"resin","display_name":"Resin","approved":true,"safety_checklist":["vat ok"]}`))
	})
	mux.HandleFunc("/api/v1/printers/resin/current-job", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"current_job":null}`))
	})
	mux.HandleFunc("/api/v1/printers/resin/check", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"rfid_code":"CAFE1234"`) {
			_, _ = w.Write([]byte(`{"allowed":true,"member_name":"Ada Lovelace"}`))
			return
		}
		_, _ = w.Write([]byte(`{"allowed":false,"reason":"unknown_fob"}`))
	})
	mux.HandleFunc("/api/v1/printers/resin/print-requests", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseMultipartForm(1 << 20)
		if r.FormValue("rfid_code") != "CAFE1234" {
			t.Errorf("print-request rfid_code = %q, want CAFE1234", r.FormValue("rfid_code"))
		}
		if r.FormValue("slack_name") != "" {
			t.Errorf("slack_name should be empty on a fob submit, got %q", r.FormValue("slack_name"))
		}
		_, _ = w.Write([]byte(`{"approved":true,"job_id":1}`))
	})
	mux.HandleFunc("/api/v1/printers/resin/jobs/1/started", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func fobAgent(t *testing.T, srv *httptest.Server, sc ScanSource) *Agent {
	t.Helper()
	return New(NewCentralClient(srv.URL, "resin", "k"), &fakeGadget{}, sc, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestFobOnlyPageHasNoNameField(t *testing.T) {
	a := fobAgent(t, fobCentral(t), &fakeScanner{})
	rec := httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	body := rec.Body.String()
	if !strings.Contains(body, "Tap your Tinkermill fob") {
		t.Error("expected the tap prompt")
	}
	if strings.Contains(body, `name="slack_name"`) {
		t.Error("fob-only page has no name input")
	}
	if !strings.Contains(body, `id="sendbtn" disabled`) {
		t.Error("send button should start disabled until a fob is tapped")
	}
}

func TestScanEndpointResolvesMember(t *testing.T) {
	sc := &fakeScanner{}
	a := fobAgent(t, fobCentral(t), sc)

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
}

func TestSubmitUsesTappedFobNotBrowserInput(t *testing.T) {
	sc := &fakeScanner{}
	sc.set("CAFE1234")
	g := &fakeGadget{}
	a := New(NewCentralClient(fobCentral(t).URL, "resin", "k"), g, sc, slog.New(slog.NewTextHandler(io.Discard, nil)))

	// The browser tries to smuggle a different identity — it must be ignored.
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("slack_name", "someone.else")
	_ = mw.WriteField("check_0", "1")
	fw, _ := mw.CreateFormFile("file", "job.goo")
	_, _ = fw.Write([]byte("GOO"))
	_ = mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/submit", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if g.wrote == "" {
		t.Error("gadget.Write not called on an approved fob submit")
	}
	if !sc.cleared {
		t.Error("scanner should be cleared after a successful submit")
	}
}

func TestSubmitRequiresTap(t *testing.T) {
	a := fobAgent(t, fobCentral(t), &fakeScanner{}) // no tap

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "job.goo")
	_, _ = fw.Write([]byte("GOO"))
	_ = mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/submit", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, req)

	if !strings.Contains(rec.Body.String(), "Tap your Tinkermill fob") {
		t.Errorf("expected a tap-first error, got: %s", rec.Body.String())
	}
}
