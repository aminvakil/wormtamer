package repository

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	ToolRead = "read"
	ToolBash = "bash"

	DefaultToolUID = 65532
	DefaultToolGID = 65532

	DefaultSetupTimeout      = 2 * time.Minute
	MaxFunctionResponseBytes = 16 << 20
	MaxCommandOutputBytes    = 16 << 20
	MaxReviewSpoolBytes      = 64 << 20
	MaxToolLines             = 2000
	MaxToolBytes             = 50 << 10
)

type ToolResult struct {
	Response map[string]any
}

type ToolBroker interface {
	Call(context.Context, string, map[string]any) (ToolResult, error)
}

type PreparedRepository struct {
	Repository      string `json:"repository"`
	Path            string `json:"path"`
	InitialRevision string `json:"initial_revision"`
}

type ReviewContext struct {
	WorkingDirectory    string
	ReviewedHead        string
	RelatedRepositories []PreparedRepository
	MemoryPath          string
}

type Workspace interface {
	ToolBroker
	Context() ReviewContext
	Close() error
}

type Memory struct {
	ID        string
	Lesson    string
	SourceURL string
	UpdatedAt time.Time
}

type ManagerConfig struct {
	Root                string
	GitLabBaseURL       string
	PersonalAccessToken string
	ToolUID             uint32
	ToolGID             uint32
	Executable          string
	SetupTimeout        time.Duration
}

type Manager struct {
	root         string
	stagingRoot  string
	baseURL      string
	token        string
	toolUID      uint32
	toolGID      uint32
	executable   string
	setupTimeout time.Duration

	mu     sync.Mutex
	closed bool
	open   map[string]struct{}
}

func NewManager(config ManagerConfig) (*Manager, error) {
	if strings.TrimSpace(config.Root) == "" {
		return nil, errors.New("repository workspace root is required")
	}
	if strings.TrimSpace(config.GitLabBaseURL) == "" || config.PersonalAccessToken == "" {
		return nil, errors.New("repository preparation credentials are required")
	}
	root, err := filepath.Abs(config.Root)
	if err != nil {
		return nil, errors.New("resolve repository workspace root")
	}
	if config.ToolUID == 0 && config.ToolGID == 0 {
		config.ToolUID = DefaultToolUID
		config.ToolGID = DefaultToolGID
	} else if config.ToolUID == 0 || config.ToolGID == 0 {
		return nil, errors.New("review-tool UID and GID must both be non-zero")
	}
	if config.Executable == "" {
		config.Executable, err = os.Executable()
		if err != nil {
			return nil, errors.New("resolve Wormtamer executable")
		}
	}
	if config.SetupTimeout <= 0 {
		config.SetupTimeout = DefaultSetupTimeout
	}
	if err := os.MkdirAll(root, 0o711); err != nil {
		return nil, fmt.Errorf("create repository workspace root: %w", err)
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("repository workspace root must be a directory, not a symlink")
	}
	if err := os.Chown(root, os.Geteuid(), os.Getegid()); err != nil {
		return nil, fmt.Errorf("own repository workspace root: %w", err)
	}
	if err := cleanDirectory(root); err != nil {
		return nil, fmt.Errorf("clean repository workspace root: %w", err)
	}
	if err := os.Chmod(root, 0o711); err != nil {
		return nil, fmt.Errorf("set repository workspace root permissions: %w", err)
	}
	stagingRoot := filepath.Join(root, ".staging")
	if err := os.Mkdir(stagingRoot, 0o700); err != nil {
		return nil, fmt.Errorf("create private repository staging root: %w", err)
	}
	return &Manager{
		root: root, stagingRoot: stagingRoot, baseURL: config.GitLabBaseURL,
		token: config.PersonalAccessToken, toolUID: config.ToolUID, toolGID: config.ToolGID,
		executable: config.Executable, setupTimeout: config.SetupTimeout, open: make(map[string]struct{}),
	}, nil
}

func (m *Manager) Close() error {
	m.mu.Lock()
	m.closed = true
	m.open = make(map[string]struct{})
	m.mu.Unlock()
	if err := cleanDirectory(m.root); err != nil {
		return fmt.Errorf("clean repository workspace root: %w", err)
	}
	return nil
}

func cleanDirectory(root string) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := os.RemoveAll(filepath.Join(root, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

type localWorkspace struct {
	manager    *Manager
	root       string
	cwd        string
	context    ReviewContext
	toolUID    uint32
	toolGID    uint32
	executable string

	spoolMu   sync.Mutex
	spoolUsed int64
	closeOnce sync.Once
	closeErr  error
}

func (w *localWorkspace) Context() ReviewContext {
	contextCopy := w.context
	contextCopy.RelatedRepositories = append([]PreparedRepository(nil), w.context.RelatedRepositories...)
	return contextCopy
}

func (w *localWorkspace) Call(ctx context.Context, name string, arguments map[string]any) (ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return ToolResult{}, err
	}
	switch name {
	case ToolRead:
		return w.callRead(ctx, arguments)
	case ToolBash:
		return w.callBash(ctx, arguments)
	default:
		return ToolResult{}, fmt.Errorf("undeclared review tool %q", name)
	}
}

func (w *localWorkspace) Close() error {
	w.closeOnce.Do(func() {
		if err := os.RemoveAll(w.root); err != nil {
			w.closeErr = fmt.Errorf("remove review workspace: %w", err)
		}
		if w.manager != nil {
			w.manager.mu.Lock()
			delete(w.manager.open, w.root)
			w.manager.mu.Unlock()
		}
	})
	return w.closeErr
}

func onlyArguments(arguments map[string]any, allowed ...string) bool {
	set := make(map[string]struct{}, len(allowed))
	for _, key := range allowed {
		set[key] = struct{}{}
	}
	for key := range arguments {
		if _, ok := set[key]; !ok {
			return false
		}
	}
	return true
}

func integerArgument(value any) (int64, bool) {
	switch number := value.(type) {
	case int:
		return int64(number), true
	case int32:
		return int64(number), true
	case int64:
		return number, true
	case float64:
		integer := int64(number)
		return integer, number == float64(integer)
	default:
		return 0, false
	}
}
