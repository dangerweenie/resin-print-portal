package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/dangerweenie/resin-print-portal/internal/store"
	"github.com/go-chi/chi/v5"
)

type ctxKey int

const (
	ctxPrinter ctxKey = iota
	ctxAdmin
)

const sessionCookie = "portal_admin"

// HashAPIKey returns the lowercase hex sha-256 of a Pi API key. The plaintext
// key is shown once at printer creation; only this hash is stored.
func HashAPIKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// printerAuth authenticates a Pi request by its bearer token and checks the
// token's printer matches the {slug} in the path.
func (s *Server) printerAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := bearerToken(r)
		if tok == "" {
			s.writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "missing bearer token"})
			return
		}
		printer, err := s.st.GetPrinterByKeyHash(r.Context(), HashAPIKey(tok))
		if errors.Is(err, store.ErrNotFound) {
			s.writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid token"})
			return
		}
		if err != nil {
			s.serverError(w, err, "printerAuth lookup")
			return
		}
		if slug := chi.URLParam(r, "slug"); slug != "" && slug != printer.Slug {
			s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "token/slug mismatch"})
			return
		}
		s.touchSeen(printer.ID)
		if v := strings.TrimSpace(r.Header.Get("X-Agent-Version")); v != "" && v != printer.AgentVersion {
			s.recordAgentVersion(printer.ID, v)
		}
		ctx := context.WithValue(r.Context(), ctxPrinter, printer)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// recordAgentVersion persists a changed pi-agent version out of band. The store
// method only writes when the value actually differs, so this is a no-op write
// on the common path even though we already guard on the loaded row.
func (s *Server) recordAgentVersion(printerID int64, version string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := s.st.SetPrinterAgentVersion(ctx, printerID, version); err != nil {
			s.log.Debug("record agent version failed", "printer", printerID, "err", err)
		}
	}()
}

func printerFrom(ctx context.Context) store.Printer {
	p, _ := ctx.Value(ctxPrinter).(store.Printer)
	return p
}

// touchSeen records a Pi heartbeat without adding latency to the request. The
// store throttles the write to at most once a minute per printer.
func (s *Server) touchSeen(printerID int64) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := s.st.TouchPrinterSeen(ctx, printerID); err != nil {
			s.log.Debug("touch last_seen failed", "printer", printerID, "err", err)
		}
	}()
}

// statusAuth guards the org-wide status endpoint with the shared key.
func (s *Server) statusAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.StatusAPIKey == "" {
			s.writeJSON(w, http.StatusNotFound, map[string]string{"error": "status endpoint disabled"})
			return
		}
		given := r.Header.Get("X-Api-Key")
		if given == "" {
			given = bearerToken(r)
		}
		if subtle.ConstantTimeCompare([]byte(given), []byte(s.cfg.StatusAPIKey)) != 1 {
			s.writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// adminAuth requires a valid signed session cookie.
func (s *Server) adminAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, ok := s.currentAdmin(r)
		if !ok {
			http.Redirect(w, r, "/admin/login?next="+r.URL.Path, http.StatusFound)
			return
		}
		ctx := context.WithValue(r.Context(), ctxAdmin, user)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func adminFrom(ctx context.Context) string {
	u, _ := ctx.Value(ctxAdmin).(string)
	return u
}

func (s *Server) currentAdmin(r *http.Request) (string, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return "", false
	}
	var v map[string]string
	if err := s.sc.Decode(sessionCookie, c.Value, &v); err != nil {
		return "", false
	}
	if v["u"] == "" {
		return "", false
	}
	return v["u"], true
}

func (s *Server) setSession(w http.ResponseWriter, r *http.Request, username string) error {
	enc, err := s.sc.Encode(sessionCookie, map[string]string{
		"u": username,
		"t": time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    enc,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   isHTTPS(r),
		MaxAge:   int((7 * 24 * time.Hour).Seconds()),
	})
	return nil
}

// isHTTPS reports whether the original client request was over TLS, accounting
// for a TLS-terminating proxy (the Ingress).
func isHTTPS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func (s *Server) clearSession(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/", HttpOnly: true, MaxAge: -1,
	})
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if strings.HasPrefix(h, "Bearer ") {
		return strings.TrimSpace(h[len("Bearer "):])
	}
	if h != "" && !strings.Contains(h, " ") {
		return h
	}
	return ""
}
