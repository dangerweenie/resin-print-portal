package piagent

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/dangerweenie/resin-print-portal/internal/config"
)

// EnrollUntilSuccess self-registers the Pi with the fleet bootstrap token,
// retrying until it works or ctx is cancelled. On success it persists the
// issued slug + API key to cfg.CredsPath, updates the client, and returns.
// Enrollment does not wait for admin approval — the agent runs and the upload
// page shows a "pending" notice until /config reports approved.
func EnrollUntilSuccess(ctx context.Context, client *CentralClient, cfg *config.PiAgent, log *slog.Logger) error {
	deviceID := DeviceID(cfg.CredsPath)
	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "printer"
	}
	log.Info("no credentials yet — enrolling with the central portal",
		"device_id", deviceID, "hostname", hostname, "central", cfg.CentralBaseURL)

	const retry = 15 * time.Second
	for {
		slug, key, approved, err := client.Enroll(ctx, cfg.EnrollToken, deviceID, hostname)
		if err == nil {
			if werr := config.WriteCredsFile(cfg.CredsPath, slug, key); werr != nil {
				log.Warn("could not persist credentials; the Pi will re-enroll on next boot",
					"path", cfg.CredsPath, "err", werr)
			}
			client.SetCredentials(slug, key)
			cfg.PrinterSlug, cfg.PrinterAPIKey = slug, key
			log.Info("enrolled", "slug", slug, "approved", approved)
			if !approved {
				log.Info("waiting for a makerspace admin to approve this printer in the portal")
			}
			return nil
		}
		log.Warn("enrollment attempt failed, retrying", "err", err, "retry_in", retry.String())
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(retry):
		}
	}
}
