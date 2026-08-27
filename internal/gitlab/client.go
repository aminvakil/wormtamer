package gitlab

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aminvakil/wormtamer/internal/failure"
)

const (
	requestTimeout              = 30 * time.Second
	metadataResponseLimit       = 256 << 10
	diffResponseLimit           = 2 << 20
	noteResponseLimit           = 2 << 20
	maxDiffPages                = 5
	diffsPerPage                = 20
	maxDiffVersions             = 100
	maxChangedFiles             = 100
	maxDiffContentBytes         = 512 << 10
	maxNotePages                = 10
	notesPerPage                = 100
	maxNotes                    = 1000
	maxNoteBodyBytes            = 64 << 10
	maxCommentContentBytes      = 512 << 10
	reconciliationResponseLimit = 2 << 20
	mergeRequestsPerPage        = 100
	maxSupportedRetryAfter      = 24 * time.Hour
	missingRateLimitDelay       = 5 * time.Minute
)

var (
	headSHAPattern = regexp.MustCompile(`^(?:[0-9a-fA-F]{40}|[0-9a-fA-F]{64})$`)
	patchIDPattern = regexp.MustCompile(`^(?:[0-9a-fA-F]{40}|[0-9a-fA-F]{64})$`)
)

const (
	PatchIDAvailable   = "available"
	PatchIDPending     = "pending"
	PatchIDUnavailable = "unavailable"
)

type Identity struct {
	GitLabInstance  string
	ProjectID       int64
	MergeRequestIID int64
	HeadSHA         string
}

type Snapshot struct {
	Identity             Identity
	ProjectPath          string
	RelatedRepositories  []string
	WorkingDirectory     string
	PreparedRepositories []PreparedRepository
	ReviewMemoryPath     string
	Title                string
	Description          string
	SourceBranch         string
	TargetBranch         string
	PatchIDStatus        string
	PatchIDSHA           string
	Files                []ChangedFile
}

type PreparedRepository struct {
	Repository      string `json:"repository"`
	Path            string `json:"path"`
	InitialRevision string `json:"initial_revision"`
}

type ChangedFile struct {
	OldPath     string `json:"old_path"`
	NewPath     string `json:"new_path"`
	Diff        string `json:"diff"`
	NewFile     bool   `json:"new_file"`
	RenamedFile bool   `json:"renamed_file"`
	DeletedFile bool   `json:"deleted_file"`
}

type ReconciliationMergeRequest struct {
	ProjectID       int64
	MergeRequestIID int64
	HeadSHA         string
	Draft           bool
	WorkInProgress  bool
}

type FeedbackRef struct {
	Identity
	ProjectPath string
}

type FeedbackEvidence struct {
	Files     []ChangedFile
	Comments  []FeedbackComment
	SourceURL string
}

type FeedbackComment struct {
	AuthorID int64  `json:"author_id"`
	Body     string `json:"body"`
}

type Client struct {
	baseURL    *url.URL
	token      string
	authorized map[string]struct{}
	sharing    map[string]map[string]struct{}
	httpClient *http.Client
	now        func() time.Time
	after      func(time.Duration) <-chan time.Time

	gateMu    sync.Mutex
	notBefore time.Time
}

type projectResponse struct {
	ID                int64  `json:"id"`
	PathWithNamespace string `json:"path_with_namespace"`
}

type reconciliationMergeRequestResponse struct {
	IID            int64  `json:"iid"`
	ProjectID      int64  `json:"project_id"`
	State          string `json:"state"`
	SHA            string `json:"sha"`
	Draft          bool   `json:"draft"`
	WorkInProgress bool   `json:"work_in_progress"`
}

type mergeRequestResponse struct {
	ID           int64  `json:"id"`
	IID          int64  `json:"iid"`
	ProjectID    int64  `json:"project_id"`
	State        string `json:"state"`
	Title        string `json:"title"`
	Description  string `json:"description"`
	SourceBranch string `json:"source_branch"`
	TargetBranch string `json:"target_branch"`
	DiffRefs     struct {
		HeadSHA string `json:"head_sha"`
	} `json:"diff_refs"`
}

type diffVersionResponse struct {
	ID             int64   `json:"id"`
	HeadCommitSHA  string  `json:"head_commit_sha"`
	MergeRequestID int64   `json:"merge_request_id"`
	State          string  `json:"state"`
	PatchIDSHA     *string `json:"patch_id_sha"`
}

type diffResponse struct {
	OldPath     string `json:"old_path"`
	NewPath     string `json:"new_path"`
	Diff        string `json:"diff"`
	NewFile     bool   `json:"new_file"`
	RenamedFile bool   `json:"renamed_file"`
	DeletedFile bool   `json:"deleted_file"`
	Collapsed   bool   `json:"collapsed"`
	TooLarge    bool   `json:"too_large"`
}

type noteResponse struct {
	ID        int64  `json:"id"`
	Body      string `json:"body"`
	System    bool   `json:"system"`
	Internal  bool   `json:"internal"`
	UpdatedAt string `json:"updated_at"`
	Author    struct {
		ID int64 `json:"id"`
	} `json:"author"`
}

type userResponse struct {
	ID int64 `json:"id"`
}

func New(baseURL, token string, authorizedRepositories []string, repositorySharing map[string][]string, providedClient *http.Client) (*Client, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, errors.New("invalid GitLab base URL")
	}
	if token == "" {
		return nil, errors.New("GitLab personal access token is required")
	}

	httpClient := &http.Client{}
	if providedClient != nil {
		*httpClient = *providedClient
	}
	httpClient.Timeout = requestTimeout
	httpClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}

	authorized := make(map[string]struct{}, len(authorizedRepositories))
	for _, repository := range authorizedRepositories {
		authorized[repository] = struct{}{}
	}
	sharing := make(map[string]map[string]struct{}, len(repositorySharing))
	for target, relatedRepositories := range repositorySharing {
		sharing[target] = make(map[string]struct{}, len(relatedRepositories))
		for _, related := range relatedRepositories {
			sharing[target][related] = struct{}{}
		}
	}
	return &Client{
		baseURL:    parsed,
		token:      token,
		authorized: authorized,
		sharing:    sharing,
		httpClient: httpClient,
		now:        time.Now,
		after:      time.After,
	}, nil
}

func (c *Client) ResolveProject(ctx context.Context, projectPath string) (int64, error) {
	project, err := c.resolveProject(ctx, projectPath)
	if err != nil {
		return 0, err
	}
	return project.ID, nil
}

func (c *Client) resolveProject(ctx context.Context, projectPath string) (projectResponse, error) {
	if _, allowed := c.authorized[projectPath]; !allowed {
		return projectResponse{}, failure.Failed("repository_unauthorized")
	}
	var project projectResponse
	endpoint := "/projects/" + url.PathEscape(projectPath)
	if _, err := c.get(ctx, endpoint, nil, metadataResponseLimit, &project); err != nil {
		return projectResponse{}, err
	}
	if project.ID <= 0 || project.PathWithNamespace == "" {
		return projectResponse{}, failure.Failed("malformed_gitlab_response")
	}
	if project.PathWithNamespace != projectPath {
		return projectResponse{}, failure.Failed("repository_unauthorized")
	}
	return project, nil
}

func (c *Client) ListOpenMergeRequests(ctx context.Context, projectID int64, page int) ([]ReconciliationMergeRequest, int, error) {
	if projectID <= 0 || page <= 0 {
		return nil, 0, failure.Failed("review_identity_mismatch")
	}
	query := url.Values{
		"page":     {strconv.Itoa(page)},
		"per_page": {strconv.Itoa(mergeRequestsPerPage)},
		"state":    {"opened"},
	}
	var response []reconciliationMergeRequestResponse
	header, err := c.get(ctx, fmt.Sprintf("/projects/%d/merge_requests", projectID), query, reconciliationResponseLimit, &response)
	if err != nil {
		return nil, 0, err
	}
	if len(response) > mergeRequestsPerPage {
		return nil, 0, failure.Failed("merge_request_list_page_limit_exceeded")
	}
	mergeRequests := make([]ReconciliationMergeRequest, 0, len(response))
	for _, mergeRequest := range response {
		if mergeRequest.ProjectID != projectID || mergeRequest.IID <= 0 || mergeRequest.State != "opened" || !headSHAPattern.MatchString(mergeRequest.SHA) {
			return nil, 0, failure.Failed("malformed_gitlab_response")
		}
		mergeRequests = append(mergeRequests, ReconciliationMergeRequest{
			ProjectID: projectID, MergeRequestIID: mergeRequest.IID,
			HeadSHA: strings.ToLower(mergeRequest.SHA), Draft: mergeRequest.Draft,
			WorkInProgress: mergeRequest.WorkInProgress,
		})
	}
	next, err := nextPage(header, page)
	if err != nil {
		return nil, 0, err
	}
	return mergeRequests, next, nil
}

func (c *Client) LoadReview(ctx context.Context, identity Identity) (Snapshot, error) {
	if err := c.validateIdentity(identity); err != nil {
		return Snapshot{}, err
	}
	projectPath, err := c.checkProject(ctx, identity.ProjectID)
	if err != nil {
		return Snapshot{}, err
	}
	mergeRequest, err := c.getMergeRequest(ctx, identity)
	if err != nil {
		return Snapshot{}, err
	}
	if err := validateMergeRequest(identity, mergeRequest); err != nil {
		return Snapshot{}, err
	}
	files, err := c.getDiffs(ctx, identity)
	if err != nil {
		return Snapshot{}, err
	}
	patchIDStatus, patchIDSHA, err := c.getPatchID(ctx, identity, mergeRequest.ID)
	if err != nil {
		return Snapshot{}, err
	}
	relatedRepositories := make([]string, 0, len(c.sharing[projectPath]))
	for repository := range c.sharing[projectPath] {
		relatedRepositories = append(relatedRepositories, repository)
	}
	sort.Strings(relatedRepositories)
	return Snapshot{
		Identity:            identity,
		ProjectPath:         projectPath,
		RelatedRepositories: relatedRepositories,
		Title:               mergeRequest.Title,
		Description:         mergeRequest.Description,
		SourceBranch:        mergeRequest.SourceBranch,
		TargetBranch:        mergeRequest.TargetBranch,
		PatchIDStatus:       patchIDStatus,
		PatchIDSHA:          patchIDSHA,
		Files:               files,
	}, nil
}

func (c *Client) CheckCurrent(ctx context.Context, identity Identity) error {
	if err := c.validateIdentity(identity); err != nil {
		return err
	}
	if _, err := c.checkProject(ctx, identity.ProjectID); err != nil {
		return err
	}
	mergeRequest, err := c.getMergeRequest(ctx, identity)
	if err != nil {
		return err
	}
	return validateMergeRequest(identity, mergeRequest)
}

func (c *Client) FindNote(ctx context.Context, identity Identity, marker string) (int64, bool, error) {
	if err := c.validateIdentity(identity); err != nil {
		return 0, false, err
	}
	if marker == "" || len(marker) > 256 {
		return 0, false, failure.Failed("invalid_publication_marker")
	}
	userID, err := c.currentUserID(ctx)
	if err != nil {
		return 0, false, err
	}
	seen := 0
	for page := 1; page <= maxNotePages; page++ {
		query := url.Values{
			"order_by": {"created_at"},
			"page":     {strconv.Itoa(page)},
			"per_page": {strconv.Itoa(notesPerPage)},
			"sort":     {"desc"},
		}
		var notes []noteResponse
		header, err := c.get(ctx, c.mergeRequestPath(identity)+"/notes", query, noteResponseLimit, &notes)
		if err != nil {
			return 0, false, err
		}
		seen += len(notes)
		if seen > maxNotes {
			return 0, false, failure.Failed("note_search_limit_exceeded")
		}
		for _, note := range notes {
			if note.ID <= 0 || note.Author.ID <= 0 {
				return 0, false, failure.Failed("malformed_gitlab_response")
			}
			if note.Author.ID == userID && strings.Contains(note.Body, marker) {
				return note.ID, true, nil
			}
		}
		next, err := nextPage(header, page)
		if err != nil {
			return 0, false, err
		}
		if next == 0 {
			return 0, false, nil
		}
		if page == maxNotePages {
			return 0, false, failure.Failed("note_search_limit_exceeded")
		}
	}
	return 0, false, failure.Failed("note_search_limit_exceeded")
}

func (c *Client) PostNote(ctx context.Context, identity Identity, body string) (int64, error) {
	if err := c.validateIdentity(identity); err != nil {
		return 0, err
	}
	if len(body) == 0 || len(body) > maxNoteBodyBytes {
		return 0, failure.Failed("note_body_limit_exceeded")
	}
	userID, err := c.currentUserID(ctx)
	if err != nil {
		return 0, err
	}
	payload, err := json.Marshal(struct {
		Body string `json:"body"`
	}{Body: body})
	if err != nil {
		return 0, failure.Failed("publication_encoding_failed")
	}
	var note noteResponse
	if _, err := c.request(ctx, http.MethodPost, c.mergeRequestPath(identity)+"/notes", nil, payload, metadataResponseLimit, &note); err != nil {
		return 0, err
	}
	if note.ID <= 0 || note.Author.ID != userID {
		return 0, failure.Failed("malformed_gitlab_response")
	}
	return note.ID, nil
}

func (c *Client) LoadFeedback(ctx context.Context, ref FeedbackRef) (FeedbackEvidence, error) {
	if err := c.validateIdentity(ref.Identity); err != nil || ref.ProjectPath == "" {
		return FeedbackEvidence{}, failure.Failed("feedback_identity_mismatch")
	}
	projectPath, err := c.checkProject(ctx, ref.ProjectID)
	if err != nil {
		return FeedbackEvidence{}, err
	}
	if projectPath != ref.ProjectPath {
		return FeedbackEvidence{}, failure.Failed("repository_unauthorized")
	}
	mergeRequest, err := c.getMergeRequest(ctx, ref.Identity)
	if err != nil {
		return FeedbackEvidence{}, err
	}
	if mergeRequest.ID <= 0 || mergeRequest.ProjectID != ref.ProjectID || mergeRequest.IID != ref.MergeRequestIID ||
		!strings.EqualFold(mergeRequest.DiffRefs.HeadSHA, ref.HeadSHA) {
		return FeedbackEvidence{}, failure.Failed("feedback_identity_mismatch")
	}
	switch mergeRequest.State {
	case "closed", "merged":
	case "opened":
		return FeedbackEvidence{}, failure.Retry("merge_request_not_terminal", 0)
	default:
		return FeedbackEvidence{}, failure.Failed("unknown_merge_request_state")
	}
	files, err := c.getDiffs(ctx, ref.Identity)
	if err != nil {
		return FeedbackEvidence{}, err
	}
	comments, err := c.listFeedbackComments(ctx, ref.Identity)
	if err != nil {
		return FeedbackEvidence{}, err
	}
	return FeedbackEvidence{Files: files, Comments: comments, SourceURL: c.mergeRequestURL(projectPath, ref.MergeRequestIID)}, nil
}

func (c *Client) listFeedbackComments(ctx context.Context, identity Identity) ([]FeedbackComment, error) {
	userID, err := c.currentUserID(ctx)
	if err != nil {
		return nil, err
	}
	comments := make([]FeedbackComment, 0)
	totalContent := 0
	seen := 0
	for page := 1; page <= maxNotePages; page++ {
		query := url.Values{
			"order_by": {"created_at"},
			"page":     {strconv.Itoa(page)},
			"per_page": {strconv.Itoa(notesPerPage)},
			"sort":     {"asc"},
		}
		var notes []noteResponse
		header, err := c.get(ctx, c.mergeRequestPath(identity)+"/notes", query, noteResponseLimit, &notes)
		if err != nil {
			return nil, err
		}
		seen += len(notes)
		if seen > maxNotes {
			return nil, failure.Failed("note_search_limit_exceeded")
		}
		for _, note := range notes {
			if note.ID <= 0 || note.Author.ID <= 0 || len(note.Body) > maxNoteBodyBytes {
				return nil, failure.Failed("malformed_gitlab_response")
			}
			if note.System || note.Internal || note.Author.ID == userID {
				continue
			}
			totalContent += len(note.Body)
			if totalContent > maxCommentContentBytes {
				return nil, failure.Failed("merge_request_comment_limit_exceeded")
			}
			comments = append(comments, FeedbackComment{AuthorID: note.Author.ID, Body: note.Body})
		}
		next, err := nextPage(header, page)
		if err != nil {
			return nil, err
		}
		if next == 0 {
			return comments, nil
		}
		if page == maxNotePages {
			return nil, failure.Failed("note_search_limit_exceeded")
		}
	}
	return nil, failure.Failed("note_search_limit_exceeded")
}

func (c *Client) mergeRequestURL(projectPath string, mergeRequestIID int64) string {
	source := *c.baseURL
	source.Path = strings.TrimSuffix(c.baseURL.Path, "/") + "/" + projectPath + "/-/merge_requests/" + strconv.FormatInt(mergeRequestIID, 10)
	source.RawPath = ""
	source.RawQuery = ""
	source.Fragment = ""
	return source.String()
}

func (c *Client) currentUserID(ctx context.Context) (int64, error) {
	var user userResponse
	if _, err := c.get(ctx, "/user", nil, metadataResponseLimit, &user); err != nil {
		return 0, err
	}
	if user.ID <= 0 {
		return 0, failure.Failed("malformed_gitlab_response")
	}
	return user.ID, nil
}

func (c *Client) validateIdentity(identity Identity) error {
	if identity.GitLabInstance != c.baseURL.String() || identity.ProjectID <= 0 || identity.MergeRequestIID <= 0 || !headSHAPattern.MatchString(identity.HeadSHA) {
		return failure.Failed("review_identity_mismatch")
	}
	return nil
}

func (c *Client) checkProject(ctx context.Context, projectID int64) (string, error) {
	var project projectResponse
	if _, err := c.get(ctx, fmt.Sprintf("/projects/%d", projectID), nil, metadataResponseLimit, &project); err != nil {
		return "", err
	}
	if project.ID != projectID || project.PathWithNamespace == "" {
		return "", failure.Failed("malformed_gitlab_response")
	}
	if _, allowed := c.authorized[project.PathWithNamespace]; !allowed {
		return "", failure.Failed("repository_unauthorized")
	}
	return project.PathWithNamespace, nil
}

func (c *Client) getMergeRequest(ctx context.Context, identity Identity) (mergeRequestResponse, error) {
	var mergeRequest mergeRequestResponse
	if _, err := c.get(ctx, c.mergeRequestPath(identity), nil, metadataResponseLimit, &mergeRequest); err != nil {
		return mergeRequestResponse{}, err
	}
	return mergeRequest, nil
}

func validateMergeRequest(identity Identity, mergeRequest mergeRequestResponse) error {
	if mergeRequest.ID <= 0 || mergeRequest.ProjectID != identity.ProjectID || mergeRequest.IID != identity.MergeRequestIID || mergeRequest.DiffRefs.HeadSHA == "" {
		return failure.Failed("malformed_gitlab_response")
	}
	switch mergeRequest.State {
	case "opened":
	case "closed", "merged", "locked":
		return failure.Obsolete("merge_request_not_open")
	default:
		return failure.Failed("unknown_merge_request_state")
	}
	if !strings.EqualFold(mergeRequest.DiffRefs.HeadSHA, identity.HeadSHA) {
		return failure.Obsolete("merge_request_head_changed")
	}
	return nil
}

func (c *Client) getPatchID(ctx context.Context, identity Identity, mergeRequestID int64) (string, string, error) {
	query := url.Values{
		"page":     {"1"},
		"per_page": {strconv.Itoa(maxDiffVersions)},
	}
	var versions []diffVersionResponse
	header, err := c.get(ctx, c.mergeRequestPath(identity)+"/versions", query, metadataResponseLimit, &versions)
	if err != nil {
		return "", "", err
	}
	if len(versions) > maxDiffVersions {
		return "", "", failure.Failed("merge_request_diff_version_limit_exceeded")
	}
	if _, err := nextPage(header, 1); err != nil {
		return "", "", err
	}
	for _, version := range versions {
		if version.ID <= 0 || version.MergeRequestID != mergeRequestID || !headSHAPattern.MatchString(version.HeadCommitSHA) {
			return "", "", failure.Failed("malformed_gitlab_response")
		}
		if !strings.EqualFold(version.HeadCommitSHA, identity.HeadSHA) {
			continue
		}
		if version.PatchIDSHA != nil {
			if !patchIDPattern.MatchString(*version.PatchIDSHA) {
				return "", "", failure.Failed("malformed_gitlab_response")
			}
			if !finalizedDiffVersionState(version.State) {
				return "", "", failure.Failed("unknown_merge_request_diff_state")
			}
			return PatchIDAvailable, strings.ToLower(*version.PatchIDSHA), nil
		}
		switch version.State {
		case "collected":
			return PatchIDPending, "", nil
		case "empty", "overflow", "without_files",
			"timeout", "overflow_commits_safe_size", "overflow_diff_files_limit", "overflow_diff_lines_limit":
			return PatchIDUnavailable, "", nil
		default:
			return "", "", failure.Failed("unknown_merge_request_diff_state")
		}
	}
	return PatchIDPending, "", nil
}

func finalizedDiffVersionState(state string) bool {
	switch state {
	case "collected", "empty", "overflow", "without_files",
		"timeout", "overflow_commits_safe_size", "overflow_diff_files_limit", "overflow_diff_lines_limit":
		return true
	default:
		return false
	}
}

func (c *Client) getDiffs(ctx context.Context, identity Identity) ([]ChangedFile, error) {
	files := make([]ChangedFile, 0)
	totalContent := 0
	for page := 1; page <= maxDiffPages; page++ {
		query := url.Values{
			"page":     {strconv.Itoa(page)},
			"per_page": {strconv.Itoa(diffsPerPage)},
		}
		var response []diffResponse
		header, err := c.get(ctx, c.mergeRequestPath(identity)+"/diffs", query, diffResponseLimit, &response)
		if err != nil {
			return nil, err
		}
		for _, file := range response {
			if file.Collapsed || file.TooLarge {
				return nil, failure.Failed("incomplete_merge_request_diff")
			}
			if file.NewPath == "" || file.OldPath == "" || len(file.NewPath) > 1024 || len(file.OldPath) > 1024 {
				return nil, failure.Failed("malformed_gitlab_response")
			}
			totalContent += len(file.Diff)
			if totalContent > maxDiffContentBytes {
				return nil, failure.Failed("merge_request_diff_limit_exceeded")
			}
			files = append(files, ChangedFile{
				OldPath: file.OldPath, NewPath: file.NewPath, Diff: file.Diff,
				NewFile: file.NewFile, RenamedFile: file.RenamedFile, DeletedFile: file.DeletedFile,
			})
			if len(files) > maxChangedFiles {
				return nil, failure.Failed("merge_request_file_limit_exceeded")
			}
		}
		next, err := nextPage(header, page)
		if err != nil {
			return nil, err
		}
		if next == 0 {
			return files, nil
		}
		if page == maxDiffPages {
			return nil, failure.Failed("merge_request_diff_page_limit_exceeded")
		}
	}
	return nil, failure.Failed("merge_request_diff_page_limit_exceeded")
}

func (c *Client) mergeRequestPath(identity Identity) string {
	return fmt.Sprintf("/projects/%d/merge_requests/%d", identity.ProjectID, identity.MergeRequestIID)
}

func (c *Client) get(ctx context.Context, endpoint string, query url.Values, limit int64, target any) (http.Header, error) {
	return c.request(ctx, http.MethodGet, endpoint, query, nil, limit, target)
}

func (c *Client) request(ctx context.Context, method, endpoint string, query url.Values, body []byte, limit int64, target any) (http.Header, error) {
	response, err := c.do(ctx, method, endpoint, query, body, "application/json")
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	contents, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, failure.Retry("gitlab_response_read_failed", 0)
	}
	if int64(len(contents)) > limit {
		return nil, failure.Failed("gitlab_response_limit_exceeded")
	}
	if err := json.Unmarshal(contents, target); err != nil {
		return nil, failure.Failed("malformed_gitlab_response")
	}
	return response.Header, nil
}

func (c *Client) do(ctx context.Context, method, endpoint string, query url.Values, body []byte, accept string) (*http.Response, error) {
	response, err := c.send(ctx, method, endpoint, query, body, accept)
	if err != nil {
		return nil, err
	}
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		response.Body.Close()
		return nil, failure.Failed("gitlab_redirect_rejected")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		err := c.statusError(response)
		response.Body.Close()
		return nil, err
	}
	return response, nil
}

func (c *Client) send(ctx context.Context, method, endpoint string, query url.Values, body []byte, accept string) (*http.Response, error) {
	if err := c.waitForGate(ctx); err != nil {
		return nil, err
	}
	requestURL := *c.baseURL
	escapedPath := strings.TrimSuffix(c.baseURL.EscapedPath(), "/") + "/api/v4" + endpoint
	decodedPath, err := url.PathUnescape(escapedPath)
	if err != nil {
		return nil, failure.Failed("gitlab_request_invalid")
	}
	requestURL.Path = decodedPath
	requestURL.RawPath = escapedPath
	requestURL.RawQuery = query.Encode()

	var requestBody io.Reader
	if body != nil {
		requestBody = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, requestURL.String(), requestBody)
	if err != nil {
		return nil, failure.Failed("gitlab_request_invalid")
	}
	request.Header.Set("PRIVATE-TOKEN", c.token)
	request.Header.Set("Accept", accept)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.httpClient.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, failure.Retry("gitlab_network_failure", 0)
	}
	return response, nil
}

func (c *Client) statusError(response *http.Response) error {
	switch response.StatusCode {
	case http.StatusUnauthorized:
		return failure.Failed("gitlab_invalid_credentials")
	case http.StatusForbidden, http.StatusNotFound:
		return failure.Failed("gitlab_authorization_failed")
	case http.StatusRequestTimeout:
		return c.retryableStatus(response, "gitlab_request_timeout")
	case http.StatusTooManyRequests:
		return c.retryableStatus(response, "gitlab_rate_limited")
	default:
		if response.StatusCode >= 500 {
			return c.retryableStatus(response, "gitlab_server_failure")
		}
		return failure.Failed("gitlab_request_rejected")
	}
}

func (c *Client) retryableStatus(response *http.Response, category string) error {
	now := c.now()
	delay, valid := parseRetryAfter(response.Header.Get("Retry-After"), now)
	if valid && delay > maxSupportedRetryAfter {
		return failure.Failed("retry_after_exceeds_limit")
	}
	if valid {
		c.extendGate(now.Add(delay))
	} else if response.StatusCode == http.StatusTooManyRequests {
		c.extendGate(now.Add(missingRateLimitDelay))
	}
	return failure.Retry(category, delay)
}

func (c *Client) waitForGate(ctx context.Context) error {
	for {
		c.gateMu.Lock()
		notBefore := c.notBefore
		c.gateMu.Unlock()
		delay := notBefore.Sub(c.now())
		if delay <= 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.after(delay):
		}
	}
}

func (c *Client) extendGate(notBefore time.Time) {
	c.gateMu.Lock()
	defer c.gateMu.Unlock()
	if notBefore.After(c.notBefore) {
		c.notBefore = notBefore
	}
}

func nextPage(header http.Header, current int) (int, error) {
	value := header.Get("X-Next-Page")
	if value == "" {
		return 0, nil
	}
	next, err := strconv.Atoi(value)
	if err != nil || next != current+1 {
		return 0, failure.Failed("malformed_gitlab_pagination")
	}
	return next, nil
}

func parseRetryAfter(value string, now time.Time) (time.Duration, bool) {
	if value == "" {
		return 0, false
	}
	if isDecimal(value) {
		seconds, err := strconv.ParseUint(value, 10, 64)
		if err != nil || seconds > uint64(maxSupportedRetryAfter/time.Second) {
			return maxSupportedRetryAfter + time.Second, true
		}
		return time.Duration(seconds) * time.Second, true
	}
	when, err := http.ParseTime(value)
	if err != nil {
		return 0, false
	}
	delay := when.Sub(now)
	if delay < 0 {
		delay = 0
	}
	return delay, true
}

func isDecimal(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}
