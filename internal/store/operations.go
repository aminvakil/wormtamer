package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

const (
	FailedJobKindReview   = "review"
	FailedJobKindFeedback = "feedback"
)

var (
	ErrJobNotFound  = errors.New("job not found")
	ErrJobNotFailed = errors.New("job is not failed")
)

type FailedJob struct {
	Kind              string
	JobID             int64
	AttemptCount      int
	LastErrorCategory string
	UpdatedAt         string
	ProjectID         int64
	MergeRequestIID   int64
	HeadSHA           string
}

func (s *Store) ListFailedJobs(ctx context.Context, limit int) ([]FailedJob, bool, error) {
	if limit <= 0 || limit > 100 {
		return nil, false, errors.New("invalid failed job limit")
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT kind, job_id, attempt_count, last_error_category, updated_at,
       project_id, merge_request_iid, head_sha
FROM (
    SELECT 'review' AS kind, id AS job_id, attempt_count, last_error_category,
           updated_at, project_id, merge_request_iid, head_sha
    FROM review_jobs
    WHERE state = 'failed'

    UNION ALL

    SELECT 'feedback' AS kind, id AS job_id, attempt_count, last_error_category,
           updated_at, project_id, merge_request_iid, head_sha
    FROM feedback_jobs
    WHERE state = 'failed'
)
ORDER BY updated_at DESC, kind, job_id DESC
LIMIT ?`, limit+1)
	if err != nil {
		return nil, false, fmt.Errorf("list failed jobs: %w", err)
	}
	defer rows.Close()

	jobs := make([]FailedJob, 0, limit)
	for rows.Next() {
		var job FailedJob
		var headSHA sql.NullString
		if err := rows.Scan(&job.Kind, &job.JobID, &job.AttemptCount, &job.LastErrorCategory,
			&job.UpdatedAt, &job.ProjectID, &job.MergeRequestIID, &headSHA); err != nil {
			return nil, false, fmt.Errorf("scan failed job: %w", err)
		}
		job.HeadSHA = headSHA.String
		jobs = append(jobs, job)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("iterate failed jobs: %w", err)
	}
	if len(jobs) > limit {
		return jobs[:limit], true, nil
	}
	return jobs, false, nil
}

func (s *Store) RetryFailedReviewJob(ctx context.Context, jobID int64, now time.Time) error {
	if jobID <= 0 || now.IsZero() {
		return errors.New("invalid failed review retry")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin failed review retry: %w", err)
	}
	defer tx.Rollback()

	result, err := tx.ExecContext(ctx, `
UPDATE review_jobs
SET state = ?, attempt_count = 0, next_attempt_at = ?,
    lease_owner = NULL, lease_expires_at = NULL,
    last_error_category = NULL, last_error_message = NULL, updated_at = ?
WHERE id = ? AND state = ?`,
		JobQueued, formatTime(now), formatTime(now), jobID, JobFailed)
	if err != nil {
		return fmt.Errorf("retry failed review job: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect failed review retry: %w", err)
	}
	if updated == 0 {
		return retryMiss(ctx, tx, "review_jobs", jobID)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit failed review retry: %w", err)
	}
	return nil
}

func (s *Store) RetryFailedFeedbackJob(ctx context.Context, jobID int64, now time.Time) error {
	if jobID <= 0 || now.IsZero() {
		return errors.New("invalid failed feedback retry")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin failed feedback retry: %w", err)
	}
	defer tx.Rollback()

	result, err := tx.ExecContext(ctx, `
UPDATE feedback_jobs
SET state = ?, attempt_count = 0, next_attempt_at = ?,
    lease_owner = NULL, lease_expires_at = NULL,
    last_error_category = NULL, updated_at = ?
WHERE id = ? AND state = ?`,
		FeedbackQueued, formatTime(now), formatTime(now), jobID, FeedbackFailed)
	if err != nil {
		return fmt.Errorf("retry failed feedback job: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect failed feedback retry: %w", err)
	}
	if updated == 0 {
		return retryMiss(ctx, tx, "feedback_jobs", jobID)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit failed feedback retry: %w", err)
	}
	return nil
}

func retryMiss(ctx context.Context, tx *sql.Tx, table string, jobID int64) error {
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM `+table+` WHERE id = ?)`, jobID).Scan(&exists); err != nil {
		return fmt.Errorf("inspect retry target: %w", err)
	}
	if exists == 0 {
		return ErrJobNotFound
	}
	return ErrJobNotFailed
}
