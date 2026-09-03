package piagent

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dangerweenie/resin-print-portal/internal/config"
)

func TestEnrollUntilSuccessPersistsCreds(t *testing.T) {
	var gotToken string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/enroll" {
			http.NotFound(w, r)
			return
		}
		gotToken = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"device_id"`) {
			t.Errorf("enroll body missing device_id: %s", body)
		}
		_, _ = w.Write([]byte(`{"slug":"resin7","api_key":"issued-key-123","approved":false}`))
	}))
	defer srv.Close()

	credsPath := filepath.Join(t.TempDir(), "sub", "creds.env")
	cfg := &config.PiAgent{
		CentralBaseURL: srv.URL,
		EnrollToken:    "fleet-secret",
		CredsPath:      credsPath,
	}
	client := NewCentralClient(srv.URL, "", "")

	if err := EnrollUntilSuccess(context.Background(), client, cfg, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatalf("EnrollUntilSuccess: %v", err)
	}
	if gotToken != "fleet-secret" {
		t.Errorf("portal saw token %q", gotToken)
	}
	if cfg.PrinterSlug != "resin7" || cfg.PrinterAPIKey != "issued-key-123" {
		t.Errorf("cfg not updated: slug=%q key=%q", cfg.PrinterSlug, cfg.PrinterAPIKey)
	}
	saved, err := os.ReadFile(credsPath)
	if err != nil {
		t.Fatalf("creds file not written: %v", err)
	}
	if !strings.Contains(string(saved), "PRINTER_SLUG=resin7") ||
		!strings.Contains(string(saved), "PRINTER_API_KEY=issued-key-123") {
		t.Errorf("creds file contents:\n%s", saved)
	}
	// The client should now be able to build printer URLs with the new slug.
	if !strings.Contains(client.url("/config"), "/printers/resin7/") {
		t.Errorf("client url = %q", client.url("/config"))
	}
}

func TestUploadPageShowsPendingNotice(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/config"):
			_, _ = w.Write([]byte(`{"slug":"resin","display_name":"Resin M7","approved":false}`))
		case strings.HasSuffix(r.URL.Path, "/current-job"):
			_, _ = w.Write([]byte(`{"current_job":null}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	a := New(NewCentralClient(srv.URL, "resin", "k"), &fakeGadget{}, &fakeScanner{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	rec := httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	body := rec.Body.String()
	if !strings.Contains(body, "Waiting for approval") {
		t.Errorf("expected the pending notice, got:\n%s", body)
	}
	if strings.Contains(body, "Tap your Tinkermill fob") {
		t.Error("the upload form (fob prompt) should be hidden while pending approval")
	}
}

// --- re-registration -------------------------------------------------------

// reRegServer serves /config with a settable status and /enroll that always
// issues a fresh printer, recording whether it was hit.
type reRegServer struct {
	*httptest.Server
	configStatus int
	enrollHits   int
}

func newReRegServer(t *testing.T) *reRegServer {
	t.Helper()
	s := &reRegServer{configStatus: http.StatusOK}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/enroll", func(w http.ResponseWriter, _ *http.Request) {
		s.enrollHits++
		_, _ = w.Write([]byte(`{"slug":"resin-new","api_key":"key-new","approved":false}`))
	})
	mux.HandleFunc("/api/v1/printers/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/config") {
			w.WriteHeader(s.configStatus)
			if s.configStatus == http.StatusOK {
				_, _ = w.Write([]byte(`{"slug":"resin-old","approved":true}`))
			}
			return
		}
		http.NotFound(w, r)
	})
	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestVerifyDetectsRejectedCredentials(t *testing.T) {
	s := newReRegServer(t)
	s.configStatus = http.StatusUnauthorized
	c := NewCentralClient(s.URL, "resin-old", "stale")
	if err := c.Verify(context.Background()); err != ErrCredentialsRejected {
		t.Fatalf("Verify() = %v, want ErrCredentialsRejected", err)
	}
}

func TestEnsureRegisteredReEnrollsWhenRejected(t *testing.T) {
	s := newReRegServer(t)
	s.configStatus = http.StatusUnauthorized

	creds := filepath.Join(t.TempDir(), "creds.env")
	_ = config.WriteCredsFile(creds, "resin-old", "stale")
	cfg := &config.PiAgent{CentralBaseURL: s.URL, CredsPath: creds, PrinterSlug: "resin-old", PrinterAPIKey: "stale"}
	c := NewCentralClient(s.URL, "resin-old", "stale")

	if err := ensureRegistered(context.Background(), c, cfg, quietLog()); err != nil {
		t.Fatalf("ensureRegistered: %v", err)
	}
	if s.enrollHits != 1 {
		t.Errorf("enroll hits = %d, want 1", s.enrollHits)
	}
	if cfg.PrinterSlug != "resin-new" || cfg.PrinterAPIKey != "key-new" {
		t.Errorf("cfg not updated: %q / %q", cfg.PrinterSlug, cfg.PrinterAPIKey)
	}
	b, _ := os.ReadFile(creds)
	if !strings.Contains(string(b), "PRINTER_SLUG=resin-new") {
		t.Errorf("creds file not rewritten:\n%s", b)
	}
}

func TestEnsureRegisteredKeepsGoodCredentials(t *testing.T) {
	s := newReRegServer(t) // /config -> 200
	cfg := &config.PiAgent{CentralBaseURL: s.URL, PrinterSlug: "resin-old", PrinterAPIKey: "good"}
	c := NewCentralClient(s.URL, "resin-old", "good")

	if err := ensureRegistered(context.Background(), c, cfg, quietLog()); err != nil {
		t.Fatalf("ensureRegistered: %v", err)
	}
	if s.enrollHits != 0 {
		t.Errorf("should not re-enroll with valid credentials, enroll hits = %d", s.enrollHits)
	}
}

func TestEnsureRegisteredKeepsCredentialsWhenPortalDown(t *testing.T) {
	s := newReRegServer(t)
	s.configStatus = http.StatusBadGateway // transient, not an auth failure

	cfg := &config.PiAgent{CentralBaseURL: s.URL, PrinterSlug: "resin-old", PrinterAPIKey: "good"}
	c := NewCentralClient(s.URL, "resin-old", "good")

	if err := ensureRegistered(context.Background(), c, cfg, quietLog()); err != nil {
		t.Fatalf("ensureRegistered: %v", err)
	}
	if s.enrollHits != 0 {
		t.Errorf("must NOT re-enroll on a transient portal error, enroll hits = %d", s.enrollHits)
	}
	if cfg.PrinterSlug != "resin-old" {
		t.Errorf("credentials should be untouched, got slug %q", cfg.PrinterSlug)
	}
}
