package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aminvakil/wormtamer/internal/review"
)

func TestReadDashboardEmptyAndCountsCurrentState(t *testing.T) {
	storage := openTestStore(t)
	defer storage.Close()
	ctx := context.Background()

	empty, err := storage.ReadDashboard(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(empty.ReviewCounts) != 0 || len(empty.FeedbackCounts) != 0 || empty.OldestQueuedReview != nil ||
		empty.OldestQueuedFeedback != nil || empty.ActiveMemoryCount != 0 ||
		len(empty.RecentReviews) != 0 || len(empty.RecentFeedback) != 0 {
		t.Fatalf("empty dashboard = %+v", empty)
	}

	first, err := storage.AcceptEvent(ctx, readyEvent("panel-dashboard-review"))
	if err != nil {
		t.Fatal(err)
	}
	queuedAt := time.Now().UTC().Add(-time.Hour)
	if _, err := storage.db.Exec(`UPDATE review_jobs SET created_at = ? WHERE id = ?`, formatTime(queuedAt), first.JobID); err != nil {
		t.Fatal(err)
	}
	feedbackID := insertFailedFeedbackJob(t, storage, 91, queuedAt.Add(time.Minute))
	if _, err := storage.db.Exec(`UPDATE feedback_jobs SET state = ?, last_error_category = NULL WHERE id = ?`, FeedbackQueued, feedbackID); err != nil {
		t.Fatal(err)
	}

	dashboard, err := storage.ReadDashboard(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if stateCount(dashboard.ReviewCounts, JobQueued) != 1 || stateCount(dashboard.FeedbackCounts, FeedbackQueued) != 1 {
		t.Fatalf("dashboard counts = review %+v feedback %+v", dashboard.ReviewCounts, dashboard.FeedbackCounts)
	}
	if dashboard.OldestQueuedReview == nil || !dashboard.OldestQueuedReview.Equal(queuedAt) ||
		dashboard.OldestQueuedFeedback == nil || len(dashboard.RecentReviews) != 1 || len(dashboard.RecentFeedback) != 1 {
		t.Fatalf("dashboard = %+v", dashboard)
	}
	if _, err := storage.ReadDashboard(ctx, 21); err == nil {
		t.Fatal("ReadDashboard accepted an excessive recent limit")
	}
}

func TestReviewRecordsAndDetail(t *testing.T) {
	storage := openTestStore(t)
	defer storage.Close()
	ctx := context.Background()
	now := time.Now().UTC().Add(time.Hour)

	event := readyEvent("panel-review")
	accepted, err := storage.AcceptEvent(ctx, event)
	if err != nil {
		t.Fatal(err)
	}
	job, err := storage.ClaimJob(ctx, "panel-owner", now, 2*time.Minute, 5)
	if err != nil || job == nil {
		t.Fatalf("ClaimJob() = %+v, %v", job, err)
	}
	findingID := review.FindingID(event.GitLabInstance, event.ProjectID, event.MergeRequestIID, event.HeadSHA, 1)
	result := []byte(`{"summary":"summary <script>alert(1)</script>","findings":[{"priority":"P2","title":"title","explanation":"explanation","recommendation":"recommendation","path":"file.go"}]}`)
	memoryID := "WT-M-" + strings.Repeat("A", 26)
	memoryUpdated := now.Add(-time.Minute)
	retrieved := now.Add(time.Second)
	if err := storage.SaveReviewResult(ctx, job.ID, "panel-owner", result, []string{findingID}, []ReviewMemoryRetrieval{{
		MemoryID: memoryID, MemoryUpdatedAt: memoryUpdated, RetrievedAt: retrieved,
	}}, PatchIDAvailable, strings.Repeat("d", 40), now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := storage.CompletePublication(ctx, job.ID, "panel-owner", "private-publication-marker", 81, now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.db.Exec(`UPDATE review_jobs SET last_error_message = 'private stored error' WHERE id = ?`, job.ID); err != nil {
		t.Fatal(err)
	}

	reconciled, err := storage.CreateReconciledJob(ctx, ReconciledReview{
		GitLabInstance: event.GitLabInstance, ProjectID: 55, MergeRequestIID: 9,
		HeadSHA: strings.Repeat("b", 40),
	})
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := storage.ClaimJob(ctx, "recovery-owner", now.Add(4*time.Second), 2*time.Minute, 5)
	if err != nil || recovered == nil || recovered.ID != reconciled.JobID {
		t.Fatalf("ClaimJob(recovered) = %+v, %v", recovered, err)
	}
	if err := storage.CompletePublication(ctx, recovered.ID, "recovery-owner", "external-marker", 82, now.Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}

	firstPage, err := storage.ListReviewRecords(ctx, "", 0, 1)
	if err != nil || len(firstPage.Records) != 1 || firstPage.NextBefore == 0 ||
		firstPage.Records[0].ID != recovered.ID || firstPage.Records[0].ProjectPath != "" ||
		firstPage.Records[0].Source != "reconciled" || !firstPage.Records[0].ExternalOnly {
		t.Fatalf("first review page = %+v, %v", firstPage, err)
	}
	secondPage, err := storage.ListReviewRecords(ctx, "", firstPage.NextBefore, 1)
	if err != nil || len(secondPage.Records) != 1 || secondPage.Records[0].ID != accepted.JobID ||
		secondPage.Records[0].ProjectPath != event.ProjectPath || secondPage.Records[0].Source != "webhook" ||
		secondPage.Records[0].FindingCount != 1 || !secondPage.Records[0].Published {
		t.Fatalf("second review page = %+v, %v", secondPage, err)
	}
	completed, err := storage.ListReviewRecords(ctx, JobCompleted, 0, 10)
	if err != nil || len(completed.Records) != 2 {
		t.Fatalf("completed reviews = %+v, %v", completed, err)
	}
	if _, err := storage.ListReviewRecords(ctx, "unknown", 0, 10); err == nil {
		t.Fatal("ListReviewRecords accepted an unknown state")
	}

	detail, err := storage.GetReviewRecord(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.ReviewID != review.ReviewID(event.GitLabInstance, event.ProjectID, event.MergeRequestIID, event.HeadSHA) ||
		detail.PatchIDStatus != PatchIDAvailable || detail.PatchIDSHA != strings.Repeat("d", 40) ||
		detail.Result == nil || detail.Result.Summary != "summary <script>alert(1)</script>" ||
		len(detail.Result.Findings) != 1 || detail.Result.Findings[0].ID != findingID ||
		!detail.Published || detail.GitLabNoteID != 81 || len(detail.Retrievals) != 1 ||
		detail.Retrievals[0].MemoryID != memoryID || !detail.Retrievals[0].RetrievedAt.Equal(retrieved) {
		t.Fatalf("review detail = %+v", detail)
	}
	external, err := storage.GetReviewRecord(ctx, recovered.ID)
	if err != nil || !external.ExternalOnly || external.PatchIDStatus != PatchIDUnknown || external.Result != nil || external.GitLabNoteID != 82 {
		t.Fatalf("external detail = %+v, %v", external, err)
	}
	if _, err := storage.GetReviewRecord(ctx, recovered.ID+1000); !errors.Is(err, ErrReviewRecordNotFound) {
		t.Fatalf("missing review error = %v", err)
	}
}

func TestFeedbackAndMemoryRecords(t *testing.T) {
	storage := openTestStore(t)
	defer storage.Close()
	ctx := context.Background()
	now := time.Now().UTC().Add(time.Hour)
	reviewJobID := preparePublishedReview(t, storage, now, "panel-feedback-review", strings.Repeat("c", 40),
		[]byte(`{"summary":"published","findings":[]}`), nil)

	activeJobID := completePanelFeedback(t, storage, reviewJobID, 91, now.Add(5*time.Second), "Keep <b>bounded</b> behavior.")
	inactiveJobID := completePanelFeedback(t, storage, reviewJobID, 92, now.Add(10*time.Second), "")
	if _, err := storage.db.Exec(`UPDATE feedback_jobs SET last_error_category = 'private_category' WHERE id = ?`, inactiveJobID); err != nil {
		t.Fatal(err)
	}

	feedback, err := storage.ListFeedbackRecords(ctx, FeedbackCompleted, 0, 1)
	if err != nil || len(feedback.Records) != 1 || feedback.NextBefore == 0 {
		t.Fatalf("feedback first page = %+v, %v", feedback, err)
	}
	first := feedback.Records[0]
	if first.ID != inactiveJobID || first.ReviewJobID != reviewJobID || first.ReviewID == "" ||
		first.DecisionCount != 1 || first.ActiveLessonCount != 0 || first.LastErrorCategory != "private_category" {
		t.Fatalf("feedback record = %+v", first)
	}
	feedback, err = storage.ListFeedbackRecords(ctx, FeedbackCompleted, feedback.NextBefore, 1)
	if err != nil || len(feedback.Records) != 1 || feedback.Records[0].ID != activeJobID ||
		feedback.Records[0].ActiveLessonCount != 1 {
		t.Fatalf("feedback second page = %+v, %v", feedback, err)
	}
	detail, err := storage.GetFeedbackRecord(ctx, activeJobID)
	if err != nil || detail.ID != activeJobID || detail.ReviewJobID != reviewJobID || detail.ActiveLessonCount != 1 || len(detail.Generations) != 0 {
		t.Fatalf("feedback detail = %+v, %v", detail, err)
	}
	if _, err := storage.GetFeedbackRecord(ctx, inactiveJobID+1000); !errors.Is(err, ErrFeedbackRecordNotFound) {
		t.Fatalf("missing feedback error = %v", err)
	}

	all, err := storage.ListMemoryRecords(ctx, nil, 0, 10)
	if err != nil || len(all.Records) != 2 {
		t.Fatalf("all memory = %+v, %v", all, err)
	}
	active := true
	activePage, err := storage.ListMemoryRecords(ctx, &active, 0, 10)
	if err != nil || len(activePage.Records) != 1 || activePage.Records[0].Lesson != "Keep <b>bounded</b> behavior." ||
		activePage.Records[0].SourceRole != "maintainer" || activePage.Records[0].ProjectPath != "group/project" {
		t.Fatalf("active memory = %+v, %v", activePage, err)
	}
	inactive := false
	inactivePage, err := storage.ListMemoryRecords(ctx, &inactive, 0, 10)
	if err != nil || len(inactivePage.Records) != 1 || inactivePage.Records[0].Active || inactivePage.Records[0].Lesson != "" {
		t.Fatalf("inactive memory = %+v, %v", inactivePage, err)
	}
	dashboard, err := storage.ReadDashboard(ctx, 10)
	if err != nil || dashboard.ActiveMemoryCount != 1 {
		t.Fatalf("dashboard active memories = %d, %v", dashboard.ActiveMemoryCount, err)
	}
}

func completePanelFeedback(t *testing.T, storage *Store, reviewJobID, noteID int64, now time.Time, lesson string) int64 {
	t.Helper()
	event := FeedbackEvent{
		DeliveryID:     "panel-note-" + time.Unix(noteID, 0).UTC().Format("150405"),
		GitLabInstance: "http://gitlab.internal", ProjectID: 42, ProjectPath: "group/project",
		MergeRequestIID: 7, NoteID: noteID, ActorID: 12, Action: "create", SourceUpdatedAt: now,
	}
	accepted, err := storage.AcceptFeedbackEvent(context.Background(), event, now)
	if err != nil {
		t.Fatal(err)
	}
	job, err := storage.ClaimFeedbackJob(context.Background(), "feedback-owner-"+time.Unix(noteID, 0).UTC().Format("150405"), now, time.Minute, 5)
	if err != nil || job == nil || job.ReviewJobID != reviewJobID {
		t.Fatalf("ClaimFeedbackJob() = %+v, %v", job, err)
	}
	memoryCharacter := "A"
	if noteID%2 == 0 {
		memoryCharacter = "B"
	}
	decision := FeedbackDecision{
		MemoryID:   "WT-M-" + strings.Repeat(memoryCharacter, 26),
		TargetType: "review", TargetID: job.ReviewTargetID, Outcome: "supports_review", Confidence: "high", Lesson: lesson,
	}
	if err := storage.CompleteFeedbackJob(context.Background(), job.ID, job.SourceEventID,
		"feedback-owner-"+time.Unix(noteID, 0).UTC().Format("150405"), 40, "maintainer",
		"http://gitlab.internal/group/project/-/merge_requests/7#note_"+time.Unix(noteID, 0).UTC().Format("05"),
		[]FeedbackDecision{decision}, now.Add(time.Second), 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	return accepted.JobID
}

func stateCount(counts []StateCount, state string) int {
	for _, count := range counts {
		if count.State == state {
			return count.Count
		}
	}
	return 0
}
