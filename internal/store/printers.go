package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

const printerCols = `id, slug, display_name, model, allowed_extensions,
	safety_checklist, slack_webhook_url, api_key_hash,
	coalesce(device_id, ''), approved, enrolled_at, last_seen_at, created_at,
	agent_version, agent_version_at, agent_target_override, agent_update_hold`

func scanPrinter(row pgx.Row) (Printer, error) {
	var p Printer
	err := row.Scan(&p.ID, &p.Slug, &p.DisplayName, &p.Model,
		&p.AllowedExtensions, &p.SafetyChecklist, &p.SlackWebhookURL,
		&p.APIKeyHash, &p.DeviceID, &p.Approved, &p.EnrolledAt, &p.LastSeenAt,
		&p.CreatedAt, &p.AgentVersion, &p.AgentVersionAt, &p.AgentTargetOverride,
		&p.AgentUpdateHold)
	return p, err
}

// CreatePrinter inserts a hand-made printer (no device_id) and returns it. It is
// approved immediately — an admin created it on purpose. apiKeyHash is the
// sha-256 hex of the bearer token handed to the Pi.
func (s *Store) CreatePrinter(ctx context.Context, p Printer) (Printer, error) {
	row := s.pool.QueryRow(ctx, `
		INSERT INTO printers (slug, display_name, model, allowed_extensions,
			safety_checklist, slack_webhook_url, api_key_hash, approved)
		VALUES ($1,$2,$3,$4,$5,$6,$7,TRUE)
		RETURNING `+printerCols,
		p.Slug, p.DisplayName, p.Model, nonNil(p.AllowedExtensions),
		jsonbSlice(p.SafetyChecklist), p.SlackWebhookURL, p.APIKeyHash)
	return scanPrinter(row)
}

// EnrollPrinter is the self-registration path: a Pi presents a stable hardware
// id and its hostname. If that device is new, a pending (approved=false)
// printer row is created with a free slug derived from the hostname and the
// given default checklist. If the device already enrolled, its row is reused
// and its API key is rotated (the Pi lost the old one — e.g. a re-flash).
// Either way a fresh plaintext key is returned; only its hash is stored.
func (s *Store) EnrollPrinter(ctx context.Context, deviceID, hostname string, defaultChecklist []string, newKey, newKeyHash string) (p Printer, isNew bool, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Printer{}, false, err
	}
	defer tx.Rollback(ctx)

	existing, err := scanPrinter(tx.QueryRow(ctx,
		`SELECT `+printerCols+` FROM printers WHERE device_id=$1`, deviceID))
	switch {
	case err == nil:
		if _, err = tx.Exec(ctx,
			`UPDATE printers SET api_key_hash=$2, last_seen_at=now() WHERE id=$1`,
			existing.ID, newKeyHash); err != nil {
			return Printer{}, false, err
		}
		existing.APIKeyHash = newKeyHash
		if err = tx.Commit(ctx); err != nil {
			return Printer{}, false, err
		}
		return existing, false, nil
	case errors.Is(err, pgx.ErrNoRows):
		// new device — find a free slug
		base := slugifyHostname(hostname)
		if base == "" {
			base = "printer"
		}
		slug := base
		for i := 2; ; i++ {
			var n int
			if err = tx.QueryRow(ctx, `SELECT count(*) FROM printers WHERE slug=$1`, slug).Scan(&n); err != nil {
				return Printer{}, false, err
			}
			if n == 0 {
				break
			}
			slug = fmt.Sprintf("%s-%d", base, i)
		}
		created, err := scanPrinter(tx.QueryRow(ctx, `
			INSERT INTO printers (slug, display_name, api_key_hash, device_id,
				approved, enrolled_at, last_seen_at, safety_checklist)
			VALUES ($1,$2,$3,$4,FALSE,now(),now(),$5)
			RETURNING `+printerCols,
			slug, hostname, newKeyHash, deviceID, jsonbSlice(defaultChecklist)))
		if err != nil {
			return Printer{}, false, err
		}
		if err = tx.Commit(ctx); err != nil {
			return Printer{}, false, err
		}
		return created, true, nil
	default:
		return Printer{}, false, err
	}
}

// SetPrinterApproved flips the approved flag (admin approve / disable).
func (s *Store) SetPrinterApproved(ctx context.Context, id int64, approved bool) error {
	_, err := s.pool.Exec(ctx, `UPDATE printers SET approved=$2 WHERE id=$1`, id, approved)
	return err
}

// CountPendingPrinters returns how many enrolled-but-unapproved printers exist.
func (s *Store) CountPendingPrinters(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM printers WHERE approved = FALSE AND device_id IS NOT NULL`).Scan(&n)
	return n, err
}

// DeletePrinter removes a printer. Fails if print jobs / decision-log rows still
// reference it (Postgres FK) — callers should only offer this for pending
// enrollments, which never have history.
func (s *Store) DeletePrinter(ctx context.Context, id int64) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM printers WHERE id=$1`, id)
	return err
}

// TouchPrinterSeen records that the Pi just made an authenticated request. The
// write is throttled to at most once a minute per printer to keep chatty
// pollers from hammering the row.
func (s *Store) TouchPrinterSeen(ctx context.Context, id int64) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE printers SET last_seen_at=now()
		WHERE id=$1 AND (last_seen_at IS NULL OR last_seen_at < now() - interval '1 minute')`, id)
	return err
}

// SetPrinterAgentVersion records the pi-agent build a Pi is running. Called
// (from a goroutine) only when the reported version differs from what's stored,
// so it doesn't add write traffic to every check-in.
func (s *Store) SetPrinterAgentVersion(ctx context.Context, id int64, version string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE printers
		   SET agent_version = $2,
		       agent_version_at = now()
		 WHERE id = $1 AND agent_version IS DISTINCT FROM $2`, id, version)
	return err
}

// SetPrinterAgentUpdate sets the per-Pi self-update controls: override pins the
// Pi to a specific version now (empty = follow the fleet default), hold keeps
// the Pi on whatever it is running.
func (s *Store) SetPrinterAgentUpdate(ctx context.Context, id int64, override string, hold bool) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE printers
		   SET agent_target_override = $2,
		       agent_update_hold = $3
		 WHERE id = $1`, id, override, hold)
	return err
}

// UpdatePrinter updates the editable fields of a printer by id.
func (s *Store) UpdatePrinter(ctx context.Context, p Printer) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE printers SET display_name=$2, model=$3,
			allowed_extensions=$4, safety_checklist=$5, slack_webhook_url=$6
		WHERE id=$1`,
		p.ID, p.DisplayName, p.Model, nonNil(p.AllowedExtensions),
		jsonbSlice(p.SafetyChecklist), p.SlackWebhookURL)
	return err
}

// RotatePrinterKey stores a new API key hash for a printer.
func (s *Store) RotatePrinterKey(ctx context.Context, id int64, apiKeyHash string) error {
	_, err := s.pool.Exec(ctx, `UPDATE printers SET api_key_hash=$2 WHERE id=$1`, id, apiKeyHash)
	return err
}

// GetPrinterBySlug looks up a printer by its URL slug.
func (s *Store) GetPrinterBySlug(ctx context.Context, slug string) (Printer, error) {
	p, err := scanPrinter(s.pool.QueryRow(ctx,
		`SELECT `+printerCols+` FROM printers WHERE slug=$1`, slug))
	if errors.Is(err, pgx.ErrNoRows) {
		return Printer{}, ErrNotFound
	}
	return p, err
}

// GetPrinterByKeyHash resolves the printer a Pi request authenticates as.
func (s *Store) GetPrinterByKeyHash(ctx context.Context, keyHash string) (Printer, error) {
	p, err := scanPrinter(s.pool.QueryRow(ctx,
		`SELECT `+printerCols+` FROM printers WHERE api_key_hash=$1`, keyHash))
	if errors.Is(err, pgx.ErrNoRows) {
		return Printer{}, ErrNotFound
	}
	return p, err
}

// ListPrinters returns all printers: pending enrollments first (newest first),
// then approved printers by slug.
func (s *Store) ListPrinters(ctx context.Context) ([]Printer, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+printerCols+` FROM printers
		ORDER BY approved ASC, enrolled_at DESC NULLS LAST, slug`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Printer
	for rows.Next() {
		p, err := scanPrinter(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func slugifyHostname(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	var b strings.Builder
	for _, r := range h {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == ' ' || r == '_' || r == '-' || r == '.':
			b.WriteRune('-')
		}
	}
	return strings.Trim(b.String(), "-")
}
