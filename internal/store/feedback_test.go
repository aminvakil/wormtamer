package store

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestTerminalEventCreatesOneFeedbackJobAndOptionalMemory(t *testing.T) {
	storage := openTestStore(t)
	defer storage.Close()
	ctx := context.Background()
	now := time.Now().UTC().Add(time.Hour)
	reviewJobID := preparePublishedReview(t, storage, now, "review", strings.Repeat("a", 40), nil, nil)

	closed := terminalEvent("closed", "close", "closed", strings.Repeat("b", 40))
	accepted, err := storage.AcceptEvent(ctx, closed)
	if err != nil || accepted.FeedbackJobID == 0 || accepted.Outcome != OutcomeFeedbackQueued {
		t.Fatalf("AcceptEvent(closed) = %+v, %v", accepted, err)
	}
	duplicate, err := storage.AcceptEvent(ctx, closed)
	if err != nil || !duplicate.DuplicateDelivery || duplicate.FeedbackJobID != accepted.FeedbackJobID {
		t.Fatalf("duplicate terminal event = %+v, %v", duplicate, err)
	}
	merged := terminalEvent("merged", "merge", "merged", strings.Repeat("c", 40))
	secondTerminal, err := storage.AcceptEvent(ctx, merged)
	if err != nil || secondTerminal.Outcome != OutcomeDuplicateFeedback || secondTerminal.FeedbackJobID != accepted.FeedbackJobID {
		t.Fatalf("second terminal event = %+v, %v", secondTerminal, err)
	}
	secondDuplicate, err := storage.AcceptEvent(ctx, merged)
	if err != nil || !secondDuplicate.DuplicateDelivery || secondDuplicate.FeedbackJobID != accepted.FeedbackJobID {
		t.Fatalf("duplicate second terminal event = %+v, %v", secondDuplicate, err)
	}

	job, err := storage.ClaimFeedbackJob(ctx, now.Add(10*time.Second))
	if err != nil || job == nil || job.ReviewJobID != reviewJobID || job.HeadSHA != closed.HeadSHA || job.TerminalState != "closed" {
		t.Fatalf("ClaimFeedbackJob() = %+v, %v", job, err)
	}
	memoryID := "WT-M-" + strings.Repeat("A", 26)
	lesson := "Generated configuration is changed through its schema source."
	sourceURL := "http://gitlab.internal/group/project/-/merge_requests/7"
	if err := storage.CompleteFeedbackJob(ctx, job.ID, memoryID, lesson, sourceURL, now.Add(11*time.Second)); err != nil {
		t.Fatal(err)
	}
	var storedLesson, storedSource, state string
	if err := storage.db.QueryRow(`
SELECT m.lesson, m.source_url, j.state
FROM review_memories m JOIN feedback_jobs j ON j.id = m.feedback_job_id`).Scan(&storedLesson, &storedSource, &state); err != nil {
		t.Fatal(err)
	}
	if storedLesson != lesson || storedSource != sourceURL || state != FeedbackCompleted {
		t.Fatalf("memory lesson=%q source=%q state=%q", storedLesson, storedSource, state)
	}
}

func TestTerminalEventWithoutPublishedLocalReviewIsIgnored(t *testing.T) {
	storage := openTestStore(t)
	defer storage.Close()
	result, err := storage.AcceptEvent(context.Background(), terminalEvent("no-review", "close", "closed", strings.Repeat("a", 40)))
	if err != nil || result.Outcome != OutcomeIgnoredFeedbackNoReview || result.FeedbackJobID != 0 {
		t.Fatalf("AcceptEvent(no review) = %+v, %v", result, err)
	}
	assertCount(t, storage.db, "webhook_events", 1)
	assertCount(t, storage.db, "feedback_jobs", 0)
}

func TestInterruptedFeedbackCompletionRemainsAtomic(t *testing.T) {
	storage := openTestStore(t)
	defer storage.Close()
	ctx := context.Background()
	now := time.Now().UTC().Add(time.Hour)
	preparePublishedReview(t, storage, now, "recovery-review", strings.Repeat("a", 40), nil, nil)
	accepted, err := storage.AcceptEvent(ctx, terminalEvent("recovery-close", "close", "closed", strings.Repeat("b", 40)))
	if err != nil {
		t.Fatal(err)
	}
	job, err := storage.ClaimFeedbackJob(ctx, now.Add(3*time.Second))
	if err != nil || job == nil || job.ID != accepted.FeedbackJobID {
		t.Fatalf("ClaimFeedbackJob() = %+v, %v", job, err)
	}

	recoveredAt := now.Add(4 * time.Second)
	if err := storage.RecoverInterruptedJobs(ctx, recoveredAt); err != nil {
		t.Fatal(err)
	}
	memoryID := "WT-M-" + strings.Repeat("A", 26)
	sourceURL := "http://gitlab.internal/group/project/-/merge_requests/7"
	if err := storage.CompleteFeedbackJob(ctx, job.ID, memoryID, "Recovered lesson.", sourceURL, recoveredAt); !errors.Is(err, ErrJobNotRunning) {
		t.Fatalf("stale CompleteFeedbackJob() error = %v", err)
	}
	assertCount(t, storage.db, "review_memories", 0)

	recovered, err := storage.ClaimFeedbackJob(ctx, recoveredAt)
	if err != nil || recovered == nil || recovered.ID != job.ID || recovered.AttemptCount != 2 {
		t.Fatalf("recovered feedback job = %+v, %v", recovered, err)
	}
	if err := storage.CompleteFeedbackJob(ctx, recovered.ID, memoryID, "Recovered lesson.", sourceURL, recoveredAt); err != nil {
		t.Fatal(err)
	}
	if err := storage.CompleteFeedbackJob(ctx, recovered.ID, memoryID, "Recovered lesson.", sourceURL, recoveredAt); !errors.Is(err, ErrJobNotRunning) {
		t.Fatalf("repeated CompleteFeedbackJob() error = %v", err)
	}
	assertCount(t, storage.db, "review_memories", 1)
}

func TestClaimFeedbackJobRollsBackWhenReviewContextIsUnavailable(t *testing.T) {
	storage := openTestStore(t)
	defer storage.Close()
	ctx := context.Background()
	now := time.Now().UTC().Add(time.Hour)
	reviewJobID := preparePublishedReview(t, storage, now, "invalid-feedback-review", strings.Repeat("a", 40), nil, nil)
	accepted, err := storage.AcceptEvent(ctx, terminalEvent("invalid-feedback-close", "close", "closed", strings.Repeat("b", 40)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.db.Exec(`UPDATE review_jobs SET state = ? WHERE id = ?`, JobFailed, reviewJobID); err != nil {
		t.Fatal(err)
	}
	if job, err := storage.ClaimFeedbackJob(ctx, now.Add(3*time.Second)); err == nil || job != nil {
		t.Fatalf("ClaimFeedbackJob() = %+v, %v", job, err)
	}
	var state string
	var attempts int
	if err := storage.db.QueryRow(`SELECT state, attempt_count FROM feedback_jobs WHERE id = ?`, accepted.FeedbackJobID).Scan(&state, &attempts); err != nil {
		t.Fatal(err)
	}
	if state != FeedbackQueued || attempts != 0 {
		t.Fatalf("rolled-back feedback claim state=%q attempts=%d", state, attempts)
	}
}

func TestClaimFeedbackJobIsAtomic(t *testing.T) {
	storage := openTestStore(t)
	defer storage.Close()
	now := time.Now().UTC().Add(time.Hour)
	preparePublishedReview(t, storage, now, "atomic-review", strings.Repeat("a", 40), nil, nil)
	if _, err := storage.AcceptEvent(context.Background(), terminalEvent("atomic-close", "close", "closed", strings.Repeat("b", 40))); err != nil {
		t.Fatal(err)
	}

	const contenders = 10
	jobs := make(chan *FeedbackJob, contenders)
	errors := make(chan error, contenders)
	var wait sync.WaitGroup
	for range contenders {
		wait.Add(1)
		go func() {
			defer wait.Done()
			job, err := storage.ClaimFeedbackJob(context.Background(), now)
			jobs <- job
			errors <- err
		}()
	}
	wait.Wait()
	close(jobs)
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatalf("ClaimFeedbackJob() error = %v", err)
		}
	}
	claimed := 0
	for job := range jobs {
		if job != nil {
			claimed++
		}
	}
	if claimed != 1 {
		t.Fatalf("claimed feedback jobs = %d, want 1", claimed)
	}
}

func TestFeedbackCompletionMayDeclineMemory(t *testing.T) {
	storage := openTestStore(t)
	defer storage.Close()
	now := time.Now().UTC().Add(time.Hour)
	preparePublishedReview(t, storage, now, "review", strings.Repeat("a", 40), nil, nil)
	accepted, err := storage.AcceptEvent(context.Background(), terminalEvent("closed", "close", "closed", strings.Repeat("a", 40)))
	if err != nil {
		t.Fatal(err)
	}
	job, err := storage.ClaimFeedbackJob(context.Background(), now.Add(10*time.Second))
	if err != nil || job == nil || job.ID != accepted.FeedbackJobID {
		t.Fatalf("ClaimFeedbackJob() = %+v, %v", job, err)
	}
	if err := storage.CompleteFeedbackJob(context.Background(), job.ID, "", "",
		"http://gitlab.internal/group/project/-/merge_requests/7", now.Add(11*time.Second)); err != nil {
		t.Fatal(err)
	}
	assertCount(t, storage.db, "review_memories", 0)
	var state string
	if err := storage.db.QueryRow(`SELECT state FROM feedback_jobs WHERE id = ?`, job.ID).Scan(&state); err != nil || state != FeedbackCompleted {
		t.Fatalf("state=%q error=%v", state, err)
	}
}

func terminalEvent(delivery, action, state, head string) Event {
	return Event{
		DeliveryID: delivery, GitLabInstance: "http://gitlab.internal", ProjectID: 42,
		ProjectPath: "group/project", MergeRequestIID: 7, HeadSHA: head,
		Action: action, Payload: []byte(`{"object_kind":"merge_request"}`),
		QueueFeedback: true, TerminalState: state,
	}
}

func preparePublishedReview(t *testing.T, storage *Store, now time.Time, delivery, head string, result []byte, findingIDs []string) int64 {
	t.Helper()
	event := readyEvent(delivery)
	event.HeadSHA = head
	accepted, err := storage.AcceptEvent(context.Background(), event)
	if err != nil {
		t.Fatal(err)
	}
	job, err := storage.ClaimJob(context.Background(), now)
	if err != nil || job == nil || job.ID != accepted.JobID {
		t.Fatalf("ClaimJob() = %+v, %v", job, err)
	}
	if result == nil {
		result = []byte(`{"summary":"review summary","findings":[]}`)
	}
	if err := storage.SaveReviewResult(context.Background(), job.ID,
		result, findingIDs, nil, PatchIDUnavailable, "", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := storage.CompletePublication(context.Background(), job.ID, "<!-- "+delivery+" -->", job.ID, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	return job.ID
}
