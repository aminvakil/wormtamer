package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestCISelectionAndClaimStayOnSameDueIdentity(t *testing.T) {
	storage := openTestStore(t)
	defer storage.Close()
	ctx := context.Background()
	first, err := storage.AcceptEvent(ctx, readyEvent("ci-first"))
	if err != nil {
		t.Fatal(err)
	}
	event := readyEvent("ci-second")
	event.MergeRequestIID++
	second, err := storage.AcceptEvent(ctx, event)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second).Add(time.Hour)
	selected, err := storage.NextJob(ctx, now)
	if err != nil || selected == nil || selected.ID != first.JobID || selected.AttemptCount != 0 || selected.State != JobQueued {
		t.Fatalf("NextJob() = %+v, %v", selected, err)
	}
	if err := storage.DeferCI(ctx, selected.ID, "running", now); err != nil {
		t.Fatal(err)
	}
	if claimed, err := storage.ClaimSelectedJob(ctx, selected.ID, "success", now); err != nil || claimed != nil {
		t.Fatalf("no-longer-due claim = %+v, %v", claimed, err)
	}
	page, err := storage.ListReviewRecords(ctx, JobQueued, 0, 10)
	if err != nil || len(page.Records) != 2 || page.Records[0].ID != second.JobID || page.Records[0].AttemptCount != 0 ||
		!page.Records[1].WaitingOnCI || page.Records[1].CIStatus != "running" || page.Records[1].AttemptCount != 0 {
		t.Fatalf("persisted queued records = %+v, %v", page, err)
	}
	claimed, err := storage.ClaimSelectedJob(ctx, second.JobID, "none", now)
	if err != nil || claimed == nil || claimed.ID != second.JobID || claimed.AttemptCount != 1 {
		t.Fatalf("other eligible claim = %+v, %v", claimed, err)
	}
	if err := storage.DeferCI(ctx, claimed.ID, "failed", now); !errors.Is(err, ErrJobNotDue) {
		t.Fatalf("running CI wait error = %v", err)
	}
	if err := storage.DeferCI(ctx, first.JobID, "running", now.Add(time.Second)); !errors.Is(err, ErrJobNotDue) {
		t.Fatalf("early CI wait error = %v", err)
	}
}
