package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

const jobCols = `id, printer_id, member_id, slack_name_used, filename, sliced_for_model,
	checklist_answers, started_at, estimated_seconds, eta_exact,
	estimated_complete_at, ended_at, status, end_reason`

func scanJob(row pgx.Row) (PrintJob, error) {
	var j PrintJob
	err := row.Scan(&j.ID, &j.PrinterID, &j.MemberID, &j.SlackNameUsed, &j.Filename,
		&j.SlicedForModel, &j.ChecklistAnswers, &j.StartedAt, &j.EstimatedSeconds,
		&j.ETAExact, &j.EstimatedCompleteAt, &j.EndedAt, &j.Status, &j.EndReason)
	return j, err
}

// CurrentJob returns the printer's active job, or ErrNotFound if idle.
func (s *Store) CurrentJob(ctx context.Context, printerID int64) (PrintJob, error) {
	j, err := scanJob(s.pool.QueryRow(ctx, `SELECT `+jobCols+`
		FROM print_jobs WHERE printer_id=$1 AND status='printing'
		ORDER BY id DESC LIMIT 1`, printerID))
	if errors.Is(err, pgx.ErrNoRows) {
		return PrintJob{}, ErrNotFound
	}
	return j, err
}

// StartJob supersedes any active job on the printer and inserts a new one, in a
// single transaction (only one physical print at a time).
func (s *Store) StartJob(ctx context.Context, j PrintJob) (PrintJob, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return PrintJob{}, err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `
		UPDATE print_jobs SET status='ended', ended_at=now(), end_reason='superseded'
		WHERE printer_id=$1 AND status='printing'`, j.PrinterID); err != nil {
		return PrintJob{}, err
	}

	row := tx.QueryRow(ctx, `
		INSERT INTO print_jobs (printer_id, member_id, slack_name_used, filename,
			sliced_for_model, checklist_answers, estimated_seconds, eta_exact,
			estimated_complete_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		RETURNING `+jobCols,
		j.PrinterID, j.MemberID, j.SlackNameUsed, j.Filename, j.SlicedForModel,
		jsonbSlice(j.ChecklistAnswers), j.EstimatedSeconds, j.ETAExact, j.EstimatedCompleteAt)
	created, err := scanJob(row)
	if err != nil {
		return PrintJob{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return PrintJob{}, err
	}
	return created, nil
}

// EndJob marks a job ended with the given reason
// (member_finished|admin_cleared). It only touches a still-printing row.
func (s *Store) EndJob(ctx context.Context, jobID int64, reason string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE print_jobs SET status='ended', ended_at=now(), end_reason=$2
		WHERE id=$1 AND status='printing'`, jobID, reason)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// GetJob returns one job scoped to a printer.
func (s *Store) GetJob(ctx context.Context, printerID, jobID int64) (PrintJob, error) {
	j, err := scanJob(s.pool.QueryRow(ctx, `SELECT `+jobCols+`
		FROM print_jobs WHERE id=$1 AND printer_id=$2`, jobID, printerID))
	if errors.Is(err, pgx.ErrNoRows) {
		return PrintJob{}, ErrNotFound
	}
	return j, err
}

// JobView is a job row plus the member's display name, for lists.
type JobView struct {
	PrintJob
	MemberName  string
	PrinterSlug string
}

func (s *Store) queryJobViews(ctx context.Context, where string, args ...any) ([]JobView, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+prefixCols("j", jobCols)+`,
			coalesce(m.name,'') AS member_name, p.slug AS printer_slug
		FROM print_jobs j
		JOIN printers p ON p.id = j.printer_id
		LEFT JOIN members m ON m.id = j.member_id
		`+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []JobView
	for rows.Next() {
		var v JobView
		if err := rows.Scan(&v.ID, &v.PrinterID, &v.MemberID, &v.SlackNameUsed,
			&v.Filename, &v.SlicedForModel, &v.ChecklistAnswers, &v.StartedAt,
			&v.EstimatedSeconds, &v.ETAExact, &v.EstimatedCompleteAt, &v.EndedAt,
			&v.Status, &v.EndReason, &v.MemberName, &v.PrinterSlug); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// RecentJobs returns the most recent jobs for one printer.
func (s *Store) RecentJobs(ctx context.Context, printerID int64, limit int) ([]JobView, error) {
	return s.queryJobViews(ctx, `WHERE j.printer_id=$1 ORDER BY j.id DESC LIMIT $2`, printerID, limit)
}

// AllRecentJobs returns recent jobs across every printer, for /api/v1/status
// and the admin log.
func (s *Store) AllRecentJobs(ctx context.Context, limit int) ([]JobView, error) {
	return s.queryJobViews(ctx, `ORDER BY j.id DESC LIMIT $1`, limit)
}

// CurrentJobs returns the active job for every printer that has one.
func (s *Store) CurrentJobs(ctx context.Context) ([]JobView, error) {
	return s.queryJobViews(ctx, `WHERE j.status='printing' ORDER BY p.slug`)
}

// DisplayStatus derives a UI status from a job and the current time, matching
// the old Flask behaviour: a printing job past its ETA is "overdue".
func DisplayStatus(j PrintJob, now time.Time) string {
	if j.Status == "ended" || j.EndedAt != nil {
		return "ended"
	}
	if j.EstimatedCompleteAt != nil && now.After(*j.EstimatedCompleteAt) {
		return "overdue"
	}
	return "printing"
}
