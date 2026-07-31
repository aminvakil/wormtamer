package feedback

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aminvakil/wormtamer/internal/gitlab"
	"github.com/aminvakil/wormtamer/internal/memory"
	"github.com/aminvakil/wormtamer/internal/store"
	_ "github.com/mattn/go-sqlite3"
)

func TestWorkerEvaluatesCurrentCommentAndActivatesMemory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wormtamer.db")
	storage, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(time.Second)
	findingID := prepareReviewFinding(t, storage, now)
	accepted, err := storage.AcceptFeedbackEvent(context.Background(), store.FeedbackEvent{
		DeliveryID: "note", GitLabInstance: "http://gitlab.internal", ProjectID: 42,
		ProjectPath: "group/project", MergeRequestIID: 7, NoteID: 91, ActorID: 12,
		Action: "create", SourceUpdatedAt: now,
	}, now)
	if err != nil || accepted.JobID == 0 {
		t.Fatalf("AcceptFeedbackEvent() = %+v, %v", accepted, err)
	}
	broker := &fakeGitLab{comment: gitlab.FeedbackComment{
		Body: "This file is generated, so the finding is not valid.", UpdatedAt: now,
		AccessLevel: 40, Role: "maintainer",
		SourceURL: "http://gitlab.internal/group/project/-/merge_requests/7#note_91",
	}}
	evaluator := &fakeEvaluator{result: memory.Result{Decisions: []memory.Decision{{
		FindingID: findingID, Outcome: "rejects_finding", Confidence: "high",
		CreateMemory: true, Lesson: "Generated files should be assessed through their source generator.",
	}}}}
	worker, err := New(storage, broker, evaluator, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	worker.now = func() time.Time { return now.Add(time.Second) }
	processed, err := worker.ProcessOne(context.Background())
	if err != nil || !processed {
		t.Fatalf("ProcessOne() = %t, %v", processed, err)
	}
	if evaluator.input.ActorRole != "maintainer" || evaluator.input.Comment != broker.comment.Body || len(evaluator.input.Findings) != 1 || evaluator.input.Findings[0].ID != findingID {
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
	var active int
	var lesson, role string
	if err := db.QueryRow(`
SELECT m.active, m.lesson, e.actor_role
FROM review_memories m JOIN feedback_evaluations e ON e.job_id = m.feedback_job_id`).Scan(&active, &lesson, &role); err != nil {
		t.Fatal(err)
	}
	if active != 1 || role != "maintainer" || lesson != evaluator.result.Decisions[0].Lesson {
		t.Fatalf("memory active=%d role=%q lesson=%q", active, role, lesson)
	}
	databaseBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(databaseBytes), broker.comment.Body) {
		t.Fatal("SQLite retained the source comment body")
	}
}

func TestRunReconcilesDueSourcesWhileFeedbackRemainsQueued(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wormtamer.db")
	storage, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	now := time.Now().UTC().Add(time.Second)
	findingID := prepareReviewFinding(t, storage, now)
	if _, err := storage.AcceptFeedbackEvent(context.Background(), store.FeedbackEvent{
		DeliveryID: "active-note", GitLabInstance: "http://gitlab.internal", ProjectID: 42,
		ProjectPath: "group/project", MergeRequestIID: 7, NoteID: 91, ActorID: 12,
		Action: "create", SourceUpdatedAt: now,
	}, now); err != nil {
		t.Fatal(err)
	}
	broker := &fakeGitLab{comment: gitlab.FeedbackComment{
		Body: "The file is generated.", UpdatedAt: now, AccessLevel: 40, Role: "maintainer",
		SourceURL: "http://gitlab.internal/group/project/-/merge_requests/7#note_91",
	}}
	evaluator := &fakeEvaluator{result: memory.Result{Decisions: []memory.Decision{{
		FindingID: findingID, Outcome: "rejects_finding", Confidence: "high",
		CreateMemory: true, Lesson: "Generated files should be assessed through their source generator.",
	}}}}
	initialWorker, err := New(storage, broker, evaluator, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	initialWorker.now = func() time.Time { return now.Add(time.Second) }
	if processed, err := initialWorker.ProcessOne(context.Background()); err != nil || !processed {
		t.Fatalf("initial ProcessOne() = %t, %v", processed, err)
	}

	for index, noteID := range []int64{92, 93} {
		if _, err := storage.AcceptFeedbackEvent(context.Background(), store.FeedbackEvent{
			DeliveryID: "queued-note-" + strconv.FormatInt(noteID, 10), GitLabInstance: "http://gitlab.internal", ProjectID: 42,
			ProjectPath: "group/project", MergeRequestIID: 7, NoteID: noteID, ActorID: 12,
			Action: "create", SourceUpdatedAt: now.Add(time.Duration(index+2) * time.Second),
		}, now.Add(time.Duration(index+2)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	wrappedStore := &cancelAfterReconciliationStore{Store: storage, cancel: cancel}
	runWorker, err := New(wrappedStore, broker, evaluator, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	dueAt := now.Add(6 * time.Minute)
	runWorker.now = func() time.Time { return dueAt }
	if err := runWorker.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if wrappedStore.reconciliations != 1 || broker.sourceChecks != 1 {
		t.Fatalf("reconciliations=%d source_checks=%d", wrappedStore.reconciliations, broker.sourceChecks)
	}
	remaining, err := storage.ClaimFeedbackJob(context.Background(), "remaining-owner", dueAt, time.Minute, 5)
	if err != nil || remaining == nil {
		t.Fatalf("remaining queued feedback = %+v, %v", remaining, err)
	}
}

func prepareReviewFinding(t *testing.T, storage *store.Store, now time.Time) string {
	t.Helper()
	_, err := storage.AcceptEvent(context.Background(), store.Event{
		DeliveryID: "review", GitLabInstance: "http://gitlab.internal", ProjectID: 42,
		ProjectPath: "group/project", MergeRequestIID: 7,
		HeadSHA: strings.Repeat("a", 40), Action: "open", Payload: []byte(`{"object_kind":"merge_request"}`), QueueReview: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	job, err := storage.ClaimJob(context.Background(), "review-owner", now, time.Minute, 5)
	if err != nil || job == nil {
		t.Fatalf("ClaimJob() = %+v, %v", job, err)
	}
	findingID := "WT-F-" + strings.Repeat("A", 26)
	result := []byte(`{"summary":"summary","findings":[{"severity":"medium","title":"title","explanation":"explanation","recommendation":"recommendation","path":"file.go"}]}`)
	if err := storage.SaveReviewResult(context.Background(), job.ID, "review-owner", result, []string{findingID}, now); err != nil {
		t.Fatal(err)
	}
	return findingID
}

type fakeGitLab struct {
	comment      gitlab.FeedbackComment
	sourceChecks int
}

func (g *fakeGitLab) LoadFeedbackComment(_ context.Context, _ gitlab.FeedbackRef) (gitlab.FeedbackComment, bool, error) {
	return g.comment, true, nil
}

func (g *fakeGitLab) CheckFeedbackSource(_ context.Context, _ gitlab.FeedbackRef) (bool, time.Time, error) {
	g.sourceChecks++
	return true, g.comment.UpdatedAt, nil
}

type fakeEvaluator struct {
	input  memory.Input
	result memory.Result
}

func (e *fakeEvaluator) Evaluate(_ context.Context, input memory.Input) (memory.Result, error) {
	e.input = input
	return e.result, nil
}

type cancelAfterReconciliationStore struct {
	*store.Store
	cancel          context.CancelFunc
	reconciliations int
}

func (s *cancelAfterReconciliationStore) ReconcileFeedbackSource(ctx context.Context, jobID int64, exists bool, sourceUpdatedAt, now time.Time, interval time.Duration) error {
	if err := s.Store.ReconcileFeedbackSource(ctx, jobID, exists, sourceUpdatedAt, now, interval); err != nil {
		return err
	}
	s.reconciliations++
	s.cancel()
	return nil
}
