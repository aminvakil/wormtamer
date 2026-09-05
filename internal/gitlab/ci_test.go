package gitlab

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCheckCIValidatesSelectedPipeline(t *testing.T) {
	generated := strings.Repeat("b", 40)
	pipeline := func(sha, status string) string {
		return fmt.Sprintf(`{"id":91,"project_id":42,"sha":%q,"status":%q}`, sha, status)
	}
	associated := fmt.Sprintf(`{"id":"gid://gitlab/Ci::Pipeline/91","sha":%q}`, generated)
	for _, test := range []struct {
		name        string
		pipeline    string
		graphqlHead string
		graphqlErr  bool
		lookupFails bool
		projectPath string
		state       string
		head        string
		want        string
		category    string
		retryable   bool
		obsolete    bool
	}{
		{name: "confirmed absence", pipeline: "null", want: CINoPipeline},
		{name: "branch success", pipeline: pipeline(testHead, "success"), want: "success"},
		{name: "failed CI is not an error", pipeline: pipeline(testHead, "failed"), want: "failed"},
		{name: "preparing", pipeline: pipeline(testHead, "preparing"), want: "preparing"},
		{name: "canceling", pipeline: pipeline(testHead, "canceling"), want: "canceling"},
		{name: "waiting for callback", pipeline: pipeline(testHead, "waiting_for_callback"), want: "waiting_for_callback"},
		{name: "skipped is not success", pipeline: pipeline(testHead, "skipped"), want: "skipped"},
		{name: "merged results", pipeline: pipeline(generated, "success"), graphqlHead: associated, want: "success"},
		{name: "stale success", pipeline: pipeline(generated, "success"), graphqlHead: "null", category: "merge_request_pipeline_identity_unresolved", retryable: true},
		{name: "selection changed during confirmation", pipeline: pipeline(generated, "success"), graphqlHead: strings.Replace(associated, "/91", "/92", 1), category: "merge_request_pipeline_identity_unresolved", retryable: true},
		{name: "GraphQL partial error", pipeline: pipeline(generated, "success"), graphqlHead: associated, graphqlErr: true, category: "gitlab_graphql_error"},
		{name: "pipeline field not exposed", category: "malformed_gitlab_response"},
		{name: "malformed pipeline", pipeline: `{}`, category: "malformed_gitlab_response"},
		{name: "unknown status", pipeline: pipeline(testHead, "future"), category: "unknown_merge_request_pipeline_status"},
		{name: "failed lookup is not absence", lookupFails: true, category: "gitlab_server_failure", retryable: true},
		{name: "authorization before CI", projectPath: "group/renamed", category: "repository_unauthorized"},
		{name: "closed before failed CI", state: "closed", pipeline: pipeline(testHead, "failed"), category: "merge_request_not_open", obsolete: true},
		{name: "superseded before failed CI", head: generated, pipeline: pipeline(testHead, "failed"), category: "merge_request_head_changed", obsolete: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("PRIVATE-TOKEN") != "token" {
					t.Error("missing broker credentials")
				}
				switch r.URL.Path {
				case "/gitlab/api/v4/projects/42":
					path := test.projectPath
					if path == "" {
						path = "group/project"
					}
					writeJSON(t, w, projectResponse{ID: 42, PathWithNamespace: path})
				case "/gitlab/api/v4/projects/42/merge_requests/7":
					if test.projectPath != "" {
						t.Error("unauthorized project reached MR lookup")
					}
					if test.lookupFails {
						w.WriteHeader(http.StatusServiceUnavailable)
						return
					}
					mr := mergeRequestResponse{ID: 8, IID: 7, ProjectID: 42, State: test.state, HeadPipeline: json.RawMessage(test.pipeline)}
					if mr.State == "" {
						mr.State = "opened"
					}
					mr.DiffRefs.HeadSHA = test.head
					if mr.DiffRefs.HeadSHA == "" {
						mr.DiffRefs.HeadSHA = testHead
					}
					writeJSON(t, w, mr)
				case "/gitlab/api/graphql":
					var payload struct {
						Variables map[string]string `json:"variables"`
					}
					if r.Method != http.MethodPost || json.NewDecoder(r.Body).Decode(&payload) != nil ||
						payload.Variables["project"] != "group/project" || payload.Variables["iid"] != "7" {
						t.Error("invalid GraphQL request")
					}
					errors := "[]"
					if test.graphqlErr {
						errors = `[{"message":"private response detail"}]`
					}
					fmt.Fprintf(w, `{"errors":%s,"data":{"project":{"id":"gid://gitlab/Project/42","mergeRequest":{"id":"gid://gitlab/MergeRequest/8","state":"opened","diffHeadSha":%q,"headPipeline":%s}}}}`, errors, testHead, test.graphqlHead)
				default:
					t.Errorf("unexpected request: %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			client := newTestClient(t, server.URL+"/gitlab", "token", server.Client())
			status, err := client.CheckCI(context.Background(), testIdentity(server.URL+"/gitlab"))
			if test.category != "" {
				assertFailure(t, err, test.category, test.retryable, test.obsolete)
				if status != "" {
					t.Fatalf("failed lookup returned status %q", status)
				}
				return
			}
			if err != nil || status != test.want {
				t.Fatalf("CheckCI() = %q, %v", status, err)
			}
		})
	}
}

func TestCheckCIRefreshesReplacementPipeline(t *testing.T) {
	selected := pipelineResponse{ID: 91, ProjectID: 42, SHA: testHead, Status: "failed"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v4/projects/42":
			writeJSON(t, w, projectResponse{ID: 42, PathWithNamespace: "group/project"})
		case "/api/v4/projects/42/merge_requests/7":
			encoded, _ := json.Marshal(selected)
			mr := mergeRequestResponse{ID: 8, IID: 7, ProjectID: 42, State: "opened", HeadPipeline: encoded}
			mr.DiffRefs.HeadSHA = testHead
			writeJSON(t, w, mr)
		default:
			t.Errorf("unexpected pipeline history lookup: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := newTestClient(t, server.URL, "token", server.Client())
	for _, status := range []string{"failed", "success"} {
		selected.ID++
		selected.Status = status
		got, err := client.CheckCI(context.Background(), testIdentity(server.URL))
		if err != nil || got != status {
			t.Fatalf("CheckCI() = %q, %v", got, err)
		}
	}
}
