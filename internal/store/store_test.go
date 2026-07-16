package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

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

	var foreignKeys, busyTimeout, fts5 int
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
	if err := storage.db.QueryRow(`SELECT sqlite_compileoption_used('ENABLE_FTS5')`).Scan(&fts5); err != nil {
		t.Fatal(err)
	}
	if foreignKeys != 1 || busyTimeout != 5000 || journalMode != "wal" || fts5 != 1 {
		t.Fatalf("SQLite settings: foreign_keys=%d busy_timeout=%d journal_mode=%q fts5=%d", foreignKeys, busyTimeout, journalMode, fts5)
	}
	if _, err := storage.db.Exec(`CREATE VIRTUAL TABLE fts5_check USING fts5(content)`); err != nil {
		t.Fatalf("create FTS5 table: %v", err)
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
	if _, err := storage.db.Exec(`PRAGMA user_version = 2`); err != nil {
		t.Fatal(err)
	}
	storage.Close()

	_, err = Open(ctx, path)
	if err == nil || !strings.Contains(err.Error(), "newer than supported") {
		t.Fatalf("Open() error = %v", err)
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
