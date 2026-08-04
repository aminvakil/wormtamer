package repository

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aminvakil/wormtamer/internal/failure"
)

func TestWorkspaceListsReadsAndSearchesAttributedText(t *testing.T) {
	manager := newTestManager(t)
	workspace, err := manager.Create(context.Background(), "reviewed-sha", testArchive(t,
		archiveEntry{name: "project-sha/internal/example.go", body: "package example\n\nfunc Check() {}\n"},
		archiveEntry{name: "project-sha/README.md", body: "Call Check here.\n"},
		archiveEntry{name: "project-sha/image.bin", body: "a\x00b"},
		archiveEntry{name: "project-sha/link", kind: tar.TypeSymlink, link: "internal/example.go"},
	))
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	defer workspace.Close()

	listed, err := workspace.Call(context.Background(), ToolListFiles, map[string]any{})
	if err != nil {
		t.Fatalf("list error = %v", err)
	}
	files := listed["files"].([]string)
	if listed["revision"] != "reviewed-sha" || strings.Join(files, ",") != "README.md,internal/example.go" {
		t.Fatalf("list = %#v", listed)
	}

	read, err := workspace.Call(context.Background(), ToolReadFile, map[string]any{
		"path": "internal/example.go", "start_line": float64(3), "line_count": float64(1),
	})
	if err != nil {
		t.Fatalf("read error = %v", err)
	}
	if strings.Join(read["lines"].([]string), "") != "func Check() {}" || read["revision"] != "reviewed-sha" {
		t.Fatalf("read = %#v", read)
	}

	searched, err := workspace.Call(context.Background(), ToolSearch, map[string]any{"query": "Check"})
	if err != nil {
		t.Fatalf("search error = %v", err)
	}
	encoded := resultJSON(t, searched)
	if !strings.Contains(encoded, `"path":"README.md"`) || !strings.Contains(encoded, `"path":"internal/example.go"`) || !strings.Contains(encoded, `"revision":"reviewed-sha"`) {
		t.Fatalf("search = %s", encoded)
	}

	for _, path := range []string{"image.bin", "link"} {
		_, err := workspace.Call(context.Background(), ToolReadFile, map[string]any{"path": path})
		assertRepositoryFailure(t, err, "repository_path_not_found")
	}
}

func TestWorkspaceAcceptsPAXGlobalHeader(t *testing.T) {
	manager := newTestManager(t)
	workspace, err := manager.Create(context.Background(), "sha", testArchive(t,
		archiveEntry{name: "pax_global_header", kind: tar.TypeXGlobalHeader, pax: map[string]string{"comment": "sha"}},
		archiveEntry{name: "project-sha/file.txt", body: "text"},
	))
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	defer workspace.Close()

	listed, err := workspace.Call(context.Background(), ToolListFiles, map[string]any{})
	if err != nil {
		t.Fatalf("list error = %v", err)
	}
	if files := listed["files"].([]string); strings.Join(files, ",") != "file.txt" {
		t.Fatalf("files = %v", files)
	}
}

func TestWorkspaceRejectsArchiveTraversalAndMixedRoots(t *testing.T) {
	tests := []struct {
		name     string
		entries  []archiveEntry
		category string
	}{
		{name: "traversal", entries: []archiveEntry{{name: "project-sha/../outside", body: "secret"}}, category: "repository_archive_invalid_path"},
		{name: "absolute", entries: []archiveEntry{{name: "/project-sha/file", body: "secret"}}, category: "repository_archive_invalid_path"},
		{name: "mixed roots", entries: []archiveEntry{{name: "one/a", body: "a"}, {name: "two/b", body: "b"}}, category: "repository_archive_invalid_path"},
		{name: "duplicate", entries: []archiveEntry{{name: "project-sha/a", body: "a"}, {name: "project-sha/a", body: "b"}}, category: "repository_archive_duplicate_path"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manager := newTestManager(t)
			_, err := manager.Create(context.Background(), "sha", testArchive(t, test.entries...))
			assertRepositoryFailure(t, err, test.category)
		})
	}
}

func TestWorkspaceRejectsInvalidToolRequestsWithoutDisclosure(t *testing.T) {
	manager := newTestManager(t)
	workspace, err := manager.Create(context.Background(), "sha", testArchive(t, archiveEntry{name: "project-sha/file.txt", body: "private"}))
	if err != nil {
		t.Fatal(err)
	}
	defer workspace.Close()

	tests := []struct {
		name      string
		tool      string
		arguments map[string]any
		category  string
	}{
		{name: "traversal", tool: ToolReadFile, arguments: map[string]any{"path": "../file.txt"}, category: "repository_path_invalid"},
		{name: "absolute", tool: ToolReadFile, arguments: map[string]any{"path": "/file.txt"}, category: "repository_path_invalid"},
		{name: "unknown argument", tool: ToolReadFile, arguments: map[string]any{"path": "file.txt", "revision": "other"}, category: "repository_tool_arguments_invalid"},
		{name: "excessive lines", tool: ToolReadFile, arguments: map[string]any{"path": "file.txt", "line_count": maxReadLines + 1}, category: "repository_tool_arguments_invalid"},
		{name: "undeclared", tool: "shell", arguments: map[string]any{}, category: "repository_tool_undeclared"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := workspace.Call(context.Background(), test.tool, test.arguments)
			if result != nil {
				t.Fatalf("result disclosed = %#v", result)
			}
			assertRepositoryFailure(t, err, test.category)
		})
	}
}

func TestWorkspaceCreationHonorsCancellationAndCleansUp(t *testing.T) {
	manager := newTestManager(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := manager.Create(ctx, "sha", testArchive(t, archiveEntry{name: "project-sha/file", body: "text"}))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Create() error = %v", err)
	}
	entries, err := os.ReadDir(manager.root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("workspace cleanup entries=%d error=%v", len(entries), err)
	}
}

func TestManagerCleansStaleAndClosedWorkspaces(t *testing.T) {
	root := filepath.Join(t.TempDir(), "workspaces")
	if err := os.MkdirAll(filepath.Join(root, "stale"), 0o700); err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "stale")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale workspace remains: %v", err)
	}
	workspace, err := manager.Create(context.Background(), "sha", testArchive(t, archiveEntry{name: "project-sha/file", body: "text"}))
	if err != nil {
		t.Fatal(err)
	}
	local := workspace.(*localWorkspace)
	if err := workspace.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(local.root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("closed workspace remains: %v", err)
	}
}

type archiveEntry struct {
	name string
	body string
	kind byte
	link string
	pax  map[string]string
}

func testArchive(t *testing.T, entries ...archiveEntry) []byte {
	t.Helper()
	var output bytes.Buffer
	compressed := gzip.NewWriter(&output)
	archive := tar.NewWriter(compressed)
	writeEntry := func(entry archiveEntry) {
		kind := entry.kind
		if kind == 0 {
			kind = tar.TypeReg
		}
		header := &tar.Header{Name: entry.name, Typeflag: kind, Mode: 0o644, Linkname: entry.link, PAXRecords: entry.pax}
		if kind == tar.TypeXGlobalHeader {
			header = &tar.Header{Typeflag: kind, PAXRecords: entry.pax}
		} else if kind == tar.TypeReg {
			header.Size = int64(len(entry.body))
		}
		if err := archive.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if kind == tar.TypeReg {
			if _, err := archive.Write([]byte(entry.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, entry := range entries {
		if entry.kind == tar.TypeXGlobalHeader {
			writeEntry(entry)
		}
	}
	if err := archive.WriteHeader(&tar.Header{Name: "project-sha/", Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.kind != tar.TypeXGlobalHeader {
			writeEntry(entry)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := compressed.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func newTestManager(t *testing.T) *Manager {
	t.Helper()
	manager, err := NewManager(filepath.Join(t.TempDir(), "workspaces"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	return manager
}

func assertRepositoryFailure(t *testing.T, err error, category string) {
	t.Helper()
	var failureError *failure.Error
	if !errors.As(err, &failureError) || failureError.Category != category {
		t.Fatalf("error = %v, want %s", err, category)
	}
}

func resultJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}
