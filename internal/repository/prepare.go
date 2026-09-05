package repository

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/aminvakil/wormtamer/internal/failure"
	"github.com/aminvakil/wormtamer/internal/gitlab"
)

var commitPattern = regexp.MustCompile(`^[0-9a-fA-F]{40}(?:[0-9a-fA-F]{24})?$`)

func (m *Manager) Prepare(ctx context.Context, snapshot gitlab.Snapshot, memories []Memory) (Workspace, error) {
	m.mu.Lock()
	closed := m.closed
	m.mu.Unlock()
	if closed {
		return nil, failure.Retry("repository_preparation_failed", 0)
	}

	setupCtx, cancel := context.WithTimeout(ctx, m.setupTimeout)
	defer cancel()
	workspace, err := m.prepare(setupCtx, snapshot, memories)
	if err == nil {
		return workspace, nil
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if errors.Is(setupCtx.Err(), context.DeadlineExceeded) {
		return nil, failure.Retry("repository_preparation_timeout", 0)
	}
	var preparationFailure *failure.Error
	if errors.As(err, &preparationFailure) {
		return nil, err
	}
	return nil, fmt.Errorf("prepare repositories: %w", failure.Retry("repository_preparation_failed", 0))
}

func (m *Manager) prepare(ctx context.Context, snapshot gitlab.Snapshot, memories []Memory) (_ Workspace, returnedErr error) {
	if snapshot.ProjectPath == "" || snapshot.Identity.MergeRequestIID <= 0 || !commitPattern.MatchString(snapshot.Identity.HeadSHA) {
		return nil, failure.Failed("review_identity_mismatch")
	}
	identifier, err := randomIdentifier()
	if err != nil {
		return nil, err
	}
	staging := filepath.Join(m.stagingRoot, "setup-"+identifier)
	if err := os.Mkdir(staging, 0o700); err != nil {
		return nil, err
	}
	defer func() {
		if returnedErr != nil {
			_ = os.RemoveAll(staging)
		}
	}()

	serviceHome := filepath.Join(staging, ".service-home")
	if err := os.Mkdir(serviceHome, 0o700); err != nil {
		return nil, err
	}
	currentDirectory := filepath.Join(staging, "current")
	currentURL, err := repositoryURL(m.baseURL, snapshot.ProjectPath)
	if err != nil {
		return nil, err
	}
	if _, err := m.runGit(ctx, staging, serviceHome, true, "clone", "--no-checkout", "--origin", "origin", "--", currentURL, currentDirectory); err != nil {
		return nil, err
	}
	mergeRequestRef := "refs/merge-requests/" + strconv.FormatInt(snapshot.Identity.MergeRequestIID, 10) + "/head"
	if _, err := m.runGit(ctx, staging, serviceHome, true, "-C", currentDirectory, "fetch", "--no-tags", "origin", "+"+mergeRequestRef+":refs/remotes/origin/merge-request-head"); err != nil {
		return nil, err
	}
	resolvedHead, err := m.runGit(ctx, staging, serviceHome, false, "-C", currentDirectory, "rev-parse", "--verify", "refs/remotes/origin/merge-request-head^{commit}")
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(strings.TrimSpace(resolvedHead), snapshot.Identity.HeadSHA) {
		return nil, failure.Obsolete("merge_request_head_changed")
	}
	if _, err := m.runGit(ctx, staging, serviceHome, false, "-C", currentDirectory, "checkout", "--detach", "--force", strings.ToLower(snapshot.Identity.HeadSHA)); err != nil {
		return nil, err
	}

	preparedRelated := make([]PreparedRepository, 0, len(snapshot.RelatedRepositories))
	for _, related := range snapshot.RelatedRepositories {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		relatedURL, err := repositoryURL(m.baseURL, related)
		if err != nil {
			return nil, err
		}
		relatedDirectory := filepath.Join(staging, "related", filepath.FromSlash(related))
		if err := os.MkdirAll(filepath.Dir(relatedDirectory), 0o700); err != nil {
			return nil, err
		}
		if _, err := m.runGit(ctx, staging, serviceHome, true, "clone", "--no-checkout", "--origin", "origin", "--", relatedURL, relatedDirectory); err != nil {
			return nil, err
		}
		revision, err := m.runGit(ctx, staging, serviceHome, false, "-C", relatedDirectory, "rev-parse", "--verify", "refs/remotes/origin/HEAD^{commit}")
		if err != nil {
			return nil, err
		}
		revision = strings.ToLower(strings.TrimSpace(revision))
		if !commitPattern.MatchString(revision) {
			return nil, errors.New("related repository default branch has an invalid revision")
		}
		if _, err := m.runGit(ctx, staging, serviceHome, false, "-C", relatedDirectory, "checkout", "--detach", "--force", revision); err != nil {
			return nil, err
		}
		preparedRelated = append(preparedRelated, PreparedRepository{
			Repository: related, Path: relatedDirectory, InitialRevision: revision,
		})
	}

	for _, repositoryDirectory := range append([]string{currentDirectory}, preparedPaths(preparedRelated)...) {
		if err := m.validateGitConfiguration(ctx, staging, serviceHome, repositoryDirectory); err != nil {
			return nil, err
		}
	}
	if err := os.RemoveAll(serviceHome); err != nil {
		return nil, err
	}

	memoryPath := filepath.Join(staging, "review-memory.json")
	if err := writeReviewMemory(memoryPath, snapshot, memories); err != nil {
		return nil, err
	}
	for _, directory := range []string{filepath.Join(staging, ".home"), filepath.Join(staging, ".tmp"), filepath.Join(staging, ".wormtamer-output")} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			return nil, err
		}
	}

	if err := chownTree(ctx, staging, int(m.toolUID), int(m.toolGID)); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	finalRoot := filepath.Join(m.root, "review-"+identifier)
	if err := os.Rename(staging, finalRoot); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		_ = os.RemoveAll(finalRoot)
		return nil, err
	}
	translate := func(path string) string {
		relative, _ := filepath.Rel(staging, path)
		return filepath.Join(finalRoot, relative)
	}
	for index := range preparedRelated {
		preparedRelated[index].Path = translate(preparedRelated[index].Path)
	}
	workspace := &localWorkspace{
		root: finalRoot, cwd: translate(currentDirectory), toolUID: m.toolUID,
		toolGID: m.toolGID, executable: m.executable,
		context: ReviewContext{
			WorkingDirectory:    translate(currentDirectory),
			RelatedRepositories: preparedRelated, MemoryPath: translate(memoryPath),
		},
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		_ = os.RemoveAll(finalRoot)
		return nil, errors.New("repository workspace manager is closed")
	}
	m.mu.Unlock()
	return workspace, nil
}

func preparedPaths(repositories []PreparedRepository) []string {
	paths := make([]string, len(repositories))
	for index := range repositories {
		paths[index] = repositories[index].Path
	}
	return paths
}

func repositoryURL(baseURL, repository string) (string, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil {
		return "", errors.New("invalid GitLab repository base URL")
	}
	segments := strings.Split(repository, "/")
	for index, segment := range segments {
		if segment == "" || segment == "." || segment == ".." {
			return "", errors.New("invalid GitLab repository path")
		}
		segments[index] = url.PathEscape(segment)
	}
	parsed.Path = strings.TrimSuffix(parsed.Path, "/") + "/" + strings.Join(segments, "/") + ".git"
	parsed.RawPath = ""
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}

func (m *Manager) runGit(ctx context.Context, cwd, home string, authenticated bool, arguments ...string) (string, error) {
	command := exec.Command("git", arguments...)
	command.Dir = cwd
	command.Env = []string{
		"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=" + home, "LANG=C.UTF-8", "LC_ALL=C.UTF-8",
		"GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
	}
	if authenticated {
		authorization := "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("oauth2:"+m.token))
		command.Env = append(command.Env,
			"GIT_CONFIG_COUNT=2",
			"GIT_CONFIG_KEY_0=http.extraHeader", "GIT_CONFIG_VALUE_0="+authorization,
			"GIT_CONFIG_KEY_1=http.followRedirects", "GIT_CONFIG_VALUE_1=false",
		)
	}
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var output boundedOutput
	command.Stdout = &output
	command.Stderr = &output
	if err := command.Start(); err != nil {
		return "", err
	}
	pid := command.Process.Pid
	wait := make(chan error, 1)
	go func() { wait <- command.Wait() }()
	select {
	case err := <-wait:
		if err != nil {
			return "", errors.New("Git repository setup command failed")
		}
		if processGroupExists(pid) {
			terminateProcessGroup(pid)
			return "", errors.New("Git repository setup process group did not exit")
		}
	case <-ctx.Done():
		terminateProcessGroup(pid)
		<-wait
		return "", ctx.Err()
	}
	return output.String(), nil
}

type boundedOutput struct {
	contents []byte
}

func (w *boundedOutput) Write(contents []byte) (int, error) {
	const limit = 64 << 10
	if len(contents) >= limit {
		w.contents = append(w.contents[:0], contents[len(contents)-limit:]...)
		return len(contents), nil
	}
	if len(w.contents)+len(contents) > limit {
		w.contents = append([]byte(nil), w.contents[len(w.contents)+len(contents)-limit:]...)
	}
	w.contents = append(w.contents, contents...)
	return len(contents), nil
}

func (w *boundedOutput) String() string { return strings.ToValidUTF8(string(w.contents), "�") }

func terminateProcessGroup(pid int) {
	if pid <= 0 {
		return
	}
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	time.Sleep(50 * time.Millisecond)
	_ = syscall.Kill(-pid, syscall.SIGKILL)
}

func processGroupExists(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(-pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func (m *Manager) validateGitConfiguration(ctx context.Context, cwd, home, repositoryDirectory string) error {
	output, err := m.runGit(ctx, cwd, home, false, "-C", repositoryDirectory, "config", "--local", "--null", "--list")
	if err != nil {
		return err
	}
	for _, record := range strings.Split(output, "\x00") {
		if record == "" {
			continue
		}
		key, value, found := strings.Cut(record, "\n")
		if !found {
			return errors.New("malformed retained Git configuration")
		}
		lowerKey := strings.ToLower(key)
		if retainedCredentialKey(lowerKey) || strings.Contains(value, m.token) {
			return errors.New("unsafe retained Git configuration")
		}
		if strings.HasPrefix(lowerKey, "remote.") && (strings.HasSuffix(lowerKey, ".url") || strings.HasSuffix(lowerKey, ".pushurl")) {
			remote, err := url.Parse(value)
			if err != nil || remote.User != nil {
				return errors.New("credential-bearing Git remote URL")
			}
		}
	}
	return nil
}

func retainedCredentialKey(key string) bool {
	if strings.HasPrefix(key, "credential.") || key == "core.askpass" || key == "core.gitproxy" || key == "http.proxy" || key == "https.proxy" {
		return true
	}
	if strings.HasPrefix(key, "http.") && (strings.HasSuffix(key, ".extraheader") || strings.HasSuffix(key, ".proxy")) {
		return true
	}
	return strings.HasPrefix(key, "remote.") && strings.HasSuffix(key, ".proxy")
}

func writeReviewMemory(path string, snapshot gitlab.Snapshot, memories []Memory) error {
	type memoryRecord struct {
		MemoryID          string `json:"memory_id"`
		Lesson            string `json:"lesson"`
		EvidenceReference string `json:"evidence_reference"`
		CreatedAt         string `json:"created_at"`
	}
	document := struct {
		Authority string `json:"authority"`
		Scope     struct {
			Type        string `json:"type"`
			ProjectID   int64  `json:"project_id"`
			ProjectPath string `json:"project_path"`
		} `json:"scope"`
		Memories []memoryRecord `json:"memories"`
	}{Authority: "untrusted_advisory", Memories: make([]memoryRecord, 0, len(memories))}
	document.Scope.Type = "repository"
	document.Scope.ProjectID = snapshot.Identity.ProjectID
	document.Scope.ProjectPath = snapshot.ProjectPath
	for _, memory := range memories {
		document.Memories = append(document.Memories, memoryRecord{
			MemoryID: memory.ID, Lesson: memory.Lesson, EvidenceReference: memory.SourceURL,
			CreatedAt: memory.UpdatedAt.UTC().Format(time.RFC3339Nano),
		})
	}
	encoded, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	return os.WriteFile(path, encoded, 0o600)
}

func chownTree(ctx context.Context, root string, uid, gid int) error {
	return filepath.Walk(root, func(path string, _ os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := os.Lchown(path, uid, gid); err != nil {
			return fmt.Errorf("transfer review workspace ownership: %w", err)
		}
		return nil
	})
}

func randomIdentifier() (string, error) {
	contents := make([]byte, 12)
	if _, err := rand.Read(contents); err != nil {
		return "", err
	}
	return hex.EncodeToString(contents), nil
}
