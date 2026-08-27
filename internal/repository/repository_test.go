package repository

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/aminvakil/wormtamer/internal/failure"
	"github.com/aminvakil/wormtamer/internal/gitlab"
)

func TestMain(m *testing.M) {
	if IsReadHelperInvocation(os.Args[1:]) {
		if err := RunReadHelper(os.Stdin, os.Stdout); err != nil {
			os.Exit(1)
		}
		return
	}
	os.Exit(m.Run())
}

func TestReadTextOffsetsLimitsAndTruncation(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "file.txt")
	lines := make([]string, MaxToolLines+2)
	for index := range lines {
		lines[index] = fmt.Sprintf("line-%04d", index+1)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o600); err != nil {
		t.Fatal(err)
	}

	offset, limit := int64(2), int64(2)
	response := executeRead(readRequest{Path: path, Offset: &offset, Limit: &limit})
	if response.Error != nil || response.Output == nil || !strings.Contains(*response.Output, "line-0002\nline-0003") ||
		!strings.Contains(*response.Output, "Use offset=4 to continue") {
		t.Fatalf("limited read = %+v", response)
	}
	response = executeRead(readRequest{Path: path})
	if response.Output == nil || !strings.Contains(*response.Output, "Showing lines 1-2000") ||
		!strings.Contains(*response.Output, "offset=2001") {
		t.Fatalf("truncated read = %+v", response)
	}

	longPath := filepath.Join(directory, "long.txt")
	if err := os.WriteFile(longPath, []byte(strings.Repeat("x", MaxToolBytes+1)+"\nnext"), 0o600); err != nil {
		t.Fatal(err)
	}
	response = executeRead(readRequest{Path: longPath})
	if response.Output == nil || !strings.Contains(*response.Output, "exceeds 50.0KB limit") ||
		!strings.Contains(*response.Output, "Use bash: sed") || !strings.Contains(*response.Output, "head -c 51200") {
		t.Fatalf("oversized first line = %+v", response)
	}

	badOffset := int64(len(lines) + 2)
	response = executeRead(readRequest{Path: path, Offset: &badOffset})
	if response.Error == nil || !strings.Contains(*response.Error, "beyond end of file") {
		t.Fatalf("invalid offset = %+v", response)
	}
}

func TestReadBoundsInputBeforeTruncatingAnOversizedLine(t *testing.T) {
	input := &repeatingReader{value: 'x'}
	response := readText(readRequest{Path: "/dev/zero"}, input)
	if response.Output == nil || !strings.Contains(*response.Output, "Line 1 exceeds 50.0KB limit") {
		t.Fatalf("bounded read = %+v", response)
	}
	if input.read > MaxToolBytes+(32<<10) {
		t.Fatalf("read consumed %d bytes before truncation", input.read)
	}
}

func TestReadHelperSupportsRelativeAndAbsolutePaths(t *testing.T) {
	workspace := testToolWorkspace(t)
	if err := os.WriteFile(filepath.Join(workspace.cwd, "relative.txt"), []byte("relative"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"relative.txt", filepath.Join(workspace.cwd, "relative.txt")} {
		result, err := workspace.Call(context.Background(), ToolRead, map[string]any{"path": path})
		if err != nil || result.Response["output"] != "relative" {
			t.Fatalf("read(%q) = %+v, %v", path, result, err)
		}
	}
	result, err := workspace.Call(context.Background(), ToolRead, map[string]any{"path": "missing"})
	if err != nil || !strings.Contains(result.Response["error"].(string), "Could not read file") {
		t.Fatalf("missing read = %+v, %v", result, err)
	}
}

func TestMalformedReadHelperOutputIsInfrastructureFailure(t *testing.T) {
	workspace := testToolWorkspace(t)
	helper := filepath.Join(t.TempDir(), "malformed-helper")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\nprintf malformed"), 0o700); err != nil {
		t.Fatal(err)
	}
	workspace.executable = helper
	_, err := workspace.Call(context.Background(), ToolRead, map[string]any{"path": "anything"})
	var failureError *failure.Error
	if !errors.As(err, &failureError) || failureError.Category != "read_helper_response_invalid" || !failureError.Retryable {
		t.Fatalf("malformed helper error = %v", err)
	}
}

func TestReadHelperFrameValidationIsStrict(t *testing.T) {
	validOutput := "ok"
	encoded, _ := json.Marshal(readHelperResponse{Output: &validOutput})
	frame := make([]byte, 4, len(encoded)+4)
	binary.BigEndian.PutUint32(frame, uint32(len(encoded)))
	frame = append(frame, encoded...)
	response, err := decodeReadHelperFrame(frame)
	if err != nil || response.Output == nil || *response.Output != validOutput {
		t.Fatalf("valid frame = %+v, %v", response, err)
	}
	frame = append(frame, 'x')
	if _, err := decodeReadHelperFrame(frame); err == nil {
		t.Fatal("trailing frame data was accepted")
	}
	unknown := []byte(`{"output":"ok","unexpected":true}`)
	frame = make([]byte, 4, len(unknown)+4)
	binary.BigEndian.PutUint32(frame, uint32(len(unknown)))
	frame = append(frame, unknown...)
	if _, err := decodeReadHelperFrame(frame); err == nil {
		t.Fatal("unknown helper field was accepted")
	}
}

func TestBashRunsInWorkspaceAndReportsCorrectableFailures(t *testing.T) {
	workspace := testToolWorkspace(t)
	result, err := workspace.Call(context.Background(), ToolBash, map[string]any{
		"command": "printf 'out'; printf 'err' >&2; printf '\\n'; pwd; touch changed.txt",
	})
	output := result.Response["output"].(string)
	if err != nil || !strings.Contains(output, "outerr") || !strings.Contains(output, workspace.cwd) {
		t.Fatalf("bash result = %+v, %v", result, err)
	}
	if _, err := os.Stat(filepath.Join(workspace.cwd, "changed.txt")); err != nil {
		t.Fatal("bash mutation is not visible")
	}

	result, err = workspace.Call(context.Background(), ToolBash, map[string]any{"command": "printf failure; exit 7"})
	if err != nil || !strings.Contains(result.Response["error"].(string), "failure\n\nCommand exited with code 7") {
		t.Fatalf("non-zero result = %+v, %v", result, err)
	}
	result, err = workspace.Call(context.Background(), ToolBash, map[string]any{"command": "sleep 5", "timeout": 0.05})
	if err != nil || !strings.Contains(result.Response["error"].(string), "Command timed out after 0.05 seconds") {
		t.Fatalf("timeout result = %+v, %v", result, err)
	}
}

func TestBashIgnoresExpectedPipeCloseAfterShellExit(t *testing.T) {
	workspace := testToolWorkspace(t)
	result, err := workspace.Call(context.Background(), ToolBash, map[string]any{"command": "sleep 0.2 &"})
	if err != nil || result.Response["output"] != "(no output)" {
		t.Fatalf("background command = %+v, %v", result, err)
	}
}

func TestBashTailTruncationSpoolsFullOutput(t *testing.T) {
	workspace := testToolWorkspace(t)
	result, err := workspace.Call(context.Background(), ToolBash, map[string]any{
		"command": "for i in $(seq 1 2100); do echo line-$i; done",
	})
	if err != nil {
		t.Fatal(err)
	}
	output := result.Response["output"].(string)
	visible := strings.Split(output, "\n\n[Showing lines")[0]
	if strings.Contains(visible, "line-1\n") || !strings.Contains(visible, "line-2100") ||
		strings.Count(visible, "line-") != MaxToolLines || !strings.Contains(output, "Full output:") {
		t.Fatalf("truncated output has %d lines: %q", strings.Count(visible, "line-"), output)
	}
	path := strings.TrimSuffix(strings.Split(output, "Full output: ")[1], "]")
	contents, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(contents), "line-1\n") || !strings.Contains(string(contents), "line-2100\n") {
		t.Fatalf("spool %q = %v", path, err)
	}
}

func TestBashOutputAndReviewSpoolLimitsAreCorrectable(t *testing.T) {
	workspace := testToolWorkspace(t)
	result, err := workspace.Call(context.Background(), ToolBash, map[string]any{
		"command": fmt.Sprintf("head -c %d /dev/zero | tr '\\0' x", MaxCommandOutputBytes+1),
	})
	if err != nil || bashErrorCategory(result) != "bash_output_limit_exceeded" {
		t.Fatalf("command output limit = %+v, %v", result, err)
	}
	workspace.spoolUsed = MaxReviewSpoolBytes - 1
	result, err = workspace.Call(context.Background(), ToolBash, map[string]any{
		"command": fmt.Sprintf("head -c %d /dev/zero | tr '\\0' y", MaxToolBytes+1),
	})
	if err != nil || bashErrorCategory(result) != "bash_output_limit_exceeded" {
		t.Fatalf("review spool limit = %+v, %v", result, err)
	}
}

func TestBashSpoolCreationRejectsReplacedOutputDirectory(t *testing.T) {
	workspace := testToolWorkspace(t)
	outside := t.TempDir()
	if err := os.RemoveAll(filepath.Join(workspace.root, ".wormtamer-output")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(workspace.root, ".wormtamer-output")); err != nil {
		t.Fatal(err)
	}
	_, err := workspace.Call(context.Background(), ToolBash, map[string]any{
		"command": fmt.Sprintf("head -c %d /dev/zero | tr '\\0' x", MaxToolBytes+1),
	})
	var failureError *failure.Error
	if !errors.As(err, &failureError) || failureError.Category != "bash_output_spool_failed" {
		t.Fatalf("replaced spool directory error = %v", err)
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatalf("spool escaped workspace: %+v, %v", entries, err)
	}
}

func TestBashParentCancellationPropagates(t *testing.T) {
	workspace := testToolWorkspace(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := workspace.Call(ctx, ToolBash, map[string]any{"command": "sleep 30"})
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("canceled bash did not exit")
	}
}

func TestBashReceivesCredentialFreeEnvironment(t *testing.T) {
	t.Setenv("WORMTAMER_TEST_SECRET", "must-not-be-inherited")
	workspace := testToolWorkspace(t)
	result, err := workspace.Call(context.Background(), ToolBash, map[string]any{"command": "env"})
	if err != nil || strings.Contains(result.Response["output"].(string), "WORMTAMER_TEST_SECRET") {
		t.Fatalf("bash environment = %+v, %v", result, err)
	}
}

func TestManagerPreparesGitRepositoriesAndMemory(t *testing.T) {
	serverRoot := t.TempDir()
	currentHead := createBareRepository(t, filepath.Join(serverRoot, "group", "project.git"), map[string]string{
		"main": "main content", "other": "other content",
	})
	setBareRef(t, filepath.Join(serverRoot, "group", "project.git"), "refs/merge-requests/7/head", currentHead)
	createBareRepository(t, filepath.Join(serverRoot, "group", "related.git"), map[string]string{"main": "related"})

	manager, err := NewManager(ManagerConfig{
		Root: filepath.Join(t.TempDir(), "reviews"), GitLabBaseURL: "file://localhost" + filepath.ToSlash(serverRoot),
		PersonalAccessToken: "test-pat", ToolUID: uint32(os.Geteuid()), ToolGID: uint32(os.Getegid()),
		Executable: os.Args[0],
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	snapshot := gitlab.Snapshot{
		Identity:    gitlab.Identity{ProjectID: 42, MergeRequestIID: 7, HeadSHA: currentHead},
		ProjectPath: "group/project", RelatedRepositories: []string{"group/related"},
	}
	workspace, err := manager.Prepare(context.Background(), snapshot, []Memory{{
		ID: "WT-M-AAAAAAAAAAAAAAAAAAAAAAAAAA", Lesson: "advisory lesson", SourceURL: "https://gitlab.example/mr/1", UpdatedAt: time.Now(),
	}})
	if err != nil {
		t.Fatal(err)
	}
	contextInfo := workspace.Context()
	if _, err := os.Stat(filepath.Join(contextInfo.WorkingDirectory, ".git")); err != nil ||
		len(contextInfo.RelatedRepositories) != 1 || contextInfo.RelatedRepositories[0].InitialRevision == "" {
		t.Fatalf("prepared context = %+v; .git err=%v", contextInfo, err)
	}
	for _, path := range []string{
		filepath.Join(contextInfo.WorkingDirectory, ".git", "config"),
		filepath.Join(contextInfo.RelatedRepositories[0].Path, ".git", "config"),
	} {
		configuration, err := os.ReadFile(path)
		if err != nil || bytes.Contains(configuration, []byte("test-pat")) || bytes.Contains(bytes.ToLower(configuration), []byte("extraheader")) {
			t.Fatalf("retained Git configuration %s = %q, %v", path, configuration, err)
		}
	}
	memory, err := os.ReadFile(contextInfo.MemoryPath)
	if err != nil || !bytes.Contains(memory, []byte("advisory lesson")) || !bytes.Contains(memory, []byte("untrusted_advisory")) {
		t.Fatalf("memory = %s, %v", memory, err)
	}
	result, err := workspace.Call(context.Background(), ToolBash, map[string]any{"command": "git switch other >/dev/null && printf switched"})
	if err != nil || !strings.Contains(result.Response["output"].(string), "switched") {
		t.Fatalf("switch branch = %+v, %v", result, err)
	}
	result, err = workspace.Call(context.Background(), ToolRead, map[string]any{"path": "content.txt"})
	if err != nil || result.Response["output"] != "other content\n" {
		t.Fatalf("read switched branch = %+v, %v", result, err)
	}
	root := workspace.(*localWorkspace).root
	if err := workspace.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("workspace remains after close: %v", err)
	}
}

func TestManagerSetupDeadlineRemovesPrivateStagingAndExposesNothing(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		select {
		case <-started:
		default:
			close(started)
		}
		<-release
	}))
	root := filepath.Join(t.TempDir(), "reviews")
	manager, err := NewManager(ManagerConfig{
		Root: root, GitLabBaseURL: server.URL, PersonalAccessToken: "timeout-pat",
		ToolUID: uint32(os.Geteuid()), ToolGID: uint32(os.Getegid()), SetupTimeout: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = manager.Prepare(context.Background(), gitlab.Snapshot{
		Identity: gitlab.Identity{MergeRequestIID: 7, HeadSHA: strings.Repeat("a", 40)}, ProjectPath: "group/project",
	}, nil)
	close(release)
	server.Close()
	var failureError *failure.Error
	if !errors.As(err, &failureError) || failureError.Category != "repository_preparation_timeout" || !failureError.Retryable {
		t.Fatalf("setup timeout error = %v", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != ".staging" {
		t.Fatalf("partial workspace exposed after timeout: %+v", entries)
	}
	staging, err := os.ReadDir(filepath.Join(root, ".staging"))
	if err != nil || len(staging) != 0 {
		t.Fatalf("private staging remains after timeout: %+v, %v", staging, err)
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCredentialBoundaryRejectsToolAccessiblePrivatePaths(t *testing.T) {
	root, err := os.MkdirTemp("", "wormtamer-credentials-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	privateDirectory := filepath.Join(root, "private")
	if err := os.Mkdir(privateDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	privateFile := filepath.Join(privateDirectory, "config.json")
	if err := os.WriteFile(privateFile, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	otherUID, otherGID := uint32(os.Geteuid()+1), uint32(os.Getegid()+1)
	if err := ValidateCredentialBoundary(otherUID, otherGID, []string{privateFile}, []string{privateDirectory}); err != nil {
		t.Fatalf("inaccessible private paths rejected: %v", err)
	}
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(privateDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(privateFile, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ValidateCredentialBoundary(otherUID, otherGID, []string{privateFile}, []string{privateDirectory}); err == nil {
		t.Fatal("tool-accessible private paths were accepted")
	}
}

func TestManagerRejectsGitRedirectWithoutFollowingOrLeakingToken(t *testing.T) {
	redirectRequests := 0
	redirected := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirectRequests++ }))
	defer redirected.Close()
	initialRequests := 0
	initial := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		initialRequests++
		http.Redirect(response, request, redirected.URL+"/leak", http.StatusFound)
	}))
	defer initial.Close()
	manager, err := NewManager(ManagerConfig{
		Root: filepath.Join(t.TempDir(), "reviews"), GitLabBaseURL: initial.URL,
		PersonalAccessToken: "redirect-test-pat", ToolUID: uint32(os.Geteuid()), ToolGID: uint32(os.Getegid()),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	_, err = manager.Prepare(context.Background(), gitlab.Snapshot{
		Identity: gitlab.Identity{MergeRequestIID: 7, HeadSHA: strings.Repeat("a", 40)}, ProjectPath: "group/project",
	}, nil)
	var failureError *failure.Error
	if !errors.As(err, &failureError) || failureError.Category != "repository_preparation_failed" || initialRequests != 1 || redirectRequests != 0 ||
		strings.Contains(err.Error(), "redirect-test-pat") {
		t.Fatalf("redirect preparation error=%v initial=%d redirected=%d", err, initialRequests, redirectRequests)
	}
}

func bashErrorCategory(result ToolResult) string {
	errorValue, _ := result.Response["error"].(map[string]any)
	category, _ := errorValue["category"].(string)
	return category
}

func testToolWorkspace(t *testing.T) *localWorkspace {
	t.Helper()
	root := t.TempDir()
	cwd := filepath.Join(root, "current")
	for _, directory := range []string{cwd, filepath.Join(root, ".home"), filepath.Join(root, ".tmp"), filepath.Join(root, ".wormtamer-output")} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return &localWorkspace{
		root: root, cwd: cwd, executable: os.Args[0], toolUID: uint32(os.Geteuid()), toolGID: uint32(os.Getegid()),
		context: ReviewContext{WorkingDirectory: cwd},
	}
}

type repeatingReader struct {
	value byte
	read  int
}

func (r *repeatingReader) Read(contents []byte) (int, error) {
	for index := range contents {
		contents[index] = r.value
	}
	r.read += len(contents)
	return len(contents), nil
}

func createBareRepository(t *testing.T, barePath string, branches map[string]string) string {
	t.Helper()
	work := t.TempDir()
	run(t, work, "git", "init", "-b", "main")
	run(t, work, "git", "config", "user.name", "Test")
	run(t, work, "git", "config", "user.email", "test@example.com")
	run(t, work, "git", "config", "commit.gpgsign", "false")
	mainContent, ok := branches["main"]
	if !ok {
		t.Fatal("test repository requires a main branch")
	}
	if err := os.WriteFile(filepath.Join(work, "content.txt"), []byte(mainContent+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run(t, work, "git", "add", "content.txt")
	run(t, work, "git", "commit", "-m", "main")
	mainHead := strings.TrimSpace(run(t, work, "git", "rev-parse", "HEAD"))
	otherBranches := make([]string, 0, len(branches)-1)
	for branch := range branches {
		if branch != "main" {
			otherBranches = append(otherBranches, branch)
		}
	}
	sort.Strings(otherBranches)
	for _, branch := range otherBranches {
		run(t, work, "git", "switch", "-C", branch, "main")
		if err := os.WriteFile(filepath.Join(work, "content.txt"), []byte(branches[branch]+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		run(t, work, "git", "add", "content.txt")
		run(t, work, "git", "commit", "-m", branch)
	}
	if err := os.MkdirAll(filepath.Dir(barePath), 0o700); err != nil {
		t.Fatal(err)
	}
	run(t, filepath.Dir(barePath), "git", "clone", "--bare", work, barePath)
	run(t, barePath, "git", "update-server-info")
	return mainHead
}

func setBareRef(t *testing.T, barePath, ref, revision string) {
	t.Helper()
	run(t, barePath, "git", "update-ref", ref, revision)
	run(t, barePath, "git", "update-server-info")
}

func run(t *testing.T, directory, name string, arguments ...string) string {
	t.Helper()
	command := exec.Command(name, arguments...)
	command.Dir = directory
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", name, arguments, err, output)
	}
	return string(output)
}
