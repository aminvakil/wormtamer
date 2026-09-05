package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aminvakil/wormtamer/internal/store"
)

func TestParseInvocation(t *testing.T) {
	if _, err := parseInvocation(nil); err == nil || !strings.Contains(err.Error(), "-config is required") {
		t.Fatalf("parseInvocation(nil) error = %v", err)
	}
	if _, err := parseInvocation([]string{"-config", "config.json", "extra"}); err == nil {
		t.Fatal("parseInvocation() accepted an unknown positional argument")
	}

	service, err := parseInvocation([]string{"-config", "config.json"})
	if err != nil || service.configPath != "config.json" || service.jobs != nil {
		t.Fatalf("parseInvocation(service) = %+v, %v", service, err)
	}
	listed, err := parseInvocation([]string{"-config", "config.json", "jobs", "list-failed"})
	if err != nil || listed.jobs == nil || listed.jobs.action != jobsActionListFailed {
		t.Fatalf("parseInvocation(list) = %+v, %v", listed, err)
	}
	retried, err := parseInvocation([]string{"-config", "config.json", "jobs", "retry", "review", "42"})
	if err != nil || retried.jobs == nil || retried.jobs.action != jobsActionRetry ||
		retried.jobs.kind != store.FailedJobKindReview || retried.jobs.jobID != 42 {
		t.Fatalf("parseInvocation(retry) = %+v, %v", retried, err)
	}
	for _, arguments := range [][]string{
		{"-config", "config.json", "jobs"},
		{"-config", "config.json", "jobs", "retry", "unknown", "1"},
		{"-config", "config.json", "jobs", "retry", "feedback", "0"},
		{"-config", "config.json", "jobs", "list-failed", "extra"},
	} {
		if _, err := parseInvocation(arguments); err == nil {
			t.Fatalf("parseInvocation(%q) accepted invalid command", arguments)
		}
	}
}

func TestConfiguredLogLevel(t *testing.T) {
	var logs bytes.Buffer
	handler := slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})
	logger := slog.New(logLevelHandler{Handler: handler, level: configuredLogLevel("info")})
	logger.Debug("hidden debug message")
	logger.Info("visible info message")
	if strings.Contains(logs.String(), "hidden debug message") || !strings.Contains(logs.String(), "visible info message") {
		t.Fatalf("info log filtering failed: %s", logs.String())
	}

	logs.Reset()
	logger = slog.New(logLevelHandler{Handler: handler, level: configuredLogLevel("debug")})
	logger.Debug("visible debug message")
	if !strings.Contains(logs.String(), "visible debug message") {
		t.Fatalf("debug log filtering failed: %s", logs.String())
	}
}

func TestServiceRoutesKeepsIngressSeparateFromPanel(t *testing.T) {
	ingress := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Test-Handler", "ingress")
		w.WriteHeader(http.StatusNoContent)
	})
	panelHandler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Test-Handler", "panel")
		w.WriteHeader(http.StatusOK)
	})
	handler := serviceRoutes(ingress, panelHandler)
	for _, test := range []struct {
		method, path, wantHandler string
		wantStatus                int
	}{
		{http.MethodGet, "/healthcheck", "ingress", http.StatusNoContent},
		{http.MethodPost, "/webhooks/gitlab", "ingress", http.StatusNoContent},
		{http.MethodGet, "/", "panel", http.StatusOK},
		{http.MethodGet, "/reviews", "panel", http.StatusOK},
		{http.MethodGet, "/healthcheck/other", "panel", http.StatusOK},
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(test.method, test.path, nil))
		if response.Code != test.wantStatus || response.Header().Get("X-Test-Handler") != test.wantHandler {
			t.Fatalf("%s %s status=%d handler=%q", test.method, test.path, response.Code, response.Header().Get("X-Test-Handler"))
		}
	}
}

func TestRunStartsAndShutsDown(t *testing.T) {
	directory := t.TempDir()
	configPath := writeConfig(t, directory, "wormtamer.db")
	var logs syncBuffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{"-config", configPath}, logger, io.Discard)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(logs.String(), "HTTP server started") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(logs.String(), "HTTP server started") {
		cancel()
		t.Fatalf("server did not start; logs: %s", logs.String())
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run() error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run() did not shut down")
	}
	if !strings.Contains(logs.String(), "HTTP server stopped") {
		t.Fatalf("shutdown log missing: %s", logs.String())
	}
}

func TestRunRejectsToolAccessibleConfigurationAndState(t *testing.T) {
	directory, err := os.MkdirTemp("", "wormtamer-boundary-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(directory)
	configPath := writeConfig(t, directory, "wormtamer.db")
	if err := os.Chmod(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(configPath, 0o644); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	err = run(context.Background(), []string{"-config", configPath}, slog.New(slog.NewJSONHandler(&logs, nil)), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "review-tool identity") {
		t.Fatalf("credential boundary error = %v", err)
	}
	if strings.Contains(logs.String(), "HTTP server started") {
		t.Fatalf("service started with an invalid credential boundary: %s", logs.String())
	}
}

func TestRunFailsBeforeListeningWhenDatabaseIsUnavailable(t *testing.T) {
	directory := t.TempDir()
	configPath := writeConfig(t, directory, filepath.Join("missing", "wormtamer.db"))
	var logs bytes.Buffer
	err := run(context.Background(), []string{"-config", configPath}, slog.New(slog.NewJSONHandler(&logs, nil)), io.Discard)
	if err == nil {
		t.Fatal("run() error = nil")
	}
	if strings.Contains(logs.String(), "HTTP server started") {
		t.Fatalf("server started before database initialization failed: %s", logs.String())
	}
}

func TestJobsCommandsDoNotStartServiceOrExposePrivateState(t *testing.T) {
	directory := t.TempDir()
	configPath := writeConfig(t, directory, "wormtamer.db")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	configuration, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	configuration = []byte(strings.Replace(string(configuration), "127.0.0.1:0", listener.Addr().String(), 1))
	if err := os.WriteFile(configPath, configuration, 0o600); err != nil {
		t.Fatal(err)
	}

	databasePath := filepath.Join(directory, "wormtamer.db")
	storage, err := store.Open(context.Background(), databasePath)
	if err != nil {
		t.Fatal(err)
	}
	created, err := storage.CreateReconciledJob(context.Background(), store.ReconciledReview{
		GitLabInstance: "http://gitlab.internal", ProjectID: 42, MergeRequestIID: 7,
		HeadSHA: "0123456789abcdef0123456789abcdef01234567",
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(time.Hour)
	job, err := storage.ClaimJob(context.Background(), now)
	if err != nil || job == nil {
		t.Fatalf("ClaimJob() = %+v, %v", job, err)
	}
	if err := storage.FinishJob(context.Background(), created.JobID, store.JobFailed,
		"gitlab_authorization_failed", "stored-private-error-message", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := storage.Close(); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	if err := run(context.Background(), []string{"-config", configPath, "jobs", "list-failed"}, logger, &output); err != nil {
		t.Fatalf("list-failed error = %v", err)
	}
	var listed struct {
		Jobs      []map[string]any `json:"jobs"`
		Truncated bool             `json:"truncated"`
	}
	if err := json.Unmarshal(output.Bytes(), &listed); err != nil {
		t.Fatalf("decode list output: %v; output=%s", err, output.String())
	}
	if listed.Truncated || len(listed.Jobs) != 1 {
		t.Fatalf("list output = %s", output.String())
	}
	wantKeys := []string{"kind", "job_id", "attempt_count", "last_error_category", "updated_at", "project_id", "merge_request_iid", "head_sha"}
	if len(listed.Jobs[0]) != len(wantKeys) {
		t.Fatalf("failed job fields = %+v", listed.Jobs[0])
	}
	for _, key := range wantKeys {
		if _, exists := listed.Jobs[0][key]; !exists {
			t.Fatalf("failed job lacks %q: %+v", key, listed.Jobs[0])
		}
	}
	for _, private := range []string{"stored-private-error-message", "gitlab-token", "gemini-key", "secret"} {
		if strings.Contains(output.String(), private) {
			t.Fatalf("list output exposes %q: %s", private, output.String())
		}
	}
	if strings.Contains(logs.String(), "HTTP server started") {
		t.Fatalf("operational command started HTTP server: %s", logs.String())
	}
	workspacePath := filepath.Join(filepath.Dir(directory), "reviews-"+filepath.Base(directory))
	if _, err := os.Stat(workspacePath); !os.IsNotExist(err) {
		t.Fatalf("operational command created workspace path: %v", err)
	}

	output.Reset()
	jobID := strconv.FormatInt(created.JobID, 10)
	if err := run(context.Background(), []string{"-config", configPath, "jobs", "retry", "review", jobID}, logger, &output); err != nil {
		t.Fatalf("retry error = %v", err)
	}
	wantRetry := `{"kind":"review","job_id":` + jobID + `,"retried":true}`
	if strings.TrimSpace(output.String()) != wantRetry {
		t.Fatalf("retry output = %s", output.String())
	}
	err = run(context.Background(), []string{"-config", configPath, "jobs", "retry", "review", jobID}, logger, &output)
	if !errors.Is(err, store.ErrJobNotFailed) {
		t.Fatalf("repeated retry error = %v", err)
	}
}

func writeConfig(t *testing.T, directory, databasePath string) string {
	t.Helper()
	contents := `{
  "listen_address": "127.0.0.1:0",
  "database_path": ` + strconv.Quote(databasePath) + `,
  "review_workspace_path": ` + strconv.Quote(filepath.Join(filepath.Dir(directory), "reviews-"+filepath.Base(directory))) + `,
  "gitlab": {
    "base_url": "http://gitlab.internal",
    "webhook_secret": "secret",
    "personal_access_token": "gitlab-token"
  },
  "gemini": {
    "api_key": "gemini-key",
    "base_url": "http://gemini.internal",
    "model": "gemini-test"
  },
  "authorized_repositories": ["group/project"]
}`
	path := filepath.Join(directory, "config.json")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

type syncBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *syncBuffer) Write(contents []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(contents)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}
