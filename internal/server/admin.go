package server

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/dangerweenie/resin-print-portal/internal/store"
	"github.com/go-chi/chi/v5"
	"golang.org/x/crypto/bcrypt"
)

func (s *Server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.currentAdmin(r); ok {
		http.Redirect(w, r, "/admin/", http.StatusFound)
		return
	}
	s.renderPage(w, "login.html", map[string]any{
		"Err":  r.URL.Query().Get("err"),
		"Next": r.URL.Query().Get("next"),
	})
}

func (s *Server) handleLoginSubmit(w http.ResponseWriter, r *http.Request) {
	username := strings.TrimSpace(r.FormValue("username"))
	password := r.FormValue("password")
	next := r.FormValue("next")
	if next == "" || !strings.HasPrefix(next, "/admin") {
		next = "/admin/"
	}

	admin, err := s.st.GetAdmin(r.Context(), username)
	if errors.Is(err, store.ErrNotFound) {
		http.Redirect(w, r, "/admin/login?err=Invalid+credentials", http.StatusFound)
		return
	}
	if err != nil {
		s.serverError(w, err, "login GetAdmin")
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(admin.PasswordHash), []byte(password)) != nil {
		http.Redirect(w, r, "/admin/login?err=Invalid+credentials", http.StatusFound)
		return
	}
	if err := s.setSession(w, r, admin.Username); err != nil {
		s.serverError(w, err, "login setSession")
		return
	}
	http.Redirect(w, r, next, http.StatusFound)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	s.clearSession(w)
	http.Redirect(w, r, "/admin/login", http.StatusFound)
}

// --- members ---

type memberRow struct {
	store.Member
	SlackNames []store.SlackIdentity
}

func (s *Server) handleMembers(w http.ResponseWriter, r *http.Request) {
	members, err := s.st.ListMembers(r.Context())
	if err != nil {
		s.serverError(w, err, "members list")
		return
	}
	rows := make([]memberRow, 0, len(members))
	for _, m := range members {
		ids, err := s.st.SlackIdentitiesFor(r.Context(), m.ID)
		if err != nil {
			s.serverError(w, err, "members slack ids")
			return
		}
		rows = append(rows, memberRow{Member: m, SlackNames: ids})
	}
	s.renderPage(w, "members.html", s.pageData(r, map[string]any{"Members": rows}))
}

func (s *Server) handleSlackLink(w http.ResponseWriter, r *http.Request) {
	memberID, _ := strconv.ParseInt(r.FormValue("member_id"), 10, 64)
	name := NormalizeName(r.FormValue("slack_name"))
	if memberID == 0 || name == "" {
		http.Redirect(w, r, "/admin/members?err=member+and+slack+name+required", http.StatusFound)
		return
	}
	if err := s.st.AddSlackIdentity(r.Context(), memberID, name, adminFrom(r.Context())); err != nil {
		s.serverError(w, err, "slack link")
		return
	}
	http.Redirect(w, r, "/admin/members?msg=linked+"+name, http.StatusFound)
}

func (s *Server) handleSlackUnlink(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.FormValue("id"), 10, 64)
	if err := s.st.RemoveSlackIdentity(r.Context(), id); err != nil {
		s.serverError(w, err, "slack unlink")
		return
	}
	http.Redirect(w, r, "/admin/members?msg=unlinked", http.StatusFound)
}

// --- printers ---

type printerRow struct {
	store.Printer
	LastSeen     string
	AgentVersion string // "unknown" until the Pi reports in
	UpdateState  string // "up to date" | "update pending" | "held" | "pinned <v>" | "auto" | "—"
	Behind       bool   // running something other than the portal's build
}

func (s *Server) handlePrinters(w http.ResponseWriter, r *http.Request) {
	printers, err := s.st.ListPrinters(r.Context())
	if err != nil {
		s.serverError(w, err, "printers list")
		return
	}
	now := s.now()
	portalVer := s.portalVersion()
	var pending, active []printerRow
	for _, p := range printers {
		row := printerRow{
			Printer:      p,
			LastSeen:     humanAgo(p.LastSeenAt, now),
			AgentVersion: orDefault(p.AgentVersion, "unknown"),
			UpdateState:  s.agentUpdateState(p),
			Behind:       p.AgentVersion != "" && p.AgentVersion != portalVer,
		}
		if p.Approved {
			active = append(active, row)
		} else {
			pending = append(pending, row)
		}
	}
	s.renderPage(w, "printers.html", s.pageData(r, map[string]any{
		"Pending":       pending,
		"Active":        active,
		"NewKey":        r.URL.Query().Get("key"), // shown once after create/rotate
		"PortalVersion": portalVer,
		"AutoUpdate":    s.cfg.AgentAutoUpdate,
		"CanServe":      s.agentBin.OK,
	}))
}

// agentUpdateState is a short human label for how a Pi's version is managed.
func (s *Server) agentUpdateState(p store.Printer) string {
	switch {
	case p.AgentTargetOverride != "":
		if p.AgentVersion == p.AgentTargetOverride {
			return "up to date"
		}
		return "update pending"
	case p.AgentUpdateHold:
		return "held"
	case s.cfg.AgentAutoUpdate:
		target := s.effectiveAgentTarget(p)
		if p.AgentVersion != "" && p.AgentVersion == target {
			return "up to date"
		}
		return "auto"
	default:
		return "—"
	}
}

// humanAgo renders a *time.Time as "just now" / "3m ago" / "2h ago" / "5d ago",
// or "never" for nil.
func humanAgo(t *time.Time, now time.Time) string {
	if t == nil {
		return "never"
	}
	d := now.Sub(*t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + "m ago"
	case d < 24*time.Hour:
		return strconv.Itoa(int(d.Hours())) + "h ago"
	default:
		return strconv.Itoa(int(d.Hours()/24)) + "d ago"
	}
}

func (s *Server) handlePrinterCreate(w http.ResponseWriter, r *http.Request) {
	slug := slugify(r.FormValue("slug"))
	if slug == "" {
		http.Redirect(w, r, "/admin/printers?err=slug+required", http.StatusFound)
		return
	}
	key := newAPIKey()
	p, err := s.st.CreatePrinter(r.Context(), store.Printer{
		Slug:              slug,
		DisplayName:       orDefault(r.FormValue("display_name"), slug),
		Model:             strings.TrimSpace(r.FormValue("model")),
		AllowedExtensions: parseExtensions(r.FormValue("allowed_extensions")),
		SafetyChecklist:   splitLines(r.FormValue("safety_checklist")),
		SlackWebhookURL:   strings.TrimSpace(r.FormValue("slack_webhook_url")),
		APIKeyHash:        HashAPIKey(key),
	})
	if err != nil {
		http.Redirect(w, r, "/admin/printers?err="+errParam(err), http.StatusFound)
		return
	}
	http.Redirect(w, r, "/admin/printers?msg=created+"+p.Slug+"&key="+key, http.StatusFound)
}

func (s *Server) handlePrinterEdit(w http.ResponseWriter, r *http.Request) {
	p, ok := s.printerByIDParam(w, r)
	if !ok {
		return
	}
	s.renderPage(w, "printer_edit.html", s.pageData(r, map[string]any{
		"P":              p,
		"ChecklistText":  strings.Join(p.SafetyChecklist, "\n"),
		"ExtensionsText": strings.Join(p.AllowedExtensions, ", "),
	}))
}

func (s *Server) handlePrinterUpdate(w http.ResponseWriter, r *http.Request) {
	p, ok := s.printerByIDParam(w, r)
	if !ok {
		return
	}
	p.DisplayName = orDefault(r.FormValue("display_name"), p.Slug)
	p.Model = strings.TrimSpace(r.FormValue("model"))
	p.AllowedExtensions = parseExtensions(r.FormValue("allowed_extensions"))
	p.SafetyChecklist = splitLines(r.FormValue("safety_checklist"))
	p.SlackWebhookURL = strings.TrimSpace(r.FormValue("slack_webhook_url"))
	if err := s.st.UpdatePrinter(r.Context(), p); err != nil {
		s.serverError(w, err, "printer update")
		return
	}
	http.Redirect(w, r, "/admin/printers?msg=saved+"+p.Slug, http.StatusFound)
}

func (s *Server) handlePrinterApprove(w http.ResponseWriter, r *http.Request) {
	p, ok := s.printerByIDParam(w, r)
	if !ok {
		return
	}
	if err := s.st.SetPrinterApproved(r.Context(), p.ID, true); err != nil {
		s.serverError(w, err, "approve printer")
		return
	}
	http.Redirect(w, r, "/admin/printers?msg=approved+"+p.Slug, http.StatusFound)
}

func (s *Server) handlePrinterDisable(w http.ResponseWriter, r *http.Request) {
	p, ok := s.printerByIDParam(w, r)
	if !ok {
		return
	}
	if err := s.st.SetPrinterApproved(r.Context(), p.ID, false); err != nil {
		s.serverError(w, err, "disable printer")
		return
	}
	http.Redirect(w, r, "/admin/printers?msg=disabled+"+p.Slug, http.StatusFound)
}

func (s *Server) handlePrinterDelete(w http.ResponseWriter, r *http.Request) {
	p, ok := s.printerByIDParam(w, r)
	if !ok {
		return
	}
	if err := s.st.DeletePrinter(r.Context(), p.ID); err != nil {
		// Most likely a foreign-key violation: the printer has print history.
		http.Redirect(w, r, "/admin/printers?err=can%27t+delete+"+p.Slug+"+%E2%80%94+it+has+print+history%3B+disable+it+instead", http.StatusFound)
		return
	}
	http.Redirect(w, r, "/admin/printers?msg=removed+"+p.Slug, http.StatusFound)
}

func (s *Server) handlePrinterRotateKey(w http.ResponseWriter, r *http.Request) {
	p, ok := s.printerByIDParam(w, r)
	if !ok {
		return
	}
	key := newAPIKey()
	if err := s.st.RotatePrinterKey(r.Context(), p.ID, HashAPIKey(key)); err != nil {
		s.serverError(w, err, "rotate key")
		return
	}
	http.Redirect(w, r, "/admin/printers?msg=rotated+"+p.Slug+"&key="+key, http.StatusFound)
}

// --- certifications ---

func (s *Server) handleCertifications(w http.ResponseWriter, r *http.Request) {
	printers, err := s.st.ListPrinters(r.Context())
	if err != nil {
		s.serverError(w, err, "cert printers")
		return
	}
	slug := r.URL.Query().Get("printer")
	if slug == "" && len(printers) > 0 {
		slug = printers[0].Slug
	}
	var rows []store.CertifiedMember
	var selected store.Printer
	if slug != "" {
		selected, err = s.st.GetPrinterBySlug(r.Context(), slug)
		if err == nil {
			rows, err = s.st.ListCertifications(r.Context(), selected.ID)
		}
		if err != nil {
			s.serverError(w, err, "cert list")
			return
		}
	}
	s.renderPage(w, "certifications.html", s.pageData(r, map[string]any{
		"Printers": printers, "Selected": selected, "Rows": rows,
	}))
}

func (s *Server) handleCertifyToggle(w http.ResponseWriter, r *http.Request) {
	printerID, _ := strconv.ParseInt(r.FormValue("printer_id"), 10, 64)
	memberID, _ := strconv.ParseInt(r.FormValue("member_id"), 10, 64)
	slug := r.FormValue("printer_slug")
	var err error
	if r.FormValue("action") == "revoke" {
		err = s.st.Revoke(r.Context(), memberID, printerID)
	} else {
		err = s.st.Certify(r.Context(), memberID, printerID, adminFrom(r.Context()))
	}
	if err != nil {
		s.serverError(w, err, "certify toggle")
		return
	}
	http.Redirect(w, r, "/admin/certifications?printer="+slug+"&msg=updated", http.StatusFound)
}

// --- jobs & log ---

func (s *Server) handleJobs(w http.ResponseWriter, r *http.Request) {
	current, err := s.st.CurrentJobs(r.Context())
	if err != nil {
		s.serverError(w, err, "jobs current")
		return
	}
	history, err := s.st.AllRecentJobs(r.Context(), 100)
	if err != nil {
		s.serverError(w, err, "jobs history")
		return
	}
	s.renderPage(w, "jobs.html", s.pageData(r, map[string]any{
		"Current": current, "History": history, "Now": s.now(),
	}))
}

func (s *Server) handleForceClear(w http.ResponseWriter, r *http.Request) {
	jobID, _ := strconv.ParseInt(r.FormValue("job_id"), 10, 64)
	if _, err := s.st.EndJob(r.Context(), jobID, "admin_cleared"); err != nil {
		s.serverError(w, err, "force clear")
		return
	}
	http.Redirect(w, r, "/admin/jobs?msg=cleared", http.StatusFound)
}

func (s *Server) handleLog(w http.ResponseWriter, r *http.Request) {
	entries, err := s.st.RecentDecisions(r.Context(), 200)
	if err != nil {
		s.serverError(w, err, "log list")
		return
	}
	s.renderPage(w, "log.html", s.pageData(r, map[string]any{"Entries": entries}))
}

// --- settings ---

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	s.renderPage(w, "settings.html", s.pageData(r, map[string]any{"User": adminFrom(r.Context())}))
}

func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	pw := r.FormValue("new_password")
	if pw == "" || pw != r.FormValue("confirm_password") {
		http.Redirect(w, r, "/admin/settings?err=passwords+empty+or+mismatched", http.StatusFound)
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	if err != nil {
		s.serverError(w, err, "hash password")
		return
	}
	if err := s.st.SetAdminPassword(r.Context(), adminFrom(r.Context()), string(hash)); err != nil {
		s.serverError(w, err, "set password")
		return
	}
	http.Redirect(w, r, "/admin/settings?msg=password+updated", http.StatusFound)
}

// --- shared ---

func (s *Server) pageData(r *http.Request, extra map[string]any) map[string]any {
	d := map[string]any{
		"Msg":  r.URL.Query().Get("msg"),
		"Err":  r.URL.Query().Get("err"),
		"User": adminFrom(r.Context()),
		"Path": r.URL.Path,
		"Year": time.Now().Year(),
	}
	for k, v := range extra {
		d[k] = v
	}
	return d
}

func (s *Server) printerByIDParam(w http.ResponseWriter, r *http.Request) (store.Printer, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return store.Printer{}, false
	}
	list, err := s.st.ListPrinters(r.Context())
	if err != nil {
		s.serverError(w, err, "printerByID")
		return store.Printer{}, false
	}
	for _, p := range list {
		if p.ID == id {
			return p, true
		}
	}
	http.Error(w, "no such printer", http.StatusNotFound)
	return store.Printer{}, false
}

func newAPIKey() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == ' ' || r == '_' || r == '-':
			b.WriteRune('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

func parseExtensions(s string) []string {
	var out []string
	for _, part := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' }) {
		part = strings.ToLower(strings.TrimSpace(part))
		if part == "" {
			continue
		}
		if !strings.HasPrefix(part, ".") {
			part = "." + part
		}
		out = append(out, part)
	}
	return out
}

func splitLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return strings.TrimSpace(s)
}

func errParam(err error) string {
	return strings.ReplaceAll(err.Error(), " ", "+")
}
