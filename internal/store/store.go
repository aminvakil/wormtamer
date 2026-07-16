package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"

	_ "github.com/mattn/go-sqlite3"
)

const schemaVersion = 1

const (
	OutcomeQueued          = "queued"
	OutcomeDuplicateReview = "duplicate_review"
	OutcomeIgnoredDraft    = "ignored_draft"
	OutcomeIgnoredAction   = "ignored_action"
)

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

	var fts5 int
	if err := s.db.QueryRowContext(ctx, `SELECT sqlite_compileoption_used('ENABLE_FTS5')`).Scan(&fts5); err != nil {
		return fmt.Errorf("verify SQLite FTS5 support: %w", err)
	}
	if fts5 != 1 {
		return errors.New("SQLite FTS5 support is required; build with -tags sqlite_fts5")
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
	if version == schemaVersion {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin schema transaction: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `
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
`); err != nil {
		return fmt.Errorf("apply schema version 1: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit schema version 1: %w", err)
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
