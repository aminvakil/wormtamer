package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestListAndRetryFailedReviewAndFeedbackJobs(t *testing.T) {
	storage := openTestStore(t)
	defer storage.Close()
	ctx := context.Background()
	now := time.Now().UTC().Add(time.Hour)

	if _, err := storage.AcceptEvent(ctx, readyEvent("failed-review")); err != nil {
		t.Fatal(err)
	}
	reviewJob, err := storage.ClaimJob(ctx, now)
	if err != nil || reviewJob == nil {
		t.Fatalf("ClaimJob() = %+v, %v", reviewJob, err)
	}
	if err := storage.FinishJob(ctx, reviewJob.ID, JobFailed, "review_failure", "review_failure", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}

	preparePublishedReview(t, storage, now.Add(2*time.Second), "feedback-review", strings.Repeat("a", 40), nil, nil)
	terminal, err := storage.AcceptEvent(ctx, terminalEvent("failed-feedback", "merge", "merged", strings.Repeat("b", 40)))
	if err != nil {
		t.Fatal(err)
	}
	feedbackJob, err := storage.ClaimFeedbackJob(ctx, now.Add(10*time.Second))
	if err != nil || feedbackJob == nil || feedbackJob.ID != terminal.FeedbackJobID {
		t.Fatalf("ClaimFeedbackJob() = %+v, %v", feedbackJob, err)
	}
	if err := storage.FinishFeedbackJob(ctx, feedbackJob.ID, "feedback_failure", now.Add(11*time.Second)); err != nil {
		t.Fatal(err)
	}

	jobs, truncated, err := storage.ListFailedJobs(ctx, 10)
	if err != nil || truncated || len(jobs) != 2 {
		t.Fatalf("ListFailedJobs() = %+v, %t, %v", jobs, truncated, err)
	}
	var foundReview, foundFeedback bool
	for _, job := range jobs {
		switch job.Kind {
		case FailedJobKindReview:
			foundReview = job.JobID == reviewJob.ID && job.HeadSHA == reviewJob.HeadSHA
		case FailedJobKindFeedback:
			foundFeedback = job.JobID == feedbackJob.ID && job.HeadSHA == feedbackJob.HeadSHA
		}
	}
	if !foundReview || !foundFeedback {
		t.Fatalf("failed jobs = %+v", jobs)
	}
	if err := storage.RetryFailedReviewJob(ctx, reviewJob.ID, now.Add(20*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := storage.RetryFailedFeedbackJob(ctx, feedbackJob.ID, now.Add(20*time.Second)); err != nil {
		t.Fatal(err)
	}
}
