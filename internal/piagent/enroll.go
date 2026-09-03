package piagent

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"time"

	"github.com/dangerweenie/resin-print-portal/internal/config"
)

// registrationCheckInterval is how often a running agent re-confirms the portal
// still knows it.
const registrationCheckInterval = 2 * time.Minute

// MaintainRegistration makes sure the Pi is registered, now and forever:
//
//   - no stored credentials         -> enroll
//   - stored credentials rejected   -> discard them and re-enroll
//   - stored credentials still good -> nothing
//
// The first pass runs synchronously (so the agent starts with valid creds when
// it can); it then keeps checking in the background so a printer that is
// deleted, key-rotated, or lost to a wiped database heals itself without a
// reboot. Transient errors (portal down, 5xx) never trigger a re-enroll.
func MaintainRegistration(ctx context.Context, client *CentralClient, cfg *config.PiAgent, log *slog.Logger) error {
	if err := ensureRegistered(ctx, client, cfg, log); err != nil {
		return err
	}
	go func() {
		t := time.NewTicker(registrationCheckInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := client.Verify(ctx); errors.Is(err, ErrCredentialsRejected) {
					log.Warn("the portal no longer recognises this Pi — re-enrolling")
					_ = ensureRegistered(ctx, client, cfg, log)
				}
			}
		}
	}()
	return nil
}

func ensureRegistered(ctx context.Context, client *CentralClient, cfg *config.PiAgent, log *slog.Logger) error {
	if !cfg.NeedsEnrollment() {
		switch err := client.Verify(ctx); {
		case err == nil:
			return nil // credentials are good
		case errors.Is(err, ErrCredentialsRejected):
			log.Warn("stored credentials were rejected by the portal — discarding and re-enrolling", "err", err)
			_ = os.Remove(cfg.CredsPath)
			cfg.PrinterSlug, cfg.PrinterAPIKey = "", ""
			client.SetCredentials("", "")
		default:
			// Portal unreachable / 5xx — keep the creds, carry on; the
			// background loop will retry.
			log.Warn("could not verify registration with the portal (keeping stored credentials)", "err", err)
			return nil
		}
	}
	return EnrollUntilSuccess(ctx, client, cfg, log)
}

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
	log.Info("enrolling with the central portal",
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
