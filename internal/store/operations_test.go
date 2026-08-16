package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestListFailedJobsIsBoundedAndNewestFirst(t *testing.T) {
	storage := openTestStore(t)
	defer storage.Close()
	ctx := context.Background()

	jobs, truncated, err := storage.ListFailedJobs(ctx, 100)
	if err != nil || len(jobs) != 0 || truncated || jobs == nil {
		t.Fatalf("ListFailedJobs(empty) = %+v, %t, %v", jobs, truncated, err)
	}

	now := time.Now().UTC()
	for index := 1; index <= 101; index++ {
		if _, err := storage.db.Exec(`
INSERT INTO review_jobs (
    gitlab_instance, project_id, merge_request_iid, head_sha, state, created_at,
    attempt_count, next_attempt_at, last_error_category, last_error_message, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			"http://gitlab.internal", 42, index, fmt.Sprintf("%040x", index), JobFailed,
			formatTime(now), 5, formatTime(now), "failure_category", "private error message",
			formatTime(now.Add(time.Duration(index)*time.Second))); err != nil {
			t.Fatal(err)
		}
	}

	jobs, truncated, err = storage.ListFailedJobs(ctx, 100)
	if err != nil {
		t.Fatalf("ListFailedJobs() error = %v", err)
	}
	if len(jobs) != 100 || !truncated {
		t.Fatalf("ListFailedJobs() count=%d truncated=%t", len(jobs), truncated)
	}
	if jobs[0].MergeRequestIID != 101 || jobs[99].MergeRequestIID != 2 {
		t.Fatalf("ListFailedJobs() order first=%+v last=%+v", jobs[0], jobs[99])
	}
	if jobs[0].Kind != FailedJobKindReview || jobs[0].HeadSHA == "" || jobs[0].NoteID != 0 ||
		jobs[0].LastErrorCategory != "failure_category" {
		t.Fatalf("review failed job = %+v", jobs[0])
	}
}

func TestListFailedJobsCombinesJobKindsDeterministically(t *testing.T) {
	storage := openTestStore(t)
	defer storage.Close()
	now := time.Now().UTC()

	if _, err := storage.db.Exec(`
INSERT INTO review_jobs (
    gitlab_instance, project_id, merge_request_iid, head_sha, state, created_at,
    attempt_count, next_attempt_at, last_error_category, updated_at
) VALUES (?, 42, 7, ?, ?, ?, 3, ?, 'review_failure', ?)`,
		"http://gitlab.internal", strings.Repeat("a", 40), JobFailed,
		formatTime(now), formatTime(now), formatTime(now)); err != nil {
		t.Fatal(err)
	}
	feedbackID := insertFailedFeedbackJob(t, storage, 91, now)
	if _, err := storage.db.Exec(`
INSERT INTO review_jobs (
    gitlab_instance, project_id, merge_request_iid, head_sha, state, created_at,
    attempt_count, next_attempt_at, last_error_category, updated_at
) VALUES (?, 42, 8, ?, ?, ?, 1, ?, 'hidden_completed_category', ?)`,
		"http://gitlab.internal", strings.Repeat("b", 40), JobCompleted,
		formatTime(now), formatTime(now), formatTime(now.Add(time.Second))); err != nil {
		t.Fatal(err)
	}

	jobs, truncated, err := storage.ListFailedJobs(context.Background(), 100)
	if err != nil || truncated || len(jobs) != 2 {
		t.Fatalf("ListFailedJobs() = %+v, %t, %v", jobs, truncated, err)
	}
	if jobs[0].Kind != FailedJobKindFeedback || jobs[0].JobID != feedbackID || jobs[0].NoteID != 91 ||
		jobs[0].HeadSHA != "" || jobs[0].LastErrorCategory != "feedback_failure" {
		t.Fatalf("feedback failed job = %+v", jobs[0])
	}
	if jobs[1].Kind != FailedJobKindReview || jobs[1].MergeRequestIID != 7 {
		t.Fatalf("review failed job = %+v", jobs[1])
	}
}

func TestRetryFailedReviewJobPreservesCheckpoint(t *testing.T) {
	storage := openTestStore(t)
	defer storage.Close()
	ctx := context.Background()
	created, err := storage.CreateReconciledJob(ctx, ReconciledReview{
		GitLabInstance: "http://gitlab.internal", ProjectID: 42, MergeRequestIID: 7,
		HeadSHA: strings.Repeat("a", 40),
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(time.Hour)
	job, err := storage.ClaimJob(ctx, "review-owner", now, time.Minute, 5)
	if err != nil || job == nil {
		t.Fatalf("ClaimJob() = %+v, %v", job, err)
	}
	resultJSON := []byte(`{"summary":"checkpointed","findings":[]}`)
	if err := storage.SaveReviewResult(ctx, job.ID, "review-owner", resultJSON, nil, nil, PatchIDUnavailable, "", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := storage.FinishJob(ctx, job.ID, "review-owner", JobFailed, "publication_failed", "private publication error", now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.db.Exec(`UPDATE review_jobs SET lease_owner = 'stale-owner', lease_expires_at = ? WHERE id = ?`,
		formatTime(now.Add(time.Hour)), job.ID); err != nil {
		t.Fatal(err)
	}

	retriedAt := now.Add(3 * time.Second)
	if err := storage.RetryFailedReviewJob(ctx, created.JobID, retriedAt); err != nil {
		t.Fatalf("RetryFailedReviewJob() error = %v", err)
	}
	var state, nextAttempt, updatedAt string
	var attempts int
	var leaseOwner, leaseExpiry, category, message sql.NullString
	if err := storage.db.QueryRow(`
SELECT state, attempt_count, next_attempt_at, lease_owner, lease_expires_at,
       last_error_category, last_error_message, updated_at
FROM review_jobs WHERE id = ?`, job.ID).Scan(&state, &attempts, &nextAttempt, &leaseOwner,
		&leaseExpiry, &category, &message, &updatedAt); err != nil {
		t.Fatal(err)
	}
	if state != JobQueued || attempts != 0 || nextAttempt != formatTime(retriedAt) || updatedAt != formatTime(retriedAt) ||
		leaseOwner.Valid || leaseExpiry.Valid || category.Valid || message.Valid {
		t.Fatalf("retried review state=%q attempts=%d next=%q lease=%+v/%+v error=%+v/%+v updated=%q",
			state, attempts, nextAttempt, leaseOwner, leaseExpiry, category, message, updatedAt)
	}
	assertCount(t, storage.db, "review_results", 1)

	resumed, err := storage.ClaimJob(ctx, "publication-owner", retriedAt, time.Minute, 5)
	if err != nil || resumed == nil || resumed.State != JobPublishing || string(resumed.ValidatedResultJSON) != string(resultJSON) {
		t.Fatalf("resumed ClaimJob() = %+v, %v", resumed, err)
	}
	if err := storage.RetryFailedReviewJob(ctx, job.ID, retriedAt); !errors.Is(err, ErrJobNotFailed) {
		t.Fatalf("repeated RetryFailedReviewJob() error = %v", err)
	}
	if err := storage.RetryFailedReviewJob(ctx, job.ID+1000, retriedAt); !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("missing RetryFailedReviewJob() error = %v", err)
	}
}

func TestRetryFailedReviewJobRejectsOtherStates(t *testing.T) {
	storage := openTestStore(t)
	defer storage.Close()
	states := []string{JobQueued, JobRunning, JobPublishing, JobCompleted, JobObsolete}
	for index, state := range states {
		result, err := storage.db.Exec(`
INSERT INTO review_jobs (
    gitlab_instance, project_id, merge_request_iid, head_sha, state, created_at,
    attempt_count, next_attempt_at, updated_at
) VALUES (?, 42, ?, ?, ?, ?, 4, ?, ?)`,
			"http://gitlab.internal", index+1, fmt.Sprintf("%040x", index+1), state,
			formatTime(time.Now()), formatTime(time.Now()), formatTime(time.Now()))
		if err != nil {
			t.Fatal(err)
		}
		jobID, _ := result.LastInsertId()
		if err := storage.RetryFailedReviewJob(context.Background(), jobID, time.Now().UTC()); !errors.Is(err, ErrJobNotFailed) {
			t.Fatalf("state %q retry error = %v", state, err)
		}
		var storedState string
		var attempts int
		if err := storage.db.QueryRow(`SELECT state, attempt_count FROM review_jobs WHERE id = ?`, jobID).Scan(&storedState, &attempts); err != nil {
			t.Fatal(err)
		}
		if storedState != state || attempts != 4 {
			t.Fatalf("state %q changed to %q with %d attempts", state, storedState, attempts)
		}
	}
}

func TestRetryFailedFeedbackJobPreservesBindingAndMemory(t *testing.T) {
	storage := openTestStore(t)
	defer storage.Close()
	ctx := context.Background()
	now := time.Now().UTC().Add(time.Second)
	reviewJobID := preparePublishedReview(t, storage, now, "feedback-review", strings.Repeat("c", 40),
		[]byte(`{"summary":"published","findings":[]}`), nil)

	first := FeedbackEvent{
		DeliveryID: "feedback-create", GitLabInstance: "http://gitlab.internal",
		ProjectID: 42, ProjectPath: "group/project", MergeRequestIID: 7,
		NoteID: 91, ActorID: 12, Action: "create", SourceUpdatedAt: now.Add(3 * time.Second),
	}
	accepted, err := storage.AcceptFeedbackEvent(ctx, first, now.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	job, err := storage.ClaimFeedbackJob(ctx, "feedback-owner", now.Add(3*time.Second), time.Minute, 5)
	if err != nil || job == nil {
		t.Fatalf("ClaimFeedbackJob() = %+v, %v", job, err)
	}
	decision := FeedbackDecision{
		MemoryID: "WT-M-" + strings.Repeat("A", 26), TargetType: "review", TargetID: job.ReviewTargetID,
		Outcome: "supports_review", Confidence: "high", Lesson: "Keep the published review guidance.",
	}
	if err := storage.CompleteFeedbackJob(ctx, job.ID, job.SourceEventID, "feedback-owner", 40, "maintainer",
		"http://gitlab.internal/group/project/-/merge_requests/7#note_91", []FeedbackDecision{decision},
		now.Add(4*time.Second), 5*time.Minute); err != nil {
		t.Fatal(err)
	}

	updated := first
	updated.DeliveryID = "feedback-update"
	updated.Action = "update"
	updated.SourceUpdatedAt = now.Add(5 * time.Second)
	updateResult, err := storage.AcceptFeedbackEvent(ctx, updated, now.Add(5*time.Second))
	if err != nil || updateResult.JobID != accepted.JobID {
		t.Fatalf("AcceptFeedbackEvent(update) = %+v, %v", updateResult, err)
	}
	failed, err := storage.ClaimFeedbackJob(ctx, "failure-owner", now.Add(5*time.Second), time.Minute, 5)
	if err != nil || failed == nil {
		t.Fatalf("ClaimFeedbackJob(failure) = %+v, %v", failed, err)
	}
	if err := storage.FinishFeedbackJob(ctx, failed.ID, failed.SourceEventID, "failure-owner", "feedback_authorization_failed", now.Add(6*time.Second)); err != nil {
		t.Fatal(err)
	}

	var memoryActive int
	var memoryLesson, memoryUpdatedAt string
	if err := storage.db.QueryRow(`SELECT active, lesson, updated_at FROM review_memories WHERE feedback_job_id = ?`, failed.ID).
		Scan(&memoryActive, &memoryLesson, &memoryUpdatedAt); err != nil {
		t.Fatal(err)
	}
	retriedAt := now.Add(7 * time.Second)
	if err := storage.RetryFailedFeedbackJob(ctx, failed.ID, retriedAt); err != nil {
		t.Fatalf("RetryFailedFeedbackJob() error = %v", err)
	}
	var state, nextAttempt, updatedAt string
	var sourceEventID, boundReviewJobID int64
	var attempts int
	var leaseOwner, leaseExpiry, category sql.NullString
	if err := storage.db.QueryRow(`
SELECT state, attempt_count, next_attempt_at, lease_owner, lease_expires_at,
       last_error_category, updated_at, source_event_id, review_job_id
FROM feedback_jobs WHERE id = ?`, failed.ID).Scan(&state, &attempts, &nextAttempt, &leaseOwner,
		&leaseExpiry, &category, &updatedAt, &sourceEventID, &boundReviewJobID); err != nil {
		t.Fatal(err)
	}
	if state != FeedbackQueued || attempts != 0 || nextAttempt != formatTime(retriedAt) || updatedAt != formatTime(retriedAt) ||
		leaseOwner.Valid || leaseExpiry.Valid || category.Valid || sourceEventID != updateResult.EventID || boundReviewJobID != reviewJobID {
		t.Fatalf("retried feedback state=%q attempts=%d next=%q lease=%+v/%+v category=%+v source=%d review=%d updated=%q",
			state, attempts, nextAttempt, leaseOwner, leaseExpiry, category, sourceEventID, boundReviewJobID, updatedAt)
	}
	var activeAfter int
	var lessonAfter, memoryUpdatedAfter string
	if err := storage.db.QueryRow(`SELECT active, lesson, updated_at FROM review_memories WHERE feedback_job_id = ?`, failed.ID).
		Scan(&activeAfter, &lessonAfter, &memoryUpdatedAfter); err != nil {
		t.Fatal(err)
	}
	if activeAfter != memoryActive || lessonAfter != memoryLesson || memoryUpdatedAfter != memoryUpdatedAt {
		t.Fatalf("memory changed from %d/%q/%q to %d/%q/%q", memoryActive, memoryLesson, memoryUpdatedAt,
			activeAfter, lessonAfter, memoryUpdatedAfter)
	}

	resumed, err := storage.ClaimFeedbackJob(ctx, "resumed-owner", retriedAt, time.Minute, 5)
	if err != nil || resumed == nil || resumed.SourceEventID != updateResult.EventID || resumed.ReviewJobID != reviewJobID {
		t.Fatalf("resumed ClaimFeedbackJob() = %+v, %v", resumed, err)
	}
	if err := storage.RetryFailedFeedbackJob(ctx, failed.ID, retriedAt); !errors.Is(err, ErrJobNotFailed) {
		t.Fatalf("repeated RetryFailedFeedbackJob() error = %v", err)
	}
	if err := storage.RetryFailedFeedbackJob(ctx, failed.ID+1000, retriedAt); !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("missing RetryFailedFeedbackJob() error = %v", err)
	}
}

func insertFailedFeedbackJob(t *testing.T, storage *Store, noteID int64, updatedAt time.Time) int64 {
	t.Helper()
	event, err := storage.db.Exec(`
INSERT INTO feedback_events (
    delivery_id, gitlab_instance, project_id, project_path, merge_request_iid,
    note_id, actor_id, action, source_updated_at, outcome
) VALUES (?, ?, 42, 'group/project', 7, ?, 12, 'create', ?, 'queued')`,
		fmt.Sprintf("feedback-%d", noteID), "http://gitlab.internal", noteID, formatTime(updatedAt))
	if err != nil {
		t.Fatal(err)
	}
	eventID, _ := event.LastInsertId()
	result, err := storage.db.Exec(`
INSERT INTO feedback_jobs (
    source_event_id, gitlab_instance, project_id, project_path, merge_request_iid,
    note_id, actor_id, state, attempt_count, next_attempt_at, last_error_category, updated_at
) VALUES (?, ?, 42, 'group/project', 7, ?, 12, ?, 5, ?, 'feedback_failure', ?)`,
		eventID, "http://gitlab.internal", noteID, FeedbackFailed, formatTime(updatedAt), formatTime(updatedAt))
	if err != nil {
		t.Fatal(err)
	}
	jobID, _ := result.LastInsertId()
	return jobID
}
