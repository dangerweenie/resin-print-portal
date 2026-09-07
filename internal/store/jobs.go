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
// (member_finished|admin_cleared). It touches a staged or printing row and
// drops any staged file bytes for it.
func (s *Store) EndJob(ctx context.Context, jobID int64, reason string) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, `
		UPDATE print_jobs SET status='ended', ended_at=now(), end_reason=$2
		WHERE id=$1 AND status IN ('staged','printing')`, jobID, reason)
	if err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM staged_files WHERE job_id=$1`, jobID); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// StageJob supersedes any still-staged job on the printer, inserts a new job in
// 'staged' state, and stores the sliced file bytes for the Pi to pull later.
func (s *Store) StageJob(ctx context.Context, j PrintJob, fileBytes []byte, sha string) (PrintJob, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return PrintJob{}, err
	}
	defer tx.Rollback(ctx)

	// Replace any earlier staged-but-unclaimed job for this printer, and drop
	// its file. A job the member already claimed is 'printing' and keeps its
	// file (keyed by job_id) until the print starts.
	if _, err := tx.Exec(ctx, `
		DELETE FROM staged_files sf USING print_jobs j
		WHERE sf.job_id = j.id AND j.printer_id=$1 AND j.status='staged'`, j.PrinterID); err != nil {
		return PrintJob{}, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE print_jobs SET status='ended', ended_at=now(), end_reason='superseded'
		WHERE printer_id=$1 AND status='staged'`, j.PrinterID); err != nil {
		return PrintJob{}, err
	}

	row := tx.QueryRow(ctx, `
		INSERT INTO print_jobs (printer_id, member_id, slack_name_used, filename,
			sliced_for_model, checklist_answers, estimated_seconds, eta_exact,
			estimated_complete_at, status)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,'staged')
		RETURNING `+jobCols,
		j.PrinterID, j.MemberID, j.SlackNameUsed, j.Filename, j.SlicedForModel,
		jsonbSlice(j.ChecklistAnswers), j.EstimatedSeconds, j.ETAExact, j.EstimatedCompleteAt)
	created, err := scanJob(row)
	if err != nil {
		return PrintJob{}, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO staged_files (job_id, printer_id, filename, bytes, sha256, size_bytes)
		VALUES ($1,$2,$3,$4,$5,$6)`,
		created.ID, created.PrinterID, created.Filename, fileBytes, sha, len(fileBytes)); err != nil {
		return PrintJob{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return PrintJob{}, err
	}
	return created, nil
}

// PeekStagedJob reports the staged job waiting for this member at this printer,
// if any, without changing anything. Used to show a "load" prompt after a tap.
func (s *Store) PeekStagedJob(ctx context.Context, printerID, memberID int64) (PrintJob, error) {
	j, err := scanJob(s.pool.QueryRow(ctx, `SELECT `+jobCols+`
		FROM print_jobs
		WHERE printer_id=$1 AND member_id=$2 AND status='staged'
		ORDER BY id DESC LIMIT 1`, printerID, memberID))
	if errors.Is(err, pgx.ErrNoRows) {
		return PrintJob{}, ErrNotFound
	}
	return j, err
}

// ClaimStagedJob is the fob-release step: the member tapped at the printer, so
// promote their staged job to 'printing', superseding whatever was printing
// before. The staged file bytes stay until the Pi confirms the start.
func (s *Store) ClaimStagedJob(ctx context.Context, printerID, memberID int64) (PrintJob, StagedMeta, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return PrintJob{}, StagedMeta{}, err
	}
	defer tx.Rollback(ctx)

	job, err := scanJob(tx.QueryRow(ctx, `SELECT `+jobCols+`
		FROM print_jobs
		WHERE printer_id=$1 AND member_id=$2 AND status='staged'
		ORDER BY id DESC LIMIT 1
		FOR UPDATE`, printerID, memberID))
	if errors.Is(err, pgx.ErrNoRows) {
		return PrintJob{}, StagedMeta{}, ErrNotFound
	}
	if err != nil {
		return PrintJob{}, StagedMeta{}, err
	}

	if _, err := tx.Exec(ctx, `
		UPDATE print_jobs SET status='ended', ended_at=now(), end_reason='superseded'
		WHERE printer_id=$1 AND status='printing'`, printerID); err != nil {
		return PrintJob{}, StagedMeta{}, err
	}
	// The print starts now, not at upload time — re-base the ETA off now.
	if _, err := tx.Exec(ctx, `
		UPDATE print_jobs
		   SET status='printing', started_at=now(),
		       estimated_complete_at = CASE WHEN estimated_seconds IS NOT NULL
		           THEN now() + make_interval(secs => estimated_seconds) ELSE NULL END
		 WHERE id=$1`, job.ID); err != nil {
		return PrintJob{}, StagedMeta{}, err
	}

	var meta StagedMeta
	meta.JobID = job.ID
	if err := tx.QueryRow(ctx,
		`SELECT filename, sha256, size_bytes FROM staged_files WHERE job_id=$1`, job.ID,
	).Scan(&meta.Filename, &meta.SHA256, &meta.SizeBytes); err != nil {
		return PrintJob{}, StagedMeta{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return PrintJob{}, StagedMeta{}, err
	}
	job.Status = "printing"
	return job, meta, nil
}

// GetJobFileBytes returns the staged file for a job (the Pi's download).
func (s *Store) GetJobFileBytes(ctx context.Context, jobID int64) ([]byte, string, error) {
	var b []byte
	var name string
	err := s.pool.QueryRow(ctx,
		`SELECT bytes, filename FROM staged_files WHERE job_id=$1`, jobID).Scan(&b, &name)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "", ErrNotFound
	}
	return b, name, err
}

// DiscardJobFile drops the staged bytes for a job once it's safely on the
// gadget. Idempotent.
func (s *Store) DiscardJobFile(ctx context.Context, jobID int64) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM staged_files WHERE job_id=$1`, jobID)
	return err
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
	if j.Status == "staged" {
		return "staged"
	}
	if j.EstimatedCompleteAt != nil && now.After(*j.EstimatedCompleteAt) {
		return "overdue"
	}
	return "printing"
}
