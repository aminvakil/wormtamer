package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aminvakil/wormtamer/internal/failure"
	"github.com/aminvakil/wormtamer/internal/publicsource"
)

func TestReviewPublicSourcesFetchesAndAttributesApprovedWebContent(t *testing.T) {
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	broker := &fakePublicBroker{web: publicsource.WebResult{
		SourceURL: "https://docs.example.com/page", ContentType: "text/plain",
		Content: "documentation", RetrievedAt: now,
	}}
	tools := newReviewPublicSources(broker, &fakeWorkspaces{}, nil)
	result, err := tools.Call(context.Background(), publicsource.ToolFetchURL, map[string]any{"url": "https://docs.example.com/page"})
	if err != nil {
		t.Fatal(err)
	}
	if broker.fetchURL != "https://docs.example.com/page" || result["authority"] != "untrusted_public" || result["content"] != "documentation" || result["retrieved_at"] != now.Format(time.RFC3339Nano) {
		t.Fatalf("result=%+v broker=%+v", result, broker)
	}
}

func TestReviewPublicSourcesPinsConfiguredRepositoryAndClosesWorkspace(t *testing.T) {
	repositoryURL := "https://github.com/nginx/nginx"
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	broker := &fakePublicBroker{snapshot: publicsource.RepositorySnapshot{
		Repository: repositoryURL, Revision: workerHead, Archive: []byte("public-archive"), RetrievedAt: now,
	}}
	workspaces := &fakeWorkspaces{}
	tools := newReviewPublicSources(broker, workspaces, []string{repositoryURL})
	result, err := tools.Call(context.Background(), publicsource.ToolReadFile, map[string]any{
		"repository": repositoryURL, "path": "src/nginx.c",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tools.Call(context.Background(), publicsource.ToolListFiles, map[string]any{"repository": repositoryURL}); err != nil {
		t.Fatal(err)
	}
	if broker.loadCalls != 1 || workspaces.createCalls != 1 || workspaces.revision != workerHead || string(workspaces.archive) != "public-archive" {
		t.Fatalf("broker=%+v workspaces=%+v", broker, workspaces)
	}
	if result["repository"] != repositoryURL || result["authority"] != "untrusted_public" || result["retrieved_at"] != now.Format(time.RFC3339Nano) {
		t.Fatalf("result = %+v", result)
	}
	if err := tools.Close(); err != nil || !workspaces.workspace.closed {
		t.Fatalf("Close() error=%v workspace=%+v", err, workspaces.workspace)
	}
}

func TestReviewPublicSourcesRejectsUnconfiguredRepositoryBeforeNetwork(t *testing.T) {
	broker := &fakePublicBroker{}
	tools := newReviewPublicSources(broker, &fakeWorkspaces{}, []string{"https://github.com/nginx/nginx"})
	_, err := tools.Call(context.Background(), publicsource.ToolListFiles, map[string]any{"repository": "https://github.com/other/project"})
	var failureError *failure.Error
	if !errors.As(err, &failureError) || failureError.Category != "public_repository_unavailable" || broker.loadCalls != 0 {
		t.Fatalf("error=%v broker=%+v", err, broker)
	}
}

type fakePublicBroker struct {
	web       publicsource.WebResult
	snapshot  publicsource.RepositorySnapshot
	fetchURL  string
	loadCalls int
}

func (b *fakePublicBroker) Fetch(_ context.Context, rawURL string) (publicsource.WebResult, error) {
	b.fetchURL = rawURL
	return b.web, nil
}

func (b *fakePublicBroker) LoadGitHubRepository(_ context.Context, _ string) (publicsource.RepositorySnapshot, error) {
	b.loadCalls++
	return b.snapshot, nil
}
