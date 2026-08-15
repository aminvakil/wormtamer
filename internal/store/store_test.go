package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

func TestAcceptEventIsDurableAndIdempotent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "wormtamer.db")
	storage, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}

	event := readyEvent("event-1")
	first, err := storage.AcceptEvent(ctx, event)
	if err != nil {
		t.Fatalf("AcceptEvent(first) error = %v", err)
	}
	if first.EventID == 0 || first.JobID == 0 || first.Outcome != OutcomeQueued || first.DuplicateDelivery {
		t.Fatalf("AcceptEvent(first) = %+v", first)
	}

	duplicate, err := storage.AcceptEvent(ctx, event)
	if err != nil {
		t.Fatalf("AcceptEvent(duplicate delivery) error = %v", err)
	}
	if duplicate.EventID != first.EventID || duplicate.JobID != first.JobID || !duplicate.DuplicateDelivery {
		t.Fatalf("AcceptEvent(duplicate delivery) = %+v, first = %+v", duplicate, first)
	}

	secondDelivery := readyEvent("event-2")
	duplicateReview, err := storage.AcceptEvent(ctx, secondDelivery)
	if err != nil {
		t.Fatalf("AcceptEvent(duplicate review) error = %v", err)
	}
	if duplicateReview.EventID == first.EventID || duplicateReview.JobID != first.JobID || duplicateReview.Outcome != OutcomeDuplicateReview {
		t.Fatalf("AcceptEvent(duplicate review) = %+v, first = %+v", duplicateReview, first)
	}

	assertCount(t, storage.db, "webhook_events", 2)
	assertCount(t, storage.db, "review_jobs", 1)
	if err := storage.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	reopened, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open(restart) error = %v", err)
	}
	defer reopened.Close()
	assertCount(t, reopened.db, "webhook_events", 2)
	assertCount(t, reopened.db, "review_jobs", 1)

	var payload string
	if err := reopened.db.QueryRow(`SELECT payload_json FROM webhook_events WHERE id = ?`, first.EventID).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if payload != string(event.Payload) {
		t.Fatalf("stored payload = %q, want %q", payload, event.Payload)
	}
}

func TestAcceptEventConcurrentReviewIdentityCreatesOneJob(t *testing.T) {
	storage := openTestStore(t)
	defer storage.Close()

	const deliveries = 10
	errors := make(chan error, deliveries)
	var wait sync.WaitGroup
	for index := 0; index < deliveries; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			_, err := storage.AcceptEvent(context.Background(), readyEvent(fmt.Sprintf("event-%d", index)))
			errors <- err
		}(index)
	}
	wait.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatalf("AcceptEvent() error = %v", err)
		}
	}
	assertCount(t, storage.db, "webhook_events", deliveries)
	assertCount(t, storage.db, "review_jobs", 1)
}

func TestCreateReconciledJobIsIdempotentAndAcceptsLaterWebhook(t *testing.T) {
	storage := openTestStore(t)
	defer storage.Close()
	ctx := context.Background()
	review := ReconciledReview{
		GitLabInstance:  "http://gitlab.internal",
		ProjectID:       42,
		MergeRequestIID: 7,
		HeadSHA:         "0123456789abcdef0123456789abcdef01234567",
	}

	first, err := storage.CreateReconciledJob(ctx, review)
	if err != nil || first.JobID == 0 || !first.NewlyQueued {
		t.Fatalf("CreateReconciledJob(first) = %+v, %v", first, err)
	}
	duplicate, err := storage.CreateReconciledJob(ctx, review)
	if err != nil || duplicate.JobID != first.JobID || duplicate.NewlyQueued {
		t.Fatalf("CreateReconciledJob(duplicate) = %+v, %v", duplicate, err)
	}

	var sourceEventID sql.NullInt64
	if err := storage.db.QueryRow(`SELECT source_event_id FROM review_jobs WHERE id = ?`, first.JobID).Scan(&sourceEventID); err != nil {
		t.Fatal(err)
	}
	if sourceEventID.Valid {
		t.Fatalf("reconciled source_event_id = %d, want NULL", sourceEventID.Int64)
	}

	accepted, err := storage.AcceptEvent(ctx, readyEvent("later-webhook"))
	if err != nil {
		t.Fatalf("AcceptEvent() error = %v", err)
	}
	if accepted.JobID != first.JobID || accepted.Outcome != OutcomeDuplicateReview {
		t.Fatalf("AcceptEvent() = %+v", accepted)
	}
	assertCount(t, storage.db, "webhook_events", 1)
	assertCount(t, storage.db, "review_jobs", 1)
}

func TestAcceptEventCommitFailureRollsBack(t *testing.T) {
	storage := openTestStore(t)
	defer storage.Close()

	_, err := storage.db.Exec(`
CREATE TABLE commit_failure (
    id INTEGER PRIMARY KEY,
    missing_event_id INTEGER NOT NULL,
    FOREIGN KEY (missing_event_id) REFERENCES webhook_events(id) DEFERRABLE INITIALLY DEFERRED
);
CREATE TRIGGER force_commit_failure AFTER INSERT ON webhook_events
BEGIN
    INSERT INTO commit_failure (missing_event_id) VALUES (-1);
END;`)
	if err != nil {
		t.Fatal(err)
	}

	_, err = storage.AcceptEvent(context.Background(), readyEvent("event-1"))
	if err == nil || !strings.Contains(err.Error(), "commit event transaction") {
		t.Fatalf("AcceptEvent() error = %v", err)
	}
	assertCount(t, storage.db, "webhook_events", 0)
	assertCount(t, storage.db, "review_jobs", 0)
}

func TestAcceptEventPersistsIgnoredEventWithoutJob(t *testing.T) {
	storage := openTestStore(t)
	defer storage.Close()

	event := readyEvent("draft-event")
	event.QueueReview = false
	event.IgnoredOutcome = OutcomeIgnoredDraft
	result, err := storage.AcceptEvent(context.Background(), event)
	if err != nil {
		t.Fatalf("AcceptEvent() error = %v", err)
	}
	if result.Outcome != OutcomeIgnoredDraft || result.JobID != 0 {
		t.Fatalf("AcceptEvent() = %+v", result)
	}
	assertCount(t, storage.db, "webhook_events", 1)
	assertCount(t, storage.db, "review_jobs", 0)
}

func TestOpenConfiguresSQLite(t *testing.T) {
	storage := openTestStore(t)
	defer storage.Close()

	var foreignKeys, busyTimeout int
	var journalMode string
	if err := storage.db.QueryRow(`PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
		t.Fatal(err)
	}
	if err := storage.db.QueryRow(`PRAGMA busy_timeout`).Scan(&busyTimeout); err != nil {
		t.Fatal(err)
	}
	if err := storage.db.QueryRow(`PRAGMA journal_mode`).Scan(&journalMode); err != nil {
		t.Fatal(err)
	}
	if foreignKeys != 1 || busyTimeout != 5000 || journalMode != "wal" {
		t.Fatalf("SQLite settings: foreign_keys=%d busy_timeout=%d journal_mode=%q", foreignKeys, busyTimeout, journalMode)
	}

	_, err := storage.db.Exec(`
INSERT INTO review_jobs (source_event_id, gitlab_instance, project_id, merge_request_iid, head_sha, state)
VALUES (999, 'http://gitlab.internal', 1, 1, 'abc', 'queued')`)
	if err == nil || !strings.Contains(err.Error(), "FOREIGN KEY") {
		t.Fatalf("foreign-key violating insert error = %v", err)
	}
}

func TestOpenRejectsNewerSchema(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "wormtamer.db")
	storage, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion+1)); err != nil {
		t.Fatal(err)
	}
	storage.Close()

	_, err = Open(ctx, path)
	if err == nil || !strings.Contains(err.Error(), "newer than supported") {
		t.Fatalf("Open() error = %v", err)
	}
}

func TestClaimLeaseCheckpointAndPublication(t *testing.T) {
	storage := openTestStore(t)
	defer storage.Close()
	ctx := context.Background()
	accepted, err := storage.AcceptEvent(ctx, readyEvent("event-1"))
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Add(time.Hour)
	job, err := storage.ClaimJob(ctx, "owner-1", now, 2*time.Minute, 5)
	if err != nil || job == nil {
		t.Fatalf("ClaimJob() = %+v, %v", job, err)
	}
	if job.ID != accepted.JobID || job.State != JobRunning || job.AttemptCount != 1 {
		t.Fatalf("claimed job = %+v", job)
	}
	if other, err := storage.ClaimJob(ctx, "owner-2", now, 2*time.Minute, 5); err != nil || other != nil {
		t.Fatalf("concurrent ClaimJob() = %+v, %v", other, err)
	}
	if renewed, err := storage.RenewLease(ctx, job.ID, "wrong-owner", now.Add(time.Second), 2*time.Minute); err != nil || renewed {
		t.Fatalf("wrong-owner RenewLease() = %t, %v", renewed, err)
	}
	if renewed, err := storage.RenewLease(ctx, job.ID, "owner-1", now.Add(time.Second), 2*time.Minute); err != nil || !renewed {
		t.Fatalf("RenewLease() = %t, %v", renewed, err)
	}

	recoveredAt := now.Add(2*time.Minute + 2*time.Second)
	recovered, err := storage.ClaimJob(ctx, "owner-2", recoveredAt, 2*time.Minute, 5)
	if err != nil || recovered == nil {
		t.Fatalf("recovered ClaimJob() = %+v, %v", recovered, err)
	}
	if recovered.AttemptCount != 2 || recovered.State != JobRunning {
		t.Fatalf("recovered job = %+v", recovered)
	}
	resultJSON := []byte(`{"summary":"ok","findings":[]}`)
	if err := storage.SaveReviewResult(ctx, recovered.ID, "owner-1", resultJSON, nil, nil, recoveredAt); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("wrong-owner SaveReviewResult() error = %v", err)
	}
	if err := storage.SaveReviewResult(ctx, recovered.ID, "owner-2", resultJSON, nil, nil, recoveredAt); err != nil {
		t.Fatalf("SaveReviewResult() error = %v", err)
	}

	publishing, err := storage.ClaimJob(ctx, "owner-3", recoveredAt.Add(2*time.Minute+time.Second), 2*time.Minute, 5)
	if err != nil || publishing == nil {
		t.Fatalf("publishing ClaimJob() = %+v, %v", publishing, err)
	}
	if publishing.State != JobPublishing || string(publishing.ValidatedResultJSON) != string(resultJSON) {
		t.Fatalf("publishing job = %+v", publishing)
	}
	if err := storage.CompletePublication(ctx, publishing.ID, "owner-3", "<!-- marker -->", 99, recoveredAt.Add(2*time.Minute+time.Second)); err != nil {
		t.Fatalf("CompletePublication() error = %v", err)
	}
	var state string
	if err := storage.db.QueryRow(`SELECT state FROM review_jobs WHERE id = ?`, publishing.ID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != JobCompleted {
		t.Fatalf("job state = %q", state)
	}
	assertCount(t, storage.db, "review_results", 1)
	assertCount(t, storage.db, "publications", 1)
}

func TestReviewCheckpointSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "wormtamer.db")
	storage, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.AcceptEvent(ctx, readyEvent("event-1")); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(time.Hour)
	job, err := storage.ClaimJob(ctx, "owner-1", now, time.Minute, 5)
	if err != nil || job == nil {
		t.Fatalf("ClaimJob() = %+v, %v", job, err)
	}
	resultJSON := []byte(`{"summary":"ok","findings":[{"priority":"P1","title":"title","explanation":"why","recommendation":"fix","path":"file.go"}]}`)
	findingIDs := []string{"WT-F-" + strings.Repeat("A", 26)}
	if err := storage.SaveReviewResult(ctx, job.ID, "owner-1", resultJSON, findingIDs, nil, now); err != nil {
		t.Fatal(err)
	}
	if err := storage.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	recovered, err := reopened.ClaimJob(ctx, "owner-2", now.Add(time.Minute+time.Second), time.Minute, 5)
	if err != nil || recovered == nil {
		t.Fatalf("recovered ClaimJob() = %+v, %v", recovered, err)
	}
	if recovered.State != JobPublishing || string(recovered.ValidatedResultJSON) != string(resultJSON) ||
		len(recovered.FindingIDs) != 1 || recovered.FindingIDs[0] != findingIDs[0] {
		t.Fatalf("recovered job = %+v", recovered)
	}
}

func TestSaveReviewResultRejectsMalformedFindingIdentifiers(t *testing.T) {
	storage := openTestStore(t)
	defer storage.Close()
	if _, err := storage.AcceptEvent(context.Background(), readyEvent("event-1")); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	job, err := storage.ClaimJob(context.Background(), "owner", now, time.Minute, 5)
	if err != nil || job == nil {
		t.Fatalf("ClaimJob() = %+v, %v", job, err)
	}
	resultJSON := []byte(`{"summary":"ok","findings":[]}`)
	if err := storage.SaveReviewResult(context.Background(), job.ID, "owner", resultJSON, []string{"model-id"}, nil, now); err == nil {
		t.Fatal("SaveReviewResult() accepted a malformed finding identifier")
	}
	assertCount(t, storage.db, "review_results", 0)
	assertCount(t, storage.db, "review_findings", 0)
}

func TestClaimJobIsAtomic(t *testing.T) {
	storage := openTestStore(t)
	defer storage.Close()
	if _, err := storage.AcceptEvent(context.Background(), readyEvent("event-1")); err != nil {
		t.Fatal(err)
	}

	const contenders = 10
	now := time.Now().UTC()
	jobs := make(chan *Job, contenders)
	errors := make(chan error, contenders)
	var wait sync.WaitGroup
	for index := 0; index < contenders; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			job, err := storage.ClaimJob(context.Background(), fmt.Sprintf("owner-%d", index), now, time.Minute, 5)
			jobs <- job
			errors <- err
		}(index)
	}
	wait.Wait()
	close(jobs)
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatalf("ClaimJob() error = %v", err)
		}
	}
	claimed := 0
	for job := range jobs {
		if job != nil {
			claimed++
		}
	}
	if claimed != 1 {
		t.Fatalf("claimed jobs = %d, want 1", claimed)
	}
}

func TestRetryJobIsDueAndExhaustsAttempts(t *testing.T) {
	storage := openTestStore(t)
	defer storage.Close()
	ctx := context.Background()
	if _, err := storage.AcceptEvent(ctx, readyEvent("event-1")); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Add(time.Hour)
	for attempt := 1; attempt <= 5; attempt++ {
		job, err := storage.ClaimJob(ctx, "owner", now, time.Minute, 5)
		if err != nil || job == nil {
			t.Fatalf("attempt %d ClaimJob() = %+v, %v", attempt, job, err)
		}
		if job.AttemptCount != attempt {
			t.Fatalf("attempt count = %d, want %d", job.AttemptCount, attempt)
		}
		next := now.Add(time.Minute)
		state, err := storage.RetryJob(ctx, job.ID, "owner", now, next, 5, "temporary", "temporary failure")
		if err != nil {
			t.Fatalf("RetryJob() error = %v", err)
		}
		if attempt < 5 && state != JobQueued {
			t.Fatalf("attempt %d state = %q", attempt, state)
		}
		if attempt == 5 && state != JobFailed {
			t.Fatalf("exhausted state = %q", state)
		}
		if early, err := storage.ClaimJob(ctx, "other", now.Add(30*time.Second), time.Minute, 5); err != nil || early != nil {
			t.Fatalf("early ClaimJob() = %+v, %v", early, err)
		}
		now = next
	}
}

func TestOpenMigratesVersionOne(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wormtamer.db")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
CREATE TABLE webhook_events (id INTEGER PRIMARY KEY);
CREATE TABLE review_jobs (
    id INTEGER PRIMARY KEY,
    source_event_id INTEGER NOT NULL REFERENCES webhook_events(id),
    gitlab_instance TEXT NOT NULL,
    project_id INTEGER NOT NULL,
    merge_request_iid INTEGER NOT NULL,
    head_sha TEXT NOT NULL,
    state TEXT NOT NULL DEFAULT 'queued',
    created_at TEXT NOT NULL,
    UNIQUE (gitlab_instance, project_id, merge_request_iid, head_sha)
);
PRAGMA user_version = 1;`)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}

	storage, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open() migration error = %v", err)
	}
	defer storage.Close()
	var version int
	if err := storage.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != schemaVersion {
		t.Fatalf("schema version = %d, want %d", version, schemaVersion)
	}
	assertCount(t, storage.db, "review_results", 0)
	assertCount(t, storage.db, "review_findings", 0)
	assertCount(t, storage.db, "publications", 0)
}

func TestOpenMigratesVersionTwoWithoutLosingWorkflowState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wormtamer.db")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
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
    payload_json BLOB NOT NULL,
    received_at TEXT NOT NULL
);
CREATE TABLE review_jobs (
    id INTEGER PRIMARY KEY,
    source_event_id INTEGER NOT NULL REFERENCES webhook_events(id),
    gitlab_instance TEXT NOT NULL,
    project_id INTEGER NOT NULL,
    merge_request_iid INTEGER NOT NULL,
    head_sha TEXT NOT NULL,
    state TEXT NOT NULL DEFAULT 'queued',
    created_at TEXT NOT NULL,
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
CREATE INDEX review_jobs_due_idx ON review_jobs (state, next_attempt_at, lease_expires_at);
CREATE TABLE review_results (
    job_id INTEGER PRIMARY KEY REFERENCES review_jobs(id) ON DELETE CASCADE,
    result_json BLOB NOT NULL,
    created_at TEXT NOT NULL
);
CREATE TABLE publications (
    job_id INTEGER PRIMARY KEY REFERENCES review_jobs(id) ON DELETE CASCADE,
    marker TEXT NOT NULL UNIQUE,
    gitlab_note_id INTEGER NOT NULL,
    created_at TEXT NOT NULL
);
INSERT INTO webhook_events VALUES (5, 'delivery', 'http://gitlab.internal', 42, 'group/project', 7, 'head', 'open', 'queued', '{}', 'created');
INSERT INTO review_jobs VALUES (9, 5, 'http://gitlab.internal', 42, 7, 'head', 'completed', 'created', NULL, NULL, 2, 'started', 'next', NULL, NULL, 'updated');
INSERT INTO review_results VALUES (9, '{"summary":"ok","findings":[]}', 'result-created');
INSERT INTO publications VALUES (9, 'marker', 11, 'publication-created');
PRAGMA user_version = 2;`)
	if closeErr := db.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}

	storage, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open() migration error = %v", err)
	}
	defer storage.Close()
	var sourceEventID sql.NullInt64
	var state string
	if err := storage.db.QueryRow(`SELECT source_event_id, state FROM review_jobs WHERE id = 9`).Scan(&sourceEventID, &state); err != nil {
		t.Fatal(err)
	}
	if !sourceEventID.Valid || sourceEventID.Int64 != 5 || state != JobCompleted {
		t.Fatalf("migrated job source=%+v state=%q", sourceEventID, state)
	}
	assertCount(t, storage.db, "review_results", 1)
	assertCount(t, storage.db, "review_findings", 0)
	assertCount(t, storage.db, "publications", 1)
	if _, err := storage.CreateReconciledJob(context.Background(), ReconciledReview{
		GitLabInstance: "http://gitlab.internal", ProjectID: 42, MergeRequestIID: 8, HeadSHA: "new-head",
	}); err != nil {
		t.Fatalf("CreateReconciledJob() after migration error = %v", err)
	}
}

func TestOpenFailsForUnavailablePath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "wormtamer.db")
	_, err := Open(context.Background(), path)
	if err == nil {
		t.Fatal("Open() error = nil")
	}
}

func readyEvent(deliveryID string) Event {
	return Event{
		DeliveryID:      deliveryID,
		GitLabInstance:  "http://gitlab.internal",
		ProjectID:       42,
		ProjectPath:     "group/project",
		MergeRequestIID: 7,
		HeadSHA:         "0123456789abcdef0123456789abcdef01234567",
		Action:          "open",
		Payload:         []byte(`{"object_kind":"merge_request"}`),
		QueueReview:     true,
	}
}

func openTestStore(t *testing.T) *Store {
	t.Helper()
	storage, err := Open(context.Background(), filepath.Join(t.TempDir(), "wormtamer.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	return storage
}

func assertCount(t *testing.T, db *sql.DB, table string, want int) {
	t.Helper()
	var got int
	if err := db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("%s count = %d, want %d", table, got, want)
	}
}
