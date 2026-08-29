package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dangerweenie/resin-print-portal/internal/config"
	"github.com/dangerweenie/resin-print-portal/internal/store"
)

type enrollFake struct {
	DataStore
	gotDevice, gotHost string
	printer            store.Printer
	pending            int
}

func (f *enrollFake) EnrollPrinter(_ context.Context, deviceID, hostname string, _ []string, _, keyHash string) (store.Printer, bool, error) {
	f.gotDevice, f.gotHost = deviceID, hostname
	p := f.printer
	p.APIKeyHash = keyHash
	return p, true, nil
}
func (f *enrollFake) GetPrinterByKeyHash(context.Context, string) (store.Printer, error) {
	return f.printer, nil
}
func (f *enrollFake) TouchPrinterSeen(context.Context, int64) error             { return nil }
func (f *enrollFake) LogDecision(context.Context, store.DecisionLogEntry) error { return nil }
func (f *enrollFake) CountPendingPrinters(context.Context) (int, error)         { return f.pending, nil }

func enrollServer(t *testing.T, token string, fs DataStore) *Server {
	t.Helper()
	s, err := New(fs, &config.Portal{
		SessionSecret: []byte(strings.Repeat("k", 32)),
		EnrollToken:   token,
	}, slog.New(slog.NewTextHandler(discard{}, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestEnrollHappyPath(t *testing.T) {
	fs := &enrollFake{printer: store.Printer{ID: 3, Slug: "resin1", Approved: false}}
	s := enrollServer(t, "fleet-secret", fs)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/enroll",
		strings.NewReader(`{"device_id":"pi-abc123","hostname":"resin1"}`))
	req.Header.Set("Authorization", "Bearer fleet-secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Slug     string `json:"slug"`
		APIKey   string `json:"api_key"`
		Approved bool   `json:"approved"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Slug != "resin1" || out.APIKey == "" || out.Approved {
		t.Errorf("out = %+v, want slug=resin1, non-empty key, approved=false", out)
	}
	if fs.gotDevice != "pi-abc123" || fs.gotHost != "resin1" {
		t.Errorf("store got device=%q host=%q", fs.gotDevice, fs.gotHost)
	}
}

func TestEnrollRejectsBadToken(t *testing.T) {
	s := enrollServer(t, "fleet-secret", &enrollFake{})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/enroll", strings.NewReader(`{"device_id":"x"}`))
	req.Header.Set("Authorization", "Bearer wrong")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestEnrollOpenWhenNoToken(t *testing.T) {
	// No token configured => enrollment is open; admin approval is still the gate.
	fs := &enrollFake{printer: store.Printer{ID: 9, Slug: "shopfloor", Approved: false}}
	s := enrollServer(t, "", fs)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/enroll",
		strings.NewReader(`{"device_id":"pi-xyz","hostname":"shopfloor"}`))
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (open enrollment): %s", rec.Code, rec.Body.String())
	}
}

func TestEnrollFloodGuardWhenTokenless(t *testing.T) {
	fs := &enrollFake{printer: store.Printer{ID: 1}, pending: maxPendingPrinters}
	s := enrollServer(t, "", fs)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/enroll",
		strings.NewReader(`{"device_id":"pi-new","hostname":"h"}`))
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 when the pending list is full", rec.Code)
	}
}

func TestCheckDeniedWhilePrinterPending(t *testing.T) {
	fs := &enrollFake{printer: store.Printer{ID: 3, Slug: "resin1", Approved: false}}
	s := enrollServer(t, "fleet-secret", fs)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/printers/resin1/check",
		strings.NewReader(`{"slack_name":"whoever"}`))
	req.Header.Set("Authorization", "Bearer sometoken")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var out struct {
		Allowed bool   `json:"allowed"`
		Reason  string `json:"reason"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Allowed || out.Reason != ReasonPendingApproval {
		t.Errorf("out = %+v, want denied/%s", out, ReasonPendingApproval)
	}
}
