package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

const schemaVersion = 3

const (
	OutcomeQueued          = "queued"
	OutcomeDuplicateReview = "duplicate_review"
	OutcomeIgnoredDraft    = "ignored_draft"
	OutcomeIgnoredAction   = "ignored_action"
)

const (
	JobQueued     = "queued"
	JobRunning    = "running"
	JobPublishing = "publishing"
	JobCompleted  = "completed"
	JobFailed     = "failed"
	JobObsolete   = "obsolete"
)

const timestampLayout = "2006-01-02T15:04:05.000000000Z"

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
	IgnoredOutcome  string
}

type AcceptResult struct {
	EventID           int64
	JobID             int64
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
	ValidatedResultJSON []byte
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
	if version > schemaVersion {
		return fmt.Errorf("database schema version %d is newer than supported version %d", version, schemaVersion)
	}

	for version < schemaVersion {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin schema transaction: %w", err)
		}

		var migration string
		switch version {
		case 0:
			migration = `
CREATE TABLE webhook_events (
    id INTEGER PRIMARY KEY,
    delivery_id TEXT NOT NULL UNIQUE,
    gitlab_instance TEXT NOT NULL,
    project_id INTEGER NOT NULL,
    project_path TEXT NOT NULL,
    merge_request_iid INTEGER NOT NULL,
    head_sha TEXT NOT NULL,
    action TEXT NOT NULL,
    outcome TEXT NOT NULL,
    payload_json BLOB NOT NULL CHECK(length(payload_json) <= 1048576),
    received_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE TABLE review_jobs (
    id INTEGER PRIMARY KEY,
    source_event_id INTEGER NOT NULL REFERENCES webhook_events(id),
    gitlab_instance TEXT NOT NULL,
    project_id INTEGER NOT NULL,
    merge_request_iid INTEGER NOT NULL,
    head_sha TEXT NOT NULL,
    state TEXT NOT NULL DEFAULT 'queued',
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    UNIQUE (gitlab_instance, project_id, merge_request_iid, head_sha)
);

PRAGMA user_version = 1;
`
		case 1:
			migration = `
ALTER TABLE review_jobs ADD COLUMN lease_owner TEXT;
ALTER TABLE review_jobs ADD COLUMN lease_expires_at TEXT;
ALTER TABLE review_jobs ADD COLUMN attempt_count INTEGER NOT NULL DEFAULT 0;
ALTER TABLE review_jobs ADD COLUMN started_at TEXT;
ALTER TABLE review_jobs ADD COLUMN next_attempt_at TEXT;
ALTER TABLE review_jobs ADD COLUMN last_error_category TEXT;
ALTER TABLE review_jobs ADD COLUMN last_error_message TEXT;
ALTER TABLE review_jobs ADD COLUMN updated_at TEXT;

UPDATE review_jobs
SET next_attempt_at = created_at, updated_at = created_at;

CREATE INDEX review_jobs_due_idx
ON review_jobs (state, next_attempt_at, lease_expires_at);

CREATE TABLE review_results (
    job_id INTEGER PRIMARY KEY REFERENCES review_jobs(id) ON DELETE CASCADE,
    result_json BLOB NOT NULL CHECK(length(result_json) <= 65536),
    created_at TEXT NOT NULL
);

CREATE TABLE publications (
    job_id INTEGER PRIMARY KEY REFERENCES review_jobs(id) ON DELETE CASCADE,
    marker TEXT NOT NULL UNIQUE CHECK(length(marker) <= 256),
    gitlab_note_id INTEGER NOT NULL CHECK(gitlab_note_id > 0),
    created_at TEXT NOT NULL
);

PRAGMA user_version = 2;
`
		case 2:
			migration = `
CREATE TABLE review_jobs_v3 (
    id INTEGER PRIMARY KEY,
    source_event_id INTEGER REFERENCES webhook_events(id),
    gitlab_instance TEXT NOT NULL,
    project_id INTEGER NOT NULL,
    merge_request_iid INTEGER NOT NULL,
    head_sha TEXT NOT NULL,
    state TEXT NOT NULL DEFAULT 'queued',
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    lease_owner TEXT,
    lease_expires_at TEXT,
    attempt_count INTEGER NOT NULL DEFAULT 0,
    started_at TEXT,
    next_attempt_at TEXT,
    last_error_category TEXT,
    last_error_message TEXT,
    updated_at TEXT,
    UNIQUE (gitlab_instance, project_id, merge_request_iid, head_sha)
);

CREATE TABLE review_results_v3 (
    job_id INTEGER PRIMARY KEY REFERENCES review_jobs_v3(id) ON DELETE CASCADE,
    result_json BLOB NOT NULL CHECK(length(result_json) <= 65536),
    created_at TEXT NOT NULL
);

CREATE TABLE publications_v3 (
    job_id INTEGER PRIMARY KEY REFERENCES review_jobs_v3(id) ON DELETE CASCADE,
    marker TEXT NOT NULL UNIQUE CHECK(length(marker) <= 256),
    gitlab_note_id INTEGER NOT NULL CHECK(gitlab_note_id > 0),
    created_at TEXT NOT NULL
);

INSERT INTO review_jobs_v3
SELECT id, source_event_id, gitlab_instance, project_id, merge_request_iid,
       head_sha, state, created_at, lease_owner, lease_expires_at, attempt_count,
       started_at, next_attempt_at, last_error_category, last_error_message, updated_at
FROM review_jobs;

INSERT INTO review_results_v3 SELECT job_id, result_json, created_at FROM review_results;
INSERT INTO publications_v3 SELECT job_id, marker, gitlab_note_id, created_at FROM publications;

DROP TABLE review_results;
DROP TABLE publications;
DROP TABLE review_jobs;
ALTER TABLE review_jobs_v3 RENAME TO review_jobs;
ALTER TABLE review_results_v3 RENAME TO review_results;
ALTER TABLE publications_v3 RENAME TO publications;

CREATE INDEX review_jobs_due_idx
ON review_jobs (state, next_attempt_at, lease_expires_at);

PRAGMA user_version = 3;
`
		}

		if _, err := tx.ExecContext(ctx, migration); err != nil {
			tx.Rollback()
			return fmt.Errorf("apply schema version %d: %w", version+1, err)
		}
		if err := tx.Commit(); err != nil {
			tx.Rollback()
			return fmt.Errorf("commit schema version %d: %w", version+1, err)
		}
		version++
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
SELECT e.id, e.outcome, COALESCE(j.id, 0)
FROM webhook_events e
LEFT JOIN review_jobs j ON
    j.gitlab_instance = e.gitlab_instance AND
    j.project_id = e.project_id AND
    j.merge_request_iid = e.merge_request_iid AND
    j.head_sha = e.head_sha
WHERE e.delivery_id = ?`, event.DeliveryID).Scan(&result.EventID, &result.Outcome, &result.JobID); err != nil {
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
	leaseText := formatTime(now.Add(leaseDuration))

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
          state, lease_owner, lease_expires_at, attempt_count`,
		JobQueued, JobPublishing, JobQueued, JobRunning, owner, leaseText, nowText, nowText,
		maxAttempts, JobQueued, nowText, JobRunning, JobPublishing, nowText)

	job := &Job{}
	var leaseExpires string
	if err := row.Scan(&job.ID, &job.GitLabInstance, &job.ProjectID, &job.MergeRequestIID,
		&job.HeadSHA, &job.State, &job.LeaseOwner, &leaseExpires, &job.AttemptCount); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("claim review job: %w", err)
	}
	job.LeaseExpiresAt, _ = time.Parse(timestampLayout, leaseExpires)
	if err := s.db.QueryRowContext(ctx, `SELECT result_json FROM review_results WHERE job_id = ?`, job.ID).Scan(&job.ValidatedResultJSON); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("read claimed review result: %w", err)
	}
	return job, nil
}

func (s *Store) RenewLease(ctx context.Context, jobID int64, owner string, now time.Time, leaseDuration time.Duration) (bool, error) {
	result, err := s.db.ExecContext(ctx, `
UPDATE review_jobs
SET lease_expires_at = ?, updated_at = ?
WHERE id = ? AND lease_owner = ? AND state IN (?, ?)
  AND julianday(lease_expires_at) > julianday(?)`,
		formatTime(now.Add(leaseDuration)), formatTime(now), jobID, owner,
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

func (s *Store) SaveReviewResult(ctx context.Context, jobID int64, owner string, resultJSON []byte, now time.Time) error {
	if len(resultJSON) == 0 || len(resultJSON) > 65536 {
		return errors.New("invalid validated review result size")
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
	update, err := tx.ExecContext(ctx, `
UPDATE review_jobs
SET state = ?, updated_at = ?
WHERE id = ? AND state = ? AND lease_owner = ?
  AND julianday(lease_expires_at) > julianday(?)`,
		JobPublishing, formatTime(now), jobID, JobRunning, owner, formatTime(now))
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
		maxAttempts, JobFailed, JobQueued, maxAttempts, formatTime(nextAttempt),
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
SET state = ?, lease_owner = NULL, lease_expires_at = NULL,
    last_error_category = NULL, last_error_message = NULL, updated_at = ?
WHERE id = ? AND state = ? AND lease_owner = ?
  AND julianday(lease_expires_at) > julianday(?)`,
		JobCompleted, formatTime(now), jobID, JobPublishing, owner, formatTime(now))
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
	return value.UTC().Format(timestampLayout)
}

func validFailure(category, message string) bool {
	return category != "" && len(category) <= 128 && len(message) <= 512
}
