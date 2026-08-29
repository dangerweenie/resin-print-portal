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

// fakeStore implements just enough of DataStore for the check-endpoint tests.
// Any unimplemented method panics via the embedded nil interface.
type fakeStore struct {
	DataStore
	printer   store.Printer
	resolve   func(norm string) (store.Member, bool, error)
	certified bool
	logged    []store.DecisionLogEntry
}

func (f *fakeStore) GetPrinterByKeyHash(context.Context, string) (store.Printer, error) {
	return f.printer, nil
}
func (f *fakeStore) ResolveSlackName(_ context.Context, norm string) (store.Member, bool, error) {
	return f.resolve(norm)
}
func (f *fakeStore) IsCertified(context.Context, int64, int64) (bool, error) {
	return f.certified, nil
}
func (f *fakeStore) LogDecision(_ context.Context, e store.DecisionLogEntry) error {
	f.logged = append(f.logged, e)
	return nil
}
func (f *fakeStore) TouchPrinterSeen(context.Context, int64) error { return nil }

func newTestServer(t *testing.T, fs *fakeStore) *Server {
	t.Helper()
	s, err := New(fs, &config.Portal{SessionSecret: []byte(strings.Repeat("k", 32))}, slog.New(slog.NewTextHandler(discard{}, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

func member(id int64, active bool) store.Member {
	return store.Member{ID: id, Name: "Jane Doe", Status: "A", Active: active}
}

func TestCheckDecisionMatrix(t *testing.T) {
	printer := store.Printer{ID: 7, Slug: "resin", Approved: true}

	tests := []struct {
		name        string
		resolve     func(string) (store.Member, bool, error)
		certified   bool
		wantAllowed bool
		wantReason  string
		wantOutcome string
	}{
		{
			name:        "approved",
			resolve:     func(string) (store.Member, bool, error) { return member(1, true), false, nil },
			certified:   true,
			wantAllowed: true, wantOutcome: OutcomeApproved,
		},
		{
			name:        "approved by name match",
			resolve:     func(string) (store.Member, bool, error) { return member(1, true), true, nil },
			certified:   true,
			wantAllowed: true, wantOutcome: OutcomeApprovedByName,
		},
		{
			name:        "unknown slack name",
			resolve:     func(string) (store.Member, bool, error) { return store.Member{}, false, store.ErrNotFound },
			wantAllowed: false, wantReason: ReasonUnknownName, wantOutcome: OutcomeDenied,
		},
		{
			name:        "ambiguous name",
			resolve:     func(string) (store.Member, bool, error) { return store.Member{}, false, store.ErrAmbiguousName },
			wantAllowed: false, wantReason: ReasonAmbiguousName, wantOutcome: OutcomeDenied,
		},
		{
			name:        "membership inactive",
			resolve:     func(string) (store.Member, bool, error) { return member(1, false), false, nil },
			certified:   true,
			wantAllowed: false, wantReason: ReasonMembershipInactive, wantOutcome: OutcomeDenied,
		},
		{
			name:        "not certified",
			resolve:     func(string) (store.Member, bool, error) { return member(1, true), false, nil },
			certified:   false,
			wantAllowed: false, wantReason: ReasonNotCertified, wantOutcome: OutcomeDenied,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fs := &fakeStore{printer: printer, resolve: tc.resolve, certified: tc.certified}
			s := newTestServer(t, fs)

			req := httptest.NewRequest(http.MethodPost, "/api/v1/printers/resin/check",
				strings.NewReader(`{"slack_name":"jane doe"}`))
			req.Header.Set("Authorization", "Bearer sometoken")
			rec := httptest.NewRecorder()
			s.Router().ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			var resp struct {
				Allowed bool   `json:"allowed"`
				Reason  string `json:"reason"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode: %v (%s)", err, rec.Body.String())
			}
			if resp.Allowed != tc.wantAllowed || resp.Reason != tc.wantReason {
				t.Errorf("allowed=%v reason=%q, want allowed=%v reason=%q",
					resp.Allowed, resp.Reason, tc.wantAllowed, tc.wantReason)
			}
			if len(fs.logged) != 1 || fs.logged[0].Outcome != tc.wantOutcome {
				t.Errorf("decision_log = %+v, want one entry outcome=%q", fs.logged, tc.wantOutcome)
			}
		})
	}
}

func TestCheckRejectsMissingBearer(t *testing.T) {
	fs := &fakeStore{
		printer: store.Printer{ID: 1, Slug: "resin"},
		resolve: func(string) (store.Member, bool, error) { return store.Member{}, false, nil },
	}
	s := newTestServer(t, fs)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/printers/resin/check",
		strings.NewReader(`{"slack_name":"x"}`))
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}
