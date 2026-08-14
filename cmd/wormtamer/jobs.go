package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/aminvakil/wormtamer/internal/store"
)

const (
	jobsActionListFailed = "list-failed"
	jobsActionRetry      = "retry"
	failedJobsLimit      = 100
)

type jobsCommand struct {
	action string
	kind   string
	jobID  int64
}

type failedJobsOutput struct {
	Jobs      []failedJobOutput `json:"jobs"`
	Truncated bool              `json:"truncated"`
}

type failedJobOutput struct {
	Kind              string `json:"kind"`
	JobID             int64  `json:"job_id"`
	AttemptCount      int    `json:"attempt_count"`
	LastErrorCategory string `json:"last_error_category"`
	UpdatedAt         string `json:"updated_at"`
	ProjectID         int64  `json:"project_id"`
	MergeRequestIID   int64  `json:"merge_request_iid"`
	HeadSHA           string `json:"head_sha,omitempty"`
	NoteID            int64  `json:"note_id,omitempty"`
}

type retriedJobOutput struct {
	Kind    string `json:"kind"`
	JobID   int64  `json:"job_id"`
	Retried bool   `json:"retried"`
}

func executeJobsCommand(ctx context.Context, storage *store.Store, command jobsCommand, output io.Writer) error {
	switch command.action {
	case jobsActionListFailed:
		jobs, truncated, err := storage.ListFailedJobs(ctx, failedJobsLimit)
		if err != nil {
			return err
		}
		response := failedJobsOutput{
			Jobs:      make([]failedJobOutput, len(jobs)),
			Truncated: truncated,
		}
		for index, job := range jobs {
			response.Jobs[index] = failedJobOutput{
				Kind: job.Kind, JobID: job.JobID, AttemptCount: job.AttemptCount,
				LastErrorCategory: job.LastErrorCategory, UpdatedAt: job.UpdatedAt,
				ProjectID: job.ProjectID, MergeRequestIID: job.MergeRequestIID,
				HeadSHA: job.HeadSHA, NoteID: job.NoteID,
			}
		}
		return writeJobsOutput(output, response)
	case jobsActionRetry:
		var err error
		switch command.kind {
		case store.FailedJobKindReview:
			err = storage.RetryFailedReviewJob(ctx, command.jobID, time.Now().UTC())
		case store.FailedJobKindFeedback:
			err = storage.RetryFailedFeedbackJob(ctx, command.jobID, time.Now().UTC())
		default:
			return errors.New("invalid job kind")
		}
		if errors.Is(err, store.ErrJobNotFound) {
			return fmt.Errorf("%s job does not exist: %w", command.kind, err)
		}
		if errors.Is(err, store.ErrJobNotFailed) {
			return fmt.Errorf("%s job is not failed: %w", command.kind, err)
		}
		if err != nil {
			return err
		}
		return writeJobsOutput(output, retriedJobOutput{Kind: command.kind, JobID: command.jobID, Retried: true})
	default:
		return errors.New("invalid jobs command")
	}
}

func writeJobsOutput(output io.Writer, value any) error {
	if err := json.NewEncoder(output).Encode(value); err != nil {
		return errors.New("write jobs output")
	}
	return nil
}
