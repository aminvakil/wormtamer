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

func TestWorkerLoadsAndClosesRequestedRepositoryWorkspace(t *testing.T) {
	storage, db := workerStore(t)
	defer storage.Close()
	defer db.Close()
	queueJob(t, storage)

	broker := &fakeGitLab{}
	workspaces := &fakeWorkspaces{}
	reviewer := &fakeReviewer{result: review.Result{Summary: "ok", Findings: []review.Finding{}}, useTools: true}
	worker, err := New(storage, broker, workspaces, reviewer, slog.Default(), nil)
	if err != nil {
		t.Fatal(err)
	}
	processed, err := worker.ProcessOne(context.Background())
	if err != nil || !processed {
		t.Fatalf("ProcessOne() = %t, %v", processed, err)
	}
	if broker.archiveCalls != 1 || workspaces.createCalls != 1 || workspaces.revision != workerHead || string(workspaces.archive) != "archive" {
		t.Fatalf("archive calls=%d workspace=%+v", broker.archiveCalls, workspaces)
	}
	if workspaces.workspace == nil || workspaces.workspace.calls != 1 || !workspaces.workspace.closed {
		t.Fatalf("review workspace = %+v", workspaces.workspace)
	}
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
			Severity: "medium", Title: "Check error", Explanation: "An error is ignored.",
			Recommendation: "Handle the error.", Path: "main.go",
		}},
	}}
	var logs bytes.Buffer
	worker := newTestWorker(t, storage, broker, reviewer, &logs)

	processed, err := worker.ProcessOne(context.Background())
	if err != nil || !processed {
		t.Fatalf("ProcessOne() = %t, %v", processed, err)
	}
	assertJobState(t, db, store.JobCompleted)
	assertCount(t, db, "review_results", 1)
	assertCount(t, db, "publications", 1)
	if broker.loadCalls != 1 || broker.archiveCalls != 0 || broker.checkCalls != 2 || broker.postCalls != 1 || reviewer.calls != 1 {
		t.Fatalf("calls: broker=%+v reviewer=%+v", broker, reviewer)
	}
	if !strings.Contains(broker.postedBody, "<!-- wormtamer:review=") || !strings.Contains(broker.postedBody, "Check error") {
		t.Fatalf("posted body = %q", broker.postedBody)
	}
	for _, secret := range []string{"gitlab-token", "gemini-key", "webhook-secret", "+private-diff", "Looks mostly good."} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("logs contain %q: %s", secret, logs.String())
		}
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
	if broker.postCalls != 1 || reviewer.calls != 1 || broker.findCalls != 2 {
		t.Fatalf("reconciled calls: post=%d review=%d find=%d", broker.postCalls, reviewer.calls, broker.findCalls)
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
	recovered, err := storage.ClaimJob(context.Background(), "recovery-owner", time.Now().UTC().Add(3*time.Minute), leaseDuration, maxAttempts)
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
	result, err := New(storage, broker, &fakeWorkspaces{}, reviewer, slog.New(slog.NewJSONHandler(logs, nil)), []string{"gitlab-token", "gemini-key", "webhook-secret"})
	if err != nil {
		t.Fatal(err)
	}
	return result
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
	_, err := storage.AcceptEvent(context.Background(), store.Event{
		DeliveryID: "event-1", GitLabInstance: "http://gitlab.internal", ProjectID: 42,
		ProjectPath: "group/project", MergeRequestIID: 7, HeadSHA: workerHead,
		Action: "open", Payload: []byte(`{"object_kind":"merge_request"}`), QueueReview: true,
	})
	if err != nil {
		t.Fatal(err)
	}
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
	checkError       error
	checkErrors      []error
	loadCalls        int
	archiveCalls     int
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
	return gitlab.Snapshot{
		Identity: identity, Title: "MR", Description: "description", SourceBranch: "feature", TargetBranch: "main",
		Files: []gitlab.ChangedFile{{OldPath: "main.go", NewPath: "main.go", Diff: "+private-diff"}},
	}, nil
}

func (g *fakeGitLab) LoadRepositoryArchive(_ context.Context, _ gitlab.Identity) ([]byte, error) {
	g.archiveCalls++
	return []byte("archive"), nil
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
	createCalls int
	revision    string
	archive     []byte
	workspace   *fakeWorkspace
}

func (m *fakeWorkspaces) Create(_ context.Context, revision string, archive []byte) (repository.Workspace, error) {
	m.createCalls++
	m.revision = revision
	m.archive = append([]byte(nil), archive...)
	m.workspace = &fakeWorkspace{}
	return m.workspace, nil
}

type fakeWorkspace struct {
	closed bool
	calls  int
}

func (w *fakeWorkspace) Call(_ context.Context, _ string, _ map[string]any) (map[string]any, error) {
	w.calls++
	return map[string]any{"files": []string{}}, nil
}

func (w *fakeWorkspace) Close() error {
	w.closed = true
	return nil
}

type fakeReviewer struct {
	result   review.Result
	err      error
	calls    int
	useTools bool
}

func (r *fakeReviewer) Review(ctx context.Context, _ gitlab.Snapshot, tools repository.ToolBroker) (review.Result, []byte, error) {
	r.calls++
	if r.err != nil {
		return review.Result{}, nil, r.err
	}
	if r.useTools {
		if _, err := tools.Call(ctx, repository.ToolListFiles, map[string]any{}); err != nil {
			return review.Result{}, nil, err
		}
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

type flakyCompletionStore struct {
	*store.Store
	failNext bool
}

func (s *flakyCompletionStore) CompletePublication(ctx context.Context, jobID int64, owner, marker string, noteID int64, now time.Time) error {
	if s.failNext {
		s.failNext = false
		return errors.New("simulated commit failure")
	}
	return s.Store.CompletePublication(ctx, jobID, owner, marker, noteID, now)
}
