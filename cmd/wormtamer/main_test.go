package main

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseConfigPath(t *testing.T) {
	if _, err := parseConfigPath(nil); err == nil || !strings.Contains(err.Error(), "-config is required") {
		t.Fatalf("parseConfigPath(nil) error = %v", err)
	}
	if _, err := parseConfigPath([]string{"-config", "config.json", "extra"}); err == nil {
		t.Fatal("parseConfigPath() accepted a positional argument")
	}
	path, err := parseConfigPath([]string{"-config", "config.json"})
	if err != nil || path != "config.json" {
		t.Fatalf("parseConfigPath() = %q, %v", path, err)
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
		done <- run(ctx, []string{"-config", configPath}, logger)
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

func TestRunFailsBeforeListeningWhenDatabaseIsUnavailable(t *testing.T) {
	directory := t.TempDir()
	configPath := writeConfig(t, directory, filepath.Join("missing", "wormtamer.db"))
	var logs bytes.Buffer
	err := run(context.Background(), []string{"-config", configPath}, slog.New(slog.NewJSONHandler(&logs, nil)))
	if err == nil {
		t.Fatal("run() error = nil")
	}
	if strings.Contains(logs.String(), "HTTP server started") {
		t.Fatalf("server started before database initialization failed: %s", logs.String())
	}
}

func writeConfig(t *testing.T, directory, databasePath string) string {
	t.Helper()
	contents := `{
  "listen_address": "127.0.0.1:0",
  "database_path": ` + quote(databasePath) + `,
  "gitlab": {
    "base_url": "http://gitlab.internal",
    "webhook_secret": "secret"
  },
  "authorized_repositories": ["group/project"]
}`
	path := filepath.Join(directory, "config.json")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func quote(value string) string {
	return `"` + strings.ReplaceAll(value, `\`, `\\`) + `"`
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
