package repository

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/aminvakil/wormtamer/internal/failure"
)

const (
	// ReviewResourceLimit bounds both repository tool calls and distinct repositories per review.
	ReviewResourceLimit  = 8
	MaxToolResponseBytes = 64 << 10
)

const (
	maxArchiveEntries = 20_000
	maxArchiveBytes   = 128 << 20
	maxWorkspaceFiles = 10_000
	maxFileBytes      = 2 << 20
	maxPathBytes      = 1024
	maxListedFiles    = 1_000
	maxReadLines      = 200
	maxSearchBytes    = 16 << 20
	maxSearchMatches  = 100
	maxQueryBytes     = 256
)

const (
	ToolListFiles = "list_repository_files"
	ToolReadFile  = "read_repository_file"
	ToolSearch    = "search_repository"
)

type ToolBroker interface {
	Call(context.Context, string, map[string]any) (map[string]any, error)
}

type Workspace interface {
	ToolBroker
	Close() error
}

type Manager struct {
	root string
}

func NewManager(root string) (*Manager, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("repository workspace root is required")
	}
	root = filepath.Clean(root)
	if err := os.RemoveAll(root); err != nil {
		return nil, fmt.Errorf("clean repository workspace root: %w", err)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create repository workspace root: %w", err)
	}
	return &Manager{root: root}, nil
}

func (m *Manager) Create(ctx context.Context, revision string, archive []byte) (Workspace, error) {
	if revision == "" {
		return nil, failure.Failed("repository_snapshot_invalid")
	}
	directory, err := os.MkdirTemp(m.root, "review-")
	if err != nil {
		return nil, failure.Retry("repository_workspace_create_failed", 0)
	}
	workspace := &localWorkspace{root: directory, revision: revision}
	if err := extractArchive(ctx, directory, archive); err != nil {
		_ = workspace.Close()
		return nil, err
	}
	return workspace, nil
}

func (m *Manager) Close() error {
	if err := os.RemoveAll(m.root); err != nil {
		return fmt.Errorf("remove repository workspace root: %w", err)
	}
	return nil
}

type localWorkspace struct {
	root     string
	revision string
}

func (w *localWorkspace) Close() error {
	if err := os.RemoveAll(w.root); err != nil {
		return fmt.Errorf("remove repository workspace: %w", err)
	}
	return nil
}

func extractArchive(ctx context.Context, destination string, contents []byte) error {
	compressed, err := gzip.NewReader(bytes.NewReader(contents))
	if err != nil {
		return failure.Failed("repository_archive_invalid")
	}
	defer compressed.Close()

	reader := tar.NewReader(compressed)
	var archiveRoot string
	entries := 0
	files := 0
	var totalBytes int64
	seen := make(map[string]struct{})
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return failure.Failed("repository_archive_invalid")
		}
		entries++
		if entries > maxArchiveEntries || header.Size < 0 {
			return failure.Failed("repository_archive_limit_exceeded")
		}
		if header.Size > maxArchiveBytes-totalBytes {
			return failure.Failed("repository_archive_limit_exceeded")
		}
		totalBytes += header.Size
		if header.Typeflag == tar.TypeXGlobalHeader {
			// Global PAX metadata has no repository-relative path and may precede the archive root.
			continue
		}

		relative, root, skip, err := archiveEntryPath(header.Name)
		if err != nil {
			return err
		}
		if archiveRoot == "" {
			archiveRoot = root
		} else if archiveRoot != root {
			return failure.Failed("repository_archive_invalid_path")
		}
		if skip {
			continue
		}
		target := filepath.Join(destination, filepath.FromSlash(relative))
		if !withinRoot(destination, target) {
			return failure.Failed("repository_archive_invalid_path")
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o700); err != nil {
				return failure.Retry("repository_workspace_write_failed", 0)
			}
		case tar.TypeReg, tar.TypeRegA:
			if header.Size > maxFileBytes {
				continue
			}
			if _, duplicate := seen[relative]; duplicate {
				return failure.Failed("repository_archive_duplicate_path")
			}
			data, err := io.ReadAll(io.LimitReader(reader, maxFileBytes+1))
			if err != nil || int64(len(data)) != header.Size {
				return failure.Failed("repository_archive_invalid")
			}
			if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
				continue
			}
			files++
			if files > maxWorkspaceFiles {
				return failure.Failed("repository_archive_limit_exceeded")
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return failure.Retry("repository_workspace_write_failed", 0)
			}
			if err := os.WriteFile(target, data, 0o600); err != nil {
				return failure.Retry("repository_workspace_write_failed", 0)
			}
			seen[relative] = struct{}{}
		default:
			// Symlinks, gitlinks, devices, and other non-regular entries are not exposed.
		}
	}
	if archiveRoot == "" {
		return failure.Failed("repository_archive_invalid")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

func archiveEntryPath(name string) (relative, root string, skip bool, err error) {
	if name == "" || len(name) > maxPathBytes+256 || !utf8.ValidString(name) || strings.ContainsRune(name, '\x00') || strings.HasPrefix(name, "/") || strings.Contains(name, `\`) {
		return "", "", false, failure.Failed("repository_archive_invalid_path")
	}
	trimmed := strings.TrimSuffix(name, "/")
	cleaned := path.Clean(trimmed)
	if cleaned != trimmed || cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", "", false, failure.Failed("repository_archive_invalid_path")
	}
	parts := strings.SplitN(cleaned, "/", 2)
	root = parts[0]
	if root == "" || root == "." || root == ".." {
		return "", "", false, failure.Failed("repository_archive_invalid_path")
	}
	if len(parts) == 1 {
		return "", root, true, nil
	}
	relative = parts[1]
	if len(relative) > maxPathBytes {
		return "", "", false, failure.Failed("repository_archive_invalid_path")
	}
	return relative, root, false, nil
}

func withinRoot(root, target string) bool {
	relative, err := filepath.Rel(root, target)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func (w *localWorkspace) Call(ctx context.Context, name string, arguments map[string]any) (map[string]any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var result map[string]any
	var err error
	switch name {
	case ToolListFiles:
		result, err = w.listFiles(ctx, arguments)
	case ToolReadFile:
		result, err = w.readFile(ctx, arguments)
	case ToolSearch:
		result, err = w.search(ctx, arguments)
	default:
		return nil, failure.Failed("repository_tool_undeclared")
	}
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return nil, failure.Failed("repository_tool_output_invalid")
	}
	if len(encoded) > MaxToolResponseBytes {
		return nil, failure.Failed("repository_tool_output_limit_exceeded")
	}
	return result, nil
}

func (w *localWorkspace) listFiles(ctx context.Context, arguments map[string]any) (map[string]any, error) {
	if err := onlyKeys(arguments, "path"); err != nil {
		return nil, err
	}
	requested, err := optionalString(arguments, "path")
	if err != nil {
		return nil, err
	}
	relative, err := normalizeRequestedPath(requested, true)
	if err != nil {
		return nil, err
	}
	start, err := w.resolve(relative)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(start)
	if err != nil || !info.IsDir() {
		return nil, failure.Failed("repository_path_not_found")
	}
	files := make([]string, 0)
	err = filepath.WalkDir(start, func(current string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		if entry.Type().IsRegular() {
			path, err := filepath.Rel(w.root, current)
			if err != nil {
				return err
			}
			files = append(files, filepath.ToSlash(path))
			if len(files) > maxListedFiles {
				return failure.Failed("repository_tool_output_limit_exceeded")
			}
		}
		return nil
	})
	if err != nil {
		var failureError *failure.Error
		if errors.As(err, &failureError) {
			return nil, err
		}
		return nil, failure.Retry("repository_workspace_read_failed", 0)
	}
	sort.Strings(files)
	return map[string]any{"revision": w.revision, "path": relative, "files": files}, nil
}

func (w *localWorkspace) readFile(ctx context.Context, arguments map[string]any) (map[string]any, error) {
	if err := onlyKeys(arguments, "path", "start_line", "line_count"); err != nil {
		return nil, err
	}
	requested, err := requiredString(arguments, "path")
	if err != nil {
		return nil, err
	}
	relative, err := normalizeRequestedPath(requested, false)
	if err != nil {
		return nil, err
	}
	start, err := optionalInteger(arguments, "start_line", 1)
	if err != nil || start < 1 {
		return nil, failure.Failed("repository_tool_arguments_invalid")
	}
	count, err := optionalInteger(arguments, "line_count", maxReadLines)
	if err != nil || count < 1 || count > maxReadLines {
		return nil, failure.Failed("repository_tool_arguments_invalid")
	}
	file, err := w.resolve(relative)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, failure.Failed("repository_path_not_found")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	first := start - 1
	if first > len(lines) {
		first = len(lines)
	}
	last := first + count
	if last > len(lines) {
		last = len(lines)
	}
	selected := append([]string(nil), lines[first:last]...)
	return map[string]any{
		"revision": w.revision, "path": relative, "start_line": start,
		"end_line": first + len(selected), "lines": selected,
	}, nil
}

func (w *localWorkspace) search(ctx context.Context, arguments map[string]any) (map[string]any, error) {
	if err := onlyKeys(arguments, "query", "path"); err != nil {
		return nil, err
	}
	query, err := requiredString(arguments, "query")
	if err != nil || query == "" || len(query) > maxQueryBytes {
		return nil, failure.Failed("repository_tool_arguments_invalid")
	}
	requested, err := optionalString(arguments, "path")
	if err != nil {
		return nil, err
	}
	relative, err := normalizeRequestedPath(requested, true)
	if err != nil {
		return nil, err
	}
	start, err := w.resolve(relative)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(start)
	if err != nil || !info.IsDir() {
		return nil, failure.Failed("repository_path_not_found")
	}
	type match struct {
		Path string `json:"path"`
		Line int    `json:"line"`
		Text string `json:"text"`
	}
	matches := make([]match, 0)
	scanned := 0
	err = filepath.WalkDir(start, func(current string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		data, err := os.ReadFile(current)
		if err != nil {
			return err
		}
		scanned += len(data)
		if scanned > maxSearchBytes {
			return failure.Failed("repository_search_limit_exceeded")
		}
		filePath, err := filepath.Rel(w.root, current)
		if err != nil {
			return err
		}
		lines := strings.Split(string(data), "\n")
		for index, line := range lines {
			if strings.Contains(line, query) {
				matches = append(matches, match{Path: filepath.ToSlash(filePath), Line: index + 1, Text: line})
				if len(matches) > maxSearchMatches {
					return failure.Failed("repository_tool_output_limit_exceeded")
				}
			}
		}
		return nil
	})
	if err != nil {
		var failureError *failure.Error
		if errors.As(err, &failureError) {
			return nil, err
		}
		return nil, failure.Retry("repository_workspace_read_failed", 0)
	}
	return map[string]any{"revision": w.revision, "path": relative, "query": query, "matches": matches}, nil
}

func (w *localWorkspace) resolve(relative string) (string, error) {
	target := filepath.Join(w.root, filepath.FromSlash(relative))
	if !withinRoot(w.root, target) {
		return "", failure.Failed("repository_path_invalid")
	}
	return target, nil
}

func normalizeRequestedPath(value string, allowRoot bool) (string, error) {
	if value == "" && allowRoot {
		return "", nil
	}
	if len(value) > maxPathBytes || !utf8.ValidString(value) || strings.ContainsRune(value, '\x00') || strings.HasPrefix(value, "/") || strings.Contains(value, `\`) {
		return "", failure.Failed("repository_path_invalid")
	}
	cleaned := path.Clean(value)
	if cleaned != value || cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", failure.Failed("repository_path_invalid")
	}
	return cleaned, nil
}

func onlyKeys(arguments map[string]any, allowed ...string) error {
	set := make(map[string]struct{}, len(allowed))
	for _, key := range allowed {
		set[key] = struct{}{}
	}
	for key := range arguments {
		if _, ok := set[key]; !ok {
			return failure.Failed("repository_tool_arguments_invalid")
		}
	}
	return nil
}

func requiredString(arguments map[string]any, key string) (string, error) {
	value, ok := arguments[key]
	if !ok {
		return "", failure.Failed("repository_tool_arguments_invalid")
	}
	text, ok := value.(string)
	if !ok {
		return "", failure.Failed("repository_tool_arguments_invalid")
	}
	return text, nil
}

func optionalString(arguments map[string]any, key string) (string, error) {
	value, ok := arguments[key]
	if !ok {
		return "", nil
	}
	text, ok := value.(string)
	if !ok {
		return "", failure.Failed("repository_tool_arguments_invalid")
	}
	return text, nil
}

func optionalInteger(arguments map[string]any, key string, fallback int) (int, error) {
	value, ok := arguments[key]
	if !ok {
		return fallback, nil
	}
	switch number := value.(type) {
	case int:
		return number, nil
	case int32:
		return int(number), nil
	case int64:
		return int(number), nil
	case float64:
		integer := int(number)
		if number != float64(integer) {
			return 0, failure.Failed("repository_tool_arguments_invalid")
		}
		return integer, nil
	default:
		return 0, failure.Failed("repository_tool_arguments_invalid")
	}
}
