package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestListActiveReviewMemoriesEnforcesCurrentScopeAndState(t *testing.T) {
	storage := openTestStore(t)
	defer storage.Close()
	now := time.Now().UTC().Add(time.Hour)

	current := activateReviewMemory(t, storage, memoryFixture{
		delivery: "current-review", feedbackDelivery: "current-feedback", instance: "http://gitlab.internal",
		projectID: 42, projectPath: "group/project", head: strings.Repeat("a", 40), noteID: 91,
		findingID: "WT-F-" + strings.Repeat("A", 26), memoryID: "WT-M-" + strings.Repeat("A", 26),
		lesson: "Generated files should be assessed through their source generator.",
	}, now)
	inactive := activateReviewMemory(t, storage, memoryFixture{
		delivery: "inactive-review", feedbackDelivery: "inactive-feedback", instance: "http://gitlab.internal",
		projectID: 42, projectPath: "group/project", head: strings.Repeat("b", 40), noteID: 92,
		findingID: "WT-F-" + strings.Repeat("B", 26), memoryID: "WT-M-" + strings.Repeat("B", 26),
		lesson: "Inactive generated-file guidance.",
	}, now.Add(time.Second))
	if _, err := storage.db.Exec(`UPDATE review_memories SET active = 0 WHERE memory_id = ?`, inactive.MemoryID); err != nil {
		t.Fatal(err)
	}
	activateReviewMemory(t, storage, memoryFixture{
		delivery: "other-project-review", feedbackDelivery: "other-project-feedback", instance: "http://gitlab.internal",
		projectID: 43, projectPath: "group/other", head: strings.Repeat("c", 40), noteID: 93,
		findingID: "WT-F-" + strings.Repeat("C", 26), memoryID: "WT-M-" + strings.Repeat("C", 26),
		lesson: "Other project generated-file guidance.",
	}, now.Add(2*time.Second))
	activateReviewMemory(t, storage, memoryFixture{
		delivery: "other-instance-review", feedbackDelivery: "other-instance-feedback", instance: "http://other.internal",
		projectID: 42, projectPath: "group/project", head: strings.Repeat("d", 40), noteID: 94,
		findingID: "WT-F-" + strings.Repeat("D", 26), memoryID: "WT-M-" + strings.Repeat("D", 26),
		lesson: "Other installation generated-file guidance.",
	}, now.Add(3*time.Second))

	memories, err := storage.ListActiveReviewMemories(context.Background(), "http://gitlab.internal", 42)
	if err != nil {
		t.Fatal(err)
	}
	if len(memories) != 1 || memories[0].MemoryID != current.MemoryID || memories[0].SourceRole != "maintainer" || memories[0].SourceURL == "" {
		t.Fatalf("scoped memories = %+v", memories)
	}
}

func TestSaveReviewResultPersistsVersionedMemoryRetrieval(t *testing.T) {
	storage := openTestStore(t)
	defer storage.Close()
	ctx := context.Background()
	if _, err := storage.AcceptEvent(ctx, readyEvent("retrieval-review")); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(time.Hour)
	job, err := storage.ClaimJob(ctx, "owner", now, time.Minute, 5)
	if err != nil || job == nil {
		t.Fatalf("ClaimJob() = %+v, %v", job, err)
	}
	retrieval := ReviewMemoryRetrieval{
		MemoryID: "WT-M-" + strings.Repeat("A", 26), MemoryUpdatedAt: now.Add(-time.Hour), RetrievedAt: now.Add(-time.Second),
	}
	if err := storage.SaveReviewResult(ctx, job.ID, "owner", []byte(`{"summary":"ok","findings":[]}`), nil, []ReviewMemoryRetrieval{retrieval}, now); err != nil {
		t.Fatal(err)
	}
	var memoryID, updatedAt, retrievedAt string
	if err := storage.db.QueryRow(`SELECT memory_id, memory_updated_at, retrieved_at FROM review_memory_retrievals WHERE job_id = ?`, job.ID).Scan(&memoryID, &updatedAt, &retrievedAt); err != nil {
		t.Fatal(err)
	}
	if memoryID != retrieval.MemoryID || updatedAt != formatTime(retrieval.MemoryUpdatedAt) || retrievedAt != formatTime(retrieval.RetrievedAt) {
		t.Fatalf("stored retrieval = %q %q %q", memoryID, updatedAt, retrievedAt)
	}
}

type memoryFixture struct {
	delivery         string
	feedbackDelivery string
	instance         string
	projectID        int64
	projectPath      string
	head             string
	noteID           int64
	findingID        string
	memoryID         string
	lesson           string
}

func activateReviewMemory(t *testing.T, storage *Store, fixture memoryFixture, now time.Time) ReviewMemory {
	t.Helper()
	ctx := context.Background()
	event := readyEvent(fixture.delivery)
	event.GitLabInstance = fixture.instance
	event.ProjectID = fixture.projectID
	event.ProjectPath = fixture.projectPath
	event.HeadSHA = fixture.head
	if _, err := storage.AcceptEvent(ctx, event); err != nil {
		t.Fatal(err)
	}
	owner := "review-" + fixture.delivery
	job, err := storage.ClaimJob(ctx, owner, now, 2*time.Minute, 5)
	if err != nil || job == nil {
		t.Fatalf("ClaimJob() = %+v, %v", job, err)
	}
	result := []byte(`{"summary":"summary","findings":[{"severity":"medium","title":"title","explanation":"explanation","recommendation":"recommendation","path":"file.go"}]}`)
	if err := storage.SaveReviewResult(ctx, job.ID, owner, result, []string{fixture.findingID}, nil, now); err != nil {
		t.Fatal(err)
	}
	accepted, err := storage.AcceptFeedbackEvent(ctx, FeedbackEvent{
		DeliveryID: fixture.feedbackDelivery, GitLabInstance: fixture.instance, ProjectID: fixture.projectID,
		ProjectPath: fixture.projectPath, MergeRequestIID: 7, NoteID: fixture.noteID, ActorID: 12,
		Action: "create", SourceUpdatedAt: now,
	}, now)
	if err != nil || accepted.JobID == 0 {
		t.Fatalf("AcceptFeedbackEvent() = %+v, %v", accepted, err)
	}
	feedbackOwner := "feedback-" + fixture.feedbackDelivery
	feedbackJob, err := storage.ClaimFeedbackJob(ctx, feedbackOwner, now, 3*time.Minute, 5)
	if err != nil || feedbackJob == nil {
		t.Fatalf("ClaimFeedbackJob() = %+v, %v", feedbackJob, err)
	}
	sourceURL := fixture.instance + "/" + fixture.projectPath + "/-/merge_requests/7#note_" + fixture.feedbackDelivery
	if err := storage.CompleteFeedbackJob(ctx, feedbackJob.ID, feedbackJob.SourceEventID, feedbackOwner, 40, "maintainer", sourceURL,
		[]FeedbackDecision{{MemoryID: fixture.memoryID, FindingID: fixture.findingID, Outcome: "corrects_finding", Confidence: "high", Lesson: fixture.lesson}},
		now, 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	return ReviewMemory{MemoryID: fixture.memoryID, FindingID: fixture.findingID, Lesson: fixture.lesson, UpdatedAt: now}
}
