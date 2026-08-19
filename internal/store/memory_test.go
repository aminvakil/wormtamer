package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestListReviewMemoriesEnforcesRepositoryScope(t *testing.T) {
	storage := openTestStore(t)
	defer storage.Close()
	now := time.Now().UTC().Add(time.Hour)
	currentID := activateMemory(t, storage, "http://gitlab.internal", 42, "group/project", 7,
		"WT-M-"+strings.Repeat("A", 26), "Generated files are changed through their source generator.", now)
	activateMemory(t, storage, "http://gitlab.internal", 43, "group/other", 8,
		"WT-M-"+strings.Repeat("B", 26), "Other project guidance.", now.Add(time.Minute))
	activateMemory(t, storage, "http://other.internal", 42, "group/project", 9,
		"WT-M-"+strings.Repeat("C", 26), "Other installation guidance.", now.Add(2*time.Minute))

	memories, err := storage.ListReviewMemories(context.Background(), "http://gitlab.internal", 42)
	if err != nil || len(memories) != 1 || memories[0].MemoryID != currentID || memories[0].SourceURL == "" {
		t.Fatalf("scoped memories = %+v, %v", memories, err)
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
	if err := storage.SaveReviewResult(ctx, job.ID, "owner", []byte(`{"summary":"ok","findings":[]}`), nil, []ReviewMemoryRetrieval{retrieval}, PatchIDUnavailable, "", now); err != nil {
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

func activateMemory(t *testing.T, storage *Store, instance string, projectID int64, projectPath string, iid int64, memoryID, lesson string, now time.Time) string {
	t.Helper()
	head := strings.Repeat(string(rune('a'+iid%6)), 40)
	reviewEvent := readyEvent("review-" + memoryID)
	reviewEvent.GitLabInstance, reviewEvent.ProjectID, reviewEvent.ProjectPath = instance, projectID, projectPath
	reviewEvent.MergeRequestIID, reviewEvent.HeadSHA = iid, head
	accepted, err := storage.AcceptEvent(context.Background(), reviewEvent)
	if err != nil {
		t.Fatal(err)
	}
	owner := "review-owner-" + memoryID
	job, err := storage.ClaimJob(context.Background(), owner, now, time.Minute, 5)
	if err != nil || job == nil || job.ID != accepted.JobID {
		t.Fatalf("ClaimJob() = %+v, %v", job, err)
	}
	if err := storage.SaveReviewResult(context.Background(), job.ID, owner, []byte(`{"summary":"summary","findings":[]}`), nil, nil, PatchIDUnavailable, "", now); err != nil {
		t.Fatal(err)
	}
	if err := storage.CompletePublication(context.Background(), job.ID, owner, "<!-- "+memoryID+" -->", job.ID, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	terminal := Event{
		DeliveryID: "close-" + memoryID, GitLabInstance: instance, ProjectID: projectID,
		ProjectPath: projectPath, MergeRequestIID: iid, HeadSHA: head, Action: "close",
		Payload: []byte(`{"object_kind":"merge_request"}`), QueueFeedback: true, TerminalState: "closed",
	}
	feedback, err := storage.AcceptEvent(context.Background(), terminal)
	if err != nil {
		t.Fatal(err)
	}
	feedbackJob, err := storage.ClaimFeedbackJob(context.Background(), "feedback-owner-"+memoryID, now.Add(2*time.Second), time.Minute, 5)
	if err != nil || feedbackJob == nil || feedbackJob.ID != feedback.FeedbackJobID {
		t.Fatalf("ClaimFeedbackJob() = %+v, %v", feedbackJob, err)
	}
	source := instance + "/" + projectPath + "/-/merge_requests/" + string(rune('0'+iid))
	if err := storage.CompleteFeedbackJob(context.Background(), feedbackJob.ID, "feedback-owner-"+memoryID,
		memoryID, lesson, source, now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	return memoryID
}
