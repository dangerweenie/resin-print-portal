package server

import (
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/dangerweenie/resin-print-portal/internal/buildinfo"
	"github.com/dangerweenie/resin-print-portal/internal/store"
)

// portalVersion is the version string of this running portal — and, because the
// pi-agent binary in this image is built from the same commit, the version the
// portal can serve for a self-update.
func (s *Server) portalVersion() string { return buildinfo.Resolve() }

// effectiveAgentTarget is the version a given Pi should be running, or "" if the
// portal isn't managing this Pi's version right now.
func (s *Server) effectiveAgentTarget(p store.Printer) string {
	if p.AgentTargetOverride != "" {
		return p.AgentTargetOverride
	}
	if p.AgentUpdateHold {
		return ""
	}
	if s.cfg.AgentAutoUpdate {
		if s.cfg.AgentTargetVersion != "" {
			return s.cfg.AgentTargetVersion
		}
		return s.agentBin.Version
	}
	return ""
}

// GET /api/v1/printers/{slug}/agent-update
// The Pi polls this on a slow ticker. It answers with the version it should be
// on and, when the portal actually holds that binary and the Pi isn't already
// running it, where to fetch it plus the checksum to verify.
func (s *Server) handleAgentUpdate(w http.ResponseWriter, r *http.Request) {
	p := printerFrom(r.Context())
	running := strings.TrimSpace(r.Header.Get("X-Agent-Version"))
	target := s.effectiveAgentTarget(p)

	resp := map[string]any{
		"portal_version":   s.portalVersion(),
		"target_version":   target,
		"update_available": false,
	}
	// The portal only serves its own bundled build. If an override pins some
	// other string we can't fulfil it — say so and stop.
	if target != "" && s.agentBin.OK && target == s.agentBin.Version && running != target {
		resp["update_available"] = true
		resp["url"] = "/api/v1/printers/" + p.Slug + "/agent-binary"
		resp["sha256"] = s.agentBin.SHA256
		resp["size"] = s.agentBin.Size
	}
	s.writeJSON(w, http.StatusOK, resp)
}

// GET /api/v1/printers/{slug}/agent-binary
// Streams the cross-compiled pi-agent binary. Printer bearer auth (same as the
// rest of /api/v1/printers/{slug}).
func (s *Server) handleAgentBinary(w http.ResponseWriter, r *http.Request) {
	if !s.agentBin.OK {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "this portal build has no pi-agent binary to serve",
		})
		return
	}
	f, err := os.Open(s.agentBin.Path)
	if err != nil {
		s.serverError(w, err, "open agent binary")
		return
	}
	defer f.Close()
	mt := time.Time{}
	if st, serr := f.Stat(); serr == nil {
		mt = st.ModTime()
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("ETag", `"`+s.agentBin.SHA256+`"`)
	http.ServeContent(w, r, "pi-agent-armv6", mt, f)
}

// POST /admin/printers/{id}/agent-update   form: action=now|hold|clear
func (s *Server) handlePrinterAgentUpdate(w http.ResponseWriter, r *http.Request) {
	p, ok := s.printerByIDParam(w, r)
	if !ok {
		return
	}
	var override string
	var hold bool
	switch r.FormValue("action") {
	case "now":
		override = s.agentBin.Version // pin to what the portal can actually serve
	case "hold":
		hold = true
	case "clear":
		// both zero — follow the fleet default
	default:
		http.Redirect(w, r, "/admin/printers?err=unknown+action", http.StatusFound)
		return
	}
	if err := s.st.SetPrinterAgentUpdate(r.Context(), p.ID, override, hold); err != nil {
		s.serverError(w, err, "set agent update")
		return
	}
	http.Redirect(w, r, "/admin/printers?msg=updated+"+p.Slug, http.StatusFound)
}
