package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dangerweenie/resin-print-portal/internal/config"
	"github.com/dangerweenie/resin-print-portal/internal/store"
)

// TestPrintersPageRendersAgentColumns exercises every field path the reworked
// printers.html template touches, so a bad {{.Field}} fails a test, not a page.
func TestPrintersPageRendersAgentColumns(t *testing.T) {
	s := newTestServer(t, &fakeStore{})
	rows := []printerRow{{
		Printer: store.Printer{
			ID: 1, Slug: "resin", DisplayName: "Resin", Approved: true,
			AgentVersion: "v1", AgentTargetOverride: "v2",
		},
		LastSeen: "3m ago", AgentVersion: "v1", UpdateState: "update pending", Behind: true,
	}}
	data := map[string]any{
		"User": "captain", "Pending": nil, "Active": rows,
		"NewKey": "", "PortalVersion": "v2", "AutoUpdate": true, "CanServe": true,
	}
	if err := s.pages["printers.html"].Execute(io.Discard, data); err != nil {
		t.Fatalf("render printers.html: %v", err)
	}
}

func agentUpdateResp(t *testing.T, s *Server, printer store.Printer, running string) map[string]any {
	t.Helper()
	fs := &fakeStore{printer: printer}
	// swap in a store that returns our printer for the bearer lookup
	s.st = fs
	req := httptest.NewRequest(http.MethodGet, "/api/v1/printers/resin/agent-update", nil)
	req.Header.Set("Authorization", "Bearer tok")
	if running != "" {
		req.Header.Set("X-Agent-Version", running)
	}
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestAgentUpdateOffersBundledBuildWhenAutoOn(t *testing.T) {
	s := newTestServer(t, &fakeStore{})
	s.cfg.AgentAutoUpdate = true
	s.agentBin = agentBinary{OK: true, Version: "v2", SHA256: "deadbeef", Size: 42}
	p := store.Printer{ID: 7, Slug: "resin", Approved: true, AgentVersion: "v1"}

	out := agentUpdateResp(t, s, p, "v1")

	if out["update_available"] != true {
		t.Fatalf("update_available = %v, want true", out["update_available"])
	}
	if out["target_version"] != "v2" || out["sha256"] != "deadbeef" {
		t.Fatalf("resp = %+v", out)
	}
	if out["url"] != "/api/v1/printers/resin/agent-binary" {
		t.Fatalf("url = %v", out["url"])
	}
}

func TestAgentUpdateSilentWhenAlreadyOnTarget(t *testing.T) {
	s := newTestServer(t, &fakeStore{})
	s.cfg.AgentAutoUpdate = true
	s.agentBin = agentBinary{OK: true, Version: "v2", SHA256: "x"}
	p := store.Printer{ID: 7, Slug: "resin", Approved: true, AgentVersion: "v2"}

	out := agentUpdateResp(t, s, p, "v2")

	if out["update_available"] != false {
		t.Fatalf("update_available = %v, want false", out["update_available"])
	}
}

func TestAgentUpdateSilentWhenAutoOffAndNoOverride(t *testing.T) {
	s := newTestServer(t, &fakeStore{})
	s.cfg = &config.Portal{AgentAutoUpdate: false}
	s.agentBin = agentBinary{OK: true, Version: "v2", SHA256: "x"}
	p := store.Printer{ID: 7, Slug: "resin", Approved: true, AgentVersion: "v1"}

	out := agentUpdateResp(t, s, p, "v1")

	if out["update_available"] != false || out["target_version"] != "" {
		t.Fatalf("resp = %+v, want no update and empty target", out)
	}
}

func TestAgentUpdateHonoursPerPiOverride(t *testing.T) {
	s := newTestServer(t, &fakeStore{})
	s.cfg = &config.Portal{AgentAutoUpdate: false} // global auto off
	s.agentBin = agentBinary{OK: true, Version: "v2", SHA256: "x"}
	p := store.Printer{ID: 7, Slug: "resin", Approved: true, AgentVersion: "v1", AgentTargetOverride: "v2"}

	out := agentUpdateResp(t, s, p, "v1")

	if out["update_available"] != true || out["target_version"] != "v2" {
		t.Fatalf("resp = %+v, want update to v2 from the per-Pi override", out)
	}
}

func TestAgentUpdateHeldPiGetsNothing(t *testing.T) {
	s := newTestServer(t, &fakeStore{})
	s.cfg.AgentAutoUpdate = true
	s.agentBin = agentBinary{OK: true, Version: "v2", SHA256: "x"}
	p := store.Printer{ID: 7, Slug: "resin", Approved: true, AgentVersion: "v1", AgentUpdateHold: true}

	out := agentUpdateResp(t, s, p, "v1")

	if out["update_available"] != false || out["target_version"] != "" {
		t.Fatalf("resp = %+v, want a held Pi left alone", out)
	}
}
