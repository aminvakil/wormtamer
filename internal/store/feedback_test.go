package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aminvakil/wormtamer/internal/review"
)

func TestFeedbackEventAndMemoryLifecycle(t *testing.T) {
	storage := openTestStore(t)
	defer storage.Close()
	ctx := context.Background()
	now := time.Now().UTC().Add(time.Second)
	findingID := prepareFinding(t, storage, now)

	event := FeedbackEvent{
		DeliveryID: "note-create", GitLabInstance: "http://gitlab.internal",
		ProjectID: 42, ProjectPath: "group/project", MergeRequestIID: 7,
		NoteID: 91, ActorID: 12, Action: "create", SourceUpdatedAt: now,
	}
	accepted, err := storage.AcceptFeedbackEvent(ctx, event, now)
	if err != nil || accepted.JobID == 0 || accepted.Outcome != FeedbackOutcomeQueued {
		t.Fatalf("AcceptFeedbackEvent() = %+v, %v", accepted, err)
	}
	duplicate, err := storage.AcceptFeedbackEvent(ctx, event, now)
	if err != nil || !duplicate.DuplicateDelivery || duplicate.EventID != accepted.EventID || duplicate.JobID != accepted.JobID {
		t.Fatalf("duplicate feedback event = %+v, %v", duplicate, err)
	}
	assertCount(t, storage.db, "feedback_events", 1)
	assertCount(t, storage.db, "feedback_jobs", 1)

	job, err := storage.ClaimFeedbackJob(ctx, "feedback-owner", now, 3*time.Minute, 5)
	if err != nil || job == nil || len(job.FindingIDs) != 1 || job.FindingIDs[0] != findingID {
		t.Fatalf("ClaimFeedbackJob() = %+v, %v", job, err)
	}
	decision := FeedbackDecision{
		MemoryID: "WT-M-" + strings.Repeat("A", 26), TargetType: "finding", TargetID: findingID,
		Outcome: "rejects_finding", Confidence: "high", Lesson: "Generated files in this repository are not reviewed manually.",
	}
	if err := storage.CompleteFeedbackJob(ctx, job.ID, job.SourceEventID, "feedback-owner", 40, "maintainer",
		"http://gitlab.internal/group/project/-/merge_requests/7#note_91", []FeedbackDecision{decision}, now.Add(time.Second), 5*time.Minute); err != nil {
		t.Fatalf("CompleteFeedbackJob() error = %v", err)
	}
	var active int
	var lesson string
	if err := storage.db.QueryRow(`SELECT active, lesson FROM review_memories`).Scan(&active, &lesson); err != nil || active != 1 || lesson != decision.Lesson {
		t.Fatalf("stored memory active=%d lesson=%q error=%v", active, lesson, err)
	}
	sources, err := storage.DueFeedbackSources(ctx, now.Add(6*time.Minute), 10)
	if err != nil || len(sources) != 1 {
		t.Fatalf("DueFeedbackSources() = %+v, %v", sources, err)
	}
	if err := storage.ReconcileFeedbackSource(ctx, sources[0].JobID, false, time.Time{}, now.Add(6*time.Minute), 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := storage.db.QueryRow(`SELECT active FROM review_memories`).Scan(&active); err != nil || active != 0 {
		t.Fatalf("deleted-source memory active=%d error=%v", active, err)
	}

	updated := event
	updated.DeliveryID = "note-update"
	updated.Action = "update"
	updated.SourceUpdatedAt = now.Add(time.Minute)
	updateResult, err := storage.AcceptFeedbackEvent(ctx, updated, now.Add(time.Minute))
	if err != nil || updateResult.JobID != job.ID || updateResult.Outcome != FeedbackOutcomeQueued {
		t.Fatalf("updated feedback event = %+v, %v", updateResult, err)
	}
	if err := storage.db.QueryRow(`SELECT active FROM review_memories`).Scan(&active); err != nil || active != 0 {
		t.Fatalf("superseded memory active=%d error=%v", active, err)
	}

	updatedJob, err := storage.ClaimFeedbackJob(ctx, "feedback-owner-2", now.Add(time.Minute), 3*time.Minute, 5)
	if err != nil || updatedJob == nil || updatedJob.SourceEventID == job.SourceEventID {
		t.Fatalf("updated ClaimFeedbackJob() = %+v, %v", updatedJob, err)
	}
	if err := storage.CompleteFeedbackJob(ctx, updatedJob.ID, updatedJob.SourceEventID, "feedback-owner-2", 30, "developer",
		"http://gitlab.internal/group/project/-/merge_requests/7#note_91", nil, now.Add(time.Minute+time.Second), 5*time.Minute); err != nil {
		t.Fatalf("complete empty feedback error = %v", err)
	}
	assertCount(t, storage.db, "review_memories", 0)

	rows, err := storage.db.Query(`PRAGMA table_info(feedback_events)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(name, "body") || strings.Contains(name, "comment") || strings.Contains(name, "payload") {
			t.Fatalf("feedback event retains comment content in column %q", name)
		}
	}
}

func TestSameSecondFeedbackUpdateRetriesMemoryReplacement(t *testing.T) {
	storage := openTestStore(t)
	defer storage.Close()
	ctx := context.Background()
	base := time.Now().UTC().Truncate(time.Second).Add(time.Hour)
	findingID := prepareFinding(t, storage, base)
	sourceUpdatedAt := base.Add(3 * time.Second)

	first, err := storage.AcceptFeedbackEvent(ctx, FeedbackEvent{
		DeliveryID: "same-second-create", GitLabInstance: "http://gitlab.internal",
		ProjectID: 42, ProjectPath: "group/project", MergeRequestIID: 7,
		NoteID: 91, ActorID: 12, Action: "create", SourceUpdatedAt: sourceUpdatedAt,
	}, sourceUpdatedAt)
	if err != nil || first.Outcome != FeedbackOutcomeQueued {
		t.Fatalf("AcceptFeedbackEvent(create) = %+v, %v", first, err)
	}
	job, err := storage.ClaimFeedbackJob(ctx, "first-owner", sourceUpdatedAt, time.Minute, 5)
	if err != nil || job == nil {
		t.Fatalf("ClaimFeedbackJob(first) = %+v, %v", job, err)
	}
	firstDecision := FeedbackDecision{
		MemoryID: "WT-M-" + strings.Repeat("A", 26), TargetType: "finding", TargetID: findingID,
		Outcome: "rejects_finding", Confidence: "high", Lesson: "First memory version.",
	}
	memoryUpdatedAt := sourceUpdatedAt.Add(time.Second)
	if err := storage.CompleteFeedbackJob(ctx, job.ID, job.SourceEventID, "first-owner", 40, "maintainer",
		"http://gitlab.internal/group/project/-/merge_requests/7#note_91", []FeedbackDecision{firstDecision}, memoryUpdatedAt, 5*time.Minute); err != nil {
		t.Fatal(err)
	}

	second, err := storage.AcceptFeedbackEvent(ctx, FeedbackEvent{
		DeliveryID: "same-second-update", GitLabInstance: "http://gitlab.internal",
		ProjectID: 42, ProjectPath: "group/project", MergeRequestIID: 7,
		NoteID: 91, ActorID: 12, Action: "update", SourceUpdatedAt: sourceUpdatedAt.Add(500 * time.Millisecond),
	}, memoryUpdatedAt)
	if err != nil || second.Outcome != FeedbackOutcomeQueued || second.EventID == first.EventID {
		t.Fatalf("AcceptFeedbackEvent(update) = %+v, %v", second, err)
	}
	updatedJob, err := storage.ClaimFeedbackJob(ctx, "second-owner", memoryUpdatedAt, time.Minute, 5)
	if err != nil || updatedJob == nil || updatedJob.SourceEventID != second.EventID {
		t.Fatalf("ClaimFeedbackJob(update) = %+v, %v", updatedJob, err)
	}
	secondDecision := firstDecision
	secondDecision.Lesson = "Second memory version."
	err = storage.CompleteFeedbackJob(ctx, updatedJob.ID, updatedJob.SourceEventID, "second-owner", 40, "maintainer",
		"http://gitlab.internal/group/project/-/merge_requests/7#note_91", []FeedbackDecision{secondDecision}, memoryUpdatedAt, 5*time.Minute)
	if !errors.Is(err, ErrMemoryVersionConflict) {
		t.Fatalf("same-second CompleteFeedbackJob() error = %v", err)
	}
	var active, count int
	var lesson, storedUpdatedAt string
	if err := storage.db.QueryRow(`
SELECT count(*), active, lesson, updated_at
FROM review_memories
WHERE feedback_job_id = ?`, updatedJob.ID).Scan(&count, &active, &lesson, &storedUpdatedAt); err != nil {
		t.Fatal(err)
	}
	if count != 1 || active != 0 || lesson != firstDecision.Lesson || storedUpdatedAt != formatTime(memoryUpdatedAt) {
		t.Fatalf("preserved memory count=%d active=%d lesson=%q updated_at=%q", count, active, lesson, storedUpdatedAt)
	}

	retryAt := memoryUpdatedAt.Add(5 * time.Second)
	state, err := storage.RetryFeedbackJob(ctx, updatedJob.ID, updatedJob.SourceEventID, "second-owner",
		memoryUpdatedAt, retryAt, 5, "memory_version_conflict")
	if err != nil || state != FeedbackQueued {
		t.Fatalf("RetryFeedbackJob() = %q, %v", state, err)
	}
	retriedJob, err := storage.ClaimFeedbackJob(ctx, "retry-owner", retryAt, time.Minute, 5)
	if err != nil || retriedJob == nil || retriedJob.SourceEventID != second.EventID {
		t.Fatalf("ClaimFeedbackJob(retry) = %+v, %v", retriedJob, err)
	}
	if err := storage.CompleteFeedbackJob(ctx, retriedJob.ID, retriedJob.SourceEventID, "retry-owner", 40, "maintainer",
		"http://gitlab.internal/group/project/-/merge_requests/7#note_91", []FeedbackDecision{secondDecision}, retryAt, 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := storage.db.QueryRow(`SELECT count(*), active, lesson, updated_at FROM review_memories WHERE feedback_job_id = ?`,
		retriedJob.ID).Scan(&count, &active, &lesson, &storedUpdatedAt); err != nil {
		t.Fatal(err)
	}
	if count != 1 || active != 1 || lesson != secondDecision.Lesson || storedUpdatedAt != formatTime(retryAt) {
		t.Fatalf("replaced memory count=%d active=%d lesson=%q updated_at=%q", count, active, lesson, storedUpdatedAt)
	}

	stale, err := storage.AcceptFeedbackEvent(ctx, FeedbackEvent{
		DeliveryID: "older-update", GitLabInstance: "http://gitlab.internal",
		ProjectID: 42, ProjectPath: "group/project", MergeRequestIID: 7,
		NoteID: 91, ActorID: 12, Action: "update", SourceUpdatedAt: sourceUpdatedAt.Add(-time.Second),
	}, retryAt.Add(time.Second))
	if err != nil || stale.Outcome != FeedbackOutcomeStale {
		t.Fatalf("AcceptFeedbackEvent(stale) = %+v, %v", stale, err)
	}
}

func TestFeedbackCompletionRejectsTargetsOutsideBoundReview(t *testing.T) {
	storage := openTestStore(t)
	defer storage.Close()
	now := time.Now().UTC().Add(time.Second)
	prepareFinding(t, storage, now)
	accepted, err := storage.AcceptFeedbackEvent(context.Background(), FeedbackEvent{
		DeliveryID: "target-check", GitLabInstance: "http://gitlab.internal", ProjectID: 42,
		ProjectPath: "group/project", MergeRequestIID: 7, NoteID: 91, ActorID: 12,
		Action: "create", SourceUpdatedAt: now,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	job, err := storage.ClaimFeedbackJob(context.Background(), "feedback-owner", now, time.Minute, 5)
	if err != nil || job == nil {
		t.Fatalf("ClaimFeedbackJob() = %+v, %v", job, err)
	}
	wrongTarget := review.ReviewID(job.GitLabInstance, job.ProjectID, job.MergeRequestIID, strings.Repeat("f", 40))
	err = storage.CompleteFeedbackJob(context.Background(), job.ID, accepted.EventID, "feedback-owner", 40, "maintainer",
		"http://gitlab.internal/group/project/-/merge_requests/7#note_91", []FeedbackDecision{{
			MemoryID: "WT-M-" + strings.Repeat("Z", 26), TargetType: "review", TargetID: wrongTarget,
			Outcome: "corrects_review", Confidence: "high",
		}}, now.Add(time.Second), 5*time.Minute)
	if err == nil {
		t.Fatal("CompleteFeedbackJob() accepted a review target outside the bound review")
	}
	assertCount(t, storage.db, "review_memories", 0)
}

func TestFeedbackSourceTimestampChangeDeactivatesMemory(t *testing.T) {
	storage := openTestStore(t)
	defer storage.Close()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second).Add(time.Second)
	findingID := prepareFinding(t, storage, now)
	accepted, err := storage.AcceptFeedbackEvent(ctx, FeedbackEvent{
		DeliveryID: "note-create", GitLabInstance: "http://gitlab.internal",
		ProjectID: 42, ProjectPath: "group/project", MergeRequestIID: 7,
		NoteID: 91, ActorID: 12, Action: "create", SourceUpdatedAt: now,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	job, err := storage.ClaimFeedbackJob(ctx, "feedback-owner", now, 3*time.Minute, 5)
	if err != nil || job == nil {
		t.Fatalf("ClaimFeedbackJob() = %+v, %v", job, err)
	}
	decision := FeedbackDecision{
		MemoryID: "WT-M-" + strings.Repeat("A", 26), TargetType: "finding", TargetID: findingID,
		Outcome: "rejects_finding", Confidence: "high", Lesson: "Generated files are maintained through their generator.",
	}
	if err := storage.CompleteFeedbackJob(ctx, job.ID, accepted.EventID, "feedback-owner", 40, "maintainer",
		"http://gitlab.internal/group/project/-/merge_requests/7#note_91", []FeedbackDecision{decision}, now.Add(time.Second), 5*time.Minute); err != nil {
		t.Fatal(err)
	}

	if err := storage.ReconcileFeedbackSource(ctx, job.ID, true, now.Add(403*time.Millisecond), now.Add(6*time.Minute), 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	var active int
	var sourceCheckStopped bool
	if err := storage.db.QueryRow(`
SELECT m.active, j.next_source_check_at IS NULL
FROM review_memories m
JOIN feedback_jobs j ON j.id = m.feedback_job_id
WHERE j.id = ?`, job.ID).Scan(&active, &sourceCheckStopped); err != nil {
		t.Fatal(err)
	}
	if active != 1 || sourceCheckStopped {
		t.Fatalf("same-second memory active=%d source_check_stopped=%t", active, sourceCheckStopped)
	}

	if err := storage.ReconcileFeedbackSource(ctx, job.ID, true, now.Add(2*time.Second), now.Add(7*time.Minute), 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := storage.db.QueryRow(`
SELECT m.active, j.next_source_check_at IS NULL
FROM review_memories m
JOIN feedback_jobs j ON j.id = m.feedback_job_id
WHERE j.id = ?`, job.ID).Scan(&active, &sourceCheckStopped); err != nil {
		t.Fatal(err)
	}
	if active != 0 || !sourceCheckStopped {
		t.Fatalf("timestamp-changed memory active=%d source_check_stopped=%t", active, sourceCheckStopped)
	}
}

func TestSupersededFeedbackFailureCannotFailNewUpdate(t *testing.T) {
	storage := openTestStore(t)
	defer storage.Close()
	ctx := context.Background()
	now := time.Now().UTC().Add(time.Second)
	prepareFinding(t, storage, now)
	first, err := storage.AcceptFeedbackEvent(ctx, FeedbackEvent{
		DeliveryID: "first", GitLabInstance: "http://gitlab.internal", ProjectID: 42,
		ProjectPath: "group/project", MergeRequestIID: 7, NoteID: 91, ActorID: 12,
		Action: "create", SourceUpdatedAt: now,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	job, err := storage.ClaimFeedbackJob(ctx, "owner", now, time.Minute, 5)
	if err != nil || job == nil {
		t.Fatalf("ClaimFeedbackJob() = %+v, %v", job, err)
	}
	second, err := storage.AcceptFeedbackEvent(ctx, FeedbackEvent{
		DeliveryID: "second", GitLabInstance: "http://gitlab.internal", ProjectID: 42,
		ProjectPath: "group/project", MergeRequestIID: 7, NoteID: 91, ActorID: 12,
		Action: "update", SourceUpdatedAt: now.Add(time.Second),
	}, now.Add(time.Second))
	if err != nil || second.JobID != first.JobID {
		t.Fatalf("updated event = %+v, %v", second, err)
	}
	if err := storage.FinishFeedbackJob(ctx, job.ID, job.SourceEventID, "owner", "permanent_failure", now.Add(2*time.Second)); !errors.Is(err, ErrFeedbackSuperseded) {
		t.Fatalf("FinishFeedbackJob() error = %v", err)
	}
	var state string
	var sourceEventID int64
	if err := storage.db.QueryRow(`SELECT state, source_event_id FROM feedback_jobs WHERE id = ?`, job.ID).Scan(&state, &sourceEventID); err != nil {
		t.Fatal(err)
	}
	if state != FeedbackQueued || sourceEventID != second.EventID {
		t.Fatalf("feedback job state=%q source=%d", state, sourceEventID)
	}
}

func TestFeedbackEventWithoutPublishedReviewIsIgnored(t *testing.T) {
	storage := openTestStore(t)
	defer storage.Close()
	now := time.Now().UTC()
	result, err := storage.AcceptFeedbackEvent(context.Background(), FeedbackEvent{
		DeliveryID: "no-review", GitLabInstance: "http://gitlab.internal",
		ProjectID: 42, ProjectPath: "group/project", MergeRequestIID: 7,
		NoteID: 91, ActorID: 12, Action: "create", SourceUpdatedAt: now,
	}, now)
	if err != nil || result.Outcome != FeedbackOutcomeIgnoredReview || result.JobID != 0 {
		t.Fatalf("AcceptFeedbackEvent(no review) = %+v, %v", result, err)
	}
	assertCount(t, storage.db, "feedback_events", 1)
	assertCount(t, storage.db, "feedback_jobs", 0)
}

func TestFeedbackIgnoresRecoveredPublicationWithoutReviewResult(t *testing.T) {
	storage := openTestStore(t)
	defer storage.Close()
	ctx := context.Background()
	now := time.Now().UTC()
	if _, err := storage.AcceptEvent(ctx, readyEvent("recovered-review")); err != nil {
		t.Fatal(err)
	}
	job, err := storage.ClaimJob(ctx, "review-owner", now, time.Minute, 5)
	if err != nil || job == nil {
		t.Fatalf("ClaimJob() = %+v, %v", job, err)
	}
	if err := storage.CompletePublication(ctx, job.ID, "review-owner", "<!-- recovered -->", 73, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}

	result, err := storage.AcceptFeedbackEvent(ctx, FeedbackEvent{
		DeliveryID: "recovered-feedback", GitLabInstance: "http://gitlab.internal",
		ProjectID: 42, ProjectPath: "group/project", MergeRequestIID: 7,
		NoteID: 91, ActorID: 12, Action: "create", SourceUpdatedAt: now.Add(2 * time.Second),
	}, now.Add(2*time.Second))
	if err != nil || result.Outcome != FeedbackOutcomeIgnoredReview || result.JobID != 0 {
		t.Fatalf("AcceptFeedbackEvent() = %+v, %v", result, err)
	}
	assertCount(t, storage.db, "review_results", 0)
	assertCount(t, storage.db, "publications", 1)
	assertCount(t, storage.db, "feedback_jobs", 0)
}

func TestFeedbackResolvesNewestEquivalentRevisionToCanonicalReview(t *testing.T) {
	storage := openTestStore(t)
	defer storage.Close()
	ctx := context.Background()
	now := time.Now().UTC().Add(time.Hour)
	patchX := strings.Repeat("a", 40)

	canonicalX := preparePublishedReview(t, storage, now, "review-x", strings.Repeat("1", 40),
		[]byte(`{"summary":"X","findings":[]}`), nil)
	if _, err := storage.db.Exec(`
UPDATE review_jobs SET patch_id_status = ?, patch_id_sha = ? WHERE id = ?`,
		PatchIDAvailable, patchX, canonicalX); err != nil {
		t.Fatal(err)
	}
	preparePublishedReview(t, storage, now.Add(10*time.Second), "review-y", strings.Repeat("2", 40),
		[]byte(`{"summary":"Y","findings":[]}`), nil)

	event := readyEvent("equivalent-x")
	event.HeadSHA = strings.Repeat("3", 40)
	equivalent, err := storage.AcceptEvent(ctx, event)
	if err != nil {
		t.Fatal(err)
	}
	job, err := storage.ClaimJob(ctx, "equivalent-owner", now.Add(20*time.Second), time.Minute, 5)
	if err != nil || job == nil || job.ID != equivalent.JobID {
		t.Fatalf("ClaimJob(equivalent) = %+v, %v", job, err)
	}
	if err := storage.CompleteEquivalentReview(ctx, job.ID, "equivalent-owner", canonicalX, patchX, now.Add(21*time.Second)); err != nil {
		t.Fatal(err)
	}

	accepted, err := storage.AcceptFeedbackEvent(ctx, FeedbackEvent{
		DeliveryID: "feedback-after-equivalent", GitLabInstance: "http://gitlab.internal",
		ProjectID: 42, ProjectPath: "group/project", MergeRequestIID: 7,
		NoteID: 91, ActorID: 12, Action: "create", SourceUpdatedAt: now.Add(22 * time.Second),
	}, now.Add(22*time.Second))
	if err != nil || accepted.JobID == 0 {
		t.Fatalf("AcceptFeedbackEvent() = %+v, %v", accepted, err)
	}
	var boundReviewID int64
	if err := storage.db.QueryRow(`SELECT review_job_id FROM feedback_jobs WHERE id = ?`, accepted.JobID).Scan(&boundReviewID); err != nil {
		t.Fatal(err)
	}
	if boundReviewID != canonicalX {
		t.Fatalf("feedback bound review = %d, want canonical X %d", boundReviewID, canonicalX)
	}

	preparePublishedReview(t, storage, now.Add(30*time.Second), "review-z", strings.Repeat("4", 40),
		[]byte(`{"summary":"Z","findings":[]}`), nil)
	updated, err := storage.AcceptFeedbackEvent(ctx, FeedbackEvent{
		DeliveryID: "feedback-after-equivalent-update", GitLabInstance: "http://gitlab.internal",
		ProjectID: 42, ProjectPath: "group/project", MergeRequestIID: 7,
		NoteID: 91, ActorID: 12, Action: "update", SourceUpdatedAt: now.Add(32 * time.Second),
	}, now.Add(32*time.Second))
	if err != nil || updated.JobID != accepted.JobID {
		t.Fatalf("AcceptFeedbackEvent(update) = %+v, %v", updated, err)
	}
	if err := storage.db.QueryRow(`SELECT review_job_id FROM feedback_jobs WHERE id = ?`, accepted.JobID).Scan(&boundReviewID); err != nil {
		t.Fatal(err)
	}
	if boundReviewID != canonicalX {
		t.Fatalf("updated feedback rebound review = %d, want %d", boundReviewID, canonicalX)
	}
}

func TestFeedbackBindsLatestPublishedReviewWithoutFallingBackToFindings(t *testing.T) {
	storage := openTestStore(t)
	defer storage.Close()
	now := time.Now().UTC().Add(time.Second)
	prepareFinding(t, storage, now)
	newHead := strings.Repeat("b", 40)
	latestID := preparePublishedReview(t, storage, now.Add(10*time.Second), "zero-review", newHead,
		[]byte(`{"summary":"No actionable findings.","findings":[]}`), nil)

	accepted, err := storage.AcceptFeedbackEvent(context.Background(), FeedbackEvent{
		DeliveryID: "natural-feedback", GitLabInstance: "http://gitlab.internal",
		ProjectID: 42, ProjectPath: "group/project", MergeRequestIID: 7,
		NoteID: 91, ActorID: 12, Action: "create", SourceUpdatedAt: now.Add(20 * time.Second),
	}, now.Add(20*time.Second))
	if err != nil || accepted.JobID == 0 || accepted.Outcome != FeedbackOutcomeQueued {
		t.Fatalf("AcceptFeedbackEvent() = %+v, %v", accepted, err)
	}
	newerFindingID := "WT-F-" + strings.Repeat("B", 26)
	preparePublishedReview(t, storage, now.Add(30*time.Second), "newer-review", strings.Repeat("c", 40),
		[]byte(`{"summary":"A newer review.","findings":[{"priority":"P2","title":"newer","explanation":"explanation","recommendation":"recommendation","path":"file.go"}]}`), []string{newerFindingID})
	updated, err := storage.AcceptFeedbackEvent(context.Background(), FeedbackEvent{
		DeliveryID: "natural-feedback-update", GitLabInstance: "http://gitlab.internal",
		ProjectID: 42, ProjectPath: "group/project", MergeRequestIID: 7,
		NoteID: 91, ActorID: 12, Action: "update", SourceUpdatedAt: now.Add(35 * time.Second),
	}, now.Add(35*time.Second))
	if err != nil || updated.JobID != accepted.JobID || updated.Outcome != FeedbackOutcomeQueued {
		t.Fatalf("updated feedback = %+v, %v", updated, err)
	}
	job, err := storage.ClaimFeedbackJob(context.Background(), "feedback-owner", now.Add(40*time.Second), time.Minute, 5)
	if err != nil || job == nil || job.ReviewJobID != latestID || job.HeadSHA != newHead || len(job.FindingIDs) != 0 {
		t.Fatalf("ClaimFeedbackJob() = %+v, %v", job, err)
	}
}

func prepareFinding(t *testing.T, storage *Store, now time.Time) string {
	t.Helper()
	findingID := "WT-F-" + strings.Repeat("A", 26)
	result := []byte(`{"summary":"summary","findings":[{"priority":"P2","title":"title","explanation":"explanation","recommendation":"recommendation","path":"file.go"}]}`)
	preparePublishedReview(t, storage, now, "review-event", readyEvent("review-event").HeadSHA, result, []string{findingID})
	return findingID
}

func preparePublishedReview(t *testing.T, storage *Store, now time.Time, delivery, head string, result []byte, findingIDs []string) int64 {
	t.Helper()
	ctx := context.Background()
	event := readyEvent(delivery)
	event.HeadSHA = head
	if _, err := storage.AcceptEvent(ctx, event); err != nil {
		t.Fatal(err)
	}
	owner := "review-owner-" + delivery
	job, err := storage.ClaimJob(ctx, owner, now, 2*time.Minute, 5)
	if err != nil || job == nil {
		t.Fatalf("ClaimJob() = %+v, %v", job, err)
	}
	if err := storage.SaveReviewResult(ctx, job.ID, owner, result, findingIDs, nil, PatchIDUnavailable, "", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := storage.CompletePublication(ctx, job.ID, owner, "<!-- "+delivery+" -->", job.ID, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	return job.ID
}
