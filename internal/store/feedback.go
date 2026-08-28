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
	AttemptCount        int
	ReviewHeadSHA       string
	ValidatedResultJSON []byte
	FindingIDs          []string
}

func (s *Store) ClaimFeedbackJob(ctx context.Context, now time.Time) (*FeedbackJob, error) {
	if now.IsZero() {
		return nil, errors.New("invalid feedback job claim")
	}
	nowText := formatTime(now)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin feedback job claim: %w", err)
	}
	defer tx.Rollback()

	row := tx.QueryRowContext(ctx, `
UPDATE feedback_jobs
SET state = ?, attempt_count = attempt_count + 1, updated_at = ?
WHERE id = (
    SELECT id FROM feedback_jobs
    WHERE state = ? AND attempt_count < ?
      AND julianday(next_attempt_at) <= julianday(?)
    ORDER BY next_attempt_at, id
    LIMIT 1
)
RETURNING id, review_job_id, gitlab_instance, project_id,
          project_path, merge_request_iid, head_sha, terminal_state, state,
          attempt_count`,
		FeedbackRunning, nowText, FeedbackQueued, MaxJobAttempts, nowText)
	job := &FeedbackJob{}
	if err := row.Scan(&job.ID, &job.ReviewJobID, &job.GitLabInstance,
		&job.ProjectID, &job.ProjectPath, &job.MergeRequestIID, &job.HeadSHA,
		&job.TerminalState, &job.State, &job.AttemptCount); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("claim feedback job: %w", err)
	}
	if err := tx.QueryRowContext(ctx, `
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
	rows, err := tx.QueryContext(ctx, `
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
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close feedback finding identifiers: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit feedback job claim: %w", err)
	}
	return job, nil
}

func (s *Store) CompleteFeedbackJob(ctx context.Context, jobID int64, memoryID, lesson, sourceURL string, now time.Time) error {
	if jobID <= 0 || now.IsZero() || sourceURL == "" || len(sourceURL) > 2048 ||
		((memoryID == "") != (lesson == "")) || (lesson != "" && (!validMemoryID(memoryID) || len(lesson) > 4096 || strings.ContainsRune(lesson, '\x00'))) {
		return errors.New("invalid feedback completion")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin feedback completion transaction: %w", err)
	}
	defer tx.Rollback()

	result, err := tx.ExecContext(ctx, `
UPDATE feedback_jobs
SET state = ?, last_error_category = NULL, updated_at = ?
WHERE id = ? AND state = ?`,
		FeedbackCompleted, formatTime(now), jobID, FeedbackRunning)
	if err != nil {
		return fmt.Errorf("complete feedback job: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect completed feedback job: %w", err)
	}
	if updated != 1 {
		return ErrJobNotRunning
	}
	if lesson != "" {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO review_memories (memory_id, feedback_job_id, lesson, source_url, created_at)
VALUES (?, ?, ?, ?, ?)`, memoryID, jobID, lesson, sourceURL, formatTime(now)); err != nil {
			return fmt.Errorf("store review memory: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit feedback completion: %w", err)
	}
	return nil
}

func (s *Store) RetryFeedbackJob(ctx context.Context, jobID int64, now, nextAttempt time.Time, category string) (string, error) {
	if jobID <= 0 || now.IsZero() || !validFailure(category, category) || nextAttempt.Before(now) {
		return "", errors.New("invalid feedback retry")
	}
	row := s.db.QueryRowContext(ctx, `
UPDATE feedback_jobs
SET state = CASE WHEN attempt_count >= ? THEN ? ELSE ? END,
    next_attempt_at = CASE WHEN attempt_count >= ? THEN next_attempt_at ELSE ? END,
    last_error_category = ?, updated_at = ?
WHERE id = ? AND state = ?
RETURNING state`, MaxJobAttempts, FeedbackFailed, FeedbackQueued, MaxJobAttempts, formatDeadline(nextAttempt),
		category, formatTime(now), jobID, FeedbackRunning)
	var state string
	if err := row.Scan(&state); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrJobNotRunning
		}
		return "", fmt.Errorf("schedule feedback retry: %w", err)
	}
	return state, nil
}

func (s *Store) FinishFeedbackJob(ctx context.Context, jobID int64, category string, now time.Time) error {
	if jobID <= 0 || now.IsZero() || !validFailure(category, category) {
		return errors.New("invalid feedback failure")
	}
	result, err := s.db.ExecContext(ctx, `
UPDATE feedback_jobs
SET state = ?, last_error_category = ?, updated_at = ?
WHERE id = ? AND state = ?`,
		FeedbackFailed, category, formatTime(now), jobID, FeedbackRunning)
	if err != nil {
		return fmt.Errorf("finish feedback job: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect feedback failure: %w", err)
	}
	if updated != 1 {
		return ErrJobNotRunning
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
