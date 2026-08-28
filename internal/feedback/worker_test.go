package feedback

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aminvakil/wormtamer/internal/gitlab"
	"github.com/aminvakil/wormtamer/internal/memory"
	"github.com/aminvakil/wormtamer/internal/store"
	_ "github.com/mattn/go-sqlite3"
)

func TestWorkerSynthesizesMemoryFromTerminalEvidence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wormtamer.db")
	storage, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(time.Hour)
	feedbackID := prepareFeedbackJob(t, storage, now)
	broker := &fakeGitLab{evidence: gitlab.FeedbackEvidence{
		Files:     []gitlab.ChangedFile{{OldPath: "generated.go", NewPath: "generated.go", Diff: "@@ -1 +1 @@\n-old\n+new"}},
		Comments:  []gitlab.FeedbackComment{{AuthorID: 12, Body: "This file must be changed through the generator."}},
		SourceURL: "http://gitlab.internal/group/project/-/merge_requests/7",
	}}
	evaluator := &fakeEvaluator{result: memory.Result{
		CreateMemory: true, Lesson: "Generated files must be changed through their source generator.",
	}}
	worker := New(storage, broker, evaluator, slog.New(slog.NewTextHandler(io.Discard, nil)))
	worker.now = func() time.Time { return now.Add(10 * time.Second) }
	processed, err := worker.ProcessOne(context.Background())
	if err != nil || !processed {
		t.Fatalf("ProcessOne() = %t, %v", processed, err)
	}
	if evaluator.input.HeadSHA != strings.Repeat("b", 40) || evaluator.input.ReviewHeadSHA != strings.Repeat("a", 40) ||
		len(evaluator.input.Files) != 1 || len(evaluator.input.Comments) != 1 || evaluator.input.Summary != "summary" {
		t.Fatalf("evaluation input = %+v", evaluator.input)
	}
	if err := storage.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var lesson, state string
	var storedJobID int64
	if err := db.QueryRow(`
SELECT m.lesson, j.state, j.id
FROM review_memories m JOIN feedback_jobs j ON j.id = m.feedback_job_id`).Scan(&lesson, &state, &storedJobID); err != nil {
		t.Fatal(err)
	}
	if lesson != evaluator.result.Lesson || state != store.FeedbackCompleted || storedJobID != feedbackID {
		t.Fatalf("lesson=%q state=%q job=%d", lesson, state, storedJobID)
	}
}

func TestWorkerCompletesWithoutMemoryWhenModelDeclines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wormtamer.db")
	storage, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	now := time.Now().UTC().Add(time.Hour)
	prepareFeedbackJob(t, storage, now)
	broker := &fakeGitLab{evidence: gitlab.FeedbackEvidence{
		SourceURL: "http://gitlab.internal/group/project/-/merge_requests/7",
	}}
	worker := New(storage, broker, &fakeEvaluator{result: memory.Result{}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	worker.now = func() time.Time { return now.Add(10 * time.Second) }
	if processed, err := worker.ProcessOne(context.Background()); err != nil || !processed {
		t.Fatalf("ProcessOne() = %t, %v", processed, err)
	}
	memories, err := storage.ListReviewMemories(context.Background(), "http://gitlab.internal", 42)
	if err != nil || len(memories) != 0 {
		t.Fatalf("memories = %+v, %v", memories, err)
	}
}

func TestWorkerRunFailsWhenRetryCheckpointCannotPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wormtamer.db")
	storage, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now().UTC().Add(time.Hour)
	feedbackJobID := prepareFeedbackJob(t, storage, now)
	checkpointErr := errors.New("simulated feedback retry checkpoint failure")
	failing := &failingRetryStore{Store: storage, err: checkpointErr}
	worker := New(failing, &fakeGitLab{err: errors.New("simulated feedback load failure")}, &fakeEvaluator{}, slog.New(slog.DiscardHandler))
	worker.now = func() time.Time { return now.Add(10 * time.Second) }
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if err := worker.Run(ctx); !errors.Is(err, checkpointErr) {
		t.Fatalf("Run() error = %v", err)
	}
	var state string
	if err := db.QueryRow(`SELECT state FROM feedback_jobs WHERE id = ?`, feedbackJobID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != store.FeedbackRunning {
		t.Fatalf("feedback job state = %q, want %q", state, store.FeedbackRunning)
	}
	recoveredAt := now.Add(11 * time.Second)
	if err := storage.RecoverInterruptedJobs(context.Background(), recoveredAt); err != nil {
		t.Fatal(err)
	}
	recovered, err := storage.ClaimFeedbackJob(context.Background(), recoveredAt)
	if err != nil || recovered == nil || recovered.ID != feedbackJobID || recovered.AttemptCount != 2 {
		t.Fatalf("recovered feedback job = %+v, %v", recovered, err)
	}
}

func prepareFeedbackJob(t *testing.T, storage *store.Store, now time.Time) int64 {
	t.Helper()
	ctx := context.Background()
	reviewHead := strings.Repeat("a", 40)
	accepted, err := storage.AcceptEvent(ctx, store.Event{
		DeliveryID: "review", GitLabInstance: "http://gitlab.internal", ProjectID: 42,
		ProjectPath: "group/project", MergeRequestIID: 7, HeadSHA: reviewHead,
		Action: "open", Payload: []byte(`{"object_kind":"merge_request"}`), QueueReview: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	job, err := storage.ClaimJob(ctx, now)
	if err != nil || job == nil || job.ID != accepted.JobID {
		t.Fatalf("ClaimJob() = %+v, %v", job, err)
	}
	if err := storage.SaveReviewResult(ctx, job.ID, []byte(`{"summary":"summary","findings":[]}`), nil, nil, store.PatchIDUnavailable, "", now); err != nil {
		t.Fatal(err)
	}
	if err := storage.CompletePublication(ctx, job.ID, "<!-- review -->", 99, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	terminal, err := storage.AcceptEvent(ctx, store.Event{
		DeliveryID: "closed", GitLabInstance: "http://gitlab.internal", ProjectID: 42,
		ProjectPath: "group/project", MergeRequestIID: 7, HeadSHA: strings.Repeat("b", 40),
		Action: "close", Payload: []byte(`{"object_kind":"merge_request"}`),
		QueueFeedback: true, TerminalState: "closed",
	})
	if err != nil || terminal.FeedbackJobID == 0 {
		t.Fatalf("AcceptEvent(terminal) = %+v, %v", terminal, err)
	}
	return terminal.FeedbackJobID
}

type failingRetryStore struct {
	*store.Store
	err error
}

func (s *failingRetryStore) RetryFeedbackJob(context.Context, int64, time.Time, time.Time, string) (string, error) {
	return "", s.err
}

type fakeGitLab struct {
	evidence gitlab.FeedbackEvidence
	err      error
}

func (g *fakeGitLab) LoadFeedback(_ context.Context, _ gitlab.FeedbackRef) (gitlab.FeedbackEvidence, error) {
	return g.evidence, g.err
}

type fakeEvaluator struct {
	input  memory.Input
	result memory.Result
	err    error
}

func (e *fakeEvaluator) Evaluate(_ context.Context, input memory.Input) (memory.Result, error) {
	e.input = input
	return e.result, e.err
}
