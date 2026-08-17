package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aminvakil/wormtamer/internal/review"
)

const (
	FeedbackQueued    = "queued"
	FeedbackRunning   = "running"
	FeedbackCompleted = "completed"
	FeedbackFailed    = "failed"

	FeedbackOutcomeQueued        = "queued"
	FeedbackOutcomeIgnoredReview = "ignored_no_review"
	FeedbackOutcomeStale         = "stale_update"
)

type FeedbackEvent struct {
	DeliveryID      string
	GitLabInstance  string
	ProjectID       int64
	ProjectPath     string
	MergeRequestIID int64
	NoteID          int64
	ActorID         int64
	Action          string
	SourceUpdatedAt time.Time
}

type AcceptFeedbackResult struct {
	EventID           int64
	JobID             int64
	Outcome           string
	DuplicateDelivery bool
}

type FeedbackJob struct {
	ID                  int64
	SourceEventID       int64
	GitLabInstance      string
	ProjectID           int64
	ProjectPath         string
	MergeRequestIID     int64
	NoteID              int64
	ActorID             int64
	State               string
	LeaseOwner          string
	LeaseExpiresAt      time.Time
	AttemptCount        int
	ReviewJobID         int64
	ReviewTargetID      string
	HeadSHA             string
	ValidatedResultJSON []byte
	FindingIDs          []string
}

type FeedbackDecision struct {
	MemoryID   string
	TargetType string
	TargetID   string
	Outcome    string
	Confidence string
	Lesson     string
}

type FeedbackSource struct {
	JobID           int64
	GitLabInstance  string
	ProjectID       int64
	ProjectPath     string
	MergeRequestIID int64
	NoteID          int64
	ActorID         int64
	SourceUpdatedAt time.Time
}

var (
	ErrFeedbackSuperseded    = errors.New("feedback job superseded")
	ErrMemoryVersionConflict = errors.New("feedback memory version conflict")
)

func (s *Store) AcceptFeedbackEvent(ctx context.Context, event FeedbackEvent, now time.Time) (result AcceptFeedbackResult, err error) {
	if event.DeliveryID == "" || event.GitLabInstance == "" || event.ProjectID <= 0 || event.ProjectPath == "" ||
		event.MergeRequestIID <= 0 || event.NoteID <= 0 || event.ActorID <= 0 ||
		(event.Action != "create" && event.Action != "update") || event.SourceUpdatedAt.IsZero() {
		return result, errors.New("invalid feedback event")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return result, fmt.Errorf("begin feedback event transaction: %w", err)
	}
	defer tx.Rollback()

	insert, err := tx.ExecContext(ctx, `
INSERT INTO feedback_events (
    delivery_id, gitlab_instance, project_id, project_path, merge_request_iid,
    note_id, actor_id, action, source_updated_at, outcome
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(delivery_id) DO NOTHING`,
		event.DeliveryID, event.GitLabInstance, event.ProjectID, event.ProjectPath,
		event.MergeRequestIID, event.NoteID, event.ActorID, event.Action,
		formatTime(event.SourceUpdatedAt), FeedbackOutcomeQueued)
	if err != nil {
		return result, fmt.Errorf("insert feedback event: %w", err)
	}
	inserted, err := insert.RowsAffected()
	if err != nil {
		return result, fmt.Errorf("inspect feedback event insertion: %w", err)
	}
	if inserted == 0 {
		if err := tx.QueryRowContext(ctx, `
SELECT e.id, e.outcome, COALESCE(j.id, 0)
FROM feedback_events e
LEFT JOIN feedback_jobs j ON
    j.gitlab_instance = e.gitlab_instance AND
    j.project_id = e.project_id AND
    j.note_id = e.note_id
WHERE e.delivery_id = ?`, event.DeliveryID).Scan(&result.EventID, &result.Outcome, &result.JobID); err != nil {
			return result, fmt.Errorf("read duplicate feedback event: %w", err)
		}
		result.DuplicateDelivery = true
		if err := tx.Commit(); err != nil {
			return AcceptFeedbackResult{}, fmt.Errorf("commit duplicate feedback event: %w", err)
		}
		return result, nil
	}
	result.EventID, err = insert.LastInsertId()
	if err != nil {
		return result, fmt.Errorf("read feedback event ID: %w", err)
	}

	var currentJobID, reviewJobID int64
	var currentUpdatedAt string
	err = tx.QueryRowContext(ctx, `
SELECT j.id, j.review_job_id, e.source_updated_at
FROM feedback_jobs j
JOIN feedback_events e ON e.id = j.source_event_id
WHERE j.gitlab_instance = ? AND j.project_id = ? AND j.note_id = ?`,
		event.GitLabInstance, event.ProjectID, event.NoteID).Scan(&currentJobID, &reviewJobID, &currentUpdatedAt)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return result, fmt.Errorf("read current feedback job: %w", err)
	}
	newJob := errors.Is(err, sql.ErrNoRows)
	if err == nil && formatTime(event.SourceUpdatedAt) < currentUpdatedAt {
		result.JobID = currentJobID
		result.Outcome = FeedbackOutcomeStale
		if _, err := tx.ExecContext(ctx, `UPDATE feedback_events SET outcome = ? WHERE id = ?`, result.Outcome, result.EventID); err != nil {
			return result, fmt.Errorf("mark stale feedback event: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return AcceptFeedbackResult{}, fmt.Errorf("commit stale feedback event: %w", err)
		}
		return result, nil
	}
	if newJob {
		err = tx.QueryRowContext(ctx, `
SELECT effective.id
FROM review_jobs candidate
JOIN review_jobs effective ON effective.id = COALESCE(candidate.equivalent_to_job_id, candidate.id)
JOIN review_results r ON r.job_id = effective.id
JOIN publications p ON p.job_id = effective.id
WHERE candidate.gitlab_instance = ? AND candidate.project_id = ?
  AND candidate.merge_request_iid = ? AND candidate.state = ?
  AND effective.gitlab_instance = candidate.gitlab_instance
  AND effective.project_id = candidate.project_id
  AND effective.merge_request_iid = candidate.merge_request_iid
  AND effective.state = ? AND effective.equivalent_to_job_id IS NULL
ORDER BY candidate.id DESC
LIMIT 1`, event.GitLabInstance, event.ProjectID, event.MergeRequestIID,
			JobCompleted, JobCompleted).Scan(&reviewJobID)
		if errors.Is(err, sql.ErrNoRows) {
			result.Outcome = FeedbackOutcomeIgnoredReview
			if _, err := tx.ExecContext(ctx, `UPDATE feedback_events SET outcome = ? WHERE id = ?`, result.Outcome, result.EventID); err != nil {
				return result, fmt.Errorf("ignore feedback without review: %w", err)
			}
			if err := tx.Commit(); err != nil {
				return AcceptFeedbackResult{}, fmt.Errorf("commit ignored feedback event: %w", err)
			}
			return result, nil
		}
		if err != nil {
			return result, fmt.Errorf("select feedback review: %w", err)
		}
	}

	nowText := formatTime(now)
	if newJob {
		jobInsert, err := tx.ExecContext(ctx, `
INSERT INTO feedback_jobs (
    source_event_id, gitlab_instance, project_id, project_path, merge_request_iid,
    note_id, actor_id, state, next_attempt_at, updated_at, review_job_id
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			result.EventID, event.GitLabInstance, event.ProjectID, event.ProjectPath,
			event.MergeRequestIID, event.NoteID, event.ActorID, FeedbackQueued, nowText, nowText, reviewJobID)
		if err != nil {
			return result, fmt.Errorf("create feedback job: %w", err)
		}
		result.JobID, err = jobInsert.LastInsertId()
		if err != nil {
			return result, fmt.Errorf("read feedback job ID: %w", err)
		}
	} else {
		result.JobID = currentJobID
		if _, err := tx.ExecContext(ctx, `
UPDATE feedback_jobs
SET source_event_id = ?, project_path = ?, merge_request_iid = ?, actor_id = ?,
    state = CASE WHEN state = ? THEN state ELSE ? END,
    attempt_count = CASE WHEN state = ? THEN attempt_count ELSE 0 END,
    next_attempt_at = ?, last_error_category = NULL, updated_at = ?, next_source_check_at = NULL
WHERE id = ?`, result.EventID, event.ProjectPath, event.MergeRequestIID, event.ActorID,
			FeedbackRunning, FeedbackQueued, FeedbackRunning, nowText, nowText, currentJobID); err != nil {
			return result, fmt.Errorf("update feedback job: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE review_memories SET active = 0, updated_at = ? WHERE feedback_job_id = ? AND active = 1`, nowText, currentJobID); err != nil {
			return result, fmt.Errorf("deactivate superseded feedback memory: %w", err)
		}
	}
	result.Outcome = FeedbackOutcomeQueued
	if err := tx.Commit(); err != nil {
		return AcceptFeedbackResult{}, fmt.Errorf("commit feedback event transaction: %w", err)
	}
	return result, nil
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
RETURNING id, source_event_id, gitlab_instance, project_id, project_path,
          merge_request_iid, note_id, actor_id, state, lease_owner,
          lease_expires_at, attempt_count, review_job_id`,
		FeedbackRunning, owner, formatDeadline(now.Add(leaseDuration)), nowText,
		maxAttempts, FeedbackQueued, nowText, FeedbackRunning, nowText)
	job := &FeedbackJob{}
	var leaseExpires string
	if err := row.Scan(&job.ID, &job.SourceEventID, &job.GitLabInstance, &job.ProjectID,
		&job.ProjectPath, &job.MergeRequestIID, &job.NoteID, &job.ActorID, &job.State,
		&job.LeaseOwner, &leaseExpires, &job.AttemptCount, &job.ReviewJobID); err != nil {
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
WHERE j.id = ? AND j.gitlab_instance = ? AND j.project_id = ? AND j.merge_request_iid = ?`,
		job.ReviewJobID, job.GitLabInstance, job.ProjectID, job.MergeRequestIID).Scan(&job.HeadSHA, &job.ValidatedResultJSON); err != nil {
		return nil, fmt.Errorf("read feedback review context: %w", err)
	}
	job.ReviewTargetID = review.ReviewID(job.GitLabInstance, job.ProjectID, job.MergeRequestIID, job.HeadSHA)
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
	return updated == 1, err
}

func (s *Store) CompleteFeedbackJob(ctx context.Context, jobID, sourceEventID int64, owner string, accessLevel int, role, sourceURL string, decisions []FeedbackDecision, now time.Time, sourceCheckInterval time.Duration) error {
	if jobID <= 0 || sourceEventID <= 0 || owner == "" || accessLevel < 0 || accessLevel > 50 || role == "" ||
		sourceURL == "" || len(sourceURL) > 2048 || sourceCheckInterval <= 0 || !validFeedbackDecisions(decisions) {
		return errors.New("invalid feedback evaluation")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin feedback completion transaction: %w", err)
	}
	defer tx.Rollback()

	var currentEventID int64
	if err := tx.QueryRowContext(ctx, `
SELECT source_event_id FROM feedback_jobs
WHERE id = ? AND state = ? AND lease_owner = ?
  AND julianday(lease_expires_at) > julianday(?)`,
		jobID, FeedbackRunning, owner, formatTime(now)).Scan(&currentEventID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrLeaseLost
		}
		return fmt.Errorf("read feedback source version: %w", err)
	}
	if currentEventID != sourceEventID {
		if _, err := tx.ExecContext(ctx, `
UPDATE feedback_jobs
SET state = ?, lease_owner = NULL, lease_expires_at = NULL, attempt_count = 0,
    next_attempt_at = ?, updated_at = ?
WHERE id = ? AND state = ? AND lease_owner = ?`,
			FeedbackQueued, formatTime(now), formatTime(now), jobID, FeedbackRunning, owner); err != nil {
			return fmt.Errorf("requeue superseded feedback job: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit superseded feedback job: %w", err)
		}
		return ErrFeedbackSuperseded
	}

	var actorID, projectID, mergeRequestIID, reviewJobID int64
	var gitLabInstance, headSHA string
	if err := tx.QueryRowContext(ctx, `
SELECT f.actor_id, f.review_job_id, j.gitlab_instance, j.project_id, j.merge_request_iid, j.head_sha
FROM feedback_jobs f
JOIN review_jobs j ON j.id = f.review_job_id
WHERE f.id = ?`, jobID).Scan(&actorID, &reviewJobID, &gitLabInstance, &projectID, &mergeRequestIID, &headSHA); err != nil {
		return fmt.Errorf("read feedback target context: %w", err)
	}
	reviewTargetID := review.ReviewID(gitLabInstance, projectID, mergeRequestIID, headSHA)
	for _, decision := range decisions {
		if decision.TargetType == "review" {
			if decision.TargetID != reviewTargetID {
				return errors.New("invalid feedback review target")
			}
			continue
		}
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_findings WHERE job_id = ? AND finding_id = ?)`,
			reviewJobID, decision.TargetID).Scan(&exists); err != nil {
			return fmt.Errorf("validate feedback finding target: %w", err)
		}
		if exists != 1 {
			return errors.New("invalid feedback finding target")
		}
	}
	memoryUpdatedAt := formatTime(now)
	var versionConflict int
	if err := tx.QueryRowContext(ctx, `
SELECT EXISTS(
    SELECT 1 FROM review_memories
    WHERE feedback_job_id = ? AND updated_at = ?
)`, jobID, memoryUpdatedAt).Scan(&versionConflict); err != nil {
		return fmt.Errorf("check feedback memory version: %w", err)
	}
	if versionConflict == 1 {
		return ErrMemoryVersionConflict
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM review_memories WHERE feedback_job_id = ?`, jobID); err != nil {
		return fmt.Errorf("replace feedback memories: %w", err)
	}
	active := false
	for _, decision := range decisions {
		memoryActive := 0
		var lesson any
		if decision.Lesson != "" {
			memoryActive = 1
			lesson = decision.Lesson
			active = true
		}
		var findingID any
		if decision.TargetType == "finding" {
			findingID = decision.TargetID
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO review_memories (
    memory_id, feedback_job_id, target_type, target_id, finding_id, outcome,
    confidence, lesson, active, actor_id, actor_access_level, source_url, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			decision.MemoryID, jobID, decision.TargetType, decision.TargetID, findingID,
			decision.Outcome, decision.Confidence, lesson, memoryActive, actorID,
			accessLevel, sourceURL, memoryUpdatedAt); err != nil {
			return fmt.Errorf("store feedback decision: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO feedback_evaluations (
    job_id, source_event_id, actor_access_level, actor_role, source_url, evaluated_at
) VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(job_id) DO UPDATE SET
    source_event_id = excluded.source_event_id,
    actor_access_level = excluded.actor_access_level,
    actor_role = excluded.actor_role,
    source_url = excluded.source_url,
    evaluated_at = excluded.evaluated_at`,
		jobID, sourceEventID, accessLevel, role, sourceURL, formatTime(now)); err != nil {
		return fmt.Errorf("store feedback evaluation: %w", err)
	}
	var nextCheck any
	if active {
		nextCheck = formatDeadline(now.Add(sourceCheckInterval))
	}
	update, err := tx.ExecContext(ctx, `
UPDATE feedback_jobs
SET state = ?, lease_owner = NULL, lease_expires_at = NULL,
    last_error_category = NULL, updated_at = ?, next_source_check_at = ?
WHERE id = ? AND source_event_id = ? AND state = ? AND lease_owner = ?
  AND julianday(lease_expires_at) > julianday(?)`,
		FeedbackCompleted, formatTime(now), nextCheck, jobID, sourceEventID,
		FeedbackRunning, owner, formatTime(now))
	if err != nil {
		return fmt.Errorf("complete feedback job: %w", err)
	}
	updated, err := update.RowsAffected()
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

func (s *Store) RetryFeedbackJob(ctx context.Context, jobID, sourceEventID int64, owner string, now, nextAttempt time.Time, maxAttempts int, category string) (string, error) {
	if !validFailure(category, category) || maxAttempts <= 0 || nextAttempt.Before(now) {
		return "", errors.New("invalid feedback retry")
	}
	row := s.db.QueryRowContext(ctx, `
UPDATE feedback_jobs
SET state = CASE WHEN attempt_count >= ? THEN ? ELSE ? END,
    next_attempt_at = CASE WHEN attempt_count >= ? THEN next_attempt_at ELSE ? END,
    lease_owner = NULL, lease_expires_at = NULL, last_error_category = ?, updated_at = ?
WHERE id = ? AND source_event_id = ? AND state = ? AND lease_owner = ?
  AND julianday(lease_expires_at) > julianday(?)
RETURNING state`, maxAttempts, FeedbackFailed, FeedbackQueued, maxAttempts, formatDeadline(nextAttempt),
		category, formatTime(now), jobID, sourceEventID, FeedbackRunning, owner, formatTime(now))
	var state string
	if err := row.Scan(&state); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			if superseded, supersedeErr := s.requeueSupersededFeedback(ctx, jobID, sourceEventID, owner, now); supersedeErr != nil {
				return "", supersedeErr
			} else if superseded {
				return "", ErrFeedbackSuperseded
			}
			return "", ErrLeaseLost
		}
		return "", fmt.Errorf("schedule feedback retry: %w", err)
	}
	return state, nil
}

func (s *Store) FinishFeedbackJob(ctx context.Context, jobID, sourceEventID int64, owner, category string, now time.Time) error {
	if !validFailure(category, category) {
		return errors.New("invalid feedback failure")
	}
	result, err := s.db.ExecContext(ctx, `
UPDATE feedback_jobs
SET state = ?, lease_owner = NULL, lease_expires_at = NULL,
    last_error_category = ?, updated_at = ?
WHERE id = ? AND source_event_id = ? AND state = ? AND lease_owner = ?
  AND julianday(lease_expires_at) > julianday(?)`,
		FeedbackFailed, category, formatTime(now), jobID, sourceEventID, FeedbackRunning, owner, formatTime(now))
	if err != nil {
		return fmt.Errorf("finish feedback job: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect feedback failure: %w", err)
	}
	if updated != 1 {
		if superseded, err := s.requeueSupersededFeedback(ctx, jobID, sourceEventID, owner, now); err != nil {
			return err
		} else if superseded {
			return ErrFeedbackSuperseded
		}
		return ErrLeaseLost
	}
	return nil
}

func (s *Store) requeueSupersededFeedback(ctx context.Context, jobID, sourceEventID int64, owner string, now time.Time) (bool, error) {
	result, err := s.db.ExecContext(ctx, `
UPDATE feedback_jobs
SET state = ?, lease_owner = NULL, lease_expires_at = NULL, attempt_count = 0,
    next_attempt_at = ?, last_error_category = NULL, updated_at = ?
WHERE id = ? AND source_event_id != ? AND state = ? AND lease_owner = ?`,
		FeedbackQueued, formatTime(now), formatTime(now), jobID, sourceEventID, FeedbackRunning, owner)
	if err != nil {
		return false, fmt.Errorf("requeue superseded feedback job: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("inspect superseded feedback job: %w", err)
	}
	return updated == 1, nil
}

func (s *Store) DueFeedbackSources(ctx context.Context, now time.Time, limit int) ([]FeedbackSource, error) {
	if limit <= 0 || limit > 100 {
		return nil, errors.New("invalid feedback source limit")
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT j.id, j.gitlab_instance, j.project_id, j.project_path, j.merge_request_iid,
       j.note_id, j.actor_id, e.source_updated_at
FROM feedback_jobs j
JOIN feedback_events e ON e.id = j.source_event_id
WHERE j.state = ? AND j.next_source_check_at IS NOT NULL
  AND julianday(j.next_source_check_at) <= julianday(?)
  AND EXISTS (SELECT 1 FROM review_memories m WHERE m.feedback_job_id = j.id AND m.active = 1)
ORDER BY j.next_source_check_at, j.id
LIMIT ?`, FeedbackCompleted, formatTime(now), limit)
	if err != nil {
		return nil, fmt.Errorf("list due feedback sources: %w", err)
	}
	defer rows.Close()
	var sources []FeedbackSource
	for rows.Next() {
		var source FeedbackSource
		var updated string
		if err := rows.Scan(&source.JobID, &source.GitLabInstance, &source.ProjectID, &source.ProjectPath,
			&source.MergeRequestIID, &source.NoteID, &source.ActorID, &updated); err != nil {
			return nil, fmt.Errorf("scan feedback source: %w", err)
		}
		source.SourceUpdatedAt, _ = time.Parse(timestampLayout, updated)
		sources = append(sources, source)
	}
	return sources, rows.Err()
}

func (s *Store) ReconcileFeedbackSource(ctx context.Context, jobID int64, exists bool, sourceUpdatedAt, now time.Time, interval time.Duration) error {
	if jobID <= 0 || interval <= 0 || (exists && sourceUpdatedAt.IsZero()) {
		return errors.New("invalid feedback source reconciliation")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin feedback source reconciliation: %w", err)
	}
	defer tx.Rollback()

	if exists {
		var storedUpdatedAt string
		if err := tx.QueryRowContext(ctx, `
SELECT e.source_updated_at
FROM feedback_jobs j
JOIN feedback_events e ON e.id = j.source_event_id
WHERE j.id = ?`, jobID).Scan(&storedUpdatedAt); err != nil {
			return fmt.Errorf("read feedback source timestamp: %w", err)
		}
		if storedUpdatedAt == formatTime(sourceUpdatedAt) {
			if _, err := tx.ExecContext(ctx, `UPDATE feedback_jobs SET next_source_check_at = ?, updated_at = ? WHERE id = ?`, formatDeadline(now.Add(interval)), formatTime(now), jobID); err != nil {
				return fmt.Errorf("schedule feedback source check: %w", err)
			}
			if err := tx.Commit(); err != nil {
				return fmt.Errorf("commit feedback source check: %w", err)
			}
			return nil
		}
	}

	if _, err := tx.ExecContext(ctx, `UPDATE review_memories SET active = 0, updated_at = ? WHERE feedback_job_id = ? AND active = 1`, formatTime(now), jobID); err != nil {
		return fmt.Errorf("deactivate feedback memory: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE feedback_jobs SET next_source_check_at = NULL, updated_at = ? WHERE id = ?`, formatTime(now), jobID); err != nil {
		return fmt.Errorf("stop feedback source checks: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit feedback source deactivation: %w", err)
	}
	return nil
}

func validFeedbackDecisions(decisions []FeedbackDecision) bool {
	if len(decisions) > 21 {
		return false
	}
	seen := make(map[string]struct{}, len(decisions))
	for _, decision := range decisions {
		if !validMemoryID(decision.MemoryID) || len(decision.Lesson) > 4096 || strings.ContainsRune(decision.Lesson, '\x00') {
			return false
		}
		key := decision.TargetType + "\x00" + decision.TargetID
		if _, exists := seen[key]; exists {
			return false
		}
		seen[key] = struct{}{}
		switch decision.TargetType {
		case "review":
			if !review.ValidReviewID(decision.TargetID) || (decision.Outcome != "supports_review" && decision.Outcome != "rejects_review" && decision.Outcome != "corrects_review") {
				return false
			}
		case "finding":
			if !review.ValidFindingID(decision.TargetID) || (decision.Outcome != "supports_finding" && decision.Outcome != "rejects_finding" && decision.Outcome != "corrects_finding") {
				return false
			}
		default:
			return false
		}
		switch decision.Confidence {
		case "low", "medium", "high":
		default:
			return false
		}
	}
	return true
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
