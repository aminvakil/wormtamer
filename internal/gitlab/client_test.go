package gitlab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aminvakil/wormtamer/internal/failure"
)

const testHead = "0123456789abcdef0123456789abcdef01234567"

func TestLoadReviewAndPublication(t *testing.T) {
	const token = "private-token"
	marker := "<!-- wormtamer:review=test -->"
	posted := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("PRIVATE-TOKEN") != token {
			t.Errorf("PRIVATE-TOKEN = %q", r.Header.Get("PRIVATE-TOKEN"))
		}
		switch r.URL.Path {
		case "/gitlab/api/v4/user":
			writeJSON(t, w, userResponse{ID: 77})
		case "/gitlab/api/v4/projects/42":
			writeJSON(t, w, projectResponse{ID: 42, PathWithNamespace: "group/project"})
		case "/gitlab/api/v4/projects/42/merge_requests/7":
			writeMergeRequest(t, w, "opened", testHead)
		case "/gitlab/api/v4/projects/42/merge_requests/7/diffs":
			if r.URL.Query().Get("page") != "1" || r.URL.Query().Get("per_page") != "20" {
				t.Errorf("diff query = %q", r.URL.RawQuery)
			}
			writeJSON(t, w, []diffResponse{{OldPath: "old.go", NewPath: "new.go", Diff: "@@ -1 +1 @@\n-old\n+new"}})
		case "/gitlab/api/v4/projects/42/merge_requests/7/versions":
			if r.URL.Query().Get("page") != "1" || r.URL.Query().Get("per_page") != "100" {
				t.Errorf("diff version query = %q", r.URL.RawQuery)
			}
			patchID := strings.ToUpper(strings.Repeat("a", 40))
			writeJSON(t, w, []diffVersionResponse{{
				ID: 9, HeadCommitSHA: testHead, MergeRequestID: 8,
				State: "collected", PatchIDSHA: &patchID,
			}})
		case "/gitlab/api/v4/projects/42/merge_requests/7/notes":
			if r.Method == http.MethodGet {
				if r.URL.Query().Get("order_by") != "created_at" || r.URL.Query().Get("sort") != "desc" {
					t.Errorf("note ordering query = %q", r.URL.RawQuery)
				}
				writeJSON(t, w, []noteResponse{testNote(12, "review\n"+marker, 77)})
				return
			}
			var request struct {
				Body string `json:"body"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			posted = request.Body
			w.WriteHeader(http.StatusCreated)
			writeJSON(t, w, testNote(13, request.Body, 77))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client, err := New(server.URL+"/gitlab", token, []string{"group/project", "group/related"}, true, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	identity := testIdentity(server.URL + "/gitlab")
	snapshot, err := client.LoadReview(context.Background(), identity)
	if err != nil {
		t.Fatalf("LoadReview() error = %v", err)
	}
	if snapshot.ProjectPath != "group/project" || len(snapshot.RelatedRepositories) != 1 || snapshot.RelatedRepositories[0] != "group/related" || snapshot.Title != "Review title" || snapshot.PatchIDStatus != PatchIDAvailable || snapshot.PatchIDSHA != strings.Repeat("a", 40) || len(snapshot.Files) != 1 || snapshot.Files[0].NewPath != "new.go" {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	if err := client.CheckCurrent(context.Background(), identity); err != nil {
		t.Fatalf("CheckCurrent() error = %v", err)
	}
	noteID, found, err := client.FindNote(context.Background(), identity, marker)
	if err != nil || !found || noteID != 12 {
		t.Fatalf("FindNote() = %d, %t, %v", noteID, found, err)
	}
	noteID, err = client.PostNote(context.Background(), identity, "summary\n"+marker)
	if err != nil || noteID != 13 || posted != "summary\n"+marker {
		t.Fatalf("PostNote() = %d, %v; body %q", noteID, err, posted)
	}
}

func TestLoadReviewUsesAllOrNothingRepositorySharing(t *testing.T) {
	tests := []struct {
		name                   string
		authorizedRepositories []string
		shareAll               bool
		want                   []string
	}{
		{name: "current repository only", authorizedRepositories: []string{"group/project", "group/zeta"}},
		{name: "all authorized sorted once", authorizedRepositories: []string{"group/zeta", "group/project", "group/alpha", "group/zeta"}, shareAll: true, want: []string{"group/alpha", "group/zeta"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v4/projects/42":
					writeJSON(t, w, projectResponse{ID: 42, PathWithNamespace: "group/project"})
				case "/api/v4/projects/42/merge_requests/7":
					writeMergeRequest(t, w, "opened", testHead)
				case "/api/v4/projects/42/merge_requests/7/diffs":
					writeJSON(t, w, []diffResponse{{OldPath: "old.go", NewPath: "new.go", Diff: "+new"}})
				case "/api/v4/projects/42/merge_requests/7/versions":
					writeJSON(t, w, []diffVersionResponse{})
				default:
					t.Fatal("unexpected request: " + r.URL.Path)
				}
			}))
			defer server.Close()

			client, err := New(server.URL, "token", test.authorizedRepositories, test.shareAll, server.Client())
			if err != nil {
				t.Fatal(err)
			}
			snapshot, err := client.LoadReview(context.Background(), testIdentity(server.URL))
			if err != nil {
				t.Fatalf("LoadReview() error = %v", err)
			}
			if !slices.Equal(snapshot.RelatedRepositories, test.want) {
				t.Fatalf("RelatedRepositories = %v, want %v", snapshot.RelatedRepositories, test.want)
			}
		})
	}
}

func TestLoadReviewClassifiesPatchID(t *testing.T) {
	valid40 := strings.Repeat("a", 40)
	valid64 := strings.Repeat("B", 64)
	tests := []struct {
		name       string
		versions   []diffVersionResponse
		wantStatus string
		wantSHA    string
		category   string
	}{
		{name: "matching SHA-1", versions: []diffVersionResponse{{ID: 2, HeadCommitSHA: testHead, MergeRequestID: 8, State: "collected", PatchIDSHA: &valid40}}, wantStatus: PatchIDAvailable, wantSHA: valid40},
		{name: "matching SHA-256", versions: []diffVersionResponse{{ID: 2, HeadCommitSHA: testHead, MergeRequestID: 8, State: "overflow", PatchIDSHA: &valid64}}, wantStatus: PatchIDAvailable, wantSHA: strings.ToLower(valid64)},
		{name: "current version not exposed", versions: []diffVersionResponse{{ID: 1, HeadCommitSHA: strings.Repeat("c", 40), MergeRequestID: 8, State: "collected", PatchIDSHA: &valid40}}, wantStatus: PatchIDPending},
		{name: "collected null", versions: []diffVersionResponse{{ID: 2, HeadCommitSHA: testHead, MergeRequestID: 8, State: "collected"}}, wantStatus: PatchIDPending},
		{name: "empty", versions: []diffVersionResponse{{ID: 2, HeadCommitSHA: testHead, MergeRequestID: 8, State: "empty"}}, wantStatus: PatchIDUnavailable},
		{name: "unknown state", versions: []diffVersionResponse{{ID: 2, HeadCommitSHA: testHead, MergeRequestID: 8, State: "collecting"}}, category: "unknown_merge_request_diff_state"},
		{name: "malformed patch ID", versions: []diffVersionResponse{{ID: 2, HeadCommitSHA: testHead, MergeRequestID: 8, State: "collected", PatchIDSHA: stringPointer("not-a-patch-id")}}, category: "malformed_gitlab_response"},
		{name: "merge request mismatch", versions: []diffVersionResponse{{ID: 2, HeadCommitSHA: testHead, MergeRequestID: 9, State: "collected", PatchIDSHA: &valid40}}, category: "malformed_gitlab_response"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v4/projects/42":
					writeJSON(t, w, projectResponse{ID: 42, PathWithNamespace: "group/project"})
				case "/api/v4/projects/42/merge_requests/7":
					writeMergeRequest(t, w, "opened", testHead)
				case "/api/v4/projects/42/merge_requests/7/diffs":
					writeJSON(t, w, []diffResponse{{OldPath: "old.go", NewPath: "new.go", Diff: "+new"}})
				case "/api/v4/projects/42/merge_requests/7/versions":
					writeJSON(t, w, test.versions)
				default:
					t.Fatal("unexpected request: " + r.URL.Path)
				}
			}))
			defer server.Close()
			client := newTestClient(t, server.URL, "token", server.Client())
			snapshot, err := client.LoadReview(context.Background(), testIdentity(server.URL))
			if test.category != "" {
				assertFailure(t, err, test.category, false, false)
				return
			}
			if err != nil || snapshot.PatchIDStatus != test.wantStatus || snapshot.PatchIDSHA != test.wantSHA {
				t.Fatalf("LoadReview() patch status=%q SHA=%q error=%v", snapshot.PatchIDStatus, snapshot.PatchIDSHA, err)
			}
		})
	}
}

func TestLoadFeedbackReturnsDiffAndHumanComments(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v4/projects/42":
			writeJSON(t, w, projectResponse{ID: 42, PathWithNamespace: "group/project"})
		case "/api/v4/projects/42/merge_requests/7":
			writeMergeRequest(t, w, "closed", testHead)
		case "/api/v4/projects/42/merge_requests/7/diffs":
			writeJSON(t, w, []diffResponse{{OldPath: "old.go", NewPath: "new.go", Diff: "+new"}})
		case "/api/v4/user":
			writeJSON(t, w, userResponse{ID: 77})
		case "/api/v4/projects/42/merge_requests/7/notes":
			if r.URL.Query().Get("sort") != "asc" || r.URL.Query().Get("order_by") != "created_at" {
				t.Errorf("notes query = %q", r.URL.RawQuery)
			}
			human := testNote(91, "This warning does not apply to generated output.", 12)
			bot := testNote(92, "Wormtamer review", 77)
			system := testNote(93, "changed state", 13)
			system.System = true
			internal := testNote(94, "private", 14)
			internal.Internal = true
			writeJSON(t, w, []noteResponse{human, bot, system, internal})
		default:
			t.Fatal("unexpected request: " + r.URL.String())
		}
	}))
	defer server.Close()
	client := newTestClient(t, server.URL, "token", server.Client())
	evidence, err := client.LoadFeedback(context.Background(), FeedbackRef{
		Identity: testIdentity(server.URL), ProjectPath: "group/project",
	})
	if err != nil || len(evidence.Files) != 1 || evidence.Files[0].NewPath != "new.go" ||
		len(evidence.Comments) != 1 || evidence.Comments[0].AuthorID != 12 ||
		evidence.Comments[0].Body != "This warning does not apply to generated output." {
		t.Fatalf("LoadFeedback() = %+v, %v", evidence, err)
	}
	if evidence.SourceURL != server.URL+"/group/project/-/merge_requests/7" {
		t.Fatalf("source URL = %q", evidence.SourceURL)
	}
}

func TestLoadFeedbackRequiresCurrentTerminalState(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v4/projects/42":
			writeJSON(t, w, projectResponse{ID: 42, PathWithNamespace: "group/project"})
		case "/api/v4/projects/42/merge_requests/7":
			writeMergeRequest(t, w, "opened", testHead)
		default:
			t.Fatal("unexpected request: " + r.URL.String())
		}
	}))
	defer server.Close()
	client := newTestClient(t, server.URL, "token", server.Client())
	_, err := client.LoadFeedback(context.Background(), FeedbackRef{
		Identity: testIdentity(server.URL), ProjectPath: "group/project",
	})
	assertFailure(t, err, "merge_request_not_terminal", true, false)
}

func TestFindNoteIgnoresMarkerFromAnotherAuthor(t *testing.T) {
	marker := "<!-- wormtamer:review=test -->"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v4/user":
			writeJSON(t, w, userResponse{ID: 77})
		case "/api/v4/projects/42/merge_requests/7/notes":
			writeJSON(t, w, []noteResponse{
				testNote(1, marker, 66),
				testNote(2, "unrelated", 77),
			})
		default:
			t.Fatal("unexpected request: " + r.URL.Path)
		}
	}))
	defer server.Close()
	client := newTestClient(t, server.URL, "token", server.Client())
	noteID, found, err := client.FindNote(context.Background(), testIdentity(server.URL), marker)
	if err != nil || found || noteID != 0 {
		t.Fatalf("FindNote() = %d, %t, %v", noteID, found, err)
	}
}

func TestFindNoteSearchesNewestNotesFirst(t *testing.T) {
	marker := "<!-- wormtamer:review=newest -->"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v4/user":
			writeJSON(t, w, userResponse{ID: 77})
		case "/api/v4/projects/42/merge_requests/7/notes":
			w.Header().Set("X-Total", "1001")
			if r.URL.Query().Get("order_by") == "created_at" && r.URL.Query().Get("sort") == "desc" {
				w.Header().Set("X-Next-Page", "2")
				writeJSON(t, w, []noteResponse{testNote(1001, marker, 77)})
				return
			}
			page, _ := strconv.Atoi(r.URL.Query().Get("page"))
			notes := make([]noteResponse, notesPerPage)
			for index := range notes {
				notes[index] = testNote(int64((page-1)*notesPerPage+index+1), "older note", 77)
			}
			w.Header().Set("X-Next-Page", strconv.Itoa(page+1))
			writeJSON(t, w, notes)
		default:
			t.Fatal("unexpected request: " + r.URL.Path)
		}
	}))
	defer server.Close()
	client := newTestClient(t, server.URL, "token", server.Client())
	noteID, found, err := client.FindNote(context.Background(), testIdentity(server.URL), marker)
	if err != nil || !found || noteID != 1001 {
		t.Fatalf("FindNote() = %d, %t, %v", noteID, found, err)
	}
}

func TestResolveProjectAndListOpenMergeRequests(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v4/projects/group/project":
			if !strings.Contains(r.URL.EscapedPath(), "group%2Fproject") {
				t.Errorf("escaped project path = %q", r.URL.EscapedPath())
			}
			writeJSON(t, w, projectResponse{ID: 42, PathWithNamespace: "group/project"})
		case "/api/v4/projects/42/merge_requests":
			if r.URL.Query().Get("state") != "opened" || r.URL.Query().Get("per_page") != "100" {
				t.Errorf("merge request query = %q", r.URL.RawQuery)
			}
			page := r.URL.Query().Get("page")
			if page == "1" {
				w.Header().Set("X-Next-Page", "2")
				writeJSON(t, w, []reconciliationMergeRequestResponse{
					{IID: 7, ProjectID: 42, State: "opened", SHA: strings.ToUpper(testHead)},
					{IID: 8, ProjectID: 42, State: "opened", SHA: testHead, Draft: true},
				})
				return
			}
			writeJSON(t, w, []reconciliationMergeRequestResponse{
				{IID: 9, ProjectID: 42, State: "opened", SHA: testHead, WorkInProgress: true},
			})
		default:
			t.Fatal("unexpected request: " + r.URL.String())
		}
	}))
	defer server.Close()
	client := newTestClient(t, server.URL, "token", server.Client())

	projectID, err := client.ResolveProject(context.Background(), "group/project")
	if err != nil || projectID != 42 {
		t.Fatalf("ResolveProject() = %d, %v", projectID, err)
	}
	first, next, err := client.ListOpenMergeRequests(context.Background(), projectID, 1)
	if err != nil || next != 2 || len(first) != 2 {
		t.Fatalf("ListOpenMergeRequests(first) = %+v, %d, %v", first, next, err)
	}
	if first[0].HeadSHA != testHead || first[1].Draft != true {
		t.Fatalf("first page = %+v", first)
	}
	second, next, err := client.ListOpenMergeRequests(context.Background(), projectID, next)
	if err != nil || next != 0 || len(second) != 1 || !second[0].WorkInProgress {
		t.Fatalf("ListOpenMergeRequests(second) = %+v, %d, %v", second, next, err)
	}
}

func TestListOpenMergeRequestsRejectsMalformedEntry(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, []reconciliationMergeRequestResponse{{
			IID: 7, ProjectID: 99, State: "opened", SHA: testHead,
		}})
	}))
	defer server.Close()
	client := newTestClient(t, server.URL, "token", server.Client())
	_, _, err := client.ListOpenMergeRequests(context.Background(), 42, 1)
	assertFailure(t, err, "malformed_gitlab_response", false, false)
}

func TestRetryAfterGateAppliesAcrossClientOperations(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		writeJSON(t, w, projectResponse{ID: 42, PathWithNamespace: "group/project"})
	}))
	defer server.Close()
	client := newTestClient(t, server.URL, "token", server.Client())
	start := time.Unix(1_700_000_000, 0)
	var nowNanos atomic.Int64
	nowNanos.Store(start.UnixNano())
	client.now = func() time.Time { return time.Unix(0, nowNanos.Load()) }
	var waited atomic.Int64
	client.after = func(delay time.Duration) <-chan time.Time {
		waited.Store(int64(delay))
		nowNanos.Store(start.Add(delay).UnixNano())
		ready := make(chan time.Time, 1)
		ready <- start.Add(delay)
		return ready
	}

	err := client.CheckCurrent(context.Background(), testIdentity(server.URL))
	assertFailure(t, err, "gitlab_rate_limited", true, false)
	projectID, err := client.ResolveProject(context.Background(), "group/project")
	if err != nil || projectID != 42 {
		t.Fatalf("ResolveProject() = %d, %v", projectID, err)
	}
	if time.Duration(waited.Load()) != missingRateLimitDelay {
		t.Fatalf("shared gate delay = %v, want %v", time.Duration(waited.Load()), missingRateLimitDelay)
	}
}

func TestAuthorizationAndMergeRequestState(t *testing.T) {
	tests := []struct {
		name        string
		projectPath string
		state       string
		head        string
		category    string
		obsolete    bool
	}{
		{name: "renamed project", projectPath: "group/renamed", state: "opened", head: testHead, category: "repository_unauthorized"},
		{name: "closed", projectPath: "group/project", state: "closed", head: testHead, category: "merge_request_not_open", obsolete: true},
		{name: "unknown state", projectPath: "group/project", state: "future", head: testHead, category: "unknown_merge_request_state"},
		{name: "changed head", projectPath: "group/project", state: "opened", head: strings.Repeat("1", 40), category: "merge_request_head_changed", obsolete: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v4/projects/42":
					writeJSON(t, w, projectResponse{ID: 42, PathWithNamespace: test.projectPath})
				case "/api/v4/projects/42/merge_requests/7":
					writeMergeRequest(t, w, test.state, test.head)
				default:
					t.Fatal("unexpected request: " + r.URL.Path)
				}
			}))
			defer server.Close()
			client, err := New(server.URL, "token", []string{"group/project", "group/related"}, true, server.Client())
			if err != nil {
				t.Fatal(err)
			}
			err = client.CheckCurrent(context.Background(), testIdentity(server.URL))
			assertFailure(t, err, test.category, false, test.obsolete)
		})
	}
}

func TestRejectsRedirectWithoutForwardingToken(t *testing.T) {
	var targetCalls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetCalls.Add(1)
		if r.Header.Get("PRIVATE-TOKEN") != "" {
			t.Error("redirect target received token")
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer server.Close()

	client := newTestClient(t, server.URL, "token", server.Client())
	err := client.CheckCurrent(context.Background(), testIdentity(server.URL))
	assertFailure(t, err, "gitlab_redirect_rejected", false, false)
	if targetCalls.Load() != 0 {
		t.Fatalf("redirect target calls = %d", targetCalls.Load())
	}
}

func TestRetryAfter(t *testing.T) {
	tests := []struct {
		name       string
		header     string
		category   string
		retryable  bool
		retryAfter time.Duration
	}{
		{name: "longer than local cap", header: "360", category: "gitlab_rate_limited", retryable: true, retryAfter: 6 * time.Minute},
		{name: "malformed uses local", header: "later", category: "gitlab_rate_limited", retryable: true},
		{name: "exceeds supported", header: "90000", category: "retry_after_exceeds_limit"},
		{name: "numeric overflow", header: "999999999999999999999999", category: "retry_after_exceeds_limit"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", test.header)
				w.WriteHeader(http.StatusTooManyRequests)
			}))
			defer server.Close()
			client := newTestClient(t, server.URL, "token", server.Client())
			err := client.CheckCurrent(context.Background(), testIdentity(server.URL))
			failureError := assertFailure(t, err, test.category, test.retryable, false)
			if failureError.RetryAfter != test.retryAfter {
				t.Fatalf("RetryAfter = %v, want %v", failureError.RetryAfter, test.retryAfter)
			}
		})
	}
}

func TestRequestTimeoutIsRetryable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "360")
		w.WriteHeader(http.StatusRequestTimeout)
	}))
	defer server.Close()
	client := newTestClient(t, server.URL, "token", server.Client())
	err := client.CheckCurrent(context.Background(), testIdentity(server.URL))
	failureError := assertFailure(t, err, "gitlab_request_timeout", true, false)
	if failureError.RetryAfter != 6*time.Minute {
		t.Fatalf("RetryAfter = %v", failureError.RetryAfter)
	}
}

func TestResponseAndInputLimitsFailClosed(t *testing.T) {
	t.Run("metadata response", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, strings.Repeat("x", metadataResponseLimit+1))
		}))
		defer server.Close()
		client := newTestClient(t, server.URL, "token", server.Client())
		err := client.CheckCurrent(context.Background(), testIdentity(server.URL))
		assertFailure(t, err, "gitlab_response_limit_exceeded", false, false)
	})

	t.Run("diff content", func(t *testing.T) {
		server := reviewServer(t, func(w http.ResponseWriter, r *http.Request) {
			writeJSON(t, w, []diffResponse{{OldPath: "a", NewPath: "a", Diff: strings.Repeat("x", maxDiffContentBytes+1)}})
		})
		defer server.Close()
		client := newTestClient(t, server.URL, "token", server.Client())
		_, err := client.LoadReview(context.Background(), testIdentity(server.URL))
		assertFailure(t, err, "merge_request_diff_limit_exceeded", false, false)
	})

	t.Run("changed files", func(t *testing.T) {
		server := reviewServer(t, func(w http.ResponseWriter, r *http.Request) {
			files := make([]diffResponse, maxChangedFiles+1)
			for index := range files {
				path := fmt.Sprintf("file-%d", index)
				files[index] = diffResponse{OldPath: path, NewPath: path}
			}
			writeJSON(t, w, files)
		})
		defer server.Close()
		client := newTestClient(t, server.URL, "token", server.Client())
		_, err := client.LoadReview(context.Background(), testIdentity(server.URL))
		assertFailure(t, err, "merge_request_file_limit_exceeded", false, false)
	})

	t.Run("note body", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Fatal("oversized note reached GitLab")
		}))
		defer server.Close()
		client := newTestClient(t, server.URL, "token", server.Client())
		_, err := client.PostNote(context.Background(), testIdentity(server.URL), strings.Repeat("x", maxNoteBodyBytes+1))
		assertFailure(t, err, "note_body_limit_exceeded", false, false)
	})

	client, err := New("http://gitlab.internal", "token", []string{"group/project"}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if client.httpClient.Timeout != requestTimeout {
		t.Fatalf("HTTP timeout = %v, want %v", client.httpClient.Timeout, requestTimeout)
	}
}

func TestDiffAndNoteBoundsFailClosed(t *testing.T) {
	t.Run("incomplete diff", func(t *testing.T) {
		server := reviewServer(t, func(w http.ResponseWriter, r *http.Request) {
			writeJSON(t, w, []diffResponse{{OldPath: "a", NewPath: "a", Diff: "diff", Collapsed: true}})
		})
		defer server.Close()
		client := newTestClient(t, server.URL, "token", server.Client())
		_, err := client.LoadReview(context.Background(), testIdentity(server.URL))
		assertFailure(t, err, "incomplete_merge_request_diff", false, false)
	})

	t.Run("diff pages", func(t *testing.T) {
		server := reviewServer(t, func(w http.ResponseWriter, r *http.Request) {
			page, _ := strconv.Atoi(r.URL.Query().Get("page"))
			files := make([]diffResponse, diffsPerPage)
			for index := range files {
				path := fmt.Sprintf("p%d-%d", page, index)
				files[index] = diffResponse{OldPath: path, NewPath: path, Diff: "x"}
			}
			w.Header().Set("X-Next-Page", strconv.Itoa(page+1))
			writeJSON(t, w, files)
		})
		defer server.Close()
		client := newTestClient(t, server.URL, "token", server.Client())
		_, err := client.LoadReview(context.Background(), testIdentity(server.URL))
		assertFailure(t, err, "merge_request_diff_page_limit_exceeded", false, false)
	})

	t.Run("note pages", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v4/user" {
				writeJSON(t, w, userResponse{ID: 77})
				return
			}
			notes := make([]noteResponse, notesPerPage)
			for index := range notes {
				notes[index] = testNote(int64(index+1), "other", 77)
			}
			page, _ := strconv.Atoi(r.URL.Query().Get("page"))
			w.Header().Set("X-Next-Page", strconv.Itoa(page+1))
			writeJSON(t, w, notes)
		}))
		defer server.Close()
		client := newTestClient(t, server.URL, "token", server.Client())
		_, _, err := client.FindNote(context.Background(), testIdentity(server.URL), "marker")
		assertFailure(t, err, "note_search_limit_exceeded", false, false)
	})
}

func reviewServer(t *testing.T, diffs http.HandlerFunc) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v4/projects/42":
			writeJSON(t, w, projectResponse{ID: 42, PathWithNamespace: "group/project"})
		case "/api/v4/projects/42/merge_requests/7":
			writeMergeRequest(t, w, "opened", testHead)
		case "/api/v4/projects/42/merge_requests/7/diffs":
			diffs(w, r)
		default:
			t.Fatal("unexpected request: " + r.URL.Path)
		}
	}))
}

func newTestClient(t *testing.T, baseURL, token string, httpClient *http.Client) *Client {
	t.Helper()
	client, err := New(baseURL, token, []string{"group/project"}, false, httpClient)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func testIdentity(baseURL string) Identity {
	return Identity{GitLabInstance: baseURL, ProjectID: 42, MergeRequestIID: 7, HeadSHA: testHead}
}

func stringPointer(value string) *string {
	return &value
}

func testNote(id int64, body string, authorID int64) noteResponse {
	note := noteResponse{ID: id, Body: body}
	note.Author.ID = authorID
	return note
}

func writeMergeRequest(t *testing.T, w http.ResponseWriter, state, head string) {
	t.Helper()
	response := mergeRequestResponse{
		ID: 8, IID: 7, ProjectID: 42, State: state, Title: "Review title",
		Description: "Description", SourceBranch: "feature", TargetBranch: "main",
	}
	response.DiffRefs.HeadSHA = head
	writeJSON(t, w, response)
}

func writeJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Fatal(err)
	}
}

func assertFailure(t *testing.T, err error, category string, retryable, obsolete bool) *failure.Error {
	t.Helper()
	var failureError *failure.Error
	if !errors.As(err, &failureError) {
		t.Fatalf("error = %v, want failure.Error", err)
	}
	if failureError.Category != category || failureError.Retryable != retryable || failureError.Obsolete != obsolete {
		t.Fatalf("failure = %+v", failureError)
	}
	return failureError
}
