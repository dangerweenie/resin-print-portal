package piagent

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"mime/multipart"
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

func multipartBody(t *testing.T, fields map[string]string, fileField, fileName, fileBody string) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		_ = mw.WriteField(k, v)
	}
	fw, err := mw.CreateFormFile(fileField, fileName)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(fw, strings.NewReader(fileBody))
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf, mw.FormDataContentType()
}

// deniedFobCentral answers every /check and /print-requests with a denial.
func deniedFobCentral(t *testing.T, reason string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/printers/resin/config", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"slug":"resin","approved":true,"safety_checklist":["vat ok"]}`))
	})
	mux.HandleFunc("/api/v1/printers/resin/current-job", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"current_job":null}`))
	})
	deny := []byte(`{"allowed":false,"approved":false,"reason":"` + reason + `"}`)
	mux.HandleFunc("/api/v1/printers/resin/check", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(deny) })
	mux.HandleFunc("/api/v1/printers/resin/print-requests", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseMultipartForm(1 << 20)
		_, _ = w.Write(deny)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestSubmitDeniedFobDoesNotWriteGadget(t *testing.T) {
	srv := deniedFobCentral(t, "not_certified")
	g := &fakeGadget{}
	sc := &fakeScanner{}
	sc.set("DEADBEEF")
	a := New(NewCentralClient(srv.URL, "resin", "k"), g, sc, slog.New(slog.NewTextHandler(io.Discard, nil)))

	body, ct := multipartBody(t, map[string]string{"check_0": "1"}, "file", "job.goo", "GOO")
	req := httptest.NewRequest(http.MethodPost, "/submit", body)
	req.Header.Set("Content-Type", ct)
	rec := httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (re-rendered form)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "not certified") {
		t.Errorf("expected a friendly not-certified message, got: %s", rec.Body.String())
	}
	if g.wrote != "" {
		t.Error("gadget.Write must NOT be called on a denied print")
	}
}

func TestDenyMessageKnownReasons(t *testing.T) {
	for _, r := range []string{
		"unknown_fob", "not_certified", "membership_inactive",
		"extension_not_allowed", "checklist_incomplete", "printer_pending_approval",
	} {
		if m := denyMessage(r); m == "" || strings.Contains(m, "(") {
			t.Errorf("denyMessage(%q) = %q, want a plain-English sentence", r, m)
		}
	}
	if m := denyMessage("weird_new_reason"); !strings.Contains(m, "weird_new_reason") {
		t.Errorf("unknown reason should fall through with the code, got %q", m)
	}
}
