package reconcile

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/aminvakil/wormtamer/internal/failure"
	"github.com/aminvakil/wormtamer/internal/gitlab"
	"github.com/aminvakil/wormtamer/internal/store"
)

const testHead = "0123456789abcdef0123456789abcdef01234567"

func TestScanQueuesReadyMergeRequestsIdempotently(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v4/projects/group/project":
			writeJSON(t, w, map[string]any{"id": 42, "path_with_namespace": "group/project"})
		case "/api/v4/projects/42/merge_requests":
			if r.URL.Query().Get("page") == "1" {
				w.Header().Set("X-Next-Page", "2")
				writeJSON(t, w, []map[string]any{
					{"iid": 7, "project_id": 42, "state": "opened", "sha": testHead, "draft": false, "work_in_progress": false},
					{"iid": 7, "project_id": 42, "state": "opened", "sha": testHead, "draft": false, "work_in_progress": false},
					{"iid": 8, "project_id": 42, "state": "opened", "sha": testHead, "draft": true, "work_in_progress": false},
				})
				return
			}
			writeJSON(t, w, []map[string]any{
				{"iid": 9, "project_id": 42, "state": "opened", "sha": testHead, "draft": false, "work_in_progress": false},
			})
		default:
			t.Fatalf("unexpected request: %s", r.URL.String())
		}
	}))
	defer server.Close()

	storage := openStore(t)
	defer storage.Close()
	client, err := gitlab.New(server.URL, "token", []string{"group/project"}, false, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	reconciler := New(storage, client, server.URL, []string{"group/project"}, slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)))
	reconciler.Scan(context.Background())
	reconciler.Scan(context.Background())

	now := time.Now().UTC().Add(time.Second)
	first, err := storage.ClaimJob(context.Background(), now)
	if err != nil || first == nil {
		t.Fatalf("ClaimJob(first) = %+v, %v", first, err)
	}
	second, err := storage.ClaimJob(context.Background(), now)
	if err != nil || second == nil {
		t.Fatalf("ClaimJob(second) = %+v, %v", second, err)
	}
	if first.MergeRequestIID != 7 || second.MergeRequestIID != 9 {
		t.Fatalf("claimed merge requests = %d, %d", first.MergeRequestIID, second.MergeRequestIID)
	}
	third, err := storage.ClaimJob(context.Background(), now)
	if err != nil || third != nil {
		t.Fatalf("ClaimJob(third) = %+v, %v", third, err)
	}
}

func TestScanKeepsJobsFromPagesBeforeFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v4/projects/group/project":
			writeJSON(t, w, map[string]any{"id": 42, "path_with_namespace": "group/project"})
		case "/api/v4/projects/42/merge_requests":
			if r.URL.Query().Get("page") == "1" {
				w.Header().Set("X-Next-Page", "2")
				writeJSON(t, w, []map[string]any{{
					"iid": 7, "project_id": 42, "state": "opened", "sha": testHead,
				}})
				return
			}
			w.WriteHeader(http.StatusInternalServerError)
		default:
			t.Fatalf("unexpected request: %s", r.URL.String())
		}
	}))
	defer server.Close()
	storage := openStore(t)
	defer storage.Close()
	client, err := gitlab.New(server.URL, "token", []string{"group/project"}, false, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	New(storage, client, server.URL, []string{"group/project"}, slog.New(slog.NewJSONHandler(&logs, nil))).Scan(context.Background())

	job, err := storage.ClaimJob(context.Background(), time.Now().UTC().Add(time.Second))
	if err != nil || job == nil || job.MergeRequestIID != 7 {
		t.Fatalf("ClaimJob() = %+v, %v", job, err)
	}
	if !bytes.Contains(logs.Bytes(), []byte("gitlab_server_failure")) {
		t.Fatalf("failure was not logged: %s", logs.String())
	}
}

func TestBackpressureStopsLaterProjects(t *testing.T) {
	broker := &fakeBroker{resolve: func(_ context.Context, project string) (int64, error) {
		if project == "one/project" {
			return 0, failure.Retry("gitlab_rate_limited", time.Minute)
		}
		t.Fatalf("later project was requested: %s", project)
		return 0, nil
	}}
	reconciler := New(&fakeStore{}, broker, "http://gitlab.internal", []string{"one/project", "two/project"}, slog.Default())
	reconciler.Scan(context.Background())
}

func TestScanReportsCancellationDuringFinalProject(t *testing.T) {
	called := make(chan struct{}, 1)
	broker := &fakeBroker{resolve: func(ctx context.Context, _ string) (int64, error) {
		called <- struct{}{}
		<-ctx.Done()
		return 0, ctx.Err()
	}}
	var logs bytes.Buffer
	reconciler := New(&fakeStore{}, broker, "http://gitlab.internal", []string{"group/project"}, slog.New(slog.NewJSONHandler(&logs, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		reconciler.Scan(ctx)
		close(done)
	}()
	select {
	case <-called:
		cancel()
	case <-time.After(time.Second):
		t.Fatal("scan did not start")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Scan() did not stop")
	}
	if !bytes.Contains(logs.Bytes(), []byte(`"outcome":"canceled"`)) || !bytes.Contains(logs.Bytes(), []byte(`"projects":0`)) {
		t.Fatalf("canceled final project was reported incorrectly: %s", logs.String())
	}
}

func TestRunScansImmediatelyAndStops(t *testing.T) {
	called := make(chan struct{}, 1)
	broker := &fakeBroker{resolve: func(ctx context.Context, _ string) (int64, error) {
		called <- struct{}{}
		<-ctx.Done()
		return 0, ctx.Err()
	}}
	reconciler := New(&fakeStore{}, broker, "http://gitlab.internal", []string{"group/project"}, slog.Default())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- reconciler.Run(ctx) }()
	select {
	case <-called:
		cancel()
	case <-time.After(time.Second):
		t.Fatal("initial scan did not start")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run() did not stop")
	}
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	storage, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "wormtamer.db"))
	if err != nil {
		t.Fatal(err)
	}
	return storage
}

func writeJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Fatal(err)
	}
}

type fakeBroker struct {
	resolve func(context.Context, string) (int64, error)
}

func (b *fakeBroker) ResolveProject(ctx context.Context, project string) (int64, error) {
	return b.resolve(ctx, project)
}

func (b *fakeBroker) ListOpenMergeRequests(context.Context, int64, int) ([]gitlab.ReconciliationMergeRequest, int, error) {
	return nil, 0, nil
}

type fakeStore struct{}

func (*fakeStore) CreateReconciledJob(context.Context, store.ReconciledReview) (store.ReconciledResult, error) {
	return store.ReconciledResult{}, nil
}
