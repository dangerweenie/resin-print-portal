package server

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// DefaultSafetyChecklist seeds a self-enrolled printer's checklist so a new Pi
// is immediately useful without the admin configuring anything. Ported from the
// old Flask app's DEFAULT_SAFETY_CHECKLIST.
var DefaultSafetyChecklist = []string{
	"I have checked the build plate for unremoved prints",
	"I have inspected the resin vat for debris or cured resin chunks",
	"I have confirmed there is sufficient resin in the vat for this print",
	"I have confirmed the build plate is properly secured and level",
	"I have confirmed the FEP film is free of damage, tears, or major cloudiness",
}

// maxPendingPrinters caps how many unapproved printers can pile up, so an open
// (tokenless) enroll endpoint can't be flooded into uselessness. Approving or
// rejecting some frees room.
const maxPendingPrinters = 100

// enrollAuth guards POST /api/v1/enroll. The fleet token is OPTIONAL hardening:
// with no token set, enrollment is open (admin approval is still the real gate);
// with a token set, it must match. Set one if the portal is reachable from the
// public internet.
func (s *Server) enrollAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.EnrollToken != "" &&
			subtle.ConstantTimeCompare([]byte(bearerToken(r)), []byte(s.cfg.EnrollToken)) != 1 {
			s.writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "bad enroll token"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// POST /api/v1/enroll   body: {"device_id": "...", "hostname": "..."}
// Returns the printer's slug + a freshly issued API key. The printer starts
// unapproved; an admin approves it once in the UI.
func (s *Server) handleEnroll(w http.ResponseWriter, r *http.Request) {
	var body struct {
		DeviceID string `json:"device_id"`
		Hostname string `json:"hostname"`
	}
	if err := decodeJSON(r, &body); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json"})
		return
	}
	body.DeviceID = strings.TrimSpace(body.DeviceID)
	body.Hostname = strings.TrimSpace(body.Hostname)
	if body.DeviceID == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "device_id is required"})
		return
	}
	if body.Hostname == "" {
		body.Hostname = "printer"
	}

	// Safety valve for the tokenless case: don't let the Pending list be flooded.
	if s.cfg.EnrollToken == "" {
		if n, err := s.st.CountPendingPrinters(r.Context()); err != nil {
			s.serverError(w, err, "enroll count pending")
			return
		} else if n >= maxPendingPrinters {
			s.writeJSON(w, http.StatusTooManyRequests, map[string]string{
				"error": "too many pending enrollments — an admin needs to approve or reject some first",
			})
			return
		}
	}

	key := newAPIKey()
	p, isNew, err := s.st.EnrollPrinter(r.Context(), body.DeviceID, body.Hostname,
		DefaultSafetyChecklist, key, HashAPIKey(key))
	if err != nil {
		s.serverError(w, err, "enroll")
		return
	}
	s.log.Info("pi enrolled", "slug", p.Slug, "device_id", body.DeviceID, "new", isNew, "approved", p.Approved)
	s.writeJSON(w, http.StatusOK, map[string]any{
		"slug":     p.Slug,
		"api_key":  key,
		"approved": p.Approved,
	})
}
