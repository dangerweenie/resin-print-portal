package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// ErrNotFound is returned by lookups that match no row.
var ErrNotFound = errors.New("store: not found")

// RosterEntry is one member as returned by the TinkerAccess get_users endpoint.
type RosterEntry struct {
	ID     int64
	Name   string
	Code   string
	Status string // "A", "I", "S"
}

// SyncResult summarizes what a roster sync changed, for logging.
type SyncResult struct {
	Received    int // rows in the fetched roster
	Deactivated int // previously-seen members no longer on the roster
}

// SyncRoster reconciles the members table with a full roster snapshot from a
// successful TinkerAccess fetch. Members absent from the snapshot are marked
// inactive (membership "expired") but their rows are kept. Callers MUST NOT
// invoke this with a partial/failed fetch — that would deactivate everyone.
func (s *Store) SyncRoster(ctx context.Context, roster []RosterEntry) (SyncResult, error) {
	var res SyncResult
	res.Received = len(roster)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return res, err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `CREATE TEMP TABLE roster_in (
		id BIGINT PRIMARY KEY, name TEXT, code TEXT, status CHAR(1)
	) ON COMMIT DROP`); err != nil {
		return res, fmt.Errorf("temp table: %w", err)
	}

	rows := make([][]any, 0, len(roster))
	for _, r := range roster {
		st := strings.ToUpper(strings.TrimSpace(r.Status))
		if st != "A" && st != "I" && st != "S" {
			st = "I"
		}
		rows = append(rows, []any{r.ID, nullIfEmpty(r.Name), nullIfEmpty(r.Code), st})
	}
	if _, err := tx.CopyFrom(ctx, pgx.Identifier{"roster_in"},
		[]string{"id", "name", "code", "status"},
		pgx.CopyFromRows(rows)); err != nil {
		return res, fmt.Errorf("copy roster: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO members (id, name, code, status, active, first_seen_at, last_synced_at, source_missing_since)
		SELECT id, name, code, status, status IN ('A','S'), now(), now(), NULL FROM roster_in
		ON CONFLICT (id) DO UPDATE SET
			name = EXCLUDED.name,
			code = EXCLUDED.code,
			status = EXCLUDED.status,
			active = EXCLUDED.status IN ('A','S'),
			last_synced_at = now(),
			source_missing_since = NULL`); err != nil {
		return res, fmt.Errorf("upsert members: %w", err)
	}

	tag, err := tx.Exec(ctx, `
		UPDATE members SET active = FALSE,
			source_missing_since = COALESCE(source_missing_since, now())
		WHERE id NOT IN (SELECT id FROM roster_in) AND active = TRUE`)
	if err != nil {
		return res, fmt.Errorf("deactivate missing: %w", err)
	}
	res.Deactivated = int(tag.RowsAffected())

	if err := tx.Commit(ctx); err != nil {
		return res, err
	}
	return res, nil
}

const memberCols = `id, name, code, status, active, first_seen_at, last_synced_at, source_missing_since`

func scanMember(row pgx.Row) (Member, error) {
	var m Member
	var name, code *string
	err := row.Scan(&m.ID, &name, &code, &m.Status, &m.Active,
		&m.FirstSeenAt, &m.LastSyncedAt, &m.SourceMissingSince)
	if err != nil {
		return Member{}, err
	}
	if name != nil {
		m.Name = *name
	}
	if code != nil {
		m.Code = *code
	}
	return m, nil
}

// GetMember returns one member by TinkerAccess id.
func (s *Store) GetMember(ctx context.Context, id int64) (Member, error) {
	m, err := scanMember(s.pool.QueryRow(ctx,
		`SELECT `+memberCols+` FROM members WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Member{}, ErrNotFound
	}
	return m, err
}

// ListMembers returns the whole roster ordered by name.
func (s *Store) ListMembers(ctx context.Context) ([]Member, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+memberCols+` FROM members ORDER BY lower(coalesce(name,'')), id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Member
	for rows.Next() {
		m, err := scanMember(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ResolveSlackName implements the identity lookup for the Pi `check` path:
// an explicit slack_identities mapping first, then a unique case-insensitive
// full-name match. byNameMatch reports which path hit (for decision_log).
func (s *Store) ResolveSlackName(ctx context.Context, normalized string) (m Member, byNameMatch bool, err error) {
	m, err = scanMember(s.pool.QueryRow(ctx, `
		SELECT `+prefixCols("mem", memberCols)+`
		FROM slack_identities si JOIN members mem ON mem.id = si.member_id
		WHERE si.slack_name_normalized = $1`, normalized))
	if err == nil {
		return m, false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Member{}, false, err
	}

	// Fallback: unique exact name match, case-insensitive, whitespace-folded.
	rows, err := s.pool.Query(ctx, `
		SELECT `+memberCols+` FROM members
		WHERE lower(btrim(regexp_replace(coalesce(name,''), '\s+', ' ', 'g'))) = $1
		LIMIT 2`, normalized)
	if err != nil {
		return Member{}, false, err
	}
	defer rows.Close()
	var matches []Member
	for rows.Next() {
		mm, err := scanMember(rows)
		if err != nil {
			return Member{}, false, err
		}
		matches = append(matches, mm)
	}
	if err := rows.Err(); err != nil {
		return Member{}, false, err
	}
	switch len(matches) {
	case 0:
		return Member{}, false, ErrNotFound
	case 1:
		return matches[0], true, nil
	default:
		return Member{}, false, ErrAmbiguousName
	}
}

// ErrAmbiguousName means a fallback name lookup matched more than one member.
var ErrAmbiguousName = errors.New("store: ambiguous slack name")

// AddSlackIdentity maps a normalized Slack name to a member.
func (s *Store) AddSlackIdentity(ctx context.Context, memberID int64, normalized, addedBy string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO slack_identities (member_id, slack_name_normalized, added_by)
		VALUES ($1, $2, $3)
		ON CONFLICT (slack_name_normalized) DO UPDATE SET member_id = EXCLUDED.member_id, added_by = EXCLUDED.added_by, added_at = now()`,
		memberID, normalized, addedBy)
	return err
}

// RemoveSlackIdentity deletes a mapping by id.
func (s *Store) RemoveSlackIdentity(ctx context.Context, id int64) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM slack_identities WHERE id = $1`, id)
	return err
}

// SlackIdentitiesFor returns the mappings for one member.
func (s *Store) SlackIdentitiesFor(ctx context.Context, memberID int64) ([]SlackIdentity, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, member_id, slack_name_normalized, added_by, added_at
		FROM slack_identities WHERE member_id = $1 ORDER BY slack_name_normalized`, memberID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SlackIdentity
	for rows.Next() {
		var si SlackIdentity
		if err := rows.Scan(&si.ID, &si.MemberID, &si.SlackNameNormalized, &si.AddedBy, &si.AddedAt); err != nil {
			return nil, err
		}
		out = append(out, si)
	}
	return out, rows.Err()
}

func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// prefixCols rewrites "a, b, c" as "t.a, t.b, t.c" for use after a JOIN.
func prefixCols(alias, cols string) string {
	parts := strings.Split(cols, ",")
	for i := range parts {
		parts[i] = alias + "." + strings.TrimSpace(parts[i])
	}
	return strings.Join(parts, ", ")
}
