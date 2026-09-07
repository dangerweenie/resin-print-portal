package server

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/dangerweenie/resin-print-portal/internal/sliced"
	"github.com/dangerweenie/resin-print-portal/internal/store"
)

// uploadView is the data the public upload page renders from.
type uploadView struct {
	Printers  []store.Printer // approved only
	Selected  string          // slug
	Checklist []string        // of the selected printer
	Exts      []string        // of the selected printer
	SlackName string          // sticky on error
	Msg       string
	Err       string
}

func (s *Server) renderUpload(w http.ResponseWriter, v uploadView) {
	// html/template on base.html; no .User so no admin chrome.
	s.renderPage(w, "portal_upload.html", map[string]any{
		"Printers": v.Printers, "Selected": v.Selected, "Checklist": v.Checklist,
		"Exts": v.Exts, "SlackName": v.SlackName, "Msg": v.Msg, "Err": v.Err,
	})
}

func (s *Server) approvedPrinters(w http.ResponseWriter, r *http.Request) ([]store.Printer, bool) {
	all, err := s.st.ListPrinters(r.Context())
	if err != nil {
		s.serverError(w, err, "upload list printers")
		return nil, false
	}
	out := make([]store.Printer, 0, len(all))
	for _, p := range all {
		if p.Approved {
			out = append(out, p)
		}
	}
	return out, true
}

func pickPrinter(printers []store.Printer, slug string) (store.Printer, bool) {
	for _, p := range printers {
		if p.Slug == slug {
			return p, true
		}
	}
	if slug == "" && len(printers) == 1 {
		return printers[0], true
	}
	return store.Printer{}, false
}

// GET /upload
func (s *Server) handleUploadForm(w http.ResponseWriter, r *http.Request) {
	printers, ok := s.approvedPrinters(w, r)
	if !ok {
		return
	}
	v := uploadView{Printers: printers, Selected: r.URL.Query().Get("printer"),
		Msg: r.URL.Query().Get("msg"), Err: r.URL.Query().Get("err")}
	if p, found := pickPrinter(printers, v.Selected); found {
		v.Selected, v.Checklist, v.Exts = p.Slug, p.SafetyChecklist, p.AllowedExtensions
	}
	s.renderUpload(w, v)
}

// POST /upload  (multipart: slack_name, printer, check_i, file)
func (s *Server) handleUploadSubmit(w http.ResponseWriter, r *http.Request) {
	printers, ok := s.approvedPrinters(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	if err := r.ParseMultipartForm(20 << 20); err != nil {
		s.renderUpload(w, uploadView{Printers: printers, Err: "Upload was too large or malformed."})
		return
	}
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()

	slackName := strings.TrimSpace(r.FormValue("slack_name"))
	p, found := pickPrinter(printers, r.FormValue("printer"))
	base := uploadView{
		Printers: printers, Selected: p.Slug, SlackName: slackName,
		Checklist: p.SafetyChecklist, Exts: p.AllowedExtensions,
	}
	fail := func(msg string) { base.Err = msg; s.renderUpload(w, base) }

	if !found {
		fail("Choose which printer this is for.")
		return
	}

	d, err := s.decideIdentity(r.Context(), p.ID, slackName)
	if err != nil {
		s.serverError(w, err, "upload decide")
		return
	}
	if !d.Allowed {
		s.logDecision(r, &p.ID, d, "", identifierOf(slackName, ""))
		fail(denyText(d.Reason))
		return
	}

	file, hdr, err := r.FormFile("file")
	if err != nil {
		fail("Choose a sliced file to upload.")
		return
	}
	defer file.Close()
	filename := sanitizeFilename(hdr.Filename)
	if filename == "" {
		filename = "print.bin"
	}

	if !extensionAllowed(p.AllowedExtensions, filename) {
		dd := denied(ReasonExtensionBlocked, d.Member)
		s.logDecision(r, &p.ID, dd, filename, identifierOf(slackName, ""))
		fail(denyText(ReasonExtensionBlocked))
		return
	}
	if missingChecklistItems(r, len(p.SafetyChecklist)) {
		dd := denied(ReasonChecklist, d.Member)
		s.logDecision(r, &p.ID, dd, filename, identifierOf(slackName, ""))
		fail(denyText(ReasonChecklist))
		return
	}

	tmp, err := os.CreateTemp("", "staged-*"+extOf(filename))
	if err != nil {
		s.serverError(w, err, "upload CreateTemp")
		return
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), file); err != nil {
		tmp.Close()
		s.serverError(w, err, "upload spool")
		return
	}
	tmp.Close()
	sha := hex.EncodeToString(h.Sum(nil))

	var (
		estSeconds *int32
		etaExact   bool
		etaAt      *time.Time
		slicedFor  string
	)
	if info, perr := sliced.GetInfo(tmpPath); perr != nil {
		s.log.Warn("no ETA for staged upload", "file", filename, "err", perr)
	} else {
		v := int32(info.EstimatedSeconds)
		estSeconds = &v
		etaExact = info.Exact
		slicedFor = info.MachineName
		if info.EstimatedSeconds > 0 {
			t := s.now().Add(time.Duration(info.EstimatedSeconds) * time.Second)
			etaAt = &t
		}
	}

	data, err := os.ReadFile(tmpPath)
	if err != nil {
		s.serverError(w, err, "upload read back")
		return
	}

	nameUsed := strings.TrimSpace(d.Member.Name)
	if nameUsed == "" {
		nameUsed = slackName
	}
	job, err := s.st.StageJob(r.Context(), store.PrintJob{
		PrinterID:           p.ID,
		MemberID:            &d.Member.ID,
		SlackNameUsed:       nameUsed,
		Filename:            filename,
		SlicedForModel:      slicedFor,
		ChecklistAnswers:    checklistAnswers(r, p.SafetyChecklist),
		EstimatedSeconds:    estSeconds,
		ETAExact:            etaExact,
		EstimatedCompleteAt: etaAt,
	}, data, sha)
	if err != nil {
		s.serverError(w, err, "upload StageJob")
		return
	}
	s.logDecision(r, &p.ID, Decision{Allowed: true, Outcome: d.Outcome, Member: d.Member},
		filename, identifierOf(slackName, ""))

	msg := "Staged `" + filename + "` for " + p.DisplayName + ". Go to the printer and tap your fob to load it."
	if job.EstimatedSeconds != nil {
		msg += " Print time ~" + sliced.FormatDuration(int(*job.EstimatedSeconds)) + "."
	}
	if warn := machineMismatch(p.Model, slicedFor); warn != "" {
		msg += " Heads up: " + warn + "."
	}
	http.Redirect(w, r, "/upload?printer="+url.QueryEscape(p.Slug)+"&msg="+url.QueryEscape(msg), http.StatusFound)
}

// denyText turns a deny reason code into a sentence for the public upload page.
func denyText(reason string) string {
	switch reason {
	case ReasonUnknownName:
		return "We couldn't match that Slack name to an active member. Check the spelling, or ask an admin to link your Slack name."
	case ReasonAmbiguousName:
		return "That name matches more than one member. Ask an admin to link your exact Slack name."
	case ReasonMembershipInactive:
		return "Your Tinkermill membership isn't showing as active. Sort that out with the front desk, then try again."
	case ReasonNotCertified:
		return "You're not certified for this printer yet. Ask a resin-printing trainer to certify you."
	case ReasonExtensionBlocked:
		return "That file type isn't accepted on this printer. Slice for this machine's format and try again."
	case ReasonChecklist:
		return "Please confirm every item on the safety checklist before submitting."
	default:
		return "The portal declined this upload (" + reason + ")."
	}
}
