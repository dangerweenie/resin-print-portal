// Package slack posts best-effort messages to a Slack Incoming Webhook. A blank
// URL is a silent no-op so Slack is always optional and never blocks a print.
package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Post sends text to the webhook. It returns an error only so callers can log
// it; they should never fail a request on the result.
func Post(ctx context.Context, webhookURL, text string) error {
	if webhookURL == "" {
		return nil
	}
	body, _ := json.Marshal(map[string]string{"text": text})

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhookURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("slack webhook: status %d", resp.StatusCode)
	}
	return nil
}
