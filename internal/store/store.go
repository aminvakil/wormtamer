package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"time"

	"github.com/aminvakil/wormtamer/internal/review"
	_ "github.com/mattn/go-sqlite3"
)

const schemaVersion = 10

var (
	ErrUnsupportedSchemaVersion  = errors.New("database schema version is unsupported")
	ErrNonemptyVersionZeroSchema = errors.New("version 0 database schema is not empty")
)

const (
	OutcomeQueued                  = "queued"
	OutcomeFeedbackQueued          = "feedback_queued"
	OutcomeDuplicateReview         = "duplicate_review"
	OutcomeDuplicateFeedback       = "duplicate_feedback"
	OutcomeIgnoredDraft            = "ignored_draft"
	OutcomeIgnoredAction           = "ignored_action"
	OutcomeIgnoredFeedbackNoReview = "ignored_feedback_no_review"
)

const (
	JobQueued     = "queued"
	JobRunning    = "running"
	JobPublishing = "publishing"
	JobCompleted  = "completed"
	JobFailed     = "failed"
	JobObsolete   = "obsolete"
)

const (
	PatchIDUnknown     = "unknown"
	PatchIDPending     = "pending"
	PatchIDAvailable   = "available"
	PatchIDUnavailable = "unavailable"
)

const timestampLayout = time.RFC3339

type Store struct {
	db *sql.DB
}

type Event struct {
	DeliveryID      string
	GitLabInstance  string
	ProjectID       int64
	ProjectPath     string
	MergeRequestIID int64
	HeadSHA         string
	Action          string
	Payload         []byte
	QueueReview     bool
	QueueFeedback   bool
	TerminalState   string
	IgnoredOutcome  string
}

type AcceptResult struct {
	EventID           int64
	JobID             int64
	FeedbackJobID     int64
	Outcome           string
	DuplicateDelivery bool
}

type ReconciledReview struct {
	GitLabInstance  string
	ProjectID       int64
	MergeRequestIID int64
	HeadSHA         string
}

type ReconciledResult struct {
	JobID       int64
	NewlyQueued bool
}

type Job struct {
	ID                  int64
	GitLabInstance      string
	ProjectID           int64
	MergeRequestIID     int64
	HeadSHA             string
	State               string
	LeaseOwner          string
	LeaseExpiresAt      time.Time
	AttemptCount        int
	PatchIDStatus       string
	PatchIDSHA          string
	EquivalentToJobID   int64
	ValidatedResultJSON []byte
	FindingIDs          []string
}

type ReviewMemory struct {
	MemoryID  string
	Lesson    string
	SourceURL string
	UpdatedAt time.Time
}

type ReviewMemoryRetrieval struct {
	MemoryID        string
	MemoryUpdatedAt time.Time
	RetrievedAt     time.Time
}

func Open(ctx context.Context, path string) (*Store, error) {
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return nil, errors.New("resolve database path")
	}
	query := url.Values{
		"_busy_timeout": {"5000"},
		"_foreign_keys": {"on"},
		"_journal_mode": {"WAL"},
		"_synchronous":  {"FULL"},
	}
	dsn := (&url.URL{Scheme: "file", Path: absolutePath, RawQuery: query.Encode()}).String()
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, errors.New("open database")
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)

	store := &Store{db: db}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("connect to database: %w", err)
	}
	if err := store.verifySQLite(ctx); err != nil {
		db.Close()
		return nil, err
	}
	if err := store.applySchema(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) verifySQLite(ctx context.Context) error {
	var foreignKeys int
	if err := s.db.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
		return fmt.Errorf("verify SQLite foreign keys: %w", err)
	}
	if foreignKeys != 1 {
		return errors.New("SQLite foreign keys are not enabled")
	}

	var busyTimeout int
	if err := s.db.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&busyTimeout); err != nil {
		return fmt.Errorf("verify SQLite busy timeout: %w", err)
	}
	if busyTimeout != 5000 {
		return fmt.Errorf("unexpected SQLite busy timeout: %d", busyTimeout)
	}

	var journalMode string
	if err := s.db.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&journalMode); err != nil {
		return fmt.Errorf("verify SQLite journal mode: %w", err)
	}
	if journalMode != "wal" {
		return fmt.Errorf("unexpected SQLite journal mode: %s", journalMode)
	}

	return nil
}

func (s *Store) applySchema(ctx context.Context) error {
	var version int
	if err := s.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if version == schemaVersion {
		return nil
	}
	if version != 0 {
		return fmt.Errorf("%w: got version %d, expected version %d", ErrUnsupportedSchemaVersion, version, schemaVersion)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin schema initialization: %w", err)
	}
	defer tx.Rollback()

	var objectType, objectName string
	err = tx.QueryRowContext(ctx, `
SELECT type, name
FROM sqlite_schema
WHERE type IN ('table', 'index', 'view', 'trigger')
  AND name NOT GLOB 'sqlite_*'
ORDER BY name
LIMIT 1`).Scan(&objectType, &objectName)
	if err == nil {
		return fmt.Errorf("%w: contains %s %q", ErrNonemptyVersionZeroSchema, objectType, objectName)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("inspect version 0 database schema: %w", err)
	}

	if _, err := tx.ExecContext(ctx, currentSchema); err != nil {
		return fmt.Errorf("initialize database schema: %w", err)
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion)); err != nil {
		return fmt.Errorf("set schema version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit schema initialization: %w", err)
	}
	return nil
}

func (s *Store) AcceptEvent(ctx context.Context, event Event) (result AcceptResult, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return result, fmt.Errorf("begin event transaction: %w", err)
	}
	defer tx.Rollback()

	outcome := event.IgnoredOutcome
	if event.QueueReview {
		outcome = OutcomeQueued
	} else if event.QueueFeedback {
		outcome = OutcomeFeedbackQueued
	}
	insert, err := tx.ExecContext(ctx, `
INSERT INTO webhook_events (
    delivery_id, gitlab_instance, project_id, project_path,
    merge_request_iid, head_sha, action, outcome, payload_json
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(delivery_id) DO NOTHING`,
		event.DeliveryID, event.GitLabInstance, event.ProjectID, event.ProjectPath,
		event.MergeRequestIID, event.HeadSHA, event.Action, outcome, event.Payload)
	if err != nil {
		return result, fmt.Errorf("insert webhook event: %w", err)
	}
	inserted, err := insert.RowsAffected()
	if err != nil {
		return result, fmt.Errorf("inspect webhook event insertion: %w", err)
	}
	if inserted == 0 {
		if err := tx.QueryRowContext(ctx, `
SELECT e.id, e.outcome,
       CASE WHEN e.outcome IN (?, ?) THEN COALESCE(j.id, 0) ELSE 0 END,
       CASE WHEN e.outcome IN (?, ?) THEN COALESCE(f.id, 0) ELSE 0 END
FROM webhook_events e
LEFT JOIN review_jobs j ON
    j.gitlab_instance = e.gitlab_instance AND
    j.project_id = e.project_id AND
    j.merge_request_iid = e.merge_request_iid AND
    j.head_sha = e.head_sha
LEFT JOIN feedback_jobs f ON
    f.gitlab_instance = e.gitlab_instance AND
    f.project_id = e.project_id AND
    f.merge_request_iid = e.merge_request_iid
WHERE e.delivery_id = ?`, OutcomeQueued, OutcomeDuplicateReview,
			OutcomeFeedbackQueued, OutcomeDuplicateFeedback, event.DeliveryID).
			Scan(&result.EventID, &result.Outcome, &result.JobID, &result.FeedbackJobID); err != nil {
			return result, fmt.Errorf("read duplicate webhook event: %w", err)
		}
		result.DuplicateDelivery = true
		if err := tx.Commit(); err != nil {
			return AcceptResult{}, fmt.Errorf("commit duplicate event transaction: %w", err)
		}
		return result, nil
	}

	result.EventID, err = insert.LastInsertId()
	if err != nil {
		return result, fmt.Errorf("read webhook event ID: %w", err)
	}
	result.Outcome = outcome

	if event.QueueReview {
		jobInsert, err := tx.ExecContext(ctx, `
INSERT INTO review_jobs (
    source_event_id, gitlab_instance, project_id, merge_request_iid, head_sha, state
) VALUES (?, ?, ?, ?, ?, 'queued')
ON CONFLICT(gitlab_instance, project_id, merge_request_iid, head_sha) DO NOTHING`,
			result.EventID, event.GitLabInstance, event.ProjectID, event.MergeRequestIID, event.HeadSHA)
		if err != nil {
			return result, fmt.Errorf("insert review job: %w", err)
		}
		jobInserted, err := jobInsert.RowsAffected()
		if err != nil {
			return result, fmt.Errorf("inspect review job insertion: %w", err)
		}
		if jobInserted == 1 {
			result.JobID, err = jobInsert.LastInsertId()
			if err != nil {
				return result, fmt.Errorf("read review job ID: %w", err)
			}
		} else {
			result.Outcome = OutcomeDuplicateReview
			if _, err := tx.ExecContext(ctx, `UPDATE webhook_events SET outcome = ? WHERE id = ?`, result.Outcome, result.EventID); err != nil {
				return result, fmt.Errorf("mark duplicate review event: %w", err)
			}
			if err := tx.QueryRowContext(ctx, `
SELECT id FROM review_jobs
WHERE gitlab_instance = ? AND project_id = ? AND merge_request_iid = ? AND head_sha = ?`,
				event.GitLabInstance, event.ProjectID, event.MergeRequestIID, event.HeadSHA).Scan(&result.JobID); err != nil {
				return result, fmt.Errorf("read existing review job: %w", err)
			}
		}
	} else if event.QueueFeedback {
		var reviewJobID int64
		err := tx.QueryRowContext(ctx, `
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
			result.Outcome = OutcomeIgnoredFeedbackNoReview
			if _, err := tx.ExecContext(ctx, `UPDATE webhook_events SET outcome = ? WHERE id = ?`, result.Outcome, result.EventID); err != nil {
				return result, fmt.Errorf("ignore terminal event without review: %w", err)
			}
		} else if err != nil {
			return result, fmt.Errorf("select feedback review: %w", err)
		} else {
			now := formatTime(time.Now().UTC())
			jobInsert, err := tx.ExecContext(ctx, `
INSERT INTO feedback_jobs (
    source_event_id, review_job_id, gitlab_instance, project_id, project_path,
    merge_request_iid, head_sha, terminal_state, state, next_attempt_at,
    created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(gitlab_instance, project_id, merge_request_iid) DO NOTHING`,
				result.EventID, reviewJobID, event.GitLabInstance, event.ProjectID, event.ProjectPath,
				event.MergeRequestIID, event.HeadSHA, event.TerminalState, FeedbackQueued, now, now, now)
			if err != nil {
				return result, fmt.Errorf("insert feedback job: %w", err)
			}
			inserted, err := jobInsert.RowsAffected()
			if err != nil {
				return result, fmt.Errorf("inspect feedback job insertion: %w", err)
			}
			if inserted == 1 {
				result.FeedbackJobID, err = jobInsert.LastInsertId()
				if err != nil {
					return result, fmt.Errorf("read feedback job ID: %w", err)
				}
			} else {
				result.Outcome = OutcomeDuplicateFeedback
				if _, err := tx.ExecContext(ctx, `UPDATE webhook_events SET outcome = ? WHERE id = ?`, result.Outcome, result.EventID); err != nil {
					return result, fmt.Errorf("mark duplicate feedback event: %w", err)
				}
				if err := tx.QueryRowContext(ctx, `
SELECT id FROM feedback_jobs
WHERE gitlab_instance = ? AND project_id = ? AND merge_request_iid = ?`,
					event.GitLabInstance, event.ProjectID, event.MergeRequestIID).Scan(&result.FeedbackJobID); err != nil {
					return result, fmt.Errorf("read existing feedback job: %w", err)
				}
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return AcceptResult{}, fmt.Errorf("commit event transaction: %w", err)
	}
	return result, nil
}

func (s *Store) CreateReconciledJob(ctx context.Context, review ReconciledReview) (result ReconciledResult, err error) {
	if review.GitLabInstance == "" || review.ProjectID <= 0 || review.MergeRequestIID <= 0 || review.HeadSHA == "" {
		return result, errors.New("invalid reconciled review")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return result, fmt.Errorf("begin reconciled job transaction: %w", err)
	}
	defer tx.Rollback()

	insert, err := tx.ExecContext(ctx, `
INSERT INTO review_jobs (
    source_event_id, gitlab_instance, project_id, merge_request_iid, head_sha, state
) VALUES (NULL, ?, ?, ?, ?, 'queued')
ON CONFLICT(gitlab_instance, project_id, merge_request_iid, head_sha) DO NOTHING`,
		review.GitLabInstance, review.ProjectID, review.MergeRequestIID, review.HeadSHA)
	if err != nil {
		return result, fmt.Errorf("insert reconciled review job: %w", err)
	}
	inserted, err := insert.RowsAffected()
	if err != nil {
		return result, fmt.Errorf("inspect reconciled review job insertion: %w", err)
	}
	result.NewlyQueued = inserted == 1
	if result.NewlyQueued {
		result.JobID, err = insert.LastInsertId()
	} else {
		err = tx.QueryRowContext(ctx, `
SELECT id FROM review_jobs
WHERE gitlab_instance = ? AND project_id = ? AND merge_request_iid = ? AND head_sha = ?`,
			review.GitLabInstance, review.ProjectID, review.MergeRequestIID, review.HeadSHA).Scan(&result.JobID)
	}
	if err != nil {
		return ReconciledResult{}, fmt.Errorf("read reconciled review job ID: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return ReconciledResult{}, fmt.Errorf("commit reconciled job transaction: %w", err)
	}
	return result, nil
}

func (s *Store) ClaimJob(ctx context.Context, owner string, now time.Time, leaseDuration time.Duration, maxAttempts int) (*Job, error) {
	if owner == "" || leaseDuration <= 0 || maxAttempts <= 0 {
		return nil, errors.New("invalid job claim")
	}
	nowText := formatTime(now)
	leaseText := formatDeadline(now.Add(leaseDuration))

	if _, err := s.db.ExecContext(ctx, `
UPDATE review_jobs
SET state = ?, lease_owner = NULL, lease_expires_at = NULL,
    last_error_category = 'attempts_exhausted',
    last_error_message = 'job attempts exhausted', updated_at = ?
WHERE state IN (?, ?) AND attempt_count >= ?
  AND julianday(lease_expires_at) <= julianday(?)`,
		JobFailed, nowText, JobRunning, JobPublishing, maxAttempts, nowText); err != nil {
		return nil, fmt.Errorf("fail exhausted job: %w", err)
	}

	row := s.db.QueryRowContext(ctx, `
UPDATE review_jobs
SET state = CASE
        WHEN state = ? AND EXISTS (SELECT 1 FROM review_results WHERE job_id = review_jobs.id) THEN ?
        WHEN state = ? THEN ?
        ELSE state
    END,
    lease_owner = ?, lease_expires_at = ?, attempt_count = attempt_count + 1,
    started_at = ?, updated_at = ?
WHERE id = (
    SELECT id FROM review_jobs
    WHERE attempt_count < ? AND (
        (state = ? AND julianday(COALESCE(next_attempt_at, created_at)) <= julianday(?)) OR
        (state IN (?, ?) AND julianday(lease_expires_at) <= julianday(?))
    )
    ORDER BY COALESCE(next_attempt_at, created_at), id
    LIMIT 1
)
RETURNING id, gitlab_instance, project_id, merge_request_iid, head_sha,
          state, lease_owner, lease_expires_at, attempt_count,
          patch_id_status, COALESCE(patch_id_sha, ''), COALESCE(equivalent_to_job_id, 0)`,
		JobQueued, JobPublishing, JobQueued, JobRunning, owner, leaseText, nowText, nowText,
		maxAttempts, JobQueued, nowText, JobRunning, JobPublishing, nowText)

	job := &Job{}
	var leaseExpires string
	if err := row.Scan(&job.ID, &job.GitLabInstance, &job.ProjectID, &job.MergeRequestIID,
		&job.HeadSHA, &job.State, &job.LeaseOwner, &leaseExpires, &job.AttemptCount,
		&job.PatchIDStatus, &job.PatchIDSHA, &job.EquivalentToJobID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("claim review job: %w", err)
	}
	job.LeaseExpiresAt, _ = time.Parse(timestampLayout, leaseExpires)
	if err := s.db.QueryRowContext(ctx, `SELECT result_json FROM review_results WHERE job_id = ?`, job.ID).Scan(&job.ValidatedResultJSON); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("read claimed review result: %w", err)
	}
	if len(job.ValidatedResultJSON) > 0 {
		rows, err := s.db.QueryContext(ctx, `
SELECT finding_index, finding_id
FROM review_findings
WHERE job_id = ?
ORDER BY finding_index`, job.ID)
		if err != nil {
			return nil, fmt.Errorf("read claimed finding identifiers: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var index int
			var id string
			if err := rows.Scan(&index, &id); err != nil {
				return nil, fmt.Errorf("scan claimed finding identifier: %w", err)
			}
			if index != len(job.FindingIDs) || !review.ValidFindingID(id) {
				return nil, errors.New("invalid stored finding identifiers")
			}
			job.FindingIDs = append(job.FindingIDs, id)
		}
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("iterate claimed finding identifiers: %w", err)
		}
	}
	return job, nil
}

func (s *Store) RenewLease(ctx context.Context, jobID int64, owner string, now time.Time, leaseDuration time.Duration) (bool, error) {
	result, err := s.db.ExecContext(ctx, `
UPDATE review_jobs
SET lease_expires_at = ?, updated_at = ?
WHERE id = ? AND lease_owner = ? AND state IN (?, ?)
  AND julianday(lease_expires_at) > julianday(?)`,
		formatDeadline(now.Add(leaseDuration)), formatTime(now), jobID, owner,
		JobRunning, JobPublishing, formatTime(now))
	if err != nil {
		return false, fmt.Errorf("renew job lease: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("inspect lease renewal: %w", err)
	}
	return updated == 1, nil
}

func (s *Store) DeferPendingPatchID(ctx context.Context, jobID int64, owner string, now, nextAttempt time.Time) error {
	if jobID <= 0 || owner == "" || now.IsZero() || nextAttempt.Before(now) {
		return errors.New("invalid pending patch ID retry")
	}
	result, err := s.db.ExecContext(ctx, `
UPDATE review_jobs
SET state = ?, patch_id_status = ?, patch_id_sha = NULL,
    next_attempt_at = ?, lease_owner = NULL, lease_expires_at = NULL,
    last_error_category = 'merge_request_patch_id_pending',
    last_error_message = 'merge_request_patch_id_pending', updated_at = ?
WHERE id = ? AND state = ? AND lease_owner = ? AND patch_id_status = ?
  AND julianday(lease_expires_at) > julianday(?)`,
		JobQueued, PatchIDPending, formatDeadline(nextAttempt), formatTime(now),
		jobID, JobRunning, owner, PatchIDUnknown, formatTime(now))
	if err != nil {
		return fmt.Errorf("defer pending patch ID: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect pending patch ID retry: %w", err)
	}
	if updated != 1 {
		return ErrLeaseLost
	}
	return nil
}

func (s *Store) FindCanonicalReviewJob(ctx context.Context, jobID int64, patchIDSHA string) (int64, bool, error) {
	if jobID <= 0 || !validPatchIDSHA(patchIDSHA) {
		return 0, false, errors.New("invalid canonical review lookup")
	}
	var canonicalID int64
	err := s.db.QueryRowContext(ctx, `
SELECT candidate.id
FROM review_jobs source
JOIN review_jobs candidate ON
    candidate.gitlab_instance = source.gitlab_instance AND
    candidate.project_id = source.project_id AND
    candidate.merge_request_iid = source.merge_request_iid
JOIN review_results result ON result.job_id = candidate.id
JOIN publications publication ON publication.job_id = candidate.id
WHERE source.id = ? AND candidate.id != source.id
  AND candidate.state = ? AND candidate.patch_id_status = ?
  AND candidate.patch_id_sha = ? AND candidate.equivalent_to_job_id IS NULL
ORDER BY candidate.id DESC
LIMIT 1`, jobID, JobCompleted, PatchIDAvailable, patchIDSHA).Scan(&canonicalID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("find canonical review job: %w", err)
	}
	return canonicalID, true, nil
}

var errCanonicalReviewUnavailable = errors.New("canonical review unavailable")

func (s *Store) CompleteEquivalentReview(ctx context.Context, jobID int64, owner string, canonicalJobID int64, patchIDSHA string, now time.Time) error {
	if jobID <= 0 || owner == "" || canonicalJobID <= 0 || jobID == canonicalJobID ||
		now.IsZero() || !validPatchIDSHA(patchIDSHA) {
		return errors.New("invalid equivalent review completion")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin equivalent review transaction: %w", err)
	}
	defer tx.Rollback()

	var validSource int
	if err := tx.QueryRowContext(ctx, `
SELECT EXISTS(
    SELECT 1 FROM review_jobs source
    WHERE source.id = ? AND source.state = ? AND source.lease_owner = ?
      AND julianday(source.lease_expires_at) > julianday(?)
      AND source.patch_id_status IN (?, ?) AND source.patch_id_sha IS NULL
      AND source.equivalent_to_job_id IS NULL
      AND NOT EXISTS (SELECT 1 FROM review_results r WHERE r.job_id = source.id)
      AND NOT EXISTS (SELECT 1 FROM publications p WHERE p.job_id = source.id)
)`, jobID, JobRunning, owner, formatTime(now), PatchIDUnknown, PatchIDPending).Scan(&validSource); err != nil {
		return fmt.Errorf("validate equivalent review source: %w", err)
	}
	if validSource != 1 {
		return ErrLeaseLost
	}
	var validCanonical int
	if err := tx.QueryRowContext(ctx, `
SELECT EXISTS(
    SELECT 1
    FROM review_jobs source
    JOIN review_jobs canonical ON canonical.id = ?
    WHERE source.id = ? AND source.id != canonical.id
      AND source.gitlab_instance = canonical.gitlab_instance
      AND source.project_id = canonical.project_id
      AND source.merge_request_iid = canonical.merge_request_iid
      AND canonical.state = ? AND canonical.patch_id_status = ?
      AND canonical.patch_id_sha = ? AND canonical.equivalent_to_job_id IS NULL
      AND EXISTS (SELECT 1 FROM review_results r WHERE r.job_id = canonical.id)
      AND EXISTS (SELECT 1 FROM publications p WHERE p.job_id = canonical.id)
)`, canonicalJobID, jobID, JobCompleted, PatchIDAvailable, patchIDSHA).Scan(&validCanonical); err != nil {
		return fmt.Errorf("validate canonical review job: %w", err)
	}
	if validCanonical != 1 {
		return errCanonicalReviewUnavailable
	}
	result, err := tx.ExecContext(ctx, `
UPDATE review_jobs
SET state = ?, patch_id_status = ?, patch_id_sha = ?, equivalent_to_job_id = ?,
    lease_owner = NULL, lease_expires_at = NULL,
    last_error_category = NULL, last_error_message = NULL, updated_at = ?
WHERE id = ? AND state = ? AND lease_owner = ?
  AND julianday(lease_expires_at) > julianday(?)`,
		JobCompleted, PatchIDAvailable, patchIDSHA, canonicalJobID, formatTime(now),
		jobID, JobRunning, owner, formatTime(now))
	if err != nil {
		return fmt.Errorf("complete equivalent review job: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect equivalent review completion: %w", err)
	}
	if updated != 1 {
		return ErrLeaseLost
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit equivalent review transaction: %w", err)
	}
	return nil
}

func (s *Store) SaveReviewResult(ctx context.Context, jobID int64, owner string, resultJSON []byte, findingIDs []string, retrievals []ReviewMemoryRetrieval, patchIDStatus, patchIDSHA string, now time.Time) error {
	if len(resultJSON) == 0 || len(resultJSON) > 65536 || len(findingIDs) > 20 ||
		!validReviewPatchID(patchIDStatus, patchIDSHA) {
		return errors.New("invalid validated review result")
	}
	seenIDs := make(map[string]struct{}, len(findingIDs))
	for _, id := range findingIDs {
		if !review.ValidFindingID(id) {
			return errors.New("invalid finding identifier")
		}
		if _, exists := seenIDs[id]; exists {
			return errors.New("duplicate finding identifier")
		}
		seenIDs[id] = struct{}{}
	}
	seenRetrievals := make(map[string]struct{}, len(retrievals))
	for _, retrieval := range retrievals {
		key := retrieval.MemoryID + "\x00" + formatTime(retrieval.MemoryUpdatedAt)
		if !validMemoryID(retrieval.MemoryID) || retrieval.MemoryUpdatedAt.IsZero() || retrieval.RetrievedAt.IsZero() {
			return errors.New("invalid review memory retrieval")
		}
		if _, exists := seenRetrievals[key]; exists {
			return errors.New("duplicate review memory retrieval")
		}
		seenRetrievals[key] = struct{}{}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin review result transaction: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `
INSERT INTO review_results (job_id, result_json, created_at)
VALUES (?, ?, ?)
ON CONFLICT(job_id) DO NOTHING`, jobID, resultJSON, formatTime(now)); err != nil {
		return fmt.Errorf("store validated review result: %w", err)
	}
	for index, id := range findingIDs {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO review_findings (finding_id, job_id, finding_index)
VALUES (?, ?, ?)`, id, jobID, index); err != nil {
			return fmt.Errorf("store finding identifier: %w", err)
		}
	}
	for _, retrieval := range retrievals {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO review_memory_retrievals (job_id, memory_id, memory_updated_at, retrieved_at)
VALUES (?, ?, ?, ?)`, jobID, retrieval.MemoryID, formatTime(retrieval.MemoryUpdatedAt), formatTime(retrieval.RetrievedAt)); err != nil {
			return fmt.Errorf("store review memory retrieval: %w", err)
		}
	}
	update, err := tx.ExecContext(ctx, `
UPDATE review_jobs
SET state = ?, patch_id_status = ?, patch_id_sha = ?, updated_at = ?
WHERE id = ? AND state = ? AND lease_owner = ? AND equivalent_to_job_id IS NULL
  AND julianday(lease_expires_at) > julianday(?)`,
		JobPublishing, patchIDStatus, nullablePatchIDSHA(patchIDSHA), formatTime(now),
		jobID, JobRunning, owner, formatTime(now))
	if err != nil {
		return fmt.Errorf("checkpoint review result: %w", err)
	}
	updated, err := update.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect review result checkpoint: %w", err)
	}
	if updated != 1 {
		return ErrLeaseLost
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit review result transaction: %w", err)
	}
	return nil
}

func (s *Store) RetryJob(ctx context.Context, jobID int64, owner string, now, nextAttempt time.Time, maxAttempts int, category, message string) (string, error) {
	if !validFailure(category, message) || maxAttempts <= 0 || nextAttempt.Before(now) {
		return "", errors.New("invalid retry record")
	}
	row := s.db.QueryRowContext(ctx, `
UPDATE review_jobs
SET state = CASE WHEN attempt_count >= ? THEN ? ELSE ? END,
    next_attempt_at = CASE WHEN attempt_count >= ? THEN next_attempt_at ELSE ? END,
    lease_owner = NULL, lease_expires_at = NULL,
    last_error_category = ?, last_error_message = ?, updated_at = ?
WHERE id = ? AND lease_owner = ? AND state IN (?, ?)
  AND julianday(lease_expires_at) > julianday(?)
RETURNING state`,
		maxAttempts, JobFailed, JobQueued, maxAttempts, formatDeadline(nextAttempt),
		category, message, formatTime(now), jobID, owner, JobRunning, JobPublishing, formatTime(now))
	var state string
	if err := row.Scan(&state); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrLeaseLost
		}
		return "", fmt.Errorf("schedule job retry: %w", err)
	}
	return state, nil
}

func (s *Store) FinishJob(ctx context.Context, jobID int64, owner, state, category, message string, now time.Time) error {
	if state != JobFailed && state != JobObsolete {
		return errors.New("invalid terminal job state")
	}
	if !validFailure(category, message) {
		return errors.New("invalid terminal job error")
	}
	result, err := s.db.ExecContext(ctx, `
UPDATE review_jobs
SET state = ?, lease_owner = NULL, lease_expires_at = NULL,
    last_error_category = ?, last_error_message = ?, updated_at = ?
WHERE id = ? AND lease_owner = ? AND state IN (?, ?)
  AND julianday(lease_expires_at) > julianday(?)`,
		state, category, message, formatTime(now), jobID, owner,
		JobRunning, JobPublishing, formatTime(now))
	if err != nil {
		return fmt.Errorf("finish review job: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect finished review job: %w", err)
	}
	if updated != 1 {
		return ErrLeaseLost
	}
	return nil
}

// CompletePublication checkpoints either a generated result in publishing state or an
// existing external publication recovered by a running job without a local result.
func (s *Store) CompletePublication(ctx context.Context, jobID int64, owner, marker string, noteID int64, now time.Time) error {
	if marker == "" || len(marker) > 256 || noteID <= 0 {
		return errors.New("invalid publication record")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin publication transaction: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `
INSERT INTO publications (job_id, marker, gitlab_note_id, created_at)
VALUES (?, ?, ?, ?)
ON CONFLICT(job_id) DO UPDATE SET
    marker = excluded.marker, gitlab_note_id = excluded.gitlab_note_id`,
		jobID, marker, noteID, formatTime(now)); err != nil {
		return fmt.Errorf("store publication: %w", err)
	}
	result, err := tx.ExecContext(ctx, `
UPDATE review_jobs
SET patch_id_status = CASE WHEN state = ? THEN ? ELSE patch_id_status END,
    patch_id_sha = CASE WHEN state = ? THEN NULL ELSE patch_id_sha END,
    state = ?, lease_owner = NULL, lease_expires_at = NULL,
    last_error_category = NULL, last_error_message = NULL, updated_at = ?
WHERE id = ? AND lease_owner = ?
  AND julianday(lease_expires_at) > julianday(?)
  AND (
      (state = ? AND EXISTS (SELECT 1 FROM review_results WHERE job_id = review_jobs.id)) OR
      (state = ? AND NOT EXISTS (SELECT 1 FROM review_results WHERE job_id = review_jobs.id))
  )
  AND equivalent_to_job_id IS NULL`,
		JobRunning, PatchIDUnknown, JobRunning,
		JobCompleted, formatTime(now), jobID, owner, formatTime(now), JobPublishing, JobRunning)
	if err != nil {
		return fmt.Errorf("complete published review job: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect completed review job: %w", err)
	}
	if updated != 1 {
		return ErrLeaseLost
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit publication transaction: %w", err)
	}
	return nil
}

var ErrLeaseLost = errors.New("job lease lost")

func formatTime(value time.Time) string {
	return value.UTC().Truncate(time.Second).Format(timestampLayout)
}

func formatDeadline(value time.Time) string {
	value = value.UTC()
	if truncated := value.Truncate(time.Second); !value.Equal(truncated) {
		value = truncated.Add(time.Second)
	}
	return value.Format(timestampLayout)
}

func validFailure(category, message string) bool {
	return category != "" && len(category) <= 128 && len(message) <= 512
}

func validReviewPatchID(status, sha string) bool {
	switch status {
	case PatchIDAvailable:
		return validPatchIDSHA(sha)
	case PatchIDUnavailable:
		return sha == ""
	default:
		return false
	}
}

func validPatchIDSHA(sha string) bool {
	if len(sha) != 40 && len(sha) != 64 {
		return false
	}
	for _, character := range sha {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func nullablePatchIDSHA(sha string) any {
	if sha == "" {
		return nil
	}
	return sha
}
