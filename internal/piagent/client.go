package piagent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// CentralClient talks to the central portal's Pi-facing API for one printer.
type CentralClient struct {
	baseURL string
	slug    string
	apiKey  string
	http    *http.Client
}

// NewCentralClient builds a client. baseURL is the portal root, e.g.
// https://portal.example.org. slug/apiKey may be empty if the Pi still has to
// enroll — call SetCredentials afterwards.
func NewCentralClient(baseURL, slug, apiKey string) *CentralClient {
	return &CentralClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		slug:    slug,
		apiKey:  apiKey,
		http:    &http.Client{Timeout: 5 * time.Minute}, // uploads can be large + slow on Pi wifi
	}
}

// SetCredentials updates the printer slug + API key after enrollment.
func (c *CentralClient) SetCredentials(slug, apiKey string) {
	c.slug = slug
	c.apiKey = apiKey
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
	return fmt.Sprintf("%s/api/v1/printers/%s%s", c.baseURL, c.slug, suffix)
}

func (c *CentralClient) do(req *http.Request, out any) error {
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("central %s: status %d: %s", req.URL.Path, resp.StatusCode, bytes.TrimSpace(body))
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

// FetchCurrentJob returns the active job, if any.
func (c *CentralClient) FetchCurrentJob(ctx context.Context) (CurrentJob, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.url("/current-job"), nil)
	var cj CurrentJob
	return cj, c.do(req, &cj)
}

// SubmitPrint uploads the sliced file at filePath plus the identity and
// checklist answers, and returns the central service's verdict.
func (c *CentralClient) SubmitPrint(ctx context.Context, slackName, filename, filePath string, checked []bool) (PrintResult, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return PrintResult{}, err
	}
	defer f.Close()

	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		defer pw.Close()
		_ = mw.WriteField("slack_name", slackName)
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
