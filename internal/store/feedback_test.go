package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
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
		MemoryID: "WT-M-" + strings.Repeat("A", 26), FindingID: findingID,
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

func TestFeedbackSourceTimestampChangeDeactivatesMemory(t *testing.T) {
	storage := openTestStore(t)
	defer storage.Close()
	ctx := context.Background()
	now := time.Now().UTC().Add(time.Second)
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
		MemoryID: "WT-M-" + strings.Repeat("A", 26), FindingID: findingID,
		Outcome: "rejects_finding", Confidence: "high", Lesson: "Generated files are maintained through their generator.",
	}
	if err := storage.CompleteFeedbackJob(ctx, job.ID, accepted.EventID, "feedback-owner", 40, "maintainer",
		"http://gitlab.internal/group/project/-/merge_requests/7#note_91", []FeedbackDecision{decision}, now.Add(time.Second), 5*time.Minute); err != nil {
		t.Fatal(err)
	}

	if err := storage.ReconcileFeedbackSource(ctx, job.ID, true, now.Add(2*time.Second), now.Add(6*time.Minute), 5*time.Minute); err != nil {
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

func TestFeedbackEventWithoutFindingsIsIgnored(t *testing.T) {
	storage := openTestStore(t)
	defer storage.Close()
	now := time.Now().UTC()
	result, err := storage.AcceptFeedbackEvent(context.Background(), FeedbackEvent{
		DeliveryID: "no-findings", GitLabInstance: "http://gitlab.internal",
		ProjectID: 42, ProjectPath: "group/project", MergeRequestIID: 7,
		NoteID: 91, ActorID: 12, Action: "create", SourceUpdatedAt: now,
	}, now)
	if err != nil || result.Outcome != FeedbackOutcomeIgnoredFindings || result.JobID != 0 {
		t.Fatalf("AcceptFeedbackEvent(no findings) = %+v, %v", result, err)
	}
	assertCount(t, storage.db, "feedback_events", 1)
	assertCount(t, storage.db, "feedback_jobs", 0)
}

func prepareFinding(t *testing.T, storage *Store, now time.Time) string {
	t.Helper()
	ctx := context.Background()
	if _, err := storage.AcceptEvent(ctx, readyEvent("review-event")); err != nil {
		t.Fatal(err)
	}
	job, err := storage.ClaimJob(ctx, "review-owner", now, 2*time.Minute, 5)
	if err != nil || job == nil {
		t.Fatalf("ClaimJob() = %+v, %v", job, err)
	}
	findingID := "WT-F-" + strings.Repeat("A", 26)
	result := []byte(`{"summary":"summary","findings":[{"severity":"medium","title":"title","explanation":"explanation","recommendation":"recommendation","path":"file.go"}]}`)
	if err := storage.SaveReviewResult(ctx, job.ID, "review-owner", result, []string{findingID}, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	return findingID
}
