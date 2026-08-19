package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	FeedbackQueued    = "queued"
	FeedbackRunning   = "running"
	FeedbackCompleted = "completed"
	FeedbackFailed    = "failed"
)

type FeedbackJob struct {
	ID                  int64
	ReviewJobID         int64
	GitLabInstance      string
	ProjectID           int64
	ProjectPath         string
	MergeRequestIID     int64
	HeadSHA             string
	TerminalState       string
	State               string
	LeaseOwner          string
	LeaseExpiresAt      time.Time
	AttemptCount        int
	ReviewHeadSHA       string
	ValidatedResultJSON []byte
	FindingIDs          []string
}

func (s *Store) ClaimFeedbackJob(ctx context.Context, owner string, now time.Time, leaseDuration time.Duration, maxAttempts int) (*FeedbackJob, error) {
	if owner == "" || leaseDuration <= 0 || maxAttempts <= 0 {
		return nil, errors.New("invalid feedback job claim")
	}
	nowText := formatTime(now)
	if _, err := s.db.ExecContext(ctx, `
UPDATE feedback_jobs
SET state = ?, lease_owner = NULL, lease_expires_at = NULL,
    last_error_category = 'attempts_exhausted', updated_at = ?
WHERE state = ? AND attempt_count >= ? AND julianday(lease_expires_at) <= julianday(?)`,
		FeedbackFailed, nowText, FeedbackRunning, maxAttempts, nowText); err != nil {
		return nil, fmt.Errorf("fail exhausted feedback job: %w", err)
	}

	row := s.db.QueryRowContext(ctx, `
UPDATE feedback_jobs
SET state = ?, lease_owner = ?, lease_expires_at = ?,
    attempt_count = attempt_count + 1, updated_at = ?
WHERE id = (
    SELECT id FROM feedback_jobs
    WHERE attempt_count < ? AND (
        (state = ? AND julianday(next_attempt_at) <= julianday(?)) OR
        (state = ? AND julianday(lease_expires_at) <= julianday(?))
    )
    ORDER BY next_attempt_at, id
    LIMIT 1
)
RETURNING id, review_job_id, gitlab_instance, project_id,
          project_path, merge_request_iid, head_sha, terminal_state, state,
          lease_owner, lease_expires_at, attempt_count`,
		FeedbackRunning, owner, formatDeadline(now.Add(leaseDuration)), nowText,
		maxAttempts, FeedbackQueued, nowText, FeedbackRunning, nowText)
	job := &FeedbackJob{}
	var leaseExpires string
	if err := row.Scan(&job.ID, &job.ReviewJobID, &job.GitLabInstance,
		&job.ProjectID, &job.ProjectPath, &job.MergeRequestIID, &job.HeadSHA,
		&job.TerminalState, &job.State, &job.LeaseOwner, &leaseExpires, &job.AttemptCount); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("claim feedback job: %w", err)
	}
	job.LeaseExpiresAt, _ = time.Parse(timestampLayout, leaseExpires)
	if err := s.db.QueryRowContext(ctx, `
SELECT j.head_sha, r.result_json
FROM review_jobs j
JOIN review_results r ON r.job_id = j.id
JOIN publications p ON p.job_id = j.id
WHERE j.id = ? AND j.gitlab_instance = ? AND j.project_id = ? AND j.merge_request_iid = ?
  AND j.state = ? AND j.equivalent_to_job_id IS NULL`,
		job.ReviewJobID, job.GitLabInstance, job.ProjectID, job.MergeRequestIID,
		JobCompleted).Scan(&job.ReviewHeadSHA, &job.ValidatedResultJSON); err != nil {
		return nil, fmt.Errorf("read feedback review context: %w", err)
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT finding_id
FROM review_findings
WHERE job_id = ?
ORDER BY finding_index`, job.ReviewJobID)
	if err != nil {
		return nil, fmt.Errorf("read feedback finding identifiers: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan feedback finding identifier: %w", err)
		}
		job.FindingIDs = append(job.FindingIDs, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate feedback finding identifiers: %w", err)
	}
	return job, nil
}

func (s *Store) RenewFeedbackLease(ctx context.Context, jobID int64, owner string, now time.Time, leaseDuration time.Duration) (bool, error) {
	result, err := s.db.ExecContext(ctx, `
UPDATE feedback_jobs SET lease_expires_at = ?, updated_at = ?
WHERE id = ? AND lease_owner = ? AND state = ?
  AND julianday(lease_expires_at) > julianday(?)`,
		formatDeadline(now.Add(leaseDuration)), formatTime(now), jobID, owner, FeedbackRunning, formatTime(now))
	if err != nil {
		return false, fmt.Errorf("renew feedback lease: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("inspect feedback lease renewal: %w", err)
	}
	return updated == 1, nil
}

func (s *Store) CompleteFeedbackJob(ctx context.Context, jobID int64, owner, memoryID, lesson, sourceURL string, now time.Time) error {
	if jobID <= 0 || owner == "" || sourceURL == "" || len(sourceURL) > 2048 ||
		((memoryID == "") != (lesson == "")) || (lesson != "" && (!validMemoryID(memoryID) || len(lesson) > 4096 || strings.ContainsRune(lesson, '\x00'))) {
		return errors.New("invalid feedback completion")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin feedback completion transaction: %w", err)
	}
	defer tx.Rollback()

	if lesson != "" {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO review_memories (memory_id, feedback_job_id, lesson, source_url, created_at)
VALUES (?, ?, ?, ?, ?)`, memoryID, jobID, lesson, sourceURL, formatTime(now)); err != nil {
			return fmt.Errorf("store review memory: %w", err)
		}
	}
	result, err := tx.ExecContext(ctx, `
UPDATE feedback_jobs
SET state = ?, lease_owner = NULL, lease_expires_at = NULL,
    last_error_category = NULL, updated_at = ?
WHERE id = ? AND state = ? AND lease_owner = ?
  AND julianday(lease_expires_at) > julianday(?)`,
		FeedbackCompleted, formatTime(now), jobID, FeedbackRunning, owner, formatTime(now))
	if err != nil {
		return fmt.Errorf("complete feedback job: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect completed feedback job: %w", err)
	}
	if updated != 1 {
		return ErrLeaseLost
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit feedback completion: %w", err)
	}
	return nil
}

func (s *Store) RetryFeedbackJob(ctx context.Context, jobID int64, owner string, now, nextAttempt time.Time, maxAttempts int, category string) (string, error) {
	if !validFailure(category, category) || maxAttempts <= 0 || nextAttempt.Before(now) {
		return "", errors.New("invalid feedback retry")
	}
	row := s.db.QueryRowContext(ctx, `
UPDATE feedback_jobs
SET state = CASE WHEN attempt_count >= ? THEN ? ELSE ? END,
    next_attempt_at = CASE WHEN attempt_count >= ? THEN next_attempt_at ELSE ? END,
    lease_owner = NULL, lease_expires_at = NULL, last_error_category = ?, updated_at = ?
WHERE id = ? AND state = ? AND lease_owner = ?
  AND julianday(lease_expires_at) > julianday(?)
RETURNING state`, maxAttempts, FeedbackFailed, FeedbackQueued, maxAttempts, formatDeadline(nextAttempt),
		category, formatTime(now), jobID, FeedbackRunning, owner, formatTime(now))
	var state string
	if err := row.Scan(&state); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrLeaseLost
		}
		return "", fmt.Errorf("schedule feedback retry: %w", err)
	}
	return state, nil
}

func (s *Store) FinishFeedbackJob(ctx context.Context, jobID int64, owner, category string, now time.Time) error {
	if !validFailure(category, category) {
		return errors.New("invalid feedback failure")
	}
	result, err := s.db.ExecContext(ctx, `
UPDATE feedback_jobs
SET state = ?, lease_owner = NULL, lease_expires_at = NULL,
    last_error_category = ?, updated_at = ?
WHERE id = ? AND state = ? AND lease_owner = ?
  AND julianday(lease_expires_at) > julianday(?)`,
		FeedbackFailed, category, formatTime(now), jobID, FeedbackRunning, owner, formatTime(now))
	if err != nil {
		return fmt.Errorf("finish feedback job: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect feedback failure: %w", err)
	}
	if updated != 1 {
		return ErrLeaseLost
	}
	return nil
}

func validMemoryID(value string) bool {
	if len(value) != 31 || !strings.HasPrefix(value, "WT-M-") {
		return false
	}
	for _, character := range value[5:] {
		if (character < 'A' || character > 'Z') && (character < '2' || character > '7') {
			return false
		}
	}
	return true
}
