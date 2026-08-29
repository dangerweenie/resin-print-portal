// Package server is the central portal's HTTP layer: the Pi-facing JSON API,
// the org-wide status endpoint, and the server-rendered admin UI.
package server

import (
	"context"
	"encoding/json"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/dangerweenie/resin-print-portal/internal/config"
	"github.com/dangerweenie/resin-print-portal/web"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/gorilla/securecookie"
)

// Server holds the dependencies shared by every handler.
type Server struct {
	st    DataStore
	cfg   *config.Portal
	log   *slog.Logger
	sc    *securecookie.SecureCookie
	pages map[string]*template.Template
	nowFn func() time.Time
}

// New builds a Server and parses the embedded templates.
func New(st DataStore, cfg *config.Portal, log *slog.Logger) (*Server, error) {
	s := &Server{
		st:    st,
		cfg:   cfg,
		log:   log,
		sc:    securecookie.New(cfg.SessionSecret, nil),
		nowFn: time.Now,
	}
	if err := s.parseTemplates(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Server) now() time.Time { return s.nowFn() }

var funcMap = template.FuncMap{
	"humanDuration": func(seconds *int32) string {
		if seconds == nil {
			return "unknown"
		}
		d := time.Duration(*seconds) * time.Second
		h := int(d.Hours())
		m := int(d.Minutes()) % 60
		if h > 0 {
			return strconv.Itoa(h) + "h " + strconv.Itoa(m) + "m"
		}
		return strconv.Itoa(m) + "m"
	},
	"ts": func(t time.Time) string { return t.Format("2006-01-02 15:04") },
	"tsp": func(t *time.Time) string {
		if t == nil {
			return "—"
		}
		return t.Format("2006-01-02 15:04")
	},
}

func (s *Server) parseTemplates() error {
	pages := []string{
		"login.html", "members.html", "printers.html", "printer_edit.html",
		"certifications.html", "jobs.html", "log.html", "settings.html",
	}
	s.pages = make(map[string]*template.Template, len(pages))
	for _, p := range pages {
		t, err := template.New("base.html").Funcs(funcMap).
			ParseFS(web.Templates, "templates/base.html", "templates/"+p)
		if err != nil {
			return err
		}
		s.pages[p] = t
	}
	return nil
}

// Router returns the fully wired HTTP handler.
func (s *Server) Router() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(s.requestLogger)
	r.Use(middleware.Recoverer)

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	r.Get("/readyz", s.handleReadyz)

	if sub, err := fs.Sub(web.Static, "static"); err == nil {
		r.Handle("/static/*", http.StripPrefix("/static/", http.FileServer(http.FS(sub))))
	}

	// Pi-facing API — bearer auth per printer.
	r.Route("/api/v1/printers/{slug}", func(r chi.Router) {
		r.Use(s.printerAuth)
		r.Get("/config", s.handlePrinterConfig)
		r.Post("/check", s.handleCheck)
		r.Post("/print-requests", s.handlePrintRequest)
		r.Get("/current-job", s.handleCurrentJob)
		r.Post("/jobs/{id}/started", s.handleJobStarted)
		r.Post("/jobs/{id}/finished", s.handleJobFinished)
	})

	// Pi self-enrollment — fleet bootstrap token.
	r.With(s.enrollAuth).Post("/api/v1/enroll", s.handleEnroll)

	// Org-wide read-only status — shared key.
	r.With(s.statusAuth).Get("/api/v1/status", s.handleStatus)

	// Admin UI — session auth.
	r.Route("/admin", func(r chi.Router) {
		r.Get("/login", s.handleLoginForm)
		r.Post("/login", s.handleLoginSubmit)
		r.Get("/logout", s.handleLogout)
		r.Group(func(r chi.Router) {
			r.Use(s.adminAuth)
			r.Get("/", func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, "/admin/printers", http.StatusFound)
			})
			r.Get("/members", s.handleMembers)
			r.Post("/members/slack-link", s.handleSlackLink)
			r.Post("/members/slack-unlink", s.handleSlackUnlink)
			r.Get("/printers", s.handlePrinters)
			r.Post("/printers", s.handlePrinterCreate)
			r.Get("/printers/{id}", s.handlePrinterEdit)
			r.Post("/printers/{id}", s.handlePrinterUpdate)
			r.Post("/printers/{id}/rotate-key", s.handlePrinterRotateKey)
			r.Post("/printers/{id}/approve", s.handlePrinterApprove)
			r.Post("/printers/{id}/disable", s.handlePrinterDisable)
			r.Post("/printers/{id}/delete", s.handlePrinterDelete)
			r.Get("/certifications", s.handleCertifications)
			r.Post("/certifications", s.handleCertifyToggle)
			r.Get("/jobs", s.handleJobs)
			r.Post("/jobs/force-clear", s.handleForceClear)
			r.Get("/log", s.handleLog)
			r.Get("/settings", s.handleSettings)
			r.Post("/settings/password", s.handleChangePassword)
		})
	})

	r.Get("/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/admin/", http.StatusFound)
	})
	return r
}

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if err := s.st.Ping(ctx); err != nil {
		http.Error(w, "db unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Write([]byte("ok"))
}

// --- small helpers ---

func (s *Server) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) renderPage(w http.ResponseWriter, page string, data map[string]any) {
	t, ok := s.pages[page]
	if !ok {
		http.Error(w, "template missing: "+page, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.Execute(w, data); err != nil {
		s.log.Error("template execute failed", "page", page, "err", err)
	}
}

func (s *Server) serverError(w http.ResponseWriter, err error, msg string) {
	s.log.Error(msg, "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

func (s *Server) requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)
		s.log.Info("http",
			"method", r.Method, "path", r.URL.Path,
			"status", ww.Status(), "bytes", ww.BytesWritten(),
			"dur_ms", time.Since(start).Milliseconds())
	})
}
