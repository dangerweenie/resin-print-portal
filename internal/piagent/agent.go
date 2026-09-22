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
	// CurrentSeq is CurrentCode plus a sequence number that only advances on a
	// genuinely new physical presentation — see rfid.Reader.CurrentSeq.
	CurrentSeq() (code string, seq int, ok bool)
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
	scanCache scanCheck // last /check result, keyed by (code, seq), to avoid re-checking a held fob

	loadMu    sync.Mutex
	loadState loadOutcome // outcome of the (possibly still in-flight) auto-load for the current (code, seq)
}

type scanCheck struct {
	code   string
	seq    int
	result CheckResult
}

// loadOutcome is the in-progress or finished result of autoLoad for one
// specific tap. status is "" (not applicable), "pending", "loaded", or
// "failed".
type loadOutcome struct {
	code    string
	seq     int
	status  string
	message string
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
	mux.HandleFunc("/finish", a.handleFinish)
	return mux
}

// WatchTaps checks every fresh fob presentation against the portal on its own
// ticker, independent of whether the upload page is even open in a browser.
// Without this, the portal (and anything gated on it — the decision_log,
// certify-by-tap) only ever heard about a tap when a browser happened to be
// polling /scan at the time; a Pi nobody was actively looking at would read
// fobs all day and report none of it. Run it in its own goroutine; it returns
// when ctx is done.
func (a *Agent) WatchTaps(ctx context.Context) {
	t := time.NewTicker(300 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			code, seq, ok := a.scanner.CurrentSeq()
			if !ok {
				continue
			}
			if _, err := a.checkFob(ctx, code, seq); err != nil {
				a.log.Debug("background fob check failed", "err", err)
			}
		}
	}
}

// GET /scan — the upload page polls this to show who just tapped. This is now
// just a status read: WatchTaps (above) is what actually drives the portal
// check, so a tap is recorded whether or not anyone has this page open.
func (a *Agent) handleScan(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	code, seq, ok := a.scanner.CurrentSeq()
	if !ok {
		_, _ = w.Write([]byte(`{"scanned":false}`))
		return
	}

	res, err := a.checkFob(r.Context(), code, seq)
	if err != nil {
		a.log.Warn("fob check failed", "err", err)
		_, _ = w.Write([]byte(`{"scanned":true,"allowed":false,"reason":"portal_unreachable"}`))
		return
	}
	loadStatus, loadMessage := a.getLoadState(code, seq)
	writeJSON(w, map[string]any{
		"scanned":         true,
		"allowed":         res.Allowed,
		"reason":          res.Reason,
		"member_name":     res.MemberName,
		"staged_filename": res.StagedFilename,
		"staged_eta":      res.StagedETA,
		"just_certified":  res.JustCertified,
		"load_status":     loadStatus,  // "" | "pending" | "loaded" | "failed"
		"load_message":    loadMessage, // set once status is "loaded" or "failed"
	})
}

// checkFob resolves a fob against the portal, caching the result per
// (code, seq) so a fob held continuously at the reader is checked — and
// logged, and allowed to consume a certify-by-tap capture — exactly once,
// not once per poll. Unlike a wall-clock cache, this can never mask a
// genuinely new tap: seq only advances on a real physical event (see
// rfid.Reader.CurrentSeq), never with the mere passage of time, so two
// distinct taps of the same fob seconds apart are never conflated into one.
func (a *Agent) checkFob(ctx context.Context, code string, seq int) (res CheckResult, err error) {
	a.scanMu.Lock()
	c := a.scanCache
	a.scanMu.Unlock()
	if c.code == code && c.seq == seq {
		return c.result, nil
	}
	res, err = a.central.CheckFob(ctx, code)
	if err != nil {
		return CheckResult{}, err
	}
	a.scanMu.Lock()
	a.scanCache = scanCheck{code: code, seq: seq, result: res}
	a.scanMu.Unlock()
	a.log.Info("fob tap", "code", code, "allowed", res.Allowed,
		"member", res.MemberName, "reason", res.Reason,
		"staged", res.StagedFilename != "", "just_certified", res.JustCertified)

	// A tap that both passes the check AND has a print waiting IS the release
	// action — load it now, no confirmation click. The safety checklist was
	// already confirmed on the portal at upload time, and physically
	// presenting the fob is already the deliberate act; a second "are you
	// sure" step here would only be friction, not safety.
	if res.Allowed && res.StagedFilename != "" {
		a.setLoadState(code, seq, "pending", "")
		go a.autoLoad(code, seq)
	}
	return res, nil
}

func (a *Agent) setLoadState(code string, seq int, status, message string) {
	a.loadMu.Lock()
	a.loadState = loadOutcome{code: code, seq: seq, status: status, message: message}
	a.loadMu.Unlock()
}

func (a *Agent) getLoadState(code string, seq int) (status, message string) {
	a.loadMu.Lock()
	defer a.loadMu.Unlock()
	if a.loadState.code == code && a.loadState.seq == seq {
		return a.loadState.status, a.loadState.message
	}
	return "", ""
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

// autoLoad is the fob-release: claim the staged job, pull the file from the
// portal, and write it to the gadget. Runs detached in its own goroutine from
// checkFob the instant a tap both passes and has something staged — nothing
// is waiting on an HTTP response, so the outcome is logged and recorded via
// setLoadState (which /scan reports) rather than returned anywhere.
func (a *Agent) autoLoad(code string, seq int) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	claim, err := a.central.ClaimStagedJob(ctx, code)
	if err != nil {
		a.log.Error("auto-load: claim failed", "err", err)
		a.setLoadState(code, seq, "failed", "Couldn't reach the print portal. Nothing was loaded.")
		return
	}
	if !claim.Claimed {
		if claim.Reason == "no_staged_job" {
			// Expected, not an error: e.g. the same fob is still resting on
			// the reader after an earlier tap already claimed and loaded it,
			// so this re-check just finds nothing left to do.
			a.log.Debug("auto-load: nothing to claim", "reason", claim.Reason)
			a.setLoadState(code, seq, "", "")
			return
		}
		a.log.Warn("auto-load: claim denied", "reason", claim.Reason)
		a.setLoadState(code, seq, "failed", denyMessage(claim.Reason))
		return
	}

	tmpDir, err := os.MkdirTemp("", "piagent-")
	if err != nil {
		a.log.Error("auto-load: scratch space", "err", err)
		a.setLoadState(code, seq, "failed", "Pi is out of scratch space.")
		return
	}
	defer os.RemoveAll(tmpDir)
	name := sanitizeFilename(claim.Filename)
	if name == "" {
		name = "print.bin"
	}
	dst := filepath.Join(tmpDir, name)

	if _, err := a.central.DownloadJobFile(ctx, claim.JobID, claim.SHA256, dst); err != nil {
		a.log.Error("auto-load: download failed", "err", err, "job", claim.JobID)
		a.setLoadState(code, seq, "failed", "The download from the portal failed or was corrupt. Ask a staff member to retry.")
		return
	}

	if err := a.gadget.Write(ctx, dst); err != nil {
		a.log.Error("auto-load: gadget write failed", "err", err)
		a.setLoadState(code, seq, "failed", "Loading the file onto the printer's drive failed. Ask a staff member.")
		return
	}
	if err := a.central.JobStarted(ctx, claim.JobID); err != nil {
		a.log.Warn("auto-load: job started callback failed", "err", err, "job", claim.JobID)
	}
	a.scanner.Clear() // next member starts fresh

	msg := "Loaded " + name + " onto the printer. Start the print from the printer's screen."
	if claim.MachineWarning != "" {
		msg += " Heads up: " + claim.MachineWarning + "."
	}
	a.log.Info("auto-load: done", "file", name, "job", claim.JobID)
	a.setLoadState(code, seq, "loaded", msg)
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
