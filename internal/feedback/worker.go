package feedback

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"time"

	"github.com/aminvakil/wormtamer/internal/failure"
	"github.com/aminvakil/wormtamer/internal/gitlab"
	"github.com/aminvakil/wormtamer/internal/memory"
	"github.com/aminvakil/wormtamer/internal/review"
	"github.com/aminvakil/wormtamer/internal/store"
	"github.com/aminvakil/wormtamer/internal/usage"
)

const (
	pollInterval        = time.Second
	leaseDuration       = 3 * time.Minute
	leaseRenewInterval  = 30 * time.Second
	maxAttempts         = 5
	initialBackoff      = 5 * time.Second
	maxLocalBackoff     = 5 * time.Minute
	sourceCheckInterval = 5 * time.Minute
)

type JobStore interface {
	ClaimFeedbackJob(context.Context, string, time.Time, time.Duration, int) (*store.FeedbackJob, error)
	RenewFeedbackLease(context.Context, int64, string, time.Time, time.Duration) (bool, error)
	CompleteFeedbackJob(context.Context, int64, int64, string, int, string, string, []store.FeedbackDecision, time.Time, time.Duration) error
	RetryFeedbackJob(context.Context, int64, int64, string, time.Time, time.Time, int, string) (string, error)
	FinishFeedbackJob(context.Context, int64, int64, string, string, time.Time) error
	DueFeedbackSources(context.Context, time.Time, int) ([]store.FeedbackSource, error)
	ReconcileFeedbackSource(context.Context, int64, bool, time.Time, time.Time, time.Duration) error
}

type GitLabBroker interface {
	LoadFeedbackComment(context.Context, gitlab.FeedbackRef) (gitlab.FeedbackComment, bool, error)
	CheckFeedbackSource(context.Context, gitlab.FeedbackRef) (bool, time.Time, error)
}

type Evaluator interface {
	Evaluate(context.Context, memory.Input) (memory.Result, error)
}

type Worker struct {
	store     JobStore
	gitlab    GitLabBroker
	evaluator Evaluator
	logger    *slog.Logger
	owner     string
	now       func() time.Time
}

func New(storage JobStore, gitLab GitLabBroker, evaluator Evaluator, logger *slog.Logger) (*Worker, error) {
	ownerBytes := make([]byte, 16)
	if _, err := rand.Read(ownerBytes); err != nil {
		return nil, errors.New("generate feedback worker lease owner")
	}
	return &Worker{
		store: storage, gitlab: gitLab, evaluator: evaluator, logger: logger,
		owner: hex.EncodeToString(ownerBytes), now: time.Now,
	}, nil
}

func (w *Worker) Run(ctx context.Context) error {
	for {
		if ctx.Err() != nil {
			return nil
		}
		processed, err := w.ProcessOne(ctx)
		if err != nil {
			w.logger.Error("feedback processing failed", "reason", "persistence_failed")
		}
		if ctx.Err() == nil {
			w.reconcileSources(ctx)
		}
		if !processed && !wait(ctx, pollInterval) {
			return nil
		}
	}
}

func (w *Worker) ProcessOne(ctx context.Context) (bool, error) {
	job, err := w.store.ClaimFeedbackJob(ctx, w.owner, w.now().UTC(), leaseDuration, maxAttempts)
	if err != nil || job == nil {
		return false, err
	}
	w.logger.Info("feedback job started", logFields(job)...)
	return true, w.processClaimed(ctx, job)
}

func (w *Worker) processClaimed(ctx context.Context, job *store.FeedbackJob) error {
	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	leaseLost := make(chan struct{}, 1)
	renewalDone := make(chan struct{})
	go func() {
		defer close(renewalDone)
		ticker := time.NewTicker(leaseRenewInterval)
		defer ticker.Stop()
		for {
			select {
			case <-workCtx.Done():
				return
			case <-ticker.C:
				renewed, err := w.store.RenewFeedbackLease(workCtx, job.ID, w.owner, w.now().UTC(), leaseDuration)
				if err != nil || !renewed {
					select {
					case leaseLost <- struct{}{}:
					default:
					}
					cancel()
					return
				}
			}
		}
	}()
	err := w.execute(workCtx, job)
	cancel()
	<-renewalDone
	select {
	case <-leaseLost:
		return store.ErrLeaseLost
	default:
	}
	if errors.Is(err, store.ErrFeedbackSuperseded) || errors.Is(err, store.ErrLeaseLost) || errors.Is(err, context.Canceled) {
		return nil
	}
	if err != nil {
		return w.handleFailure(ctx, job, err)
	}
	w.logger.Info("feedback job completed", append(logFields(job), "outcome", store.FeedbackCompleted)...)
	return nil
}

func (w *Worker) execute(ctx context.Context, job *store.FeedbackJob) error {
	ref := gitlab.FeedbackRef{
		GitLabInstance: job.GitLabInstance, ProjectID: job.ProjectID, ProjectPath: job.ProjectPath,
		MergeRequestIID: job.MergeRequestIID, NoteID: job.NoteID, ActorID: job.ActorID,
	}
	comment, found, err := w.gitlab.LoadFeedbackComment(ctx, ref)
	if err != nil {
		return err
	}
	if !found || comment.Ignored {
		role := comment.Role
		if role == "" {
			role = "source_unavailable"
		}
		return w.store.CompleteFeedbackJob(ctx, job.ID, job.SourceEventID, w.owner, comment.AccessLevel,
			role, comment.SourceURL, nil, w.now().UTC(), sourceCheckInterval)
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
	evaluationCtx := usage.WithScope(ctx, usage.Scope{
		RequestKind: usage.RequestFeedback, FeedbackJobID: job.ID, Attempt: job.AttemptCount,
	})
	assessment, err := w.evaluator.Evaluate(evaluationCtx, memory.Input{
		ProjectID: job.ProjectID, ProjectPath: job.ProjectPath, MergeRequestIID: job.MergeRequestIID,
		ReviewTargetID: job.ReviewTargetID, HeadSHA: job.HeadSHA, Summary: result.Summary,
		ActorID: job.ActorID, ActorAccess: comment.AccessLevel, ActorRole: comment.Role,
		Comment: comment.Body, Findings: findings,
	})
	if err != nil {
		return err
	}
	decisions := make([]store.FeedbackDecision, len(assessment.Decisions))
	for index, decision := range assessment.Decisions {
		decisions[index] = store.FeedbackDecision{
			MemoryID:   memory.ID(job.GitLabInstance, job.ProjectID, job.NoteID, decision.TargetType, decision.TargetID),
			TargetType: decision.TargetType, TargetID: decision.TargetID, Outcome: decision.Outcome,
			Confidence: decision.Confidence, Lesson: decision.Lesson,
		}
	}
	return w.store.CompleteFeedbackJob(ctx, job.ID, job.SourceEventID, w.owner, comment.AccessLevel,
		comment.Role, comment.SourceURL, decisions, w.now().UTC(), sourceCheckInterval)
}

func (w *Worker) handleFailure(ctx context.Context, job *store.FeedbackJob, err error) error {
	var failureError *failure.Error
	if !errors.As(err, &failureError) {
		failureError = &failure.Error{Category: "internal_feedback_failure", Retryable: true}
	}
	now := w.now().UTC()
	if !failureError.Retryable {
		if err := w.store.FinishFeedbackJob(ctx, job.ID, job.SourceEventID, w.owner, failureError.Category, now); err != nil {
			if errors.Is(err, store.ErrFeedbackSuperseded) {
				return nil
			}
			return err
		}
		w.logger.Warn("feedback job failed", append(logFields(job), "outcome", store.FeedbackFailed, "reason", failureError.Category)...)
		return nil
	}
	delay := localBackoff(job.AttemptCount)
	if failureError.RetryAfter > delay {
		delay = failureError.RetryAfter
	}
	state, retryErr := w.store.RetryFeedbackJob(ctx, job.ID, job.SourceEventID, w.owner, now, now.Add(delay), maxAttempts, failureError.Category)
	if retryErr != nil {
		if errors.Is(retryErr, store.ErrFeedbackSuperseded) {
			return nil
		}
		return retryErr
	}
	w.logger.Info("feedback job deferred", append(logFields(job), "outcome", state, "reason", failureError.Category)...)
	return nil
}

func (w *Worker) reconcileSources(ctx context.Context) {
	now := w.now().UTC()
	sources, err := w.store.DueFeedbackSources(ctx, now, 10)
	if err != nil {
		w.logger.Error("feedback source reconciliation failed", "reason", "persistence_failed")
		return
	}
	for _, source := range sources {
		ref := gitlab.FeedbackRef{
			GitLabInstance: source.GitLabInstance, ProjectID: source.ProjectID, ProjectPath: source.ProjectPath,
			MergeRequestIID: source.MergeRequestIID, NoteID: source.NoteID, ActorID: source.ActorID,
		}
		exists, updatedAt, checkErr := w.gitlab.CheckFeedbackSource(ctx, ref)
		if checkErr != nil {
			w.logger.Warn("feedback source check deferred", "feedback_job_id", source.JobID, "reason", failureCategory(checkErr))
			exists, updatedAt = true, source.SourceUpdatedAt
		}
		if err := w.store.ReconcileFeedbackSource(ctx, source.JobID, exists, updatedAt, now, sourceCheckInterval); err != nil {
			w.logger.Error("feedback source reconciliation failed", "feedback_job_id", source.JobID, "reason", "persistence_failed")
		}
	}
}

func failureCategory(err error) string {
	var failureError *failure.Error
	if errors.As(err, &failureError) {
		return failureError.Category
	}
	return "feedback_source_check_failed"
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
		"note_id", job.NoteID,
		"actor_id", job.ActorID,
		"attempt", job.AttemptCount,
	}
}
