package reconcile

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/aminvakil/wormtamer/internal/failure"
	"github.com/aminvakil/wormtamer/internal/gitlab"
	"github.com/aminvakil/wormtamer/internal/store"
)

const (
	cycleInterval = 5 * time.Minute
	maxPages      = 10
)

type JobStore interface {
	CreateReconciledJob(context.Context, store.ReconciledReview) (store.ReconciledResult, error)
}

type GitLabBroker interface {
	ResolveProject(context.Context, string) (int64, error)
	ListOpenMergeRequests(context.Context, int64, int) ([]gitlab.ReconciliationMergeRequest, int, error)
}

type Reconciler struct {
	store          JobStore
	gitlab         GitLabBroker
	gitlabInstance string
	projects       []string
	logger         *slog.Logger
	interval       time.Duration
	now            func() time.Time
}

func New(storage JobStore, gitLab GitLabBroker, gitLabInstance string, projects []string, logger *slog.Logger) *Reconciler {
	return &Reconciler{
		store: storage, gitlab: gitLab, gitlabInstance: gitLabInstance,
		projects: append([]string(nil), projects...), logger: logger,
		interval: cycleInterval, now: time.Now,
	}
}

func (r *Reconciler) Run(ctx context.Context) error {
	for {
		r.Scan(ctx)
		if !wait(ctx, r.interval) {
			return nil
		}
	}
}

func (r *Reconciler) Scan(ctx context.Context) {
	started := r.now()
	projectsScanned := 0
	queued := 0
	existing := 0
	outcome := "completed"
	for _, projectPath := range r.projects {
		if ctx.Err() != nil {
			outcome = "canceled"
			break
		}
		projectQueued, projectExisting, stop := r.scanProject(ctx, projectPath)
		queued += projectQueued
		existing += projectExisting
		if ctx.Err() != nil {
			outcome = "canceled"
			break
		}
		projectsScanned++
		if stop {
			outcome = "backpressured"
			break
		}
	}
	r.logger.Info("reconciliation cycle finished",
		"projects", projectsScanned,
		"queued", queued,
		"existing", existing,
		"duration_ms", r.now().Sub(started).Milliseconds(),
		"outcome", outcome,
	)
}

func (r *Reconciler) scanProject(ctx context.Context, projectPath string) (queued int, existing int, stopCycle bool) {
	started := r.now()
	projectID, err := r.gitlab.ResolveProject(ctx, projectPath)
	if err != nil {
		if ctx.Err() != nil {
			return 0, 0, false
		}
		r.logProjectFailure(projectPath, 0, 0, 0, 0, 0, started, failureCategory(err))
		return 0, 0, isBackpressure(err)
	}

	pages := 0
	observed := 0
	for page := 1; page <= maxPages; {
		mergeRequests, next, err := r.gitlab.ListOpenMergeRequests(ctx, projectID, page)
		if err != nil {
			if ctx.Err() != nil {
				return queued, existing, false
			}
			r.logProjectFailure(projectPath, projectID, pages, observed, queued, existing, started, failureCategory(err))
			return queued, existing, isBackpressure(err)
		}
		pages++
		observed += len(mergeRequests)
		for _, mergeRequest := range mergeRequests {
			if mergeRequest.Draft || mergeRequest.WorkInProgress {
				continue
			}
			result, err := r.store.CreateReconciledJob(ctx, store.ReconciledReview{
				GitLabInstance: r.gitlabInstance, ProjectID: mergeRequest.ProjectID,
				MergeRequestIID: mergeRequest.MergeRequestIID, HeadSHA: mergeRequest.HeadSHA,
			})
			if err != nil {
				if ctx.Err() != nil {
					return queued, existing, false
				}
				r.logProjectFailure(projectPath, projectID, pages, observed, queued, existing, started, "persistence_failed")
				return queued, existing, false
			}
			if result.NewlyQueued {
				queued++
			} else {
				existing++
			}
		}
		if next == 0 {
			r.logger.Info("reconciliation project finished",
				"project_path", bounded(projectPath), "project_id", projectID,
				"pages", pages, "merge_requests", observed,
				"queued", queued, "existing", existing,
				"duration_ms", r.now().Sub(started).Milliseconds(), "outcome", "completed")
			return queued, existing, false
		}
		if page == maxPages {
			r.logProjectFailure(projectPath, projectID, pages, observed, queued, existing, started, "merge_request_list_page_limit_exceeded")
			return queued, existing, false
		}
		page = next
	}
	return queued, existing, false
}

func (r *Reconciler) logProjectFailure(projectPath string, projectID int64, pages, observed, queued, existing int, started time.Time, category string) {
	r.logger.Warn("reconciliation project failed",
		"project_path", bounded(projectPath), "project_id", projectID,
		"pages", pages, "merge_requests", observed, "queued", queued, "existing", existing,
		"duration_ms", r.now().Sub(started).Milliseconds(),
		"outcome", "failed", "reason", category)
}

func isBackpressure(err error) bool {
	var failureError *failure.Error
	if !errors.As(err, &failureError) {
		return false
	}
	return failureError.Category == "gitlab_rate_limited" || failureError.RetryAfter > 0
}

func failureCategory(err error) string {
	var failureError *failure.Error
	if errors.As(err, &failureError) {
		return failureError.Category
	}
	return "gitlab_request_failed"
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

func bounded(value string) string {
	if len(value) <= 256 {
		return value
	}
	return value[:256]
}
