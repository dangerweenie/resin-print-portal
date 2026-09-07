// Package piagent is the thin on-Pi service. It serves a single upload page on
// the makerspace LAN, forwards each submission to the central portal for the
// membership + certification + checklist decision, and on approval writes the
// file onto the USB gadget.
package piagent

import (
	"context"
	_ "embed"
	"encoding/json"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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

// FobScan is the current tapped fob, if any.
type FobScan struct {
	Code string
}

// ScanSource is the slice of *rfid.Reader the agent uses.
type ScanSource interface {
	// CurrentCode returns the code of the fob currently held at the reader
	// (within its TTL), and whether there is one.
	CurrentCode() (string, bool)
	// Clear forgets the current tap.
	Clear()
}

// Agent is the Pi-side HTTP handler. Identity is always a fob tap.
type Agent struct {
	central *CentralClient
	gadget  GadgetWriter
	scanner ScanSource
	log     *slog.Logger

	scanMu    sync.Mutex
	scanCache scanCheck // last /check result, keyed by code, to avoid re-checking a held fob
}

type scanCheck struct {
	code   string
	result CheckResult
	at     time.Time
}

// New builds an Agent.
func New(central *CentralClient, g GadgetWriter, scanner ScanSource, log *slog.Logger) *Agent {
	return &Agent{central: central, gadget: g, scanner: scanner, log: log}
}

// Handler returns the routed HTTP handler.
func (a *Agent) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("/", a.handleIndex)
	mux.HandleFunc("/scan", a.handleScan)
	mux.HandleFunc("/load", a.handleLoad)
	mux.HandleFunc("/finish", a.handleFinish)
	return mux
}

// GET /scan — the upload page polls this to show who just tapped. The portal
// call (and its decision_log row) happens once per distinct fob, not once per
// poll: a held fob is answered from cache.
func (a *Agent) handleScan(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	code, ok := a.scanner.CurrentCode()
	if !ok {
		_, _ = w.Write([]byte(`{"scanned":false}`))
		return
	}

	res, cached, err := a.checkFob(r.Context(), code)
	if err != nil {
		a.log.Warn("fob check failed", "err", err)
		_, _ = w.Write([]byte(`{"scanned":true,"allowed":false,"reason":"portal_unreachable"}`))
		return
	}
	if !cached {
		a.log.Info("fob tap", "code", code, "allowed", res.Allowed,
			"member", res.MemberName, "reason", res.Reason,
			"staged", res.StagedFilename != "", "certified", res.Certified)
	}
	writeJSON(w, map[string]any{
		"scanned":         true,
		"allowed":         res.Allowed,
		"reason":          res.Reason,
		"member_name":     res.MemberName,
		"staged_filename": res.StagedFilename,
		"staged_eta":      res.StagedETA,
		"certified":       res.Certified,
	})
}

// checkFob resolves a fob against the portal, caching the result per code for a
// short window so a fob held at the reader isn't re-checked (and re-logged) on
// every 1.5s poll.
func (a *Agent) checkFob(ctx context.Context, code string) (res CheckResult, cached bool, err error) {
	a.scanMu.Lock()
	c := a.scanCache
	a.scanMu.Unlock()
	if c.code == code && time.Since(c.at) < 30*time.Second {
		return c.result, true, nil
	}
	res, err = a.central.CheckFob(ctx, code)
	if err != nil {
		return CheckResult{}, false, err
	}
	a.scanMu.Lock()
	a.scanCache = scanCheck{code: code, result: res, at: time.Now()}
	a.scanMu.Unlock()
	return res, false, nil
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

// handleLoad is the fob-release: the member tapped, and their staged print is
// pulled from the portal and written to the gadget. No file is uploaded here —
// that already happened on the portal.
func (a *Agent) handleLoad(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	// Identity is the fob the agent read itself — the browser never handles it.
	code, ok := a.scanner.CurrentCode()
	if !ok {
		a.render(w, r, pageData{Error: "Tap your Tinkermill fob on the reader, then load."})
		return
	}

	claim, err := a.central.ClaimStagedJob(r.Context(), code)
	if err != nil {
		a.log.Error("claim from central failed", "err", err)
		a.render(w, r, pageData{Error: "Couldn't reach the print portal. Nothing was loaded."})
		return
	}
	if !claim.Claimed {
		a.render(w, r, pageData{Error: denyMessage(claim.Reason)})
		return
	}

	tmpDir, err := os.MkdirTemp("", "piagent-")
	if err != nil {
		a.render(w, r, pageData{Error: "Pi is out of scratch space."})
		return
	}
	defer os.RemoveAll(tmpDir)
	name := sanitizeFilename(claim.Filename)
	if name == "" {
		name = "print.bin"
	}
	dst := filepath.Join(tmpDir, name)

	if _, err := a.central.DownloadJobFile(r.Context(), claim.JobID, claim.SHA256, dst); err != nil {
		a.log.Error("download staged file failed", "err", err, "job", claim.JobID)
		a.render(w, r, pageData{Error: "The download from the portal failed or was corrupt. Try again in a moment."})
		return
	}

	if err := a.gadget.Write(r.Context(), dst); err != nil {
		a.log.Error("gadget write failed", "err", err)
		a.render(w, r, pageData{Error: "Loading the file onto the printer's drive failed. Ask a staff member."})
		return
	}
	if err := a.central.JobStarted(r.Context(), claim.JobID); err != nil {
		a.log.Warn("job started callback failed", "err", err, "job", claim.JobID)
	}
	a.scanner.Clear() // next member starts fresh

	msg := "Loaded " + name + " onto the printer. Start the print from the printer's screen."
	if claim.MachineWarning != "" {
		msg += " Heads up: " + claim.MachineWarning + "."
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
	case "unknown_fob":
		return "That fob isn't linked to an active Tinkermill member. Check with the front desk that your membership and fob are current."
	case "no_staged_job":
		return "No queued print for you on this printer. Upload one on the portal first, then tap again."
	case "certification_recorded":
		return "You're now certified for this printer."
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

func writeJSON(w http.ResponseWriter, v any) {
	_ = json.NewEncoder(w).Encode(v)
}

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
