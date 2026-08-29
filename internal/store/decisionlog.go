package store

import "context"

// LogDecision records one check/upload attempt. printerID and memberID are
// optional (0 => NULL).
func (s *Store) LogDecision(ctx context.Context, e DecisionLogEntry) error {
	var printerID, memberID any
	if e.PrinterID != nil && *e.PrinterID != 0 {
		printerID = *e.PrinterID
	}
	if e.MemberID != nil && *e.MemberID != 0 {
		memberID = *e.MemberID
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO decision_log (printer_id, slack_name_used, member_id, filename, outcome, reason)
		VALUES ($1,$2,$3,$4,$5,$6)`,
		printerID, e.SlackNameUsed, memberID, e.Filename, e.Outcome, e.Reason)
	return err
}

// DecisionLogView is a decision_log row with names resolved, for the admin log.
type DecisionLogView struct {
	DecisionLogEntry
	MemberName  string
	PrinterSlug string
}

// RecentDecisions returns the newest decision_log rows.
func (s *Store) RecentDecisions(ctx context.Context, limit int) ([]DecisionLogView, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT d.id, d.printer_id, d.slack_name_used, d.member_id, d.filename, d.ts,
			d.outcome, d.reason,
			coalesce(m.name,'') AS member_name, coalesce(p.slug,'') AS printer_slug
		FROM decision_log d
		LEFT JOIN members m ON m.id = d.member_id
		LEFT JOIN printers p ON p.id = d.printer_id
		ORDER BY d.id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DecisionLogView
	for rows.Next() {
		var v DecisionLogView
		if err := rows.Scan(&v.ID, &v.PrinterID, &v.SlackNameUsed, &v.MemberID,
			&v.Filename, &v.TS, &v.Outcome, &v.Reason,
			&v.MemberName, &v.PrinterSlug); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
