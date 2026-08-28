package feedback

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/aminvakil/wormtamer/internal/failure"
	"github.com/aminvakil/wormtamer/internal/gitlab"
	"github.com/aminvakil/wormtamer/internal/memory"
	"github.com/aminvakil/wormtamer/internal/review"
	"github.com/aminvakil/wormtamer/internal/store"
)

const (
	pollInterval    = time.Second
	initialBackoff  = 5 * time.Second
	maxLocalBackoff = 5 * time.Minute
)

type JobStore interface {
	ClaimFeedbackJob(context.Context, time.Time) (*store.FeedbackJob, error)
	CompleteFeedbackJob(context.Context, int64, string, string, string, time.Time) error
	RetryFeedbackJob(context.Context, int64, time.Time, time.Time, string) (string, error)
	FinishFeedbackJob(context.Context, int64, string, time.Time) error
}

type GitLabBroker interface {
	LoadFeedback(context.Context, gitlab.FeedbackRef) (gitlab.FeedbackEvidence, error)
}

type Evaluator interface {
	Evaluate(context.Context, memory.Input) (memory.Result, error)
}

type Worker struct {
	store     JobStore
	gitlab    GitLabBroker
	evaluator Evaluator
	logger    *slog.Logger
	now       func() time.Time
}

func New(storage JobStore, gitLab GitLabBroker, evaluator Evaluator, logger *slog.Logger) *Worker {
	return &Worker{store: storage, gitlab: gitLab, evaluator: evaluator, logger: logger, now: time.Now}
}

func (w *Worker) Run(ctx context.Context) error {
	for {
		if ctx.Err() != nil {
			return nil
		}
		job, err := w.claim(ctx)
		if err != nil {
			w.logger.Error("feedback job claim failed", "reason", "persistence_failed")
			if !wait(ctx, pollInterval) {
				return nil
			}
			continue
		}
		if job == nil {
			if !wait(ctx, pollInterval) {
				return nil
			}
			continue
		}
		w.logger.Info("feedback job started", logFields(job)...)
		if err := w.processClaimed(ctx, job); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			w.logger.Error("feedback job processing failed", "feedback_job_id", job.ID, "reason", "persistence_failed")
			return fmt.Errorf("process feedback job %d: %w", job.ID, err)
		}
	}
}

func (w *Worker) ProcessOne(ctx context.Context) (bool, error) {
	job, err := w.claim(ctx)
	if err != nil || job == nil {
		return false, err
	}
	w.logger.Info("feedback job started", logFields(job)...)
	return true, w.processClaimed(ctx, job)
}

func (w *Worker) claim(ctx context.Context) (*store.FeedbackJob, error) {
	return w.store.ClaimFeedbackJob(ctx, w.now().UTC())
}

func (w *Worker) processClaimed(ctx context.Context, job *store.FeedbackJob) error {
	err := w.execute(ctx, job)
	if ctx.Err() != nil {
		return nil
	}
	if errors.Is(err, store.ErrJobNotRunning) {
		return err
	}
	if err != nil {
		return w.handleFailure(ctx, job, err)
	}
	w.logger.Info("feedback job completed", append(logFields(job), "outcome", store.FeedbackCompleted)...)
	return nil
}

func (w *Worker) execute(ctx context.Context, job *store.FeedbackJob) error {
	evidence, err := w.gitlab.LoadFeedback(ctx, gitlab.FeedbackRef{
		Identity: gitlab.Identity{
			GitLabInstance: job.GitLabInstance, ProjectID: job.ProjectID,
			MergeRequestIID: job.MergeRequestIID, HeadSHA: job.HeadSHA,
		},
		ProjectPath: job.ProjectPath,
	})
	if err != nil {
		return err
	}
	result, err := review.DecodeStored(job.ValidatedResultJSON)
	if err != nil || len(result.Findings) != len(job.FindingIDs) {
		return failure.Failed("invalid_stored_review_result")
	}
	findings := make([]memory.Finding, len(result.Findings))
	for index := range result.Findings {
		if !review.ValidFindingID(job.FindingIDs[index]) {
			return failure.Failed("invalid_stored_review_result")
		}
		findings[index] = memory.Finding{TargetID: job.FindingIDs[index], Finding: result.Findings[index]}
	}
	assessment, err := w.evaluator.Evaluate(ctx, memory.Input{
		ProjectID: job.ProjectID, ProjectPath: job.ProjectPath, MergeRequestIID: job.MergeRequestIID,
		HeadSHA: job.HeadSHA, ReviewHeadSHA: job.ReviewHeadSHA, Files: evidence.Files,
		Comments: evidence.Comments, Summary: result.Summary, Findings: findings,
	})
	if err != nil {
		return err
	}
	memoryID, lesson := "", ""
	if assessment.CreateMemory {
		memoryID = memory.ID(job.GitLabInstance, job.ProjectID, job.MergeRequestIID)
		lesson = assessment.Lesson
	}
	return w.store.CompleteFeedbackJob(ctx, job.ID, memoryID, lesson, evidence.SourceURL, w.now().UTC())
}

func (w *Worker) handleFailure(ctx context.Context, job *store.FeedbackJob, err error) error {
	var failureError *failure.Error
	if !errors.As(err, &failureError) {
		failureError = &failure.Error{Category: "internal_feedback_failure", Retryable: true}
	}
	now := w.now().UTC()
	if !failureError.Retryable {
		if err := w.store.FinishFeedbackJob(ctx, job.ID, failureError.Category, now); err != nil {
			return err
		}
		w.logger.Warn("feedback job failed", append(logFields(job), "outcome", store.FeedbackFailed, "reason", failureError.Category)...)
		return nil
	}
	delay := localBackoff(job.AttemptCount)
	if failureError.RetryAfter > delay {
		delay = failureError.RetryAfter
	}
	state, retryErr := w.store.RetryFeedbackJob(ctx, job.ID, now, now.Add(delay), failureError.Category)
	if retryErr != nil {
		return retryErr
	}
	w.logger.Info("feedback job deferred", append(logFields(job), "outcome", state, "reason", failureError.Category)...)
	return nil
}

func localBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	delay := initialBackoff
	for index := 1; index < attempt && delay < maxLocalBackoff; index++ {
		delay *= 2
	}
	if delay > maxLocalBackoff {
		return maxLocalBackoff
	}
	return delay
}

func wait(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func logFields(job *store.FeedbackJob) []any {
	return []any{
		"feedback_job_id", job.ID,
		"project_id", job.ProjectID,
		"merge_request_iid", job.MergeRequestIID,
		"head_sha", job.HeadSHA,
		"terminal_state", job.TerminalState,
		"attempt", job.AttemptCount,
	}
}
