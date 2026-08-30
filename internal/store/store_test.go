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

func TestTimestampsUseSecondPrecision(t *testing.T) {
	input := time.Date(2026, 8, 17, 7, 54, 21, 403000000, time.UTC)
	if got, want := formatTime(input), "2026-08-17T07:54:21Z"; got != want {
		t.Fatalf("formatTime() = %q, want %q", got, want)
	}
	if got, want := formatDeadline(input), "2026-08-17T07:54:22Z"; got != want {
		t.Fatalf("formatDeadline() = %q, want %q", got, want)
	}
	if got, want := formatDeadline(input.Truncate(time.Second)), "2026-08-17T07:54:21Z"; got != want {
		t.Fatalf("formatDeadline(whole second) = %q, want %q", got, want)
	}

	storage := openTestStore(t)
	defer storage.Close()
	accepted, err := storage.AcceptEvent(context.Background(), readyEvent("second-precision"))
	if err != nil {
		t.Fatal(err)
	}
	var receivedAt, createdAt string
	if err := storage.db.QueryRow(`
SELECT e.received_at, j.created_at
FROM webhook_events e
JOIN review_jobs j ON j.source_event_id = e.id
WHERE j.id = ?`, accepted.JobID).Scan(&receivedAt, &createdAt); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{"received_at": receivedAt, "created_at": createdAt} {
		if strings.Contains(value, ".") {
			t.Fatalf("%s = %q, want whole seconds", name, value)
		}
		if _, err := time.Parse(timestampLayout, value); err != nil {
			t.Fatalf("parse %s %q: %v", name, value, err)
		}
	}
}

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
	accepted, err := storage.AcceptEvent(context.Background(), readyEvent("patch-constraints"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.db.Exec(`UPDATE review_jobs SET patch_id_status = ?, patch_id_sha = ? WHERE id = ?`,
		PatchIDAvailable, strings.Repeat("A", 40), accepted.JobID); err == nil || !strings.Contains(err.Error(), "CHECK") {
		t.Fatalf("malformed patch ID constraint error = %v", err)
	}
	if _, err := storage.db.Exec(`UPDATE review_jobs SET patch_id_status = ? WHERE id = ?`,
		PatchIDAvailable, accepted.JobID); err == nil || !strings.Contains(err.Error(), "CHECK") {
		t.Fatalf("missing patch ID constraint error = %v", err)
	}
}

func TestOpenInitializesCurrentSchema(t *testing.T) {
	storage := openTestStore(t)
	defer storage.Close()

	var version int
	if err := storage.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != schemaVersion {
		t.Fatalf("schema version = %d, want %d", version, schemaVersion)
	}

	got := strings.Join(schemaObjects(t, storage.db), "\n")
	want := strings.Join([]string{
		"index feedback_jobs_due_idx",
		"index review_jobs_due_idx",
		"index review_jobs_patch_id_idx",
		"table feedback_jobs",
		"table publications",
		"table review_findings",
		"table review_jobs",
		"table review_memories",
		"table review_memory_retrievals",
		"table review_results",
		"table webhook_events",
	}, "\n")
	if got != want {
		t.Fatalf("schema objects:\n%s\nwant:\n%s", got, want)
	}
}

func TestOpenRejectsUnsupportedSchema(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "wormtamer.db")
	version := schemaVersion + 1
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(fmt.Sprintf(`
CREATE TABLE preserved (value TEXT NOT NULL);
INSERT INTO preserved VALUES ('unchanged');
PRAGMA user_version = %d;`, version)); err != nil {
		db.Close()
		t.Fatal(err)
	}
	before := strings.Join(schemaObjects(t, db), "\n")
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	_, err = Open(ctx, path)
	if !errors.Is(err, ErrUnsupportedSchemaVersion) {
		t.Fatalf("Open() error = %v", err)
	}

	db, err = sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var gotVersion int
	var value string
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&gotVersion); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT value FROM preserved`).Scan(&value); err != nil {
		t.Fatal(err)
	}
	after := strings.Join(schemaObjects(t, db), "\n")
	if gotVersion != version || value != "unchanged" || after != before {
		t.Fatalf("database changed: version=%d value=%q objects=%q, want version=%d value=unchanged objects=%q",
			gotVersion, value, after, version, before)
	}
}

func TestClaimCheckpointRecoveryAndPublication(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "wormtamer.db")
	storage, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := storage.AcceptEvent(ctx, readyEvent("event-1"))
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Add(time.Hour)
	job, err := storage.ClaimJob(ctx, now)
	if err != nil || job == nil {
		t.Fatalf("ClaimJob() = %+v, %v", job, err)
	}
	if job.ID != accepted.JobID || job.State != JobRunning || job.AttemptCount != 1 || job.PatchIDStatus != PatchIDUnknown {
		t.Fatalf("claimed job = %+v", job)
	}
	if other, err := storage.ClaimJob(ctx, now); err != nil || other != nil {
		t.Fatalf("concurrent ClaimJob() = %+v, %v", other, err)
	}

	resultJSON := []byte(`{"summary":"ok","findings":[{"priority":"P1","title":"title","explanation":"why","recommendation":"fix","path":"file.go"}]}`)
	findingIDs := []string{"WT-F-" + strings.Repeat("A", 26)}
	if err := storage.SaveReviewResult(ctx, job.ID, resultJSON, findingIDs, nil, PatchIDUnavailable, "", now); err != nil {
		t.Fatalf("SaveReviewResult() error = %v", err)
	}
	var checkpointState string
	if err := storage.db.QueryRow(`SELECT state FROM review_jobs WHERE id = ?`, job.ID).Scan(&checkpointState); err != nil {
		t.Fatal(err)
	}
	if checkpointState != JobRunning {
		t.Fatalf("checkpoint state = %q", checkpointState)
	}
	if err := storage.Close(); err != nil {
		t.Fatal(err)
	}

	storage, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	recoveredAt := now.Add(time.Minute)
	if err := storage.RecoverInterruptedJobs(ctx, recoveredAt); err != nil {
		t.Fatal(err)
	}
	recovered, err := storage.ClaimJob(ctx, recoveredAt)
	if err != nil || recovered == nil {
		t.Fatalf("recovered ClaimJob() = %+v, %v", recovered, err)
	}
	if recovered.AttemptCount != 2 || recovered.State != JobRunning || recovered.PatchIDStatus != PatchIDUnavailable ||
		string(recovered.ValidatedResultJSON) != string(resultJSON) ||
		len(recovered.FindingIDs) != 1 || recovered.FindingIDs[0] != findingIDs[0] {
		t.Fatalf("recovered job = %+v", recovered)
	}
	if err := storage.CompletePublication(ctx, recovered.ID, "<!-- marker -->", 99, recoveredAt); err != nil {
		t.Fatalf("CompletePublication() error = %v", err)
	}
	if err := storage.SaveReviewResult(ctx, recovered.ID, []byte(`{"summary":"completed","findings":[]}`), nil, nil, PatchIDUnavailable, "", recoveredAt); !errors.Is(err, ErrJobNotRunning) {
		t.Fatalf("completed SaveReviewResult() error = %v", err)
	}
	var state string
	if err := storage.db.QueryRow(`SELECT state FROM review_jobs WHERE id = ?`, recovered.ID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != JobCompleted {
		t.Fatalf("job state = %q", state)
	}
	assertCount(t, storage.db, "review_results", 1)
	assertCount(t, storage.db, "publications", 1)
}

func TestRecoverInterruptedJobsRequeuesOrFailsOnlyRunningWork(t *testing.T) {
	storage := openTestStore(t)
	defer storage.Close()
	ctx := context.Background()
	now := time.Now().UTC().Add(time.Hour)

	publishReview := func(delivery string, iid int64, head string) int64 {
		event := readyEvent(delivery)
		event.MergeRequestIID, event.HeadSHA = iid, head
		accepted, err := storage.AcceptEvent(ctx, event)
		if err != nil {
			t.Fatal(err)
		}
		job, err := storage.ClaimJob(ctx, now)
		if err != nil || job == nil || job.ID != accepted.JobID {
			t.Fatalf("ClaimJob(%s) = %+v, %v", delivery, job, err)
		}
		if err := storage.SaveReviewResult(ctx, job.ID, []byte(`{"summary":"done","findings":[]}`), nil, nil, PatchIDUnavailable, "", now); err != nil {
			t.Fatal(err)
		}
		if err := storage.CompletePublication(ctx, job.ID, "<!-- "+delivery+" -->", job.ID, now); err != nil {
			t.Fatal(err)
		}
		return job.ID
	}
	queueFeedback := func(delivery string, iid int64, head string) int64 {
		event := terminalEvent(delivery, "close", "closed", head)
		event.MergeRequestIID = iid
		accepted, err := storage.AcceptEvent(ctx, event)
		if err != nil || accepted.FeedbackJobID == 0 {
			t.Fatalf("AcceptEvent(%s) = %+v, %v", delivery, accepted, err)
		}
		return accepted.FeedbackJobID
	}

	completedReviewID := publishReview("completed-review-7", 7, strings.Repeat("1", 40))
	feedbackRunningID := queueFeedback("feedback-running", 7, strings.Repeat("2", 40))
	if job, err := storage.ClaimFeedbackJob(ctx, now); err != nil || job == nil || job.ID != feedbackRunningID {
		t.Fatalf("ClaimFeedbackJob(running) = %+v, %v", job, err)
	}
	publishReview("completed-review-8", 8, strings.Repeat("3", 40))
	feedbackExhaustedID := queueFeedback("feedback-exhausted", 8, strings.Repeat("4", 40))
	if job, err := storage.ClaimFeedbackJob(ctx, now); err != nil || job == nil || job.ID != feedbackExhaustedID {
		t.Fatalf("ClaimFeedbackJob(exhausted) = %+v, %v", job, err)
	}
	if _, err := storage.db.Exec(`UPDATE feedback_jobs SET attempt_count = ? WHERE id = ?`, MaxJobAttempts, feedbackExhaustedID); err != nil {
		t.Fatal(err)
	}

	createReview := func(delivery string, iid int64, state string) int64 {
		event := readyEvent(delivery)
		event.MergeRequestIID, event.HeadSHA = iid, strings.Repeat(fmt.Sprintf("%x", iid%16), 40)
		accepted, err := storage.AcceptEvent(ctx, event)
		if err != nil {
			t.Fatal(err)
		}
		if state != JobQueued {
			if _, err := storage.db.Exec(`UPDATE review_jobs SET state = ?, updated_at = ? WHERE id = ?`, state, formatTime(now), accepted.JobID); err != nil {
				t.Fatal(err)
			}
		}
		return accepted.JobID
	}
	runningReviewID := createReview("running-review", 9, JobQueued)
	if job, err := storage.ClaimJob(ctx, now); err != nil || job == nil || job.ID != runningReviewID {
		t.Fatalf("ClaimJob(running) = %+v, %v", job, err)
	}
	exhaustedReviewID := createReview("exhausted-review", 10, JobQueued)
	if job, err := storage.ClaimJob(ctx, now); err != nil || job == nil || job.ID != exhaustedReviewID {
		t.Fatalf("ClaimJob(exhausted) = %+v, %v", job, err)
	}
	if _, err := storage.db.Exec(`UPDATE review_jobs SET attempt_count = ? WHERE id = ?`, MaxJobAttempts, exhaustedReviewID); err != nil {
		t.Fatal(err)
	}
	queuedReviewID := createReview("queued-review", 11, JobQueued)
	failedReviewID := createReview("failed-review", 12, JobFailed)
	obsoleteReviewID := createReview("obsolete-review", 13, JobObsolete)

	recoveredAt := now.Add(time.Minute)
	if err := storage.RecoverInterruptedJobs(ctx, recoveredAt); err != nil {
		t.Fatal(err)
	}
	assertState := func(table string, id int64, want string) {
		var state string
		if err := storage.db.QueryRow(`SELECT state FROM `+table+` WHERE id = ?`, id).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if state != want {
			t.Fatalf("%s job %d state = %q, want %q", table, id, state, want)
		}
	}
	assertState("review_jobs", runningReviewID, JobQueued)
	assertState("review_jobs", exhaustedReviewID, JobFailed)
	assertState("review_jobs", queuedReviewID, JobQueued)
	assertState("review_jobs", failedReviewID, JobFailed)
	assertState("review_jobs", obsoleteReviewID, JobObsolete)
	assertState("review_jobs", completedReviewID, JobCompleted)
	assertState("feedback_jobs", feedbackRunningID, FeedbackQueued)
	assertState("feedback_jobs", feedbackExhaustedID, FeedbackFailed)

	for table, id := range map[string]int64{"review_jobs": exhaustedReviewID, "feedback_jobs": feedbackExhaustedID} {
		var category string
		if err := storage.db.QueryRow(`SELECT last_error_category FROM `+table+` WHERE id = ?`, id).Scan(&category); err != nil {
			t.Fatal(err)
		}
		if category != "attempts_exhausted" {
			t.Fatalf("%s exhausted category = %q", table, category)
		}
	}
}

func TestPatchIDRetryAndEquivalentCompletion(t *testing.T) {
	storage := openTestStore(t)
	defer storage.Close()
	ctx := context.Background()
	now := time.Now().UTC().Add(time.Hour)
	patchID := strings.Repeat("a", 64)

	canonicalEvent := readyEvent("canonical")
	canonical, err := storage.AcceptEvent(ctx, canonicalEvent)
	if err != nil {
		t.Fatal(err)
	}
	canonicalJob, err := storage.ClaimJob(ctx, now)
	if err != nil || canonicalJob == nil {
		t.Fatalf("ClaimJob(canonical) = %+v, %v", canonicalJob, err)
	}
	if err := storage.SaveReviewResult(ctx, canonicalJob.ID,
		[]byte(`{"summary":"canonical","findings":[]}`), nil, nil,
		PatchIDAvailable, patchID, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := storage.CompletePublication(ctx, canonicalJob.ID, "canonical-marker", 71, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}

	equivalentEvent := readyEvent("equivalent")
	equivalentEvent.HeadSHA = strings.Repeat("b", 40)
	equivalent, err := storage.AcceptEvent(ctx, equivalentEvent)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := storage.ClaimJob(ctx, now.Add(3*time.Second))
	if err != nil || claimed == nil || claimed.ID != equivalent.JobID || claimed.PatchIDStatus != PatchIDUnknown {
		t.Fatalf("ClaimJob(equivalent) = %+v, %v", claimed, err)
	}
	if err := storage.DeferPendingPatchID(ctx, claimed.ID, now.Add(3*time.Second), now.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}
	claimed, err = storage.ClaimJob(ctx, now.Add(5*time.Second))
	if err != nil || claimed == nil || claimed.PatchIDStatus != PatchIDPending || claimed.AttemptCount != 2 {
		t.Fatalf("ClaimJob(pending) = %+v, %v", claimed, err)
	}
	canonicalID, found, err := storage.FindCanonicalReviewJob(ctx, claimed.ID, patchID)
	if err != nil || !found || canonicalID != canonical.JobID {
		t.Fatalf("FindCanonicalReviewJob() = %d, %t, %v", canonicalID, found, err)
	}
	if err := storage.CompleteEquivalentReview(ctx, claimed.ID, canonicalID, patchID, now.Add(6*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := storage.CompleteEquivalentReview(ctx, claimed.ID, canonicalID, patchID, now.Add(6*time.Second)); !errors.Is(err, ErrJobNotRunning) {
		t.Fatalf("repeated CompleteEquivalentReview() error = %v", err)
	}

	var state, status, storedPatch string
	var equivalentTo int64
	if err := storage.db.QueryRow(`
SELECT state, patch_id_status, patch_id_sha, equivalent_to_job_id
FROM review_jobs WHERE id = ?`, equivalent.JobID).Scan(&state, &status, &storedPatch, &equivalentTo); err != nil {
		t.Fatal(err)
	}
	if state != JobCompleted || status != PatchIDAvailable || storedPatch != patchID || equivalentTo != canonical.JobID {
		t.Fatalf("equivalent state=%q status=%q patch=%q canonical=%d", state, status, storedPatch, equivalentTo)
	}
	var ownResults, ownPublications int
	if err := storage.db.QueryRow(`
SELECT EXISTS(SELECT 1 FROM review_results WHERE job_id = ?),
       EXISTS(SELECT 1 FROM publications WHERE job_id = ?)`, equivalent.JobID, equivalent.JobID).Scan(&ownResults, &ownPublications); err != nil {
		t.Fatal(err)
	}
	if ownResults != 0 || ownPublications != 0 {
		t.Fatalf("equivalent owns result=%d publication=%d", ownResults, ownPublications)
	}
	detail, err := storage.GetReviewRecord(ctx, equivalent.JobID)
	if err != nil || !detail.Equivalent || detail.EquivalentToJobID != canonical.JobID ||
		detail.PatchIDStatus != PatchIDAvailable || detail.PatchIDSHA != patchID ||
		detail.HasResult || detail.Published || detail.ExternalOnly || detail.Result != nil {
		t.Fatalf("equivalent panel detail = %+v, %v", detail, err)
	}
}

func TestCanonicalReviewLookupRejectsIneligibleAndOtherMergeRequestJobs(t *testing.T) {
	storage := openTestStore(t)
	defer storage.Close()
	ctx := context.Background()
	now := time.Now().UTC().Add(time.Hour)
	patchID := strings.Repeat("c", 40)

	unknownID := preparePublishedReview(t, storage, now, "unknown-patch", strings.Repeat("1", 40),
		[]byte(`{"summary":"unknown","findings":[]}`), nil)
	if _, err := storage.db.Exec(`UPDATE review_jobs SET patch_id_status = ?, patch_id_sha = NULL WHERE id = ?`, PatchIDUnknown, unknownID); err != nil {
		t.Fatal(err)
	}

	unpublishedEvent := readyEvent("unpublished")
	unpublishedEvent.HeadSHA = strings.Repeat("2", 40)
	if _, err := storage.AcceptEvent(ctx, unpublishedEvent); err != nil {
		t.Fatal(err)
	}
	unpublished, err := storage.ClaimJob(ctx, now.Add(3*time.Second))
	if err != nil || unpublished == nil {
		t.Fatalf("ClaimJob(unpublished) = %+v, %v", unpublished, err)
	}
	if err := storage.SaveReviewResult(ctx, unpublished.ID,
		[]byte(`{"summary":"unpublished","findings":[]}`), nil, nil,
		PatchIDAvailable, patchID, now.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}

	otherEvent := readyEvent("other-merge-request")
	otherEvent.MergeRequestIID = 8
	otherEvent.HeadSHA = strings.Repeat("3", 40)
	if _, err := storage.AcceptEvent(ctx, otherEvent); err != nil {
		t.Fatal(err)
	}
	other, err := storage.ClaimJob(ctx, now.Add(5*time.Second))
	if err != nil || other == nil {
		t.Fatalf("ClaimJob(other MR) = %+v, %v", other, err)
	}
	if err := storage.SaveReviewResult(ctx, other.ID,
		[]byte(`{"summary":"other","findings":[]}`), nil, nil,
		PatchIDAvailable, patchID, now.Add(6*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := storage.CompletePublication(ctx, other.ID, "other-marker", 72, now.Add(7*time.Second)); err != nil {
		t.Fatal(err)
	}

	sourceEvent := readyEvent("lookup-source")
	sourceEvent.HeadSHA = strings.Repeat("4", 40)
	source, err := storage.AcceptEvent(ctx, sourceEvent)
	if err != nil {
		t.Fatal(err)
	}
	if canonicalID, found, err := storage.FindCanonicalReviewJob(ctx, source.JobID, patchID); err != nil || found || canonicalID != 0 {
		t.Fatalf("FindCanonicalReviewJob() = %d, %t, %v", canonicalID, found, err)
	}
}

func TestCompletePublicationWithoutLocalReviewResult(t *testing.T) {
	storage := openTestStore(t)
	defer storage.Close()
	ctx := context.Background()
	accepted, err := storage.AcceptEvent(ctx, readyEvent("existing-publication"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	job, err := storage.ClaimJob(ctx, now)
	if err != nil || job == nil {
		t.Fatalf("ClaimJob() = %+v, %v", job, err)
	}
	if err := storage.DeferPendingPatchID(ctx, job.ID, now, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	job, err = storage.ClaimJob(ctx, now.Add(2*time.Second))
	if err != nil || job == nil || job.PatchIDStatus != PatchIDPending {
		t.Fatalf("ClaimJob(pending) = %+v, %v", job, err)
	}
	if err := storage.CompletePublication(ctx, job.ID, "<!-- existing -->", 73, now.Add(3*time.Second)); err != nil {
		t.Fatalf("CompletePublication() error = %v", err)
	}
	var state, marker, patchIDStatus string
	var noteID int64
	if err := storage.db.QueryRow(`
SELECT j.state, p.marker, p.gitlab_note_id, j.patch_id_status
FROM review_jobs j
JOIN publications p ON p.job_id = j.id
WHERE j.id = ?`, accepted.JobID).Scan(&state, &marker, &noteID, &patchIDStatus); err != nil {
		t.Fatal(err)
	}
	if state != JobCompleted || marker != "<!-- existing -->" || noteID != 73 || patchIDStatus != PatchIDUnknown {
		t.Fatalf("recovered publication state=%q marker=%q note=%d patch_status=%q", state, marker, noteID, patchIDStatus)
	}
	assertCount(t, storage.db, "review_results", 0)
	assertCount(t, storage.db, "publications", 1)
}

func TestSaveReviewResultRejectsMalformedFindingIdentifiers(t *testing.T) {
	storage := openTestStore(t)
	defer storage.Close()
	if _, err := storage.AcceptEvent(context.Background(), readyEvent("event-1")); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	job, err := storage.ClaimJob(context.Background(), now)
	if err != nil || job == nil {
		t.Fatalf("ClaimJob() = %+v, %v", job, err)
	}
	resultJSON := []byte(`{"summary":"ok","findings":[]}`)
	if err := storage.SaveReviewResult(context.Background(), job.ID, resultJSON, []string{"model-id"}, nil, PatchIDUnavailable, "", now); err == nil {
		t.Fatal("SaveReviewResult() accepted a malformed finding identifier")
	}
	assertCount(t, storage.db, "review_results", 0)
	assertCount(t, storage.db, "review_findings", 0)
}

func TestClaimJobRollsBackWhenStoredContextIsInvalid(t *testing.T) {
	storage := openTestStore(t)
	defer storage.Close()
	ctx := context.Background()
	accepted, err := storage.AcceptEvent(ctx, readyEvent("invalid-claim-context"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(time.Hour)
	if _, err := storage.db.Exec(`
INSERT INTO review_results (job_id, result_json, created_at) VALUES (?, ?, ?);
INSERT INTO review_findings (finding_id, job_id, finding_index) VALUES (?, ?, 1);`,
		accepted.JobID, []byte(`{"summary":"stored","findings":[]}`), formatTime(now),
		"WT-F-"+strings.Repeat("A", 26), accepted.JobID); err != nil {
		t.Fatal(err)
	}
	if job, err := storage.ClaimJob(ctx, now); err == nil || job != nil {
		t.Fatalf("ClaimJob() = %+v, %v", job, err)
	}
	var state string
	var attempts int
	if err := storage.db.QueryRow(`SELECT state, attempt_count FROM review_jobs WHERE id = ?`, accepted.JobID).Scan(&state, &attempts); err != nil {
		t.Fatal(err)
	}
	if state != JobQueued || attempts != 0 {
		t.Fatalf("rolled-back claim state=%q attempts=%d", state, attempts)
	}
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
			job, err := storage.ClaimJob(context.Background(), now)
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

	now := time.Now().UTC().Truncate(time.Second).Add(time.Hour)
	for attempt := 1; attempt <= 5; attempt++ {
		job, err := storage.ClaimJob(ctx, now)
		if err != nil || job == nil {
			t.Fatalf("attempt %d ClaimJob() = %+v, %v", attempt, job, err)
		}
		if job.AttemptCount != attempt {
			t.Fatalf("attempt count = %d, want %d", job.AttemptCount, attempt)
		}
		next := now.Add(time.Minute)
		state, err := storage.RetryJob(ctx, job.ID, now, next, "temporary", "temporary failure")
		if err != nil {
			t.Fatalf("RetryJob() error = %v", err)
		}
		if attempt < 5 && state != JobQueued {
			t.Fatalf("attempt %d state = %q", attempt, state)
		}
		if attempt == 5 && state != JobFailed {
			t.Fatalf("exhausted state = %q", state)
		}
		if early, err := storage.ClaimJob(ctx, now.Add(30*time.Second)); err != nil || early != nil {
			t.Fatalf("early ClaimJob() = %+v, %v", early, err)
		}
		now = next
	}
}

func TestOpenRejectsNonemptyVersionZeroSchema(t *testing.T) {
	tests := []struct {
		name string
		sql  string
	}{
		{name: "unrelated table", sql: `CREATE TABLE unrelated (value TEXT)`},
		{name: "view", sql: `CREATE VIEW unrelated_view AS SELECT 1 AS value`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "wormtamer.db")
			db, err := sql.Open("sqlite3", path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(test.sql); err != nil {
				db.Close()
				t.Fatal(err)
			}
			before := strings.Join(schemaObjects(t, db), "\n")
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}

			_, err = Open(context.Background(), path)
			if !errors.Is(err, ErrNonemptyVersionZeroSchema) {
				t.Fatalf("Open() error = %v", err)
			}

			db, err = sql.Open("sqlite3", path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			var version int
			if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
				t.Fatal(err)
			}
			after := strings.Join(schemaObjects(t, db), "\n")
			if version != 0 || after != before {
				t.Fatalf("database changed: version=%d objects=%q, want version=0 objects=%q", version, after, before)
			}
		})
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

func schemaObjects(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`
SELECT type, name
FROM sqlite_schema
WHERE type IN ('table', 'index', 'view', 'trigger')
  AND name NOT GLOB 'sqlite_*'
ORDER BY type, name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var objects []string
	for rows.Next() {
		var objectType, name string
		if err := rows.Scan(&objectType, &name); err != nil {
			t.Fatal(err)
		}
		objects = append(objects, objectType+" "+name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return objects
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
