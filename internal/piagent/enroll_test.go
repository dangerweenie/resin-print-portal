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

	a := New(NewCentralClient(srv.URL, "resin", "k"), &fakeGadget{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	rec := httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	body := rec.Body.String()
	if !strings.Contains(body, "Waiting for approval") {
		t.Errorf("expected the pending notice, got:\n%s", body)
	}
	if strings.Contains(body, `name="slack_name"`) {
		t.Error("the upload form should be hidden while pending approval")
	}
}
