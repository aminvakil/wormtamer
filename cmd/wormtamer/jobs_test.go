package main

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aminvakil/wormtamer/internal/store"
)

func TestExecuteJobsCommandListsEmptyDatabase(t *testing.T) {
	storage, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "wormtamer.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	var output bytes.Buffer
	if err := executeJobsCommand(context.Background(), storage, jobsCommand{action: jobsActionListFailed}, &output); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(output.String()) != `{"jobs":[],"truncated":false}` {
		t.Fatalf("list output = %s", output.String())
	}
}

func TestExecuteJobsCommandPreservesRetryErrors(t *testing.T) {
	storage, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "wormtamer.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	ctx := context.Background()
	var output bytes.Buffer

	err = executeJobsCommand(ctx, storage, jobsCommand{
		action: jobsActionRetry, kind: store.FailedJobKindReview, jobID: 1,
	}, &output)
	if !errors.Is(err, store.ErrJobNotFound) {
		t.Fatalf("missing retry error = %v", err)
	}
	created, err := storage.CreateReconciledJob(ctx, store.ReconciledReview{
		GitLabInstance: "http://gitlab.internal", ProjectID: 42, MergeRequestIID: 7,
		HeadSHA: strings.Repeat("a", 40),
	})
	if err != nil {
		t.Fatal(err)
	}
	err = executeJobsCommand(ctx, storage, jobsCommand{
		action: jobsActionRetry, kind: store.FailedJobKindReview, jobID: created.JobID,
	}, &output)
	if !errors.Is(err, store.ErrJobNotFailed) {
		t.Fatalf("non-failed retry error = %v", err)
	}
}

func TestExecuteJobsCommandRetriesFeedback(t *testing.T) {
	storage, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "wormtamer.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	ctx := context.Background()
	now := time.Now().UTC().Add(time.Second)
	accepted, err := storage.AcceptEvent(ctx, store.Event{
		DeliveryID: "review", GitLabInstance: "http://gitlab.internal", ProjectID: 42,
		ProjectPath: "group/project", MergeRequestIID: 7, HeadSHA: strings.Repeat("a", 40),
		Action: "open", Payload: []byte(`{"object_kind":"merge_request"}`), QueueReview: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	reviewJob, err := storage.ClaimJob(ctx, now)
	if err != nil || reviewJob == nil {
		t.Fatalf("ClaimJob() = %+v, %v", reviewJob, err)
	}
	if err := storage.SaveReviewResult(ctx, accepted.JobID, []byte(`{"summary":"done","findings":[]}`), nil, nil, store.PatchIDUnavailable, "", now); err != nil {
		t.Fatal(err)
	}
	if err := storage.CompletePublication(ctx, accepted.JobID, "<!-- review -->", 99, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	feedbackResult, err := storage.AcceptEvent(ctx, store.Event{
		DeliveryID: "feedback", GitLabInstance: "http://gitlab.internal", ProjectID: 42,
		ProjectPath: "group/project", MergeRequestIID: 7, HeadSHA: strings.Repeat("a", 40),
		Action: "close", Payload: []byte(`{"object_kind":"merge_request"}`),
		QueueFeedback: true, TerminalState: "closed",
	})
	if err != nil {
		t.Fatal(err)
	}
	feedbackJob, err := storage.ClaimFeedbackJob(ctx, now.Add(2*time.Second))
	if err != nil || feedbackJob == nil {
		t.Fatalf("ClaimFeedbackJob() = %+v, %v", feedbackJob, err)
	}
	if err := storage.FinishFeedbackJob(ctx, feedbackJob.ID,
		"gitlab_authorization_failed", now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	if err := executeJobsCommand(ctx, storage, jobsCommand{
		action: jobsActionRetry, kind: store.FailedJobKindFeedback, jobID: feedbackResult.FeedbackJobID,
	}, &output); err != nil {
		t.Fatal(err)
	}
	want := `{"kind":"feedback","job_id":` + strconv.FormatInt(feedbackResult.FeedbackJobID, 10) + `,"retried":true}`
	if strings.TrimSpace(output.String()) != want {
		t.Fatalf("retry output = %s", output.String())
	}
	resumed, err := storage.ClaimFeedbackJob(ctx, time.Now().UTC())
	if err != nil || resumed == nil || resumed.ID != feedbackResult.FeedbackJobID {
		t.Fatalf("resumed feedback = %+v, %v", resumed, err)
	}
}
