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

// fakeCentral stands in for the portal's Pi-facing API.
func fakeCentral(t *testing.T, approve bool, reason string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/printers/resin/config", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"slug":"resin","display_name":"Resin","approved":true,"safety_checklist":["vat ok"]}`))
	})
	mux.HandleFunc("/api/v1/printers/resin/current-job", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"current_job":null}`))
	})
	mux.HandleFunc("/api/v1/printers/resin/print-requests", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("central: parse form: %v", err)
		}
		if r.FormValue("slack_name") == "" {
			t.Error("central: missing slack_name")
		}
		if _, _, err := r.FormFile("file"); err != nil {
			t.Errorf("central: missing file: %v", err)
		}
		if approve {
			_, _ = w.Write([]byte(`{"approved":true,"job_id":42,"eta_seconds":3600,"eta_exact":true}`))
		} else {
			_, _ = w.Write([]byte(`{"approved":false,"reason":"` + reason + `"}`))
		}
	})
	mux.HandleFunc("/api/v1/printers/resin/jobs/42/started", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

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

func newAgent(t *testing.T, central *httptest.Server, g GadgetWriter) http.Handler {
	t.Helper()
	c := NewCentralClient(central.URL, "resin", "key")
	return New(c, g, slog.New(slog.NewTextHandler(io.Discard, nil))).Handler()
}

func TestSubmitApprovedWritesGadget(t *testing.T) {
	central := fakeCentral(t, true, "")
	g := &fakeGadget{}
	h := newAgent(t, central, g)

	body, ct := multipartBody(t, map[string]string{
		"slack_name": "jane doe", "check_0": "1",
	}, "file", "job.pwsz", "PWSZBYTES")
	req := httptest.NewRequest(http.MethodPost, "/submit", body)
	req.Header.Set("Content-Type", ct)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302 (body: %s)", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "msg=") {
		t.Errorf("redirect Location = %q, want a success msg", loc)
	}
	if g.wrote == "" {
		t.Error("gadget.Write was not called on an approved print")
	}
}

func TestSubmitDeniedDoesNotWriteGadget(t *testing.T) {
	central := fakeCentral(t, false, "not_certified")
	g := &fakeGadget{}
	h := newAgent(t, central, g)

	body, ct := multipartBody(t, map[string]string{"slack_name": "jane doe"}, "file", "job.pwsz", "X")
	req := httptest.NewRequest(http.MethodPost, "/submit", body)
	req.Header.Set("Content-Type", ct)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

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
		"unknown_slack_name", "ambiguous_name", "membership_inactive",
		"not_certified", "extension_not_allowed", "checklist_incomplete",
	} {
		if m := denyMessage(r); m == "" || strings.Contains(m, "(") {
			t.Errorf("denyMessage(%q) = %q, want a plain-English sentence", r, m)
		}
	}
	if m := denyMessage("weird_new_reason"); !strings.Contains(m, "weird_new_reason") {
		t.Errorf("unknown reason should fall through with the code, got %q", m)
	}
}
