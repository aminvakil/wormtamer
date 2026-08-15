package worker

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"time"

	"github.com/aminvakil/wormtamer/internal/failure"
	"github.com/aminvakil/wormtamer/internal/gitlab"
	"github.com/aminvakil/wormtamer/internal/publicsource"
	"github.com/aminvakil/wormtamer/internal/repository"
	"github.com/aminvakil/wormtamer/internal/review"
	"github.com/aminvakil/wormtamer/internal/store"
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
	SaveReviewResult(context.Context, int64, string, []byte, []string, []store.ReviewMemoryRetrieval, time.Time) error
	ListActiveReviewMemories(context.Context, string, int64) ([]store.ReviewMemory, error)
	RetryJob(context.Context, int64, string, time.Time, time.Time, int, string, string) (string, error)
	FinishJob(context.Context, int64, string, string, string, string, time.Time) error
	CompletePublication(context.Context, int64, string, string, int64, time.Time) error
}

type GitLabBroker interface {
	LoadReview(context.Context, gitlab.Identity) (gitlab.Snapshot, error)
	LoadRepositoryArchive(context.Context, gitlab.Identity) ([]byte, error)
	LoadRelatedRepositoryArchive(context.Context, string, string) (string, []byte, error)
	CheckCurrent(context.Context, gitlab.Identity) error
	FindNote(context.Context, gitlab.Identity, string) (int64, bool, error)
	PostNote(context.Context, gitlab.Identity, string) (int64, error)
}

type RepositoryWorkspaces interface {
	Create(context.Context, string, []byte) (repository.Workspace, error)
}

type Reviewer interface {
	Review(context.Context, gitlab.Snapshot, repository.ToolBroker) (review.Result, []byte, error)
}

type Worker struct {
	store                    JobStore
	gitlab                   GitLabBroker
	public                   publicsource.Broker
	allowedPublicDomains     []string
	publicGitHubRepositories []string
	workspaces               RepositoryWorkspaces
	reviewer                 Reviewer
	logger                   *slog.Logger
	owner                    string
	forbidden                []string
	now                      func() time.Time
	shutdownGrace            time.Duration
}

type reviewRepository struct {
	identity            gitlab.Identity
	currentRepository   string
	relatedRepositories map[string]struct{}
	gitlab              GitLabBroker
	workspaces          RepositoryWorkspaces
	open                map[string]repository.Workspace
}

func newReviewRepository(snapshot gitlab.Snapshot, gitLab GitLabBroker, workspaces RepositoryWorkspaces) *reviewRepository {
	related := make(map[string]struct{}, len(snapshot.RelatedRepositories))
	for _, repositoryPath := range snapshot.RelatedRepositories {
		related[repositoryPath] = struct{}{}
	}
	return &reviewRepository{
		identity: snapshot.Identity, currentRepository: snapshot.ProjectPath,
		relatedRepositories: related, gitlab: gitLab, workspaces: workspaces,
		open: make(map[string]repository.Workspace),
	}
}

func (r *reviewRepository) Call(ctx context.Context, name string, arguments map[string]any) (map[string]any, error) {
	requested, ok := arguments["repository"].(string)
	if !ok || requested == "" {
		return nil, failure.Failed("repository_tool_arguments_invalid")
	}
	if requested != r.currentRepository {
		if _, allowed := r.relatedRepositories[requested]; !allowed {
			return nil, failure.Failed("repository_unavailable")
		}
	}
	workspace := r.open[requested]
	if workspace == nil {
		if len(r.open) >= repository.ReviewResourceLimit {
			return nil, failure.Retry("repository_limit_exceeded", 0)
		}
		var revision string
		var archive []byte
		var err error
		if requested == r.currentRepository {
			revision = r.identity.HeadSHA
			archive, err = r.gitlab.LoadRepositoryArchive(ctx, r.identity)
		} else {
			revision, archive, err = r.gitlab.LoadRelatedRepositoryArchive(ctx, r.currentRepository, requested)
		}
		if err != nil {
			return nil, err
		}
		workspace, err = r.workspaces.Create(ctx, revision, archive)
		if err != nil {
			return nil, err
		}
		r.open[requested] = workspace
	}
	workspaceArguments := make(map[string]any, len(arguments)-1)
	for key, value := range arguments {
		if key != "repository" {
			workspaceArguments[key] = value
		}
	}
	result, err := workspace.Call(ctx, name, workspaceArguments)
	if err != nil {
		return nil, err
	}
	result["repository"] = requested
	encoded, err := json.Marshal(result)
	if err != nil {
		return nil, failure.Failed("repository_tool_output_invalid")
	}
	if len(encoded) > repository.MaxToolResponseBytes {
		return nil, failure.Failed("repository_tool_output_limit_exceeded")
	}
	return result, nil
}

func (r *reviewRepository) Close() error {
	var firstError error
	for _, workspace := range r.open {
		if err := workspace.Close(); err != nil && firstError == nil {
			firstError = err
		}
	}
	return firstError
}

func New(storage JobStore, gitLab GitLabBroker, publicBroker publicsource.Broker, allowedPublicDomains, publicGitHubRepositories []string, workspaces RepositoryWorkspaces, reviewer Reviewer, logger *slog.Logger, forbidden []string) (*Worker, error) {
	ownerBytes := make([]byte, 16)
	if _, err := rand.Read(ownerBytes); err != nil {
		return nil, errors.New("generate worker lease owner")
	}
	return &Worker{
		store: storage, gitlab: gitLab, public: publicBroker,
		allowedPublicDomains:     append([]string(nil), allowedPublicDomains...),
		publicGitHubRepositories: append([]string(nil), publicGitHubRepositories...),
		workspaces:               workspaces, reviewer: reviewer, logger: logger,
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
		snapshot.AllowedPublicDomains = append([]string(nil), w.allowedPublicDomains...)
		snapshot.PublicGitHubRepositories = append([]string(nil), w.publicGitHubRepositories...)
		tools := newReviewTools(snapshot, w.gitlab, w.public, w.workspaces, w.store, w.now)
		validated, encoded, reviewErr := w.reviewer.Review(ctx, snapshot, tools)
		closeErr := tools.Close()
		if reviewErr != nil {
			return reviewErr
		}
		if closeErr != nil {
			return failure.Retry("repository_workspace_cleanup_failed", 0)
		}
		job.FindingIDs = findingIDs(identity, len(validated.Findings))
		if err := applyFindingIDs(&validated, job.FindingIDs); err != nil {
			return err
		}
		if err := w.store.SaveReviewResult(ctx, job.ID, w.owner, encoded, job.FindingIDs, tools.Retrievals(), w.now().UTC()); err != nil {
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
