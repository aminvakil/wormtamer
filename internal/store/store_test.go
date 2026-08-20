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
		"index model_generations_feedback_idx",
		"index model_generations_review_idx",
		"index model_generations_time_idx",
		"index review_jobs_due_idx",
		"index review_jobs_patch_id_idx",
		"table feedback_jobs",
		"table model_generations",
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
	for _, version := range []int{1, 9, schemaVersion + 1} {
		t.Run(fmt.Sprintf("version_%d", version), func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "wormtamer.db")
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
		})
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
	if job.ID != accepted.JobID || job.State != JobRunning || job.AttemptCount != 1 || job.PatchIDStatus != PatchIDUnknown {
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
	if err := storage.SaveReviewResult(ctx, recovered.ID, "owner-1", resultJSON, nil, nil, PatchIDUnavailable, "", recoveredAt); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("wrong-owner SaveReviewResult() error = %v", err)
	}
	if err := storage.SaveReviewResult(ctx, recovered.ID, "owner-2", resultJSON, nil, nil, PatchIDUnavailable, "", recoveredAt); err != nil {
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
	canonicalJob, err := storage.ClaimJob(ctx, "canonical-owner", now, time.Minute, 5)
	if err != nil || canonicalJob == nil {
		t.Fatalf("ClaimJob(canonical) = %+v, %v", canonicalJob, err)
	}
	if err := storage.SaveReviewResult(ctx, canonicalJob.ID, "canonical-owner",
		[]byte(`{"summary":"canonical","findings":[]}`), nil, nil,
		PatchIDAvailable, patchID, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := storage.CompletePublication(ctx, canonicalJob.ID, "canonical-owner", "canonical-marker", 71, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}

	equivalentEvent := readyEvent("equivalent")
	equivalentEvent.HeadSHA = strings.Repeat("b", 40)
	equivalent, err := storage.AcceptEvent(ctx, equivalentEvent)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := storage.ClaimJob(ctx, "equivalent-owner", now.Add(3*time.Second), time.Minute, 5)
	if err != nil || claimed == nil || claimed.ID != equivalent.JobID || claimed.PatchIDStatus != PatchIDUnknown {
		t.Fatalf("ClaimJob(equivalent) = %+v, %v", claimed, err)
	}
	if err := storage.DeferPendingPatchID(ctx, claimed.ID, "equivalent-owner", now.Add(3*time.Second), now.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}
	claimed, err = storage.ClaimJob(ctx, "equivalent-owner-2", now.Add(5*time.Second), time.Minute, 5)
	if err != nil || claimed == nil || claimed.PatchIDStatus != PatchIDPending || claimed.AttemptCount != 2 {
		t.Fatalf("ClaimJob(pending) = %+v, %v", claimed, err)
	}
	canonicalID, found, err := storage.FindCanonicalReviewJob(ctx, claimed.ID, patchID)
	if err != nil || !found || canonicalID != canonical.JobID {
		t.Fatalf("FindCanonicalReviewJob() = %d, %t, %v", canonicalID, found, err)
	}
	if err := storage.CompleteEquivalentReview(ctx, claimed.ID, "wrong-owner", canonicalID, patchID, now.Add(6*time.Second)); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("wrong-owner CompleteEquivalentReview() error = %v", err)
	}
	if err := storage.CompleteEquivalentReview(ctx, claimed.ID, "equivalent-owner-2", canonicalID, patchID, now.Add(6*time.Second)); err != nil {
		t.Fatal(err)
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
	unpublished, err := storage.ClaimJob(ctx, "unpublished-owner", now.Add(3*time.Second), time.Minute, 5)
	if err != nil || unpublished == nil {
		t.Fatalf("ClaimJob(unpublished) = %+v, %v", unpublished, err)
	}
	if err := storage.SaveReviewResult(ctx, unpublished.ID, "unpublished-owner",
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
	other, err := storage.ClaimJob(ctx, "other-owner", now.Add(5*time.Second), time.Minute, 5)
	if err != nil || other == nil {
		t.Fatalf("ClaimJob(other MR) = %+v, %v", other, err)
	}
	if err := storage.SaveReviewResult(ctx, other.ID, "other-owner",
		[]byte(`{"summary":"other","findings":[]}`), nil, nil,
		PatchIDAvailable, patchID, now.Add(6*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := storage.CompletePublication(ctx, other.ID, "other-owner", "other-marker", 72, now.Add(7*time.Second)); err != nil {
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
	job, err := storage.ClaimJob(ctx, "publication-owner", now, time.Minute, 5)
	if err != nil || job == nil {
		t.Fatalf("ClaimJob() = %+v, %v", job, err)
	}
	if err := storage.DeferPendingPatchID(ctx, job.ID, "publication-owner", now, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	job, err = storage.ClaimJob(ctx, "publication-owner-2", now.Add(2*time.Second), time.Minute, 5)
	if err != nil || job == nil || job.PatchIDStatus != PatchIDPending {
		t.Fatalf("ClaimJob(pending) = %+v, %v", job, err)
	}
	if err := storage.CompletePublication(ctx, job.ID, "publication-owner-2", "<!-- existing -->", 73, now.Add(3*time.Second)); err != nil {
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
	if err := storage.SaveReviewResult(ctx, job.ID, "owner-1", resultJSON, findingIDs, nil, PatchIDUnavailable, "", now); err != nil {
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
	if recovered.State != JobPublishing || recovered.PatchIDStatus != PatchIDUnavailable ||
		string(recovered.ValidatedResultJSON) != string(resultJSON) ||
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
	if err := storage.SaveReviewResult(context.Background(), job.ID, "owner", resultJSON, []string{"model-id"}, nil, PatchIDUnavailable, "", now); err == nil {
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

	now := time.Now().UTC().Truncate(time.Second).Add(time.Hour)
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

func TestOpenRejectsNonemptyVersionZeroSchema(t *testing.T) {
	tests := []struct {
		name string
		sql  string
	}{
		{name: "unrelated table", sql: `CREATE TABLE unrelated (value TEXT)`},
		{name: "partial application schema", sql: `CREATE TABLE webhook_events (id INTEGER PRIMARY KEY)`},
		{name: "removed historical table", sql: `CREATE TABLE feedback_events (id INTEGER PRIMARY KEY)`},
		{name: "index", sql: `CREATE TABLE indexed (value TEXT); CREATE INDEX unrelated_index ON indexed (value)`},
		{name: "view", sql: `CREATE VIEW unrelated_view AS SELECT 1 AS value`},
		{name: "trigger", sql: `
CREATE TABLE triggered (value TEXT);
CREATE TRIGGER unrelated_trigger AFTER INSERT ON triggered BEGIN SELECT 1; END`},
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
