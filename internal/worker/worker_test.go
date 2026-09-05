package worker

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aminvakil/wormtamer/internal/failure"
	"github.com/aminvakil/wormtamer/internal/gitlab"
	"github.com/aminvakil/wormtamer/internal/repository"
	"github.com/aminvakil/wormtamer/internal/review"
	"github.com/aminvakil/wormtamer/internal/store"
	_ "github.com/mattn/go-sqlite3"
)

const workerHead = "0123456789abcdef0123456789abcdef01234567"

func TestWorkerPreservesReviewAndWorkspaceCleanupFailures(t *testing.T) {
	storage, db := workerStore(t)
	defer storage.Close()
	defer db.Close()
	queueJob(t, storage)

	reviewErr := failure.Retry("review_failed", 0)
	closeErr := errors.New("close failed")
	workspaces := &fakeWorkspaces{closeErr: closeErr}
	worker := New(storage, &fakeGitLab{}, workspaces, &fakeReviewer{err: reviewErr}, slog.Default(), nil)
	job, err := storage.ClaimJob(context.Background(), time.Now().UTC())
	if err != nil || job == nil {
		t.Fatalf("claim = %+v, %v", job, err)
	}
	err = worker.execute(context.Background(), job)
	if !errors.Is(err, reviewErr) || !errors.Is(err, closeErr) {
		t.Fatalf("combined error = %v", err)
	}
	var failureError *failure.Error
	if !errors.As(err, &failureError) || failureError.Category != "repository_workspace_cleanup_failed" {
		t.Fatalf("prioritized failure = %v", err)
	}
}

func TestWorkerRetrievesScopedMemoryAndPersistsSuccessfulAudit(t *testing.T) {
	storage, db := workerStore(t)
	defer storage.Close()
	defer db.Close()
	now := time.Now().UTC().Add(time.Hour)
	memoryID := prepareMemoryForWorker(t, storage, now)
	queueNewWorkerRevision(t, storage, "memory-review", strings.Repeat("b", 40))

	reviewer := &fakeReviewer{result: review.Result{Summary: "used current evidence", Findings: []review.Finding{}}}
	worker := newTestWorker(t, storage, &fakeGitLab{}, reviewer, nil)
	worker.now = func() time.Time { return now.Add(10 * time.Second) }
	processed, err := worker.ProcessOne(context.Background())
	if err != nil || !processed {
		t.Fatalf("ProcessOne() = %t, %v", processed, err)
	}
	if len(reviewer.memories) != 1 || reviewer.memories[0].ID != memoryID ||
		reviewer.memories[0].Lesson == "" || reviewer.memories[0].SourceURL == "" {
		t.Fatalf("materialized memories = %+v", reviewer.memories)
	}
	var auditMemoryID string
	if err := db.QueryRow(`SELECT memory_id FROM review_memory_retrievals`).Scan(&auditMemoryID); err != nil {
		t.Fatal(err)
	}
	if auditMemoryID != memoryID {
		t.Fatalf("audit memory ID = %q", auditMemoryID)
	}
	assertCount(t, db, "review_memory_retrievals", 1)
}

func TestWorkerRetriesMemoryStoreFailureWithoutPublishing(t *testing.T) {
	storage, db := workerStore(t)
	defer storage.Close()
	defer db.Close()
	queueJob(t, storage)
	failing := &failingMemoryStore{Store: storage}
	reviewer := &fakeReviewer{result: review.Result{Summary: "degraded", Findings: []review.Finding{}}}
	broker := &fakeGitLab{}
	worker := newTestWorker(t, failing, broker, reviewer, nil)
	processed, err := worker.ProcessOne(context.Background())
	if err != nil || !processed {
		t.Fatalf("ProcessOne() = %t, %v", processed, err)
	}
	assertJobState(t, db, store.JobQueued)
	if broker.postCalls != 0 {
		t.Fatalf("memory failure published %d notes", broker.postCalls)
	}
	assertCount(t, db, "review_memory_retrievals", 0)
}

func TestWorkerCompletesEndToEndReview(t *testing.T) {
	storage, db := workerStore(t)
	defer storage.Close()
	defer db.Close()
	queueJob(t, storage)

	broker := &fakeGitLab{}
	reviewer := &fakeReviewer{result: review.Result{
		Summary: "Looks mostly good.",
		Findings: []review.Finding{{
			Priority: "P2", Title: "Check error", Explanation: "An error is ignored.",
			Recommendation: "Handle the error.", Path: "main.go",
		}},
	}}
	var logs bytes.Buffer
	workspaces := &fakeWorkspaces{}
	worker := New(storage, broker, workspaces, reviewer, slog.New(slog.NewJSONHandler(&logs, nil)),
		[]string{"gitlab-token", "gemini-key", "webhook-secret"})

	processed, err := worker.ProcessOne(context.Background())
	if err != nil || !processed {
		t.Fatalf("ProcessOne() = %t, %v", processed, err)
	}
	if workspaces.prepareCalls != 1 || workspaces.workspace == nil || !workspaces.workspace.closed {
		t.Fatalf("prepared workspace = %+v", workspaces)
	}
	if reviewer.snapshot.WorkingDirectory != "/review/current" || reviewer.snapshot.ReviewMemoryPath != "/review/review-memory.json" {
		t.Fatalf("review snapshot context = %+v", reviewer.snapshot)
	}
	assertJobState(t, db, store.JobCompleted)
	assertCount(t, db, "review_results", 1)
	assertCount(t, db, "review_findings", 1)
	assertCount(t, db, "publications", 1)
	if broker.loadCalls != 1 || broker.checkCalls != 2 || broker.postCalls != 1 || reviewer.calls != 1 {
		t.Fatalf("calls: broker=%+v reviewer=%+v", broker, reviewer)
	}
	expectedFindingID := review.FindingID("http://gitlab.internal", 42, 7, workerHead, 1)
	var storedFindingID string
	if err := db.QueryRow(`SELECT finding_id FROM review_findings`).Scan(&storedFindingID); err != nil {
		t.Fatal(err)
	}
	if storedFindingID != expectedFindingID || !strings.Contains(broker.postedBody, "<!-- wormtamer:review=") ||
		!strings.Contains(broker.postedBody, "Finding ID: `"+expectedFindingID+"`") || !strings.Contains(broker.postedBody, "Check error") {
		t.Fatalf("stored finding ID = %q, posted body = %q", storedFindingID, broker.postedBody)
	}
	for _, secret := range []string{"gitlab-token", "gemini-key", "webhook-secret", "+private-diff", "Looks mostly good."} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("logs contain %q: %s", secret, logs.String())
		}
	}
}

func TestWorkerCompletesEquivalentPatchWithoutReviewOrPublication(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wormtamer.db")
	storage, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	queueJob(t, storage)
	broker := &fakeGitLab{}
	reviewer := &fakeReviewer{result: review.Result{Summary: "No problems.", Findings: []review.Finding{}}}
	worker := newTestWorker(t, storage, broker, reviewer, nil)
	now := time.Now().UTC().Add(time.Hour)
	worker.now = func() time.Time { return now }
	if processed, err := worker.ProcessOne(context.Background()); err != nil || !processed {
		t.Fatalf("canonical ProcessOne() = %t, %v", processed, err)
	}
	if err := storage.Close(); err != nil {
		t.Fatal(err)
	}

	storage, err = store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	queueNewWorkerRevision(t, storage, "equivalent-revision", strings.Repeat("b", 40))
	now = now.Add(time.Minute)
	worker = newTestWorker(t, storage, broker, reviewer, nil)
	worker.now = func() time.Time { return now }
	if processed, err := worker.ProcessOne(context.Background()); err != nil || !processed {
		t.Fatalf("equivalent ProcessOne() = %t, %v", processed, err)
	}

	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var canonicalID, equivalentID, equivalentTo int64
	rows, err := db.Query(`SELECT id, COALESCE(equivalent_to_job_id, 0) FROM review_jobs ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("missing canonical review job")
	}
	if err := rows.Scan(&canonicalID, &equivalentTo); err != nil || equivalentTo != 0 {
		t.Fatalf("canonical review job = %d, %d, %v", canonicalID, equivalentTo, err)
	}
	if !rows.Next() {
		t.Fatal("missing equivalent review job")
	}
	if err := rows.Scan(&equivalentID, &equivalentTo); err != nil || equivalentID <= canonicalID || equivalentTo != canonicalID {
		t.Fatalf("equivalent job canonical = %d, want %d, error = %v", equivalentTo, canonicalID, err)
	}
	if rows.Next() {
		t.Fatal("unexpected extra review job")
	}
	if reviewer.calls != 1 || broker.loadCalls != 2 || broker.postCalls != 1 {
		t.Fatalf("broker=%+v reviewer calls=%d", broker, reviewer.calls)
	}
	assertCount(t, db, "review_results", 1)
	assertCount(t, db, "publications", 1)
}

func TestWorkerDoesNotCompleteEquivalentReviewAfterHeadChange(t *testing.T) {
	storage, db := workerStore(t)
	defer storage.Close()
	defer db.Close()
	queueJob(t, storage)
	broker := &fakeGitLab{}
	reviewer := &fakeReviewer{result: review.Result{Summary: "Canonical.", Findings: []review.Finding{}}}
	worker := newTestWorker(t, storage, broker, reviewer, nil)
	now := time.Now().UTC().Add(time.Hour)
	worker.now = func() time.Time { return now }
	if processed, err := worker.ProcessOne(context.Background()); err != nil || !processed {
		t.Fatalf("canonical ProcessOne() = %t, %v", processed, err)
	}

	queueNewWorkerRevision(t, storage, "changed-equivalent", strings.Repeat("b", 40))
	now = now.Add(time.Minute)
	broker.checkErrors = []error{nil, nil, failure.Obsolete("merge_request_head_changed")}
	if processed, err := worker.ProcessOne(context.Background()); err != nil || !processed {
		t.Fatalf("equivalent ProcessOne() = %t, %v", processed, err)
	}
	var state string
	var equivalentTo sql.NullInt64
	if err := db.QueryRow(`SELECT state, equivalent_to_job_id FROM review_jobs ORDER BY id DESC LIMIT 1`).Scan(&state, &equivalentTo); err != nil {
		t.Fatal(err)
	}
	if state != store.JobObsolete || equivalentTo.Valid || reviewer.calls != 1 || broker.postCalls != 1 {
		t.Fatalf("state=%q equivalent=%+v broker=%+v reviewer=%d", state, equivalentTo, broker, reviewer.calls)
	}
}

func TestWorkerDefersPendingPatchIDOnlyOnce(t *testing.T) {
	storage, db := workerStore(t)
	defer storage.Close()
	defer db.Close()
	queueJob(t, storage)
	broker := &fakeGitLab{patchIDStatus: gitlab.PatchIDPending}
	reviewer := &fakeReviewer{result: review.Result{Summary: "Reviewed without patch identity.", Findings: []review.Finding{}}}
	worker := newTestWorker(t, storage, broker, reviewer, nil)
	now := time.Now().UTC().Add(time.Hour)
	worker.now = func() time.Time { return now }

	if processed, err := worker.ProcessOne(context.Background()); err != nil || !processed {
		t.Fatalf("pending ProcessOne() = %t, %v", processed, err)
	}
	assertJobState(t, db, store.JobQueued)
	var status, category string
	if err := db.QueryRow(`SELECT patch_id_status, last_error_category FROM review_jobs`).Scan(&status, &category); err != nil {
		t.Fatal(err)
	}
	if status != store.PatchIDPending || category != "merge_request_patch_id_pending" || reviewer.calls != 0 || broker.postCalls != 0 {
		t.Fatalf("pending status=%q category=%q broker=%+v reviewer=%d", status, category, broker, reviewer.calls)
	}

	now = now.Add(initialBackoff + time.Second)
	if processed, err := worker.ProcessOne(context.Background()); err != nil || !processed {
		t.Fatalf("unavailable ProcessOne() = %t, %v", processed, err)
	}
	assertJobState(t, db, store.JobCompleted)
	var attempts int
	if err := db.QueryRow(`SELECT patch_id_status, attempt_count FROM review_jobs`).Scan(&status, &attempts); err != nil {
		t.Fatal(err)
	}
	if status != store.PatchIDUnavailable || attempts != 2 || reviewer.calls != 1 || broker.postCalls != 1 {
		t.Fatalf("final status=%q attempts=%d broker=%+v reviewer=%d", status, attempts, broker, reviewer.calls)
	}
}

func TestWorkerDoesNotDeferPendingPatchIDWithoutThreeReviewClaimsRemaining(t *testing.T) {
	storage, db := workerStore(t)
	defer storage.Close()
	defer db.Close()
	queueJob(t, storage)
	ctx := context.Background()
	now := time.Now().UTC().Add(time.Hour)
	for attempt := 1; attempt <= 2; attempt++ {
		job, err := storage.ClaimJob(ctx, now)
		if err != nil || job == nil {
			t.Fatalf("pre-patch ClaimJob(%d) = %+v, %v", attempt, job, err)
		}
		if _, err := storage.RetryJob(ctx, job.ID, now, now.Add(time.Second),
			"gitlab_network_failure", "gitlab_network_failure"); err != nil {
			t.Fatal(err)
		}
		now = now.Add(2 * time.Second)
	}

	broker := &fakeGitLab{patchIDStatus: gitlab.PatchIDPending}
	reviewer := &fakeReviewer{result: review.Result{Summary: "Reviewed with reserved claims.", Findings: []review.Finding{}}}
	worker := newTestWorker(t, storage, broker, reviewer, nil)
	worker.now = func() time.Time { return now }
	if processed, err := worker.ProcessOne(ctx); err != nil || !processed {
		t.Fatalf("ProcessOne() = %t, %v", processed, err)
	}
	assertJobState(t, db, store.JobCompleted)
	var status string
	var attempts int
	if err := db.QueryRow(`SELECT patch_id_status, attempt_count FROM review_jobs`).Scan(&status, &attempts); err != nil {
		t.Fatal(err)
	}
	if status != store.PatchIDUnavailable || attempts != 3 || reviewer.calls != 1 || broker.postCalls != 1 {
		t.Fatalf("status=%q attempts=%d broker=%+v reviewer=%d", status, attempts, broker, reviewer.calls)
	}
}

func TestWorkerSkipsReviewForExistingPublication(t *testing.T) {
	storage, db := workerStore(t)
	defer storage.Close()
	defer db.Close()
	queueJob(t, storage)

	identity := gitlab.Identity{
		GitLabInstance: "http://gitlab.internal", ProjectID: 42,
		MergeRequestIID: 7, HeadSHA: workerHead,
	}
	broker := &fakeGitLab{noteID: 73, postedBody: publicationMarker(identity)}
	reviewer := &fakeReviewer{err: errors.New("review must not run")}
	var logs bytes.Buffer
	worker := newTestWorker(t, storage, broker, reviewer, &logs)

	processed, err := worker.ProcessOne(context.Background())
	if err != nil || !processed {
		t.Fatalf("ProcessOne() = %t, %v", processed, err)
	}
	assertJobState(t, db, store.JobCompleted)
	assertCount(t, db, "review_results", 0)
	assertCount(t, db, "publications", 1)
	if reviewer.calls != 0 || broker.loadCalls != 0 || broker.findCalls != 1 || broker.checkCalls != 1 || broker.postCalls != 0 {
		t.Fatalf("broker=%+v reviewer calls=%d", broker, reviewer.calls)
	}
	var noteID int64
	if err := db.QueryRow(`SELECT gitlab_note_id FROM publications`).Scan(&noteID); err != nil || noteID != 73 {
		t.Fatalf("stored note ID = %d, error = %v", noteID, err)
	}
	if !strings.Contains(logs.String(), "review generation skipped") || !strings.Contains(logs.String(), "existing_publication") {
		t.Fatalf("skip log missing: %s", logs.String())
	}
}

func TestWorkerRejectsExistingPublicationForObsoleteRevision(t *testing.T) {
	storage, db := workerStore(t)
	defer storage.Close()
	defer db.Close()
	queueJob(t, storage)

	identity := gitlab.Identity{
		GitLabInstance: "http://gitlab.internal", ProjectID: 42,
		MergeRequestIID: 7, HeadSHA: workerHead,
	}
	broker := &fakeGitLab{
		noteID: 73, postedBody: publicationMarker(identity),
		checkError: failure.Obsolete("merge_request_head_changed"),
	}
	reviewer := &fakeReviewer{err: errors.New("review must not run")}
	worker := newTestWorker(t, storage, broker, reviewer, nil)

	processed, err := worker.ProcessOne(context.Background())
	if err != nil || !processed {
		t.Fatalf("ProcessOne() = %t, %v", processed, err)
	}
	assertJobState(t, db, store.JobObsolete)
	assertCount(t, db, "review_results", 0)
	assertCount(t, db, "publications", 0)
	if reviewer.calls != 0 || broker.loadCalls != 0 || broker.findCalls != 1 || broker.checkCalls != 1 || broker.postCalls != 0 {
		t.Fatalf("broker=%+v reviewer calls=%d", broker, reviewer.calls)
	}
}

func TestWorkerReconcilesPostAfterLocalPersistenceFailure(t *testing.T) {
	storage, db := workerStore(t)
	defer storage.Close()
	defer db.Close()
	queueJob(t, storage)
	flaky := &flakyCompletionStore{Store: storage, failNext: true}
	broker := &fakeGitLab{}
	reviewer := &fakeReviewer{result: review.Result{Summary: "No problems.", Findings: []review.Finding{}}}
	worker := newTestWorker(t, flaky, broker, reviewer, nil)
	now := time.Now().UTC().Add(time.Hour)
	worker.now = func() time.Time { return now }

	processed, err := worker.ProcessOne(context.Background())
	if err != nil || !processed {
		t.Fatalf("first ProcessOne() = %t, %v", processed, err)
	}
	assertJobState(t, db, store.JobQueued)
	if broker.postCalls != 1 || reviewer.calls != 1 {
		t.Fatalf("first calls: post=%d review=%d", broker.postCalls, reviewer.calls)
	}

	now = now.Add(initialBackoff + time.Second)
	processed, err = worker.ProcessOne(context.Background())
	if err != nil || !processed {
		t.Fatalf("second ProcessOne() = %t, %v", processed, err)
	}
	assertJobState(t, db, store.JobCompleted)
	if broker.postCalls != 1 || reviewer.calls != 1 || broker.findCalls != 3 {
		t.Fatalf("reconciled calls: post=%d review=%d find=%d", broker.postCalls, reviewer.calls, broker.findCalls)
	}
}

func TestWorkerOperatorRetryResumesPublicationWithoutReview(t *testing.T) {
	storage, db := workerStore(t)
	defer storage.Close()
	defer db.Close()
	queueJob(t, storage)
	ctx := context.Background()
	now := time.Now().UTC().Add(time.Hour)
	job, err := storage.ClaimJob(ctx, now)
	if err != nil || job == nil {
		t.Fatalf("ClaimJob() = %+v, %v", job, err)
	}
	resultJSON := []byte(`{"summary":"Already reviewed.","findings":[]}`)
	if err := storage.SaveReviewResult(ctx, job.ID, resultJSON, nil, nil, store.PatchIDUnavailable, "", now); err != nil {
		t.Fatal(err)
	}
	if err := storage.FinishJob(ctx, job.ID, store.JobFailed,
		"gitlab_authorization_failed", "gitlab_authorization_failed", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	retriedAt := now.Add(2 * time.Second)
	if err := storage.RetryFailedReviewJob(ctx, job.ID, retriedAt); err != nil {
		t.Fatal(err)
	}

	broker := &fakeGitLab{}
	reviewer := &fakeReviewer{err: errors.New("review must not run")}
	worker := newTestWorker(t, storage, broker, reviewer, nil)
	worker.now = func() time.Time { return retriedAt }
	processed, err := worker.ProcessOne(ctx)
	if err != nil || !processed {
		t.Fatalf("ProcessOne() = %t, %v", processed, err)
	}
	assertJobState(t, db, store.JobCompleted)
	if reviewer.calls != 0 || broker.loadCalls != 0 || broker.postCalls != 1 {
		t.Fatalf("calls: review=%d load=%d post=%d", reviewer.calls, broker.loadCalls, broker.postCalls)
	}
}

func TestWorkerReconcilesLostPostResponse(t *testing.T) {
	storage, db := workerStore(t)
	defer storage.Close()
	defer db.Close()
	queueJob(t, storage)
	broker := &fakeGitLab{losePostResponse: true}
	reviewer := &fakeReviewer{result: review.Result{Summary: "No problems.", Findings: []review.Finding{}}}
	worker := newTestWorker(t, storage, broker, reviewer, nil)
	now := time.Now().UTC().Add(time.Hour)
	worker.now = func() time.Time { return now }

	if processed, err := worker.ProcessOne(context.Background()); err != nil || !processed {
		t.Fatalf("first ProcessOne() = %t, %v", processed, err)
	}
	assertJobState(t, db, store.JobQueued)
	now = now.Add(initialBackoff + time.Second)
	if processed, err := worker.ProcessOne(context.Background()); err != nil || !processed {
		t.Fatalf("second ProcessOne() = %t, %v", processed, err)
	}
	assertJobState(t, db, store.JobCompleted)
	if broker.postCalls != 1 || reviewer.calls != 1 {
		t.Fatalf("calls: post=%d review=%d", broker.postCalls, reviewer.calls)
	}
}

func TestWorkerRunFailsWhenRetryCheckpointCannotPersist(t *testing.T) {
	storage, db := workerStore(t)
	defer storage.Close()
	defer db.Close()
	queueJob(t, storage)
	checkpointErr := errors.New("simulated retry checkpoint failure")
	failing := &failingRetryStore{Store: storage, err: checkpointErr}
	worker := newTestWorker(t, failing, &fakeGitLab{loadError: failure.Retry("gitlab_network_failure", 0)}, &fakeReviewer{}, nil)
	now := time.Now().UTC().Add(time.Hour)
	worker.now = func() time.Time { return now }
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if err := worker.Run(ctx); !errors.Is(err, checkpointErr) {
		t.Fatalf("Run() error = %v", err)
	}
	assertJobState(t, db, store.JobRunning)
	if err := storage.RecoverInterruptedJobs(context.Background(), now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	recovered, err := storage.ClaimJob(context.Background(), now.Add(time.Second))
	if err != nil || recovered == nil || recovered.AttemptCount != 2 {
		t.Fatalf("recovered job = %+v, %v", recovered, err)
	}
}

func TestWorkerShutdownLeavesActiveJobRecoverable(t *testing.T) {
	storage, db := workerStore(t)
	defer storage.Close()
	defer db.Close()
	queueJob(t, storage)
	blocking := &blockingReviewer{started: make(chan struct{})}
	worker := newTestWorker(t, storage, &fakeGitLab{}, blocking, nil)
	worker.shutdownGrace = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()
	select {
	case <-blocking.started:
	case <-time.After(time.Second):
		t.Fatal("review did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("worker did not stop")
	}
	assertJobState(t, db, store.JobRunning)
	recoveredAt := time.Now().UTC().Add(time.Minute)
	if err := storage.RecoverInterruptedJobs(context.Background(), recoveredAt); err != nil {
		t.Fatal(err)
	}
	recovered, err := storage.ClaimJob(context.Background(), recoveredAt)
	if err != nil || recovered == nil || recovered.AttemptCount != 2 {
		t.Fatalf("recovered job = %+v, %v", recovered, err)
	}
}

func TestWorkerShutdownDeadlineDoesNotWaitForUncooperativeOperation(t *testing.T) {
	storage, db := workerStore(t)
	defer storage.Close()
	defer db.Close()
	queueJob(t, storage)
	stubborn := &stubbornReviewer{
		started:  make(chan struct{}),
		release:  make(chan struct{}),
		finished: make(chan struct{}),
	}
	var logs bytes.Buffer
	worker := newTestWorker(t, storage, &fakeGitLab{}, stubborn, &logs)
	worker.shutdownGrace = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()
	select {
	case <-stubborn.started:
	case <-time.After(time.Second):
		t.Fatal("review did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
	case <-time.After(250 * time.Millisecond):
		close(stubborn.release)
		<-done
		t.Fatal("worker exceeded its shutdown deadline")
	}
	select {
	case <-stubborn.finished:
		t.Fatal("worker waited for an operation that ignored cancellation")
	default:
	}
	assertJobState(t, db, store.JobRunning)
	if !strings.Contains(logs.String(), "shutdown_deadline_exceeded") {
		t.Fatalf("shutdown log lacks stable reason: %s", logs.String())
	}
	close(stubborn.release)
	select {
	case <-stubborn.finished:
	case <-time.After(time.Second):
		t.Fatal("blocked review did not finish after release")
	}
}

func TestWorkerRechecksHeadImmediatelyBeforePosting(t *testing.T) {
	storage, db := workerStore(t)
	defer storage.Close()
	defer db.Close()
	queueJob(t, storage)
	broker := &fakeGitLab{checkErrors: []error{nil, failure.Obsolete("merge_request_head_changed")}}
	reviewer := &fakeReviewer{result: review.Result{Summary: "No problems.", Findings: []review.Finding{}}}
	worker := newTestWorker(t, storage, broker, reviewer, nil)
	if processed, err := worker.ProcessOne(context.Background()); err != nil || !processed {
		t.Fatalf("ProcessOne() = %t, %v", processed, err)
	}
	assertJobState(t, db, store.JobObsolete)
	if broker.checkCalls != 2 || broker.postCalls != 0 {
		t.Fatalf("checks=%d posts=%d", broker.checkCalls, broker.postCalls)
	}
}

func TestWorkerClassifiesTerminalFailures(t *testing.T) {
	tests := []struct {
		name      string
		failure   error
		wantState string
	}{
		{name: "obsolete", failure: failure.Obsolete("merge_request_not_open"), wantState: store.JobObsolete},
		{name: "authorization", failure: failure.Failed("repository_unauthorized"), wantState: store.JobFailed},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			storage, db := workerStore(t)
			defer storage.Close()
			defer db.Close()
			queueJob(t, storage)
			broker := &fakeGitLab{loadError: test.failure}
			worker := newTestWorker(t, storage, broker, &fakeReviewer{}, nil)
			processed, err := worker.ProcessOne(context.Background())
			if err != nil || !processed {
				t.Fatalf("ProcessOne() = %t, %v", processed, err)
			}
			assertJobState(t, db, test.wantState)
		})
	}
}

func TestWorkerUsesRetryAfterAsMinimum(t *testing.T) {
	storage, db := workerStore(t)
	defer storage.Close()
	defer db.Close()
	queueJob(t, storage)
	broker := &fakeGitLab{loadError: failure.Retry("gitlab_rate_limited", 6*time.Minute)}
	worker := newTestWorker(t, storage, broker, &fakeReviewer{}, nil)
	now := time.Now().UTC().Add(time.Hour)
	worker.now = func() time.Time { return now }

	processed, err := worker.ProcessOne(context.Background())
	if err != nil || !processed {
		t.Fatalf("ProcessOne() = %t, %v", processed, err)
	}
	assertJobState(t, db, store.JobQueued)
	var seconds float64
	if err := db.QueryRow(`SELECT (julianday(next_attempt_at) - julianday(?)) * 86400 FROM review_jobs`, now.Format(time.RFC3339Nano)).Scan(&seconds); err != nil {
		t.Fatal(err)
	}
	if seconds < (6*time.Minute).Seconds()-0.01 {
		t.Fatalf("retry delay = %f seconds", seconds)
	}
}

func TestLocalBackoffIsBounded(t *testing.T) {
	if got := localBackoff(1); got != 5*time.Second {
		t.Fatalf("attempt 1 backoff = %v", got)
	}
	if got := localBackoff(20); got != maxLocalBackoff {
		t.Fatalf("attempt 20 backoff = %v", got)
	}
}

func newTestWorker(t *testing.T, storage JobStore, broker *fakeGitLab, reviewer Reviewer, logs *bytes.Buffer) *Worker {
	t.Helper()
	if logs == nil {
		logs = &bytes.Buffer{}
	}
	return New(storage, broker, &fakeWorkspaces{}, reviewer, slog.New(slog.NewJSONHandler(logs, nil)), []string{"gitlab-token", "gemini-key", "webhook-secret"})
}

func workerStore(t *testing.T) (*store.Store, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "wormtamer.db")
	storage, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		storage.Close()
		t.Fatal(err)
	}
	return storage, db
}

func queueJob(t *testing.T, storage *store.Store) {
	t.Helper()
	queueNewWorkerRevision(t, storage, "event-1", workerHead)
}

func queueNewWorkerRevision(t *testing.T, storage *store.Store, deliveryID, headSHA string) {
	t.Helper()
	_, err := storage.AcceptEvent(context.Background(), store.Event{
		DeliveryID: deliveryID, GitLabInstance: "http://gitlab.internal", ProjectID: 42,
		ProjectPath: "group/project", MergeRequestIID: 7, HeadSHA: headSHA,
		Action: "open", Payload: []byte(`{"object_kind":"merge_request"}`), QueueReview: true,
	})
	if err != nil {
		t.Fatal(err)
	}
}

func prepareMemoryForWorker(t *testing.T, storage *store.Store, now time.Time) string {
	t.Helper()
	ctx := context.Background()
	queueJob(t, storage)
	job, err := storage.ClaimJob(ctx, now)
	if err != nil || job == nil {
		t.Fatalf("ClaimJob() = %+v, %v", job, err)
	}
	findingID := "WT-F-" + strings.Repeat("A", 26)
	result := []byte(`{"summary":"source","findings":[{"priority":"P2","title":"title","explanation":"why","recommendation":"fix","path":"main.go"}]}`)
	if err := storage.SaveReviewResult(ctx, job.ID, result, []string{findingID}, nil, store.PatchIDUnavailable, "", now); err != nil {
		t.Fatal(err)
	}
	if err := storage.CompletePublication(ctx, job.ID, "<!-- memory-source-review -->", 99, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	accepted, err := storage.AcceptEvent(ctx, store.Event{
		DeliveryID: "memory-feedback", GitLabInstance: "http://gitlab.internal", ProjectID: 42,
		ProjectPath: "group/project", MergeRequestIID: 7, HeadSHA: workerHead,
		Action: "close", Payload: []byte(`{"object_kind":"merge_request"}`),
		QueueFeedback: true, TerminalState: "closed",
	})
	if err != nil || accepted.FeedbackJobID == 0 {
		t.Fatalf("AcceptEvent(feedback) = %+v, %v", accepted, err)
	}
	feedbackJob, err := storage.ClaimFeedbackJob(ctx, now.Add(2*time.Second))
	if err != nil || feedbackJob == nil {
		t.Fatalf("ClaimFeedbackJob() = %+v, %v", feedbackJob, err)
	}
	memoryID := "WT-M-" + strings.Repeat("A", 26)
	if err := storage.CompleteFeedbackJob(ctx, feedbackJob.ID, memoryID,
		"Generated source must be fixed through its generator.",
		"http://gitlab.internal/group/project/-/merge_requests/7", now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	return memoryID
}

func assertJobState(t *testing.T, db *sql.DB, want string) {
	t.Helper()
	var state string
	if err := db.QueryRow(`SELECT state FROM review_jobs`).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != want {
		t.Fatalf("job state = %q, want %q", state, want)
	}
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

type fakeGitLab struct {
	loadError        error
	patchIDStatus    string
	patchIDSHA       string
	checkError       error
	checkErrors      []error
	loadCalls        int
	checkCalls       int
	findCalls        int
	postCalls        int
	postedBody       string
	noteID           int64
	losePostResponse bool
}

func (g *fakeGitLab) LoadReview(_ context.Context, identity gitlab.Identity) (gitlab.Snapshot, error) {
	g.loadCalls++
	if g.loadError != nil {
		return gitlab.Snapshot{}, g.loadError
	}
	patchIDStatus := g.patchIDStatus
	patchIDSHA := g.patchIDSHA
	if patchIDStatus == "" {
		patchIDStatus = gitlab.PatchIDAvailable
		patchIDSHA = strings.Repeat("a", 40)
	}
	return gitlab.Snapshot{
		Identity: identity, ProjectPath: "group/project", RelatedRepositories: []string{"group/related"},
		Title: "MR", Description: "description", SourceBranch: "feature", TargetBranch: "main",
		PatchIDStatus: patchIDStatus, PatchIDSHA: patchIDSHA,
		Files: []gitlab.ChangedFile{{OldPath: "main.go", NewPath: "main.go", Diff: "+private-diff"}},
	}, nil
}

func (g *fakeGitLab) CheckCurrent(_ context.Context, _ gitlab.Identity) error {
	g.checkCalls++
	if g.checkCalls <= len(g.checkErrors) {
		return g.checkErrors[g.checkCalls-1]
	}
	return g.checkError
}

func (g *fakeGitLab) FindNote(_ context.Context, _ gitlab.Identity, marker string) (int64, bool, error) {
	g.findCalls++
	if g.noteID > 0 && strings.Contains(g.postedBody, marker) {
		return g.noteID, true, nil
	}
	return 0, false, nil
}

func (g *fakeGitLab) PostNote(_ context.Context, _ gitlab.Identity, body string) (int64, error) {
	g.postCalls++
	g.postedBody = body
	g.noteID = 99
	if g.losePostResponse {
		g.losePostResponse = false
		return 0, failure.Retry("gitlab_network_failure", 0)
	}
	return g.noteID, nil
}

type fakeWorkspaces struct {
	prepareCalls int
	snapshot     gitlab.Snapshot
	memories     []repository.Memory
	workspace    *fakeWorkspace
	prepareErr   error
	closeErr     error
}

func (m *fakeWorkspaces) Prepare(_ context.Context, snapshot gitlab.Snapshot, memories []repository.Memory) (repository.Workspace, error) {
	m.prepareCalls++
	m.snapshot = snapshot
	m.memories = append([]repository.Memory(nil), memories...)
	if m.prepareErr != nil {
		return nil, m.prepareErr
	}
	m.workspace = &fakeWorkspace{
		memories: append([]repository.Memory(nil), memories...), closeErr: m.closeErr,
		context: repository.ReviewContext{
			WorkingDirectory: "/review/current",
			RelatedRepositories: []repository.PreparedRepository{{
				Repository: "group/related", Path: "/review/related/group/related", InitialRevision: strings.Repeat("b", 40),
			}},
			MemoryPath: "/review/review-memory.json",
		},
	}
	return m.workspace, nil
}

type fakeWorkspace struct {
	closed    bool
	calls     int
	arguments map[string]any
	result    repository.ToolResult
	memories  []repository.Memory
	context   repository.ReviewContext
	closeErr  error
}

func (w *fakeWorkspace) Context() repository.ReviewContext { return w.context }

func (w *fakeWorkspace) Call(_ context.Context, _ string, arguments map[string]any) (repository.ToolResult, error) {
	w.calls++
	w.arguments = arguments
	if w.result.Response != nil {
		return w.result, nil
	}
	return repository.ToolResult{Response: map[string]any{"output": "ok"}}, nil
}

func (w *fakeWorkspace) Close() error {
	w.closed = true
	return w.closeErr
}

type fakeReviewer struct {
	result   review.Result
	snapshot gitlab.Snapshot
	err      error
	calls    int
	memories []repository.Memory
}

func (r *fakeReviewer) Review(_ context.Context, snapshot gitlab.Snapshot, tools repository.ToolBroker) (review.Result, []byte, error) {
	r.calls++
	r.snapshot = snapshot
	if workspace, ok := tools.(*fakeWorkspace); ok {
		r.memories = append([]repository.Memory(nil), workspace.memories...)
	}
	if r.err != nil {
		return review.Result{}, nil, r.err
	}
	encoded, err := json.Marshal(r.result)
	return r.result, encoded, err
}

type blockingReviewer struct {
	started chan struct{}
}

func (r *blockingReviewer) Review(ctx context.Context, _ gitlab.Snapshot, _ repository.ToolBroker) (review.Result, []byte, error) {
	close(r.started)
	<-ctx.Done()
	return review.Result{}, nil, ctx.Err()
}

type stubbornReviewer struct {
	started  chan struct{}
	release  chan struct{}
	finished chan struct{}
}

func (r *stubbornReviewer) Review(_ context.Context, _ gitlab.Snapshot, _ repository.ToolBroker) (review.Result, []byte, error) {
	close(r.started)
	<-r.release
	close(r.finished)
	return review.Result{}, nil, context.Canceled
}

type failingMemoryStore struct {
	*store.Store
}

func (s *failingMemoryStore) ListReviewMemories(context.Context, string, int64) ([]store.ReviewMemory, error) {
	return nil, errors.New("simulated memory failure")
}

type failingRetryStore struct {
	*store.Store
	err error
}

func (s *failingRetryStore) RetryJob(context.Context, int64, time.Time, time.Time, string, string) (string, error) {
	return "", s.err
}

type flakyCompletionStore struct {
	*store.Store
	failNext bool
}

func (s *flakyCompletionStore) CompletePublication(ctx context.Context, jobID int64, marker string, noteID int64, now time.Time) error {
	if s.failNext {
		s.failNext = false
		return errors.New("simulated commit failure")
	}
	return s.Store.CompletePublication(ctx, jobID, marker, noteID, now)
}
