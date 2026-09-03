package piagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dangerweenie/resin-print-portal/internal/buildinfo"
)

// ErrCredentialsRejected means the portal no longer recognises this Pi's
// stored slug + API key (deleted printer, rotated key, wiped database). The
// agent should re-enroll.
var ErrCredentialsRejected = errors.New("piagent: portal rejected the stored credentials")

// APIError carries the HTTP status from a non-2xx portal response.
type APIError struct {
	Status int
	Body   string
	Path   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("central %s: status %d: %s", e.Path, e.Status, e.Body)
}

func (e *APIError) Is(target error) bool {
	return target == ErrCredentialsRejected &&
		(e.Status == http.StatusUnauthorized || e.Status == http.StatusForbidden || e.Status == http.StatusNotFound)
}

// CentralClient talks to the central portal's Pi-facing API for one printer.
type CentralClient struct {
	baseURL string
	http    *http.Client
	version string // reported to the portal via X-Agent-Version on every call

	mu     sync.RWMutex
	slug   string
	apiKey string
}

// NewCentralClient builds a client. baseURL is the portal root, e.g.
// https://portal.example.org. slug/apiKey may be empty if the Pi still has to
// enroll — call SetCredentials afterwards.
func NewCentralClient(baseURL, slug, apiKey string) *CentralClient {
	return &CentralClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		slug:    slug,
		apiKey:  apiKey,
		version: buildinfo.Resolve(),
		http:    &http.Client{Timeout: 5 * time.Minute}, // uploads can be large + slow on Pi wifi
	}
}

// SetCredentials updates the printer slug + API key after (re-)enrollment.
func (c *CentralClient) SetCredentials(slug, apiKey string) {
	c.mu.Lock()
	c.slug, c.apiKey = slug, apiKey
	c.mu.Unlock()
}

func (c *CentralClient) creds() (slug, apiKey string) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.slug, c.apiKey
}

// Enroll self-registers this Pi with the fleet bootstrap token and returns the
// slug + API key the portal issued. The printer starts unapproved.
func (c *CentralClient) Enroll(ctx context.Context, enrollToken, deviceID, hostname string) (slug, apiKey string, approved bool, err error) {
	payload, _ := json.Marshal(map[string]string{"device_id": deviceID, "hostname": hostname})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/v1/enroll", bytes.NewReader(payload))
	if err != nil {
		return "", "", false, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Agent-Version", c.version)
	if enrollToken != "" {
		req.Header.Set("Authorization", "Bearer "+enrollToken)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return "", "", false, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", false, fmt.Errorf("enroll: status %d: %s", resp.StatusCode, bytes.TrimSpace(b))
	}
	var out struct {
		Slug     string `json:"slug"`
		APIKey   string `json:"api_key"`
		Approved bool   `json:"approved"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return "", "", false, fmt.Errorf("enroll: decode: %w", err)
	}
	if out.Slug == "" || out.APIKey == "" {
		return "", "", false, fmt.Errorf("enroll: portal returned an incomplete response")
	}
	return out.Slug, out.APIKey, out.Approved, nil
}

// Config is the printer configuration served to the Pi.
type Config struct {
	Slug              string   `json:"slug"`
	DisplayName       string   `json:"display_name"`
	Model             string   `json:"model"`
	AllowedExtensions []string `json:"allowed_extensions"`
	Approved          bool     `json:"approved"`
	SafetyChecklist   []string `json:"safety_checklist"`
}

// PrintResult is the central service's verdict on a print request.
type PrintResult struct {
	Approved       bool   `json:"approved"`
	Reason         string `json:"reason"`
	JobID          int64  `json:"job_id"`
	ETASeconds     int    `json:"eta_seconds"`
	ETAExact       bool   `json:"eta_exact"`
	MachineWarning string `json:"machine_warning"`
}

// CurrentJob is a slim view of the printer's active job.
type CurrentJob struct {
	CurrentJob *struct {
		ID             int64  `json:"id"`
		Filename       string `json:"filename"`
		SlackName      string `json:"slack_name"`
		Status         string `json:"status"`
		RemainingHuman string `json:"remaining_human"`
	} `json:"current_job"`
}

func (c *CentralClient) url(suffix string) string {
	slug, _ := c.creds()
	return fmt.Sprintf("%s/api/v1/printers/%s%s", c.baseURL, slug, suffix)
}

func (c *CentralClient) do(req *http.Request, out any) error {
	_, apiKey := c.creds()
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("X-Agent-Version", c.version)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &APIError{Status: resp.StatusCode, Path: req.URL.Path, Body: string(bytes.TrimSpace(body))}
	}
	if out == nil || len(body) == 0 {
		return nil
	}
	return json.Unmarshal(body, out)
}

// FetchConfig gets the printer configuration.
func (c *CentralClient) FetchConfig(ctx context.Context) (Config, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.url("/config"), nil)
	var cfg Config
	return cfg, c.do(req, &cfg)
}

// Verify checks the portal still recognises the stored credentials. It returns
// ErrCredentialsRejected if the portal 401/403/404s, nil if they're good, and
// the raw error for anything transient (network, 5xx) — do NOT re-enroll on
// those.
func (c *CentralClient) Verify(ctx context.Context) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.url("/config"), nil)
	err := c.do(req, nil)
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrCredentialsRejected) {
		return ErrCredentialsRejected
	}
	return err
}

// AgentUpdateInfo is the portal's answer to "what pi-agent build should I be
// running, and where do I get it".
type AgentUpdateInfo struct {
	UpdateAvailable bool   `json:"update_available"`
	TargetVersion   string `json:"target_version"`
	PortalVersion   string `json:"portal_version"`
	URL             string `json:"url"`    // path (or absolute URL) to the binary
	SHA256          string `json:"sha256"` // hex, of the binary at URL
	Size            int64  `json:"size"`
}

// AgentUpdate asks the portal whether this Pi should replace its own binary.
func (c *CentralClient) AgentUpdate(ctx context.Context) (AgentUpdateInfo, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.url("/agent-update"), nil)
	var out AgentUpdateInfo
	return out, c.do(req, &out)
}

// DownloadAgent fetches the binary named by AgentUpdateInfo.URL. The caller owns
// the returned body and must close it. ref may be an absolute URL or a path
// rooted at the portal.
func (c *CentralClient) DownloadAgent(ctx context.Context, ref string) (io.ReadCloser, int64, error) {
	full := ref
	if strings.HasPrefix(ref, "/") {
		full = c.baseURL + ref
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, full, nil)
	if err != nil {
		return nil, 0, err
	}
	_, apiKey := c.creds()
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("X-Agent-Version", c.version)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		resp.Body.Close()
		return nil, 0, &APIError{Status: resp.StatusCode, Path: req.URL.Path, Body: string(bytes.TrimSpace(b))}
	}
	return resp.Body, resp.ContentLength, nil
}

// FetchCurrentJob returns the active job, if any.
func (c *CentralClient) FetchCurrentJob(ctx context.Context) (CurrentJob, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.url("/current-job"), nil)
	var cj CurrentJob
	return cj, c.do(req, &cj)
}

// CheckResult is the portal's verdict on an identity, with no file involved.
type CheckResult struct {
	Allowed    bool   `json:"allowed"`
	Reason     string `json:"reason"`
	MemberName string `json:"member_name"`
}

// CheckFob asks the portal who a tapped fob belongs to and whether they may
// print here — used to show "Tapped: Jane Doe" on the page before submit.
func (c *CentralClient) CheckFob(ctx context.Context, code string) (CheckResult, error) {
	body, _ := json.Marshal(map[string]string{"rfid_code": code})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, c.url("/check"), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	var out CheckResult
	return out, c.do(req, &out)
}

// SubmitPrint uploads the sliced file at filePath plus the tapped fob code and
// checklist answers, and returns the central service's verdict.
func (c *CentralClient) SubmitPrint(ctx context.Context, fobCode, filename, filePath string, checked []bool) (PrintResult, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return PrintResult{}, err
	}
	defer f.Close()

	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		defer pw.Close()
		_ = mw.WriteField("rfid_code", fobCode)
		_ = mw.WriteField("filename", filename)
		for i, ok := range checked {
			if ok {
				_ = mw.WriteField("check_"+strconv.Itoa(i), "1")
			}
		}
		part, err := mw.CreateFormFile("file", filename)
		if err != nil {
			pw.CloseWithError(err)
			return
		}
		if _, err := io.Copy(part, f); err != nil {
			pw.CloseWithError(err)
			return
		}
		pw.CloseWithError(mw.Close())
	}()

	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, c.url("/print-requests"), pr)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	var res PrintResult
	if err := c.do(req, &res); err != nil {
		return PrintResult{}, err
	}
	return res, nil
}

// JobStarted tells central the file is live on the gadget.
func (c *CentralClient) JobStarted(ctx context.Context, jobID int64) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
		c.url("/jobs/"+strconv.FormatInt(jobID, 10)+"/started"), nil)
	return c.do(req, nil)
}

// JobFinished tells central the member pulled their print.
func (c *CentralClient) JobFinished(ctx context.Context, jobID int64) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
		c.url("/jobs/"+strconv.FormatInt(jobID, 10)+"/finished"), nil)
	return c.do(req, nil)
}
