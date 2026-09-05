package worker

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/aminvakil/wormtamer/internal/failure"
	"github.com/aminvakil/wormtamer/internal/gitlab"
	"github.com/aminvakil/wormtamer/internal/review"
	"github.com/aminvakil/wormtamer/internal/store"
)

func TestCIWaitingSurvivesRestartAndReleasesWorker(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "wormtamer.db")
	storage, err := store.Open(ctx, path, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { storage.Close() }()
	first, err := storage.CreateReconciledJob(ctx, store.ReconciledReview{
		GitLabInstance: "http://gitlab.internal", ProjectID: 42, MergeRequestIID: 7, HeadSHA: workerHead,
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := storage.CreateReconciledJob(ctx, store.ReconciledReview{
		GitLabInstance: "http://gitlab.internal", ProjectID: 42, MergeRequestIID: 8, HeadSHA: workerHead,
	})
	if err != nil {
		t.Fatal(err)
	}
	ciStatus := "failed"
	broker := &fakeGitLab{ciCheck: func(_ context.Context, identity gitlab.Identity) (string, error) {
		if identity.MergeRequestIID == 7 {
			return ciStatus, nil
		}
		return gitlab.CINoPipeline, nil
	}}
	reviewer := &fakeReviewer{result: review.Result{Summary: "Reviewed.", Findings: []review.Finding{}}}
	workspaces := &fakeWorkspaces{}
	worker := New(storage, broker, workspaces, reviewer, slog.New(slog.DiscardHandler), nil, true)
	record, err := storage.GetReviewRecord(ctx, first.JobID)
	if err != nil || record.NextAttemptAt == nil {
		t.Fatalf("initial record = %+v, %v", record, err)
	}
	now := record.NextAttemptAt.Add(-time.Second)
	worker.now = func() time.Time { return now }
	if processed, err := worker.ProcessOne(ctx); err != nil || processed || broker.ciCalls != 0 {
		t.Fatalf("before grace deadline = %t, %v; CI calls = %d", processed, err, broker.ciCalls)
	}
	now = record.NextAttemptAt.Add(time.Second)
	if processed, err := worker.ProcessOne(ctx); err != nil || !processed {
		t.Fatalf("CI wait = %t, %v", processed, err)
	}
	if workspaces.prepareCalls != 0 || reviewer.calls != 0 {
		t.Fatal("CI waiting prepared or reviewed a repository")
	}
	if processed, err := worker.ProcessOne(ctx); err != nil || !processed {
		t.Fatalf("other MR = %t, %v", processed, err)
	}
	other, err := storage.GetReviewRecord(ctx, second.JobID)
	if err != nil || other.State != store.JobCompleted || other.AttemptCount != 1 {
		t.Fatalf("other MR record = %+v, %v", other, err)
	}

	if err := storage.Close(); err != nil {
		t.Fatal(err)
	}
	storage, err = store.Open(ctx, path, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.RecoverInterruptedJobs(ctx, now); err != nil {
		t.Fatal(err)
	}
	worker = New(storage, broker, workspaces, reviewer, slog.New(slog.DiscardHandler), nil, true)
	worker.now = func() time.Time { return now }
	for check := 0; check < store.MaxJobAttempts+1; check++ {
		record, err = storage.GetReviewRecord(ctx, first.JobID)
		if err != nil || record.State != store.JobQueued || !record.WaitingOnCI || record.CIStatus != "failed" ||
			record.AttemptCount != 0 || record.NextAttemptAt == nil || !record.NextAttemptAt.Equal(now.Add(time.Minute)) {
			t.Fatalf("waiting record = %+v, %v", record, err)
		}
		// A later webhook for the reconciled revision must not reset its CI deadline.
		queueJob(t, storage)
		if processed, err := worker.ProcessOne(ctx); err != nil || processed {
			t.Fatalf("before persisted deadline = %t, %v", processed, err)
		}
		now = *record.NextAttemptAt
		if processed, err := worker.ProcessOne(ctx); err != nil || !processed {
			t.Fatalf("repeated wait = %t, %v", processed, err)
		}
	}
	ciStatus = "success"
	now = now.Add(time.Minute)
	if processed, err := worker.ProcessOne(ctx); err != nil || !processed {
		t.Fatalf("green CI = %t, %v", processed, err)
	}
	record, err = storage.GetReviewRecord(ctx, first.JobID)
	if err != nil || record.State != store.JobCompleted || record.WaitingOnCI || record.CIStatus != "success" || record.AttemptCount != 1 {
		t.Fatalf("admitted record = %+v, %v", record, err)
	}
	page, err := storage.ListReviewRecords(ctx, "", 0, 10)
	if err != nil || len(page.Records) != 2 || reviewer.calls != 2 || workspaces.prepareCalls != 2 {
		t.Fatalf("reviews = %+v, %v; preparations=%d reviews=%d", page, err, workspaces.prepareCalls, reviewer.calls)
	}
}

func TestCIErrorsRetainBudgetAcrossWaitingAndInterruptedPreflight(t *testing.T) {
	storage, db := workerStore(t)
	defer storage.Close()
	defer db.Close()
	queueJob(t, storage)
	ctx := context.Background()
	var ciErr error
	broker := &fakeGitLab{ciCheck: func(context.Context, gitlab.Identity) (string, error) {
		if ciErr != nil {
			return "", ciErr
		}
		return "failed", nil
	}}
	worker := newTestWorker(t, storage, broker, &fakeReviewer{}, nil)
	worker.waitOnCI = true
	now := time.Now().UTC().Truncate(time.Second).Add(time.Hour)
	worker.now = func() time.Time { return now }
	job, err := storage.NextJob(ctx, now)
	if err != nil || job == nil {
		t.Fatalf("NextJob() = %+v, %v", job, err)
	}
	for attempt := 1; attempt <= store.MaxJobAttempts; attempt++ {
		if attempt == store.MaxJobAttempts {
			// Interrupt the uncharged check while only the last review attempt remains.
			check := broker.ciCheck
			checkCtx, cancel := context.WithCancel(ctx)
			broker.ciCheck = func(context.Context, gitlab.Identity) (string, error) {
				cancel()
				return "", checkCtx.Err()
			}
			if processed, err := worker.ProcessOne(checkCtx); err != nil || !processed {
				t.Fatalf("interrupted preflight = %t, %v", processed, err)
			}
			broker.ciCheck = check
			if err := storage.RecoverInterruptedJobs(ctx, now); err != nil {
				t.Fatal(err)
			}
			record, err := storage.GetReviewRecord(ctx, job.ID)
			if err != nil || record.State != store.JobQueued || record.AttemptCount != store.MaxJobAttempts-1 {
				t.Fatalf("interrupted eligibility record = %+v, %v", record, err)
			}
		}
		ciErr = failure.Retry("gitlab_network_failure", 0)
		if processed, err := worker.ProcessOne(ctx); err != nil || !processed {
			t.Fatalf("CI error = %t, %v", processed, err)
		}
		record, err := storage.GetReviewRecord(ctx, job.ID)
		if err != nil || record.AttemptCount != attempt || record.WaitingOnCI {
			t.Fatalf("error record = %+v, %v", record, err)
		}
		if attempt == store.MaxJobAttempts {
			if record.State != store.JobFailed {
				t.Fatalf("exhausted state = %s", record.State)
			}
			break
		}
		now = *record.NextAttemptAt
		ciErr = nil
		if processed, err := worker.ProcessOne(ctx); err != nil || !processed {
			t.Fatalf("interleaved wait = %t, %v", processed, err)
		}
		record, err = storage.GetReviewRecord(ctx, job.ID)
		if err != nil || !record.WaitingOnCI || record.AttemptCount != attempt || record.LastErrorCategory != "gitlab_network_failure" {
			t.Fatalf("waiting erased failures: %+v, %v", record, err)
		}
		now = *record.NextAttemptAt
	}
	now = now.Add(24 * time.Hour)
	if processed, err := worker.ProcessOne(ctx); err != nil || processed {
		t.Fatalf("terminal job still polled = %t, %v", processed, err)
	}
}

func TestShutdownDoesNotAdmitReviewAfterCIPreflight(t *testing.T) {
	storage, db := workerStore(t)
	defer storage.Close()
	defer db.Close()
	queueJob(t, storage)
	started := make(chan struct{})
	release := make(chan struct{})
	broker := &fakeGitLab{ciCheck: func(context.Context, gitlab.Identity) (string, error) {
		close(started)
		<-release
		return "success", nil
	}}
	worker := newTestWorker(t, storage, broker, &fakeReviewer{}, nil)
	worker.waitOnCI = true
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("CI preflight did not start")
	}
	cancel()
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("worker did not stop")
	}
	var state string
	var attempts int
	if err := db.QueryRow(`SELECT state, attempt_count FROM review_jobs`).Scan(&state, &attempts); err != nil {
		t.Fatal(err)
	}
	if state != store.JobQueued || attempts != 0 {
		t.Fatalf("shutdown admitted charged work: state=%s attempts=%d", state, attempts)
	}
}

func TestDisablingCIWaitPreservesDeadline(t *testing.T) {
	storage, db := workerStore(t)
	defer storage.Close()
	defer db.Close()
	queueJob(t, storage)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second).Add(time.Hour)
	job, err := storage.NextJob(ctx, now)
	if err != nil || job == nil {
		t.Fatalf("NextJob() = %+v, %v", job, err)
	}
	if err := storage.DeferCI(ctx, job.ID, "manual", now); err != nil {
		t.Fatal(err)
	}
	broker := &fakeGitLab{}
	reviewer := &fakeReviewer{result: review.Result{Summary: "Reviewed.", Findings: []review.Finding{}}}
	worker := newTestWorker(t, storage, broker, reviewer, nil)
	worker.now = func() time.Time { return now }
	if processed, err := worker.ProcessOne(ctx); err != nil || processed {
		t.Fatalf("disabled before deadline = %t, %v", processed, err)
	}
	now = now.Add(time.Minute)
	if processed, err := worker.ProcessOne(ctx); err != nil || !processed || broker.ciCalls != 0 || reviewer.calls != 1 {
		t.Fatalf("disabled at deadline = %t, %v; CI=%d review=%d", processed, err, broker.ciCalls, reviewer.calls)
	}
}
