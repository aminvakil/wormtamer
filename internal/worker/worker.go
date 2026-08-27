package worker

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"strconv"
	"time"

	"github.com/aminvakil/wormtamer/internal/failure"
	"github.com/aminvakil/wormtamer/internal/gitlab"
	"github.com/aminvakil/wormtamer/internal/repository"
	"github.com/aminvakil/wormtamer/internal/review"
	"github.com/aminvakil/wormtamer/internal/store"
	"github.com/aminvakil/wormtamer/internal/usage"
)

const (
	pollInterval        = time.Second
	leaseDuration       = 2 * time.Minute
	leaseRenewInterval  = 30 * time.Second
	shutdownGracePeriod = 10 * time.Second
	maxAttempts         = 5
	initialBackoff      = 5 * time.Second
	maxLocalBackoff     = 5 * time.Minute
)

type JobStore interface {
	ClaimJob(context.Context, string, time.Time, time.Duration, int) (*store.Job, error)
	RenewLease(context.Context, int64, string, time.Time, time.Duration) (bool, error)
	DeferPendingPatchID(context.Context, int64, string, time.Time, time.Time) error
	FindCanonicalReviewJob(context.Context, int64, string) (int64, bool, error)
	CompleteEquivalentReview(context.Context, int64, string, int64, string, time.Time) error
	SaveReviewResult(context.Context, int64, string, []byte, []string, []store.ReviewMemoryRetrieval, string, string, time.Time) error
	ListReviewMemories(context.Context, string, int64) ([]store.ReviewMemory, error)
	RetryJob(context.Context, int64, string, time.Time, time.Time, int, string, string) (string, error)
	FinishJob(context.Context, int64, string, string, string, string, time.Time) error
	CompletePublication(context.Context, int64, string, string, int64, time.Time) error
}

type GitLabBroker interface {
	LoadReview(context.Context, gitlab.Identity) (gitlab.Snapshot, error)
	CheckCurrent(context.Context, gitlab.Identity) error
	FindNote(context.Context, gitlab.Identity, string) (int64, bool, error)
	PostNote(context.Context, gitlab.Identity, string) (int64, error)
}

type RepositoryWorkspaces interface {
	Prepare(context.Context, gitlab.Snapshot, []repository.Memory) (repository.Workspace, error)
}

type Reviewer interface {
	Review(context.Context, gitlab.Snapshot, repository.ToolBroker) (review.Result, []byte, error)
}

var errPatchIDDeferred = errors.New("patch ID deferred")

type Worker struct {
	store         JobStore
	gitlab        GitLabBroker
	workspaces    RepositoryWorkspaces
	reviewer      Reviewer
	logger        *slog.Logger
	owner         string
	forbidden     []string
	now           func() time.Time
	shutdownGrace time.Duration
}

func New(storage JobStore, gitLab GitLabBroker, workspaces RepositoryWorkspaces, reviewer Reviewer, logger *slog.Logger, forbidden []string) (*Worker, error) {
	ownerBytes := make([]byte, 16)
	if _, err := rand.Read(ownerBytes); err != nil {
		return nil, errors.New("generate worker lease owner")
	}
	return &Worker{
		store: storage, gitlab: gitLab, workspaces: workspaces, reviewer: reviewer, logger: logger,
		owner: hex.EncodeToString(ownerBytes), forbidden: append([]string(nil), forbidden...),
		now: time.Now, shutdownGrace: shutdownGracePeriod,
	}, nil
}

func (w *Worker) Run(ctx context.Context) error {
	for {
		if ctx.Err() != nil {
			return nil
		}
		job, err := w.claim(ctx)
		if err != nil {
			w.logger.Error("review job claim failed", "reason", "persistence_failed")
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

		jobCtx, cancelJob := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			done <- w.processClaimed(jobCtx, job)
		}()
		select {
		case err := <-done:
			cancelJob()
			if err != nil && !errors.Is(err, store.ErrLeaseLost) {
				w.logger.Error("review job processing failed", "job_id", job.ID, "reason", "persistence_failed")
			}
		case <-ctx.Done():
			timer := time.NewTimer(w.shutdownGrace)
			select {
			case err := <-done:
				timer.Stop()
				if err != nil && !errors.Is(err, store.ErrLeaseLost) {
					w.logger.Error("review job processing failed during shutdown", "job_id", job.ID, "reason", "persistence_failed")
				}
			case <-timer.C:
				cancelJob()
				w.logger.Warn("review job abandoned during shutdown",
					append(jobLogFields(job), "reason", "shutdown_deadline_exceeded")...)
			}
			cancelJob()
			return nil
		}
	}
}

func (w *Worker) ProcessOne(ctx context.Context) (bool, error) {
	job, err := w.claim(ctx)
	if err != nil || job == nil {
		return false, err
	}
	return true, w.processClaimed(ctx, job)
}

func (w *Worker) claim(ctx context.Context) (*store.Job, error) {
	return w.store.ClaimJob(ctx, w.owner, w.now().UTC(), leaseDuration, maxAttempts)
}

func (w *Worker) processClaimed(ctx context.Context, job *store.Job) error {
	w.logger.Info("review job started", jobLogFields(job)...)
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
				now := w.now().UTC()
				renewed, err := w.store.RenewLease(workCtx, job.ID, w.owner, now, leaseDuration)
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
	if ctx.Err() != nil || errors.Is(err, context.Canceled) {
		return nil
	}
	if err == nil {
		w.logger.Info("review job completed", append(jobLogFields(job), "outcome", store.JobCompleted)...)
		return nil
	}
	if errors.Is(err, errPatchIDDeferred) {
		return nil
	}
	return w.handleFailure(ctx, job, err)
}

func (w *Worker) execute(ctx context.Context, job *store.Job) error {
	identity := gitlab.Identity{
		GitLabInstance: job.GitLabInstance, ProjectID: job.ProjectID,
		MergeRequestIID: job.MergeRequestIID, HeadSHA: job.HeadSHA,
	}
	marker := publicationMarker(identity)
	if len(job.ValidatedResultJSON) == 0 {
		noteID, found, err := w.gitlab.FindNote(ctx, identity, marker)
		if err != nil {
			return err
		}
		if found {
			if err := w.gitlab.CheckCurrent(ctx, identity); err != nil {
				return err
			}
			if err := w.store.CompletePublication(ctx, job.ID, w.owner, marker, noteID, w.now().UTC()); err != nil {
				if errors.Is(err, store.ErrLeaseLost) {
					return err
				}
				return failure.Retry("persistence_failed", 0)
			}
			w.logger.Info("review generation skipped",
				append(jobLogFields(job), "outcome", "existing_publication")...)
			return nil
		}
	}

	var result review.Result
	if len(job.ValidatedResultJSON) == 0 {
		snapshot, err := w.gitlab.LoadReview(ctx, identity)
		if err != nil {
			return err
		}
		if snapshot.PatchIDStatus == gitlab.PatchIDPending {
			if job.PatchIDStatus == store.PatchIDUnknown && maxAttempts-job.AttemptCount >= 3 {
				now := w.now().UTC()
				if err := w.store.DeferPendingPatchID(ctx, job.ID, w.owner, now, now.Add(localBackoff(job.AttemptCount))); err != nil {
					if errors.Is(err, store.ErrLeaseLost) {
						return err
					}
					return failure.Retry("persistence_failed", 0)
				}
				w.logger.Info("review job deferred",
					append(jobLogFields(job), "outcome", store.JobQueued, "reason", "merge_request_patch_id_pending")...)
				return errPatchIDDeferred
			}
			snapshot.PatchIDStatus = gitlab.PatchIDUnavailable
			snapshot.PatchIDSHA = ""
		}
		if snapshot.PatchIDStatus == gitlab.PatchIDAvailable {
			canonicalJobID, found, err := w.store.FindCanonicalReviewJob(ctx, job.ID, snapshot.PatchIDSHA)
			if err != nil {
				return failure.Retry("persistence_failed", 0)
			}
			if found {
				if err := w.gitlab.CheckCurrent(ctx, identity); err != nil {
					return err
				}
				if err := w.store.CompleteEquivalentReview(ctx, job.ID, w.owner, canonicalJobID, snapshot.PatchIDSHA, w.now().UTC()); err != nil {
					if errors.Is(err, store.ErrLeaseLost) {
						return err
					}
					return failure.Retry("persistence_failed", 0)
				}
				w.logger.Info("review generation skipped",
					append(jobLogFields(job), "outcome", "equivalent_patch", "canonical_job_id", canonicalJobID)...)
				return nil
			}
		}
		memories, err := w.store.ListReviewMemories(ctx, snapshot.Identity.GitLabInstance, snapshot.Identity.ProjectID)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return failure.Retry("memory_retrieval_failed", 0)
		}
		materialized := make([]repository.Memory, len(memories))
		retrievedAt := w.now().UTC()
		retrievals := make([]store.ReviewMemoryRetrieval, len(memories))
		for index, memory := range memories {
			materialized[index] = repository.Memory{
				ID: memory.MemoryID, Lesson: memory.Lesson, SourceURL: memory.SourceURL, UpdatedAt: memory.UpdatedAt,
			}
			retrievals[index] = store.ReviewMemoryRetrieval{
				MemoryID: memory.MemoryID, MemoryUpdatedAt: memory.UpdatedAt, RetrievedAt: retrievedAt,
			}
		}
		workspace, err := w.workspaces.Prepare(ctx, snapshot, materialized)
		if err != nil {
			return err
		}
		prepared := workspace.Context()
		snapshot.WorkingDirectory = prepared.WorkingDirectory
		snapshot.ReviewMemoryPath = prepared.MemoryPath
		snapshot.PreparedRepositories = make([]gitlab.PreparedRepository, len(prepared.RelatedRepositories))
		for index, related := range prepared.RelatedRepositories {
			snapshot.PreparedRepositories[index] = gitlab.PreparedRepository{
				Repository: related.Repository, Path: related.Path, InitialRevision: related.InitialRevision,
			}
		}
		reviewCtx := usage.WithScope(ctx, usage.Scope{
			RequestKind: usage.RequestReview, ReviewJobID: job.ID, Attempt: job.AttemptCount,
		})
		validated, encoded, reviewErr := w.reviewer.Review(reviewCtx, snapshot, workspace)
		closeErr := workspace.Close()
		if closeErr != nil {
			return errors.Join(failure.Retry("repository_workspace_cleanup_failed", 0), closeErr, reviewErr)
		}
		if reviewErr != nil {
			return reviewErr
		}
		job.FindingIDs = findingIDs(identity, len(validated.Findings))
		if err := applyFindingIDs(&validated, job.FindingIDs); err != nil {
			return err
		}
		patchIDStatus := store.PatchIDUnavailable
		if snapshot.PatchIDStatus == gitlab.PatchIDAvailable {
			patchIDStatus = store.PatchIDAvailable
		}
		if err := w.store.SaveReviewResult(ctx, job.ID, w.owner, encoded, job.FindingIDs, retrievals, patchIDStatus, snapshot.PatchIDSHA, w.now().UTC()); err != nil {
			if errors.Is(err, store.ErrLeaseLost) {
				return err
			}
			return failure.Retry("persistence_failed", 0)
		}
		result = validated
		job.ValidatedResultJSON = encoded
		job.State = store.JobPublishing
	} else {
		decoded, err := review.DecodeStored(job.ValidatedResultJSON)
		if err != nil {
			return failure.Failed("invalid_stored_review_result")
		}
		if err := applyFindingIDs(&decoded, job.FindingIDs); err != nil {
			return err
		}
		result = decoded
	}

	if err := w.gitlab.CheckCurrent(ctx, identity); err != nil {
		return err
	}
	noteID, found, err := w.gitlab.FindNote(ctx, identity, marker)
	if err != nil {
		return err
	}
	if !found {
		if err := w.gitlab.CheckCurrent(ctx, identity); err != nil {
			return err
		}
		body, err := review.RenderNote(result, marker, w.forbidden)
		if err != nil {
			return err
		}
		noteID, err = w.gitlab.PostNote(ctx, identity, body)
		if err != nil {
			return err
		}
	}
	if err := w.store.CompletePublication(ctx, job.ID, w.owner, marker, noteID, w.now().UTC()); err != nil {
		if errors.Is(err, store.ErrLeaseLost) {
			return err
		}
		return failure.Retry("persistence_failed", 0)
	}
	return nil
}

func (w *Worker) handleFailure(ctx context.Context, job *store.Job, err error) error {
	if errors.Is(err, store.ErrLeaseLost) {
		return err
	}
	var failureError *failure.Error
	if !errors.As(err, &failureError) {
		failureError = &failure.Error{Category: "internal_worker_failure", Retryable: true}
	}
	now := w.now().UTC()
	if failureError.Obsolete {
		if err := w.store.FinishJob(ctx, job.ID, w.owner, store.JobObsolete, failureError.Category, failureError.Category, now); err != nil {
			return err
		}
		w.logger.Info("review job stopped", append(jobLogFields(job), "outcome", store.JobObsolete, "reason", failureError.Category)...)
		return nil
	}
	if !failureError.Retryable {
		if err := w.store.FinishJob(ctx, job.ID, w.owner, store.JobFailed, failureError.Category, failureError.Category, now); err != nil {
			return err
		}
		w.logger.Warn("review job failed", append(jobLogFields(job), "outcome", store.JobFailed, "reason", failureError.Category)...)
		return nil
	}

	delay := localBackoff(job.AttemptCount)
	if failureError.RetryAfter > delay {
		delay = failureError.RetryAfter
	}
	state, retryErr := w.store.RetryJob(ctx, job.ID, w.owner, now, now.Add(delay), maxAttempts, failureError.Category, failureError.Category)
	if retryErr != nil {
		return retryErr
	}
	w.logger.Info("review job deferred", append(jobLogFields(job), "outcome", state, "reason", failureError.Category)...)
	return nil
}

func findingIDs(identity gitlab.Identity, count int) []string {
	ids := make([]string, count)
	for index := range ids {
		ids[index] = review.FindingID(
			identity.GitLabInstance, identity.ProjectID, identity.MergeRequestIID,
			identity.HeadSHA, index+1,
		)
	}
	return ids
}

func applyFindingIDs(result *review.Result, ids []string) error {
	if len(result.Findings) != len(ids) {
		return failure.Failed("invalid_stored_review_result")
	}
	seen := make(map[string]struct{}, len(ids))
	for index, id := range ids {
		if !review.ValidFindingID(id) {
			return failure.Failed("invalid_stored_review_result")
		}
		if _, exists := seen[id]; exists {
			return failure.Failed("invalid_stored_review_result")
		}
		seen[id] = struct{}{}
		result.Findings[index].ID = id
	}
	return nil
}

func publicationMarker(identity gitlab.Identity) string {
	digest := sha256.New()
	_, _ = digest.Write([]byte(identity.GitLabInstance))
	_, _ = digest.Write([]byte{0})
	_, _ = digest.Write([]byte(strconv.FormatInt(identity.ProjectID, 10)))
	_, _ = digest.Write([]byte{0})
	_, _ = digest.Write([]byte(strconv.FormatInt(identity.MergeRequestIID, 10)))
	_, _ = digest.Write([]byte{0})
	_, _ = digest.Write([]byte(identity.HeadSHA))
	return "<!-- wormtamer:review=" + hex.EncodeToString(digest.Sum(nil)) + " -->"
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

func jobLogFields(job *store.Job) []any {
	return []any{
		"job_id", job.ID,
		"project_id", job.ProjectID,
		"merge_request_iid", job.MergeRequestIID,
		"head_sha", bounded(job.HeadSHA),
		"attempt", job.AttemptCount,
		"state", job.State,
	}
}

func bounded(value string) string {
	if len(value) <= 128 {
		return value
	}
	return value[:128]
}
