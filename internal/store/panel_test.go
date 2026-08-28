package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestPanelReadsTerminalFeedbackAndMemory(t *testing.T) {
	storage := openTestStore(t)
	defer storage.Close()
	ctx := context.Background()
	now := time.Now().UTC().Add(time.Hour)
	reviewJobID := preparePublishedReview(t, storage, now, "panel-review", strings.Repeat("a", 40), nil, nil)
	accepted, err := storage.AcceptEvent(ctx, terminalEvent("panel-close", "close", "closed", strings.Repeat("b", 40)))
	if err != nil {
		t.Fatal(err)
	}
	job, err := storage.ClaimFeedbackJob(ctx, now.Add(10*time.Second))
	if err != nil || job == nil {
		t.Fatalf("ClaimFeedbackJob() = %+v, %v", job, err)
	}
	memoryID := "WT-M-" + strings.Repeat("A", 26)
	lesson := "Generated files are changed through their source generator."
	source := "http://gitlab.internal/group/project/-/merge_requests/7"
	if err := storage.CompleteFeedbackJob(ctx, job.ID, memoryID, lesson, source, now.Add(11*time.Second)); err != nil {
		t.Fatal(err)
	}

	feedback, err := storage.ListFeedbackRecords(ctx, "", 0, 10)
	if err != nil || len(feedback.Records) != 1 {
		t.Fatalf("ListFeedbackRecords() = %+v, %v", feedback, err)
	}
	record := feedback.Records[0]
	if record.ID != accepted.FeedbackJobID || record.ReviewJobID != reviewJobID || record.HeadSHA != strings.Repeat("b", 40) ||
		record.TerminalState != "closed" || !record.MemoryCreated || record.ReviewID == "" {
		t.Fatalf("feedback record = %+v", record)
	}
	detail, err := storage.GetFeedbackRecord(ctx, record.ID)
	if err != nil || detail.ID != record.ID || !detail.MemoryCreated {
		t.Fatalf("GetFeedbackRecord() = %+v, %v", detail, err)
	}
	memories, err := storage.ListMemoryRecords(ctx, 0, 10)
	if err != nil || len(memories.Records) != 1 || memories.Records[0].MemoryID != memoryID ||
		memories.Records[0].Lesson != lesson || memories.Records[0].SourceURL != source {
		t.Fatalf("ListMemoryRecords() = %+v, %v", memories, err)
	}
	dashboard, err := storage.ReadDashboard(ctx, 5)
	if err != nil || dashboard.MemoryCount != 1 || len(dashboard.RecentFeedback) != 1 {
		t.Fatalf("ReadDashboard() = %+v, %v", dashboard, err)
	}
}

func TestPanelReadsReviewResultAndPublication(t *testing.T) {
	storage := openTestStore(t)
	defer storage.Close()
	now := time.Now().UTC().Add(time.Hour)
	jobID := preparePublishedReview(t, storage, now, "panel-review-detail", strings.Repeat("c", 40), nil, nil)
	detail, err := storage.GetReviewRecord(context.Background(), jobID)
	if err != nil || detail.ID != jobID || detail.Result == nil || detail.Result.Summary != "review summary" || !detail.Published {
		t.Fatalf("GetReviewRecord() = %+v, %v", detail, err)
	}
}
