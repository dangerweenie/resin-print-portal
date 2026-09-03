package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/dangerweenie/resin-print-portal/internal/slack"
	"github.com/dangerweenie/resin-print-portal/internal/sliced"
	"github.com/dangerweenie/resin-print-portal/internal/store"
	"github.com/go-chi/chi/v5"
)

const maxUploadBytes = 600 << 20 // 600 MiB, matches the old Flask cap

// GET /api/v1/printers/{slug}/config
func (s *Server) handlePrinterConfig(w http.ResponseWriter, r *http.Request) {
	p := printerFrom(r.Context())
	s.writeJSON(w, http.StatusOK, map[string]any{
		"slug":               p.Slug,
		"display_name":       p.DisplayName,
		"model":              p.Model,
		"allowed_extensions": p.AllowedExtensions,
		"safety_checklist":   p.SafetyChecklist,
		"approved":           p.Approved,
	})
}

// POST /api/v1/printers/{slug}/check  body: {"slack_name": "..."} or {"rfid_code": "..."}
func (s *Server) handleCheck(w http.ResponseWriter, r *http.Request) {
	p := printerFrom(r.Context())
	var body struct {
		SlackName string `json:"slack_name"`
		RFIDCode  string `json:"rfid_code"`
	}
	if err := decodeJSON(r, &body); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json"})
		return
	}

	if !p.Approved {
		d := denied(ReasonPendingApproval, nil)
		s.logDecision(r, &p.ID, d, "", identifierOf(body.SlackName, body.RFIDCode))
		s.writeJSON(w, http.StatusOK, map[string]any{"allowed": false, "reason": d.Reason})
		return
	}

	d, err := s.decide(r.Context(), p.ID, body.SlackName, body.RFIDCode)
	if err != nil {
		s.serverError(w, err, "check decide")
		return
	}
	s.logDecision(r, &p.ID, d, "", identifierOf(body.SlackName, body.RFIDCode))

	resp := map[string]any{"allowed": d.Allowed, "reason": d.Reason}
	if d.Member != nil {
		resp["member_name"] = d.Member.Name
	}
	s.writeJSON(w, http.StatusOK, resp)
}

// POST /api/v1/printers/{slug}/print-requests  (multipart form)
//
//	fields: slack_name, filename, check_0..check_N
//	file:   file
func (s *Server) handlePrintRequest(w http.ResponseWriter, r *http.Request) {
	p := printerFrom(r.Context())
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	if err := r.ParseMultipartForm(20 << 20); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad multipart form: " + err.Error()})
		return
	}
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()

	slackName := r.FormValue("slack_name")
	fobCode := r.FormValue("rfid_code")
	who := identifierOf(slackName, fobCode)
	filename := sanitizeFilename(r.FormValue("filename"))

	if !p.Approved {
		d := denied(ReasonPendingApproval, nil)
		s.logDecision(r, &p.ID, d, filename, who)
		s.writeJSON(w, http.StatusOK, printDenied(d.Reason))
		return
	}

	// 1. Identity / membership / certification.
	d, err := s.decide(r.Context(), p.ID, slackName, fobCode)
	if err != nil {
		s.serverError(w, err, "print-request decide")
		return
	}
	if !d.Allowed {
		s.logDecision(r, &p.ID, d, filename, who)
		s.writeJSON(w, http.StatusOK, printDenied(d.Reason))
		return
	}

	// 2. Pull the uploaded file to a temp path for parsing.
	file, hdr, err := r.FormFile("file")
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing file part"})
		return
	}
	defer file.Close()
	if filename == "" {
		filename = sanitizeFilename(hdr.Filename)
	}

	// 3. Extension allow-list.
	if !extensionAllowed(p.AllowedExtensions, filename) {
		d := denied(ReasonExtensionBlocked, d.Member)
		s.logDecision(r, &p.ID, d, filename, who)
		s.writeJSON(w, http.StatusOK, printDenied(ReasonExtensionBlocked))
		return
	}

	// 4. Safety checklist — every configured item must be checked.
	if missing := missingChecklistItems(r, len(p.SafetyChecklist)); missing {
		d := denied(ReasonChecklist, d.Member)
		s.logDecision(r, &p.ID, d, filename, who)
		s.writeJSON(w, http.StatusOK, printDenied(ReasonChecklist))
		return
	}

	tmp, err := os.CreateTemp("", "sliced-*"+extOf(filename))
	if err != nil {
		s.serverError(w, err, "print-request CreateTemp")
		return
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := io.Copy(tmp, file); err != nil {
		tmp.Close()
		s.serverError(w, err, "print-request spool upload")
		return
	}
	tmp.Close()

	// 5. Parse ETA + embedded machine name (best effort).
	var (
		estSeconds  *int32
		etaExact    bool
		etaAt       *time.Time
		machineWarn string
		slicedFor   string
	)
	if info, perr := sliced.GetInfo(tmpPath); perr != nil {
		s.log.Warn("no ETA for upload", "file", filename, "err", perr)
	} else {
		v := int32(info.EstimatedSeconds)
		estSeconds = &v
		etaExact = info.Exact
		slicedFor = info.MachineName
		if info.EstimatedSeconds > 0 {
			t := s.now().Add(time.Duration(info.EstimatedSeconds) * time.Second)
			etaAt = &t
		}
		if warn := machineMismatch(p.Model, info.MachineName); warn != "" {
			machineWarn = warn
		}
	}

	checklist := checklistAnswers(r, p.SafetyChecklist)

	nameUsed := strings.TrimSpace(d.Member.Name)
	if nameUsed == "" {
		nameUsed = who
	}
	job, err := s.st.StartJob(r.Context(), store.PrintJob{
		PrinterID:           p.ID,
		MemberID:            &d.Member.ID,
		SlackNameUsed:       nameUsed,
		Filename:            filename,
		SlicedForModel:      slicedFor,
		ChecklistAnswers:    checklist,
		EstimatedSeconds:    estSeconds,
		ETAExact:            etaExact,
		EstimatedCompleteAt: etaAt,
	})
	if err != nil {
		s.serverError(w, err, "print-request StartJob")
		return
	}

	s.logDecision(r, &p.ID, d, filename, who)

	resp := map[string]any{
		"approved":  true,
		"job_id":    job.ID,
		"eta_exact": etaExact,
	}
	if estSeconds != nil {
		resp["eta_seconds"] = *estSeconds
	}
	if machineWarn != "" {
		resp["machine_warning"] = machineWarn
	}
	s.writeJSON(w, http.StatusOK, resp)
}

// GET /api/v1/printers/{slug}/current-job
func (s *Server) handleCurrentJob(w http.ResponseWriter, r *http.Request) {
	p := printerFrom(r.Context())
	job, err := s.st.CurrentJob(r.Context(), p.ID)
	if errors.Is(err, store.ErrNotFound) {
		s.writeJSON(w, http.StatusOK, map[string]any{"current_job": nil})
		return
	}
	if err != nil {
		s.serverError(w, err, "current-job")
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"current_job": jobJSON(job, s.now())})
}

// POST /api/v1/printers/{slug}/jobs/{id}/started
func (s *Server) handleJobStarted(w http.ResponseWriter, r *http.Request) {
	p := printerFrom(r.Context())
	job, ok := s.lookupJob(w, r, p)
	if !ok {
		return
	}
	eta := ""
	if job.EstimatedSeconds != nil {
		tag := "estimated"
		if job.ETAExact {
			tag = "exact"
		}
		eta = fmt.Sprintf(" — ETA %s (%s)", sliced.FormatDuration(int(*job.EstimatedSeconds)), tag)
	}
	s.postSlack(r, p, fmt.Sprintf(":large_green_circle: *%s* started printing `%s` on *%s*%s",
		displayName(job), job.Filename, p.DisplayName, eta))
	w.WriteHeader(http.StatusNoContent)
}

// POST /api/v1/printers/{slug}/jobs/{id}/finished
func (s *Server) handleJobFinished(w http.ResponseWriter, r *http.Request) {
	p := printerFrom(r.Context())
	job, ok := s.lookupJob(w, r, p)
	if !ok {
		return
	}
	ended, err := s.st.EndJob(r.Context(), job.ID, "member_finished")
	if err != nil {
		s.serverError(w, err, "job finished EndJob")
		return
	}
	if ended {
		s.postSlack(r, p, fmt.Sprintf(":checkered_flag: *%s* marked `%s` finished and removed it from *%s*.",
			displayName(job), job.Filename, p.DisplayName))
	}
	w.WriteHeader(http.StatusNoContent)
}

// GET /api/v1/status  (X-Api-Key)
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	current, err := s.st.CurrentJobs(r.Context())
	if err != nil {
		s.serverError(w, err, "status CurrentJobs")
		return
	}
	history, err := s.st.AllRecentJobs(r.Context(), 25)
	if err != nil {
		s.serverError(w, err, "status AllRecentJobs")
		return
	}
	now := s.now()
	cur := make([]map[string]any, 0, len(current))
	for _, j := range current {
		m := jobJSON(j.PrintJob, now)
		m["printer"] = j.PrinterSlug
		m["member"] = orUnknown(j.MemberName)
		cur = append(cur, m)
	}
	hist := make([]map[string]any, 0, len(history))
	for _, j := range history {
		hist = append(hist, map[string]any{
			"printer":    j.PrinterSlug,
			"member":     orUnknown(j.MemberName),
			"filename":   j.Filename,
			"started_at": j.StartedAt,
			"ended_at":   j.EndedAt,
			"status":     store.DisplayStatus(j.PrintJob, now),
			"end_reason": j.EndReason,
		})
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"current_jobs": cur, "history": hist})
}

// --- helpers ---

func (s *Server) lookupJob(w http.ResponseWriter, r *http.Request, p store.Printer) (store.PrintJob, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad job id"})
		return store.PrintJob{}, false
	}
	job, err := s.st.GetJob(r.Context(), p.ID, id)
	if errors.Is(err, store.ErrNotFound) {
		s.writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such job"})
		return store.PrintJob{}, false
	}
	if err != nil {
		s.serverError(w, err, "lookupJob")
		return store.PrintJob{}, false
	}
	return job, true
}

// identifierOf renders whichever identity the caller presented, for the audit
// log: "fob:<code>" for a tap, the Slack name for a typed name, "-" for neither.
func identifierOf(slackName, fobCode string) string {
	if c := strings.TrimSpace(fobCode); c != "" {
		return "fob:" + c
	}
	if n := strings.TrimSpace(slackName); n != "" {
		return n
	}
	return "-"
}

func (s *Server) logDecision(r *http.Request, printerID *int64, d Decision, filename, identifier string) {
	var memberID *int64
	if d.Member != nil {
		memberID = &d.Member.ID
	}
	if identifier == "" {
		identifier = "-"
	}
	entry := store.DecisionLogEntry{
		PrinterID:     printerID,
		SlackNameUsed: identifier,
		MemberID:      memberID,
		Filename:      filename,
		Outcome:       d.Outcome,
		Reason:        d.Reason,
	}
	if err := s.st.LogDecision(r.Context(), entry); err != nil {
		s.log.Warn("decision_log write failed", "err", err)
	}
}

func (s *Server) postSlack(r *http.Request, p store.Printer, text string) {
	if p.SlackWebhookURL == "" {
		return
	}
	if err := slack.Post(r.Context(), p.SlackWebhookURL, text); err != nil {
		s.log.Warn("slack post failed", "printer", p.Slug, "err", err)
	}
}

func printDenied(reason string) map[string]any {
	return map[string]any{"approved": false, "reason": reason}
}

func decodeJSON(r *http.Request, v any) error {
	defer io.Copy(io.Discard, r.Body)
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	return dec.Decode(v)
}

func missingChecklistItems(r *http.Request, n int) bool {
	for i := 0; i < n; i++ {
		if !truthy(r.FormValue("check_" + strconv.Itoa(i))) {
			return true
		}
	}
	return false
}

func checklistAnswers(r *http.Request, items []string) []string {
	out := make([]string, 0, len(items))
	for i, label := range items {
		if truthy(r.FormValue("check_" + strconv.Itoa(i))) {
			out = append(out, label)
		}
	}
	return out
}

func truthy(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "0", "false", "off", "no":
		return false
	}
	return true
}

func sanitizeFilename(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	name = strings.TrimSpace(name)
	name = strings.Map(func(rn rune) rune {
		switch {
		case rn >= 'a' && rn <= 'z', rn >= 'A' && rn <= 'Z', rn >= '0' && rn <= '9':
			return rn
		case strings.ContainsRune("._- ()", rn):
			return rn
		default:
			return '_'
		}
	}, name)
	return name
}

func extOf(name string) string {
	if i := strings.LastIndex(name, "."); i >= 0 {
		return name[i:]
	}
	return ""
}

func machineMismatch(printerModel, slicedFor string) string {
	pm := strings.ToLower(strings.TrimSpace(printerModel))
	sf := strings.ToLower(strings.TrimSpace(slicedFor))
	if pm == "" || sf == "" || sf == "unknown" {
		return ""
	}
	if strings.Contains(pm, sf) || strings.Contains(sf, pm) {
		return ""
	}
	return fmt.Sprintf("file was sliced for %q but this printer is %q", slicedFor, printerModel)
}

func displayName(j store.PrintJob) string {
	if n := strings.TrimSpace(j.SlackNameUsed); n != "" {
		return n
	}
	return "someone"
}

func orUnknown(s string) string {
	if strings.TrimSpace(s) == "" {
		return "unknown"
	}
	return s
}

func jobJSON(j store.PrintJob, now time.Time) map[string]any {
	m := map[string]any{
		"id":                    j.ID,
		"filename":              j.Filename,
		"slack_name":            j.SlackNameUsed,
		"started_at":            j.StartedAt,
		"status":                store.DisplayStatus(j, now),
		"eta_exact":             j.ETAExact,
		"estimated_complete_at": j.EstimatedCompleteAt,
		"sliced_for_model":      j.SlicedForModel,
	}
	if j.EstimatedSeconds != nil {
		m["estimated_seconds"] = *j.EstimatedSeconds
	}
	if j.EstimatedCompleteAt != nil && store.DisplayStatus(j, now) == "printing" {
		rem := int(j.EstimatedCompleteAt.Sub(now).Seconds())
		if rem < 0 {
			rem = 0
		}
		m["remaining_seconds"] = rem
		m["remaining_human"] = sliced.FormatDuration(rem)
	}
	return m
}
