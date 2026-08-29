// Package piagent is the thin on-Pi service. It serves a single upload page on
// the makerspace LAN, forwards each submission to the central portal for the
// membership + certification + checklist decision, and on approval writes the
// file onto the USB gadget.
package piagent

import (
	"context"
	_ "embed"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

//go:embed upload.html
var uploadHTML string

var uploadTmpl = template.Must(template.New("upload").Parse(uploadHTML))

// GadgetWriter is the slice of internal/gadget the agent uses; a fake stands in
// for it in tests.
type GadgetWriter interface {
	Write(ctx context.Context, srcPath string) error
	Clear(ctx context.Context) error
}

// Agent is the Pi-side HTTP handler.
type Agent struct {
	central *CentralClient
	gadget  GadgetWriter
	log     *slog.Logger
}

// New builds an Agent.
func New(central *CentralClient, g GadgetWriter, log *slog.Logger) *Agent {
	return &Agent{central: central, gadget: g, log: log}
}

// Handler returns the routed HTTP handler.
func (a *Agent) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("/", a.handleIndex)
	mux.HandleFunc("/submit", a.handleSubmit)
	mux.HandleFunc("/finish", a.handleFinish)
	return mux
}

type pageData struct {
	Config     Config
	CurrentJob *currentJobView
	Message    string
	Error      string
	Warning    string
}

type currentJobView struct {
	ID        int64
	Filename  string
	SlackName string
	Status    string
	Remaining string
}

func (a *Agent) render(w http.ResponseWriter, r *http.Request, d pageData) {
	ctx := r.Context()
	if d.Config.Slug == "" {
		if cfg, err := a.central.FetchConfig(ctx); err != nil {
			a.log.Warn("fetch config failed", "err", err)
			d.Error = orFirst(d.Error, "Can't reach the print portal right now. Try again in a minute.")
		} else {
			d.Config = cfg
		}
	}
	if cj, err := a.central.FetchCurrentJob(ctx); err == nil && cj.CurrentJob != nil {
		d.CurrentJob = &currentJobView{
			ID: cj.CurrentJob.ID, Filename: cj.CurrentJob.Filename,
			SlackName: cj.CurrentJob.SlackName, Status: cj.CurrentJob.Status,
			Remaining: cj.CurrentJob.RemainingHuman,
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := uploadTmpl.Execute(w, d); err != nil {
		a.log.Error("template execute failed", "err", err)
	}
}

func (a *Agent) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	a.render(w, r, pageData{
		Message: r.URL.Query().Get("msg"),
		Error:   r.URL.Query().Get("err"),
	})
}

func (a *Agent) handleSubmit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 600<<20)
	if err := r.ParseMultipartForm(20 << 20); err != nil {
		a.render(w, r, pageData{Error: "Upload was too large or malformed."})
		return
	}
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()

	slackName := r.FormValue("slack_name")
	if slackName == "" {
		a.render(w, r, pageData{Error: "Enter your Tinkermill Slack name."})
		return
	}

	file, hdr, err := r.FormFile("file")
	if err != nil {
		a.render(w, r, pageData{Error: "Choose a sliced file to upload."})
		return
	}
	defer file.Close()

	// Stage the upload under its real (sanitized) name inside a temp dir, so
	// the file lands on the gadget with the exact filename — and extension —
	// the printer expects, not a scratch name.
	origName := sanitizeFilename(hdr.Filename)
	if origName == "" {
		origName = "print.bin"
	}
	tmpDir, err := os.MkdirTemp("", "piagent-")
	if err != nil {
		a.render(w, r, pageData{Error: "Pi is out of scratch space."})
		return
	}
	defer os.RemoveAll(tmpDir)
	stagedPath := filepath.Join(tmpDir, origName)
	staged, err := os.Create(stagedPath)
	if err != nil {
		a.render(w, r, pageData{Error: "Pi is out of scratch space."})
		return
	}
	if _, err := io.Copy(staged, file); err != nil {
		staged.Close()
		a.render(w, r, pageData{Error: "Upload interrupted."})
		return
	}
	staged.Close()

	// Which checklist boxes were ticked.
	cfg, _ := a.central.FetchConfig(r.Context())
	checked := make([]bool, len(cfg.SafetyChecklist))
	for i := range checked {
		checked[i] = r.FormValue("check_"+strconv.Itoa(i)) != ""
	}

	res, err := a.central.SubmitPrint(r.Context(), slackName, origName, stagedPath, checked)
	if err != nil {
		a.log.Error("submit to central failed", "err", err)
		a.render(w, r, pageData{Error: "Couldn't reach the print portal. Nothing was sent to the printer."})
		return
	}
	if !res.Approved {
		a.render(w, r, pageData{Error: denyMessage(res.Reason)})
		return
	}

	if err := a.gadget.Write(r.Context(), stagedPath); err != nil {
		a.log.Error("gadget write failed", "err", err)
		a.render(w, r, pageData{Error: "The portal approved your file but writing it to the printer's drive failed. Ask a staff member."})
		return
	}
	if err := a.central.JobStarted(r.Context(), res.JobID); err != nil {
		a.log.Warn("job started callback failed", "err", err, "job", res.JobID)
	}

	msg := "Your file is on the printer. Start the print from the printer's screen."
	if res.MachineWarning != "" {
		msg += " Heads up: " + res.MachineWarning + "."
	}
	http.Redirect(w, r, "/?msg="+urlEncode(msg), http.StatusFound)
}

func (a *Agent) handleFinish(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	jobID, _ := strconv.ParseInt(r.FormValue("job_id"), 10, 64)
	if jobID != 0 {
		if err := a.central.JobFinished(r.Context(), jobID); err != nil {
			a.log.Warn("job finished callback failed", "err", err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := a.gadget.Clear(ctx); err != nil {
		a.log.Error("gadget clear failed", "err", err)
		http.Redirect(w, r, "/?err="+urlEncode("Couldn't clear the printer's drive — ask a staff member."), http.StatusFound)
		return
	}
	http.Redirect(w, r, "/?msg="+urlEncode("Marked finished and cleared the drive."), http.StatusFound)
}

func denyMessage(reason string) string {
	switch reason {
	case "unknown_slack_name":
		return "We couldn't match that Slack name to an active member. Check the spelling, or ask an admin to link your Slack name in the portal."
	case "ambiguous_name":
		return "That name matches more than one member. Ask an admin to link your exact Slack name in the portal."
	case "membership_inactive":
		return "Your Tinkermill membership isn't showing as active. Sort that out with the front desk, then try again."
	case "not_certified":
		return "You're not certified for this printer yet. Ask a resin-printing trainer to certify you."
	case "extension_not_allowed":
		return "That file type isn't accepted on this printer. Slice for this machine's format and try again."
	case "checklist_incomplete":
		return "Please confirm every item on the safety checklist before submitting."
	case "printer_pending_approval":
		return "This printer is still waiting for a makerspace admin to approve it in the portal."
	default:
		return "The portal declined this print (" + reason + ")."
	}
}

func orFirst(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func urlEncode(s string) string { return url.QueryEscape(s) }

// sanitizeFilename strips any path and keeps only characters safe on a FAT32
// volume and a picky printer's file browser. The central service applies the
// same rules; this is the Pi-side copy so the gadget filename is right even if
// the two ever drift.
func sanitizeFilename(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	name = filepath.Base(name)
	name = strings.TrimSpace(name)
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case strings.ContainsRune("._- ()", r):
			return r
		default:
			return '_'
		}
	}, name)
}
