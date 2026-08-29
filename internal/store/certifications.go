package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// IsCertified reports whether a member holds an active (un-revoked)
// certification for a printer.
func (s *Store) IsCertified(ctx context.Context, memberID, printerID int64) (bool, error) {
	var one int
	err := s.pool.QueryRow(ctx, `
		SELECT 1 FROM certifications
		WHERE member_id=$1 AND printer_id=$2 AND revoked_at IS NULL
		LIMIT 1`, memberID, printerID).Scan(&one)
	if err == pgx.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// Certify grants a member certification for a printer. Idempotent: a second
// call while an active cert exists is a no-op.
func (s *Store) Certify(ctx context.Context, memberID, printerID int64, by string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO certifications (member_id, printer_id, certified_by)
		VALUES ($1,$2,$3)
		ON CONFLICT (member_id, printer_id) WHERE revoked_at IS NULL DO NOTHING`,
		memberID, printerID, by)
	return err
}

// Revoke ends any active certification for a member+printer.
func (s *Store) Revoke(ctx context.Context, memberID, printerID int64) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE certifications SET revoked_at = now()
		WHERE member_id=$1 AND printer_id=$2 AND revoked_at IS NULL`,
		memberID, printerID)
	return err
}

// CertifiedMember is a roster row joined with its cert state for one printer.
type CertifiedMember struct {
	Member
	Certified   bool
	CertifiedBy string
}

// ListCertifications returns every member with a flag for whether they are
// currently certified on the given printer, roster order.
func (s *Store) ListCertifications(ctx context.Context, printerID int64) ([]CertifiedMember, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+prefixCols("m", memberCols)+`,
			(c.id IS NOT NULL) AS certified,
			coalesce(c.certified_by, '') AS certified_by
		FROM members m
		LEFT JOIN certifications c
			ON c.member_id = m.id AND c.printer_id = $1 AND c.revoked_at IS NULL
		ORDER BY lower(coalesce(m.name,'')), m.id`, printerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CertifiedMember
	for rows.Next() {
		var cm CertifiedMember
		var name, code *string
		if err := rows.Scan(&cm.ID, &name, &code, &cm.Status, &cm.Active,
			&cm.FirstSeenAt, &cm.LastSyncedAt, &cm.SourceMissingSince,
			&cm.Certified, &cm.CertifiedBy); err != nil {
			return nil, err
		}
		if name != nil {
			cm.Name = *name
		}
		if code != nil {
			cm.Code = *code
		}
		out = append(out, cm)
	}
	return out, rows.Err()
}
