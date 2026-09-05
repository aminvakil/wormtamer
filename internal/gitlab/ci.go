package gitlab

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/aminvakil/wormtamer/internal/failure"
)

const CINoPipeline = "none"

type pipelineResponse struct {
	ID        int64  `json:"id"`
	ProjectID int64  `json:"project_id"`
	SHA       string `json:"sha"`
	Status    string `json:"status"`
}

// CheckCI validates current authorization and revision before evaluating GitLab's
// selected MR pipeline. Only an explicit REST null confirms no pipeline.
func (c *Client) CheckCI(ctx context.Context, identity Identity) (string, error) {
	if err := c.validateIdentity(identity); err != nil {
		return "", err
	}
	projectPath, err := c.checkProject(ctx, identity.ProjectID)
	if err != nil {
		return "", err
	}
	mr, err := c.getMergeRequest(ctx, identity)
	if err != nil {
		return "", err
	}
	if err := validateMergeRequest(identity, mr); err != nil {
		return "", err
	}
	if bytes.Equal(bytes.TrimSpace(mr.HeadPipeline), []byte("null")) {
		return CINoPipeline, nil
	}
	var pipeline pipelineResponse
	if err := json.Unmarshal(mr.HeadPipeline, &pipeline); err != nil ||
		pipeline.ID <= 0 || pipeline.ProjectID <= 0 || !headSHAPattern.MatchString(pipeline.SHA) {
		return "", failure.Failed("malformed_gitlab_response")
	}
	if !strings.EqualFold(pipeline.SHA, identity.HeadSHA) {
		if err := c.confirmPipelineSourceHead(ctx, identity, projectPath, mr.ID, pipeline); err != nil {
			return "", err
		}
	}
	switch pipeline.Status {
	case "success", "created", "waiting_for_resource", "preparing", "pending", "running",
		"failed", "canceling", "canceled", "skipped", "manual", "scheduled", "waiting_for_callback":
		return pipeline.Status, nil
	default:
		return "", failure.Failed("unknown_merge_request_pipeline_status")
	}
}

// REST does not expose source_sha. GraphQL headPipeline resolves GitLab's
// diff_head_pipeline, which checks sha OR source_sha against the current diff.
// See docs/agents/reliability.md#pipeline-association for the API evidence.
func (c *Client) confirmPipelineSourceHead(ctx context.Context, identity Identity, projectPath string, mrID int64, pipeline pipelineResponse) error {
	const query = `query($project: ID!, $iid: String!) {
  project(fullPath: $project) {
    id
    mergeRequest(iid: $iid) {
      id state diffHeadSha
      headPipeline { id sha }
    }
  }
}`
	payload, err := json.Marshal(struct {
		Query     string            `json:"query"`
		Variables map[string]string `json:"variables"`
	}{Query: query, Variables: map[string]string{
		"project": projectPath, "iid": fmt.Sprint(identity.MergeRequestIID),
	}})
	if err != nil {
		return failure.Failed("gitlab_request_invalid")
	}
	var response struct {
		Errors []json.RawMessage `json:"errors"`
		Data   struct {
			Project struct {
				ID           string `json:"id"`
				MergeRequest struct {
					ID           string `json:"id"`
					State        string `json:"state"`
					DiffHeadSHA  string `json:"diffHeadSha"`
					HeadPipeline *struct {
						ID  string `json:"id"`
						SHA string `json:"sha"`
					} `json:"headPipeline"`
				} `json:"mergeRequest"`
			} `json:"project"`
		} `json:"data"`
	}
	if _, err := c.requestPath(ctx, http.MethodPost, "/api/graphql", nil, payload, metadataResponseLimit, &response); err != nil {
		return err
	}
	if len(response.Errors) > 0 {
		return failure.Failed("gitlab_graphql_error")
	}
	project := response.Data.Project
	mr := project.MergeRequest
	if project.ID != fmt.Sprintf("gid://gitlab/Project/%d", identity.ProjectID) ||
		mr.ID != fmt.Sprintf("gid://gitlab/MergeRequest/%d", mrID) || !headSHAPattern.MatchString(mr.DiffHeadSHA) {
		return failure.Failed("malformed_gitlab_response")
	}
	current := mergeRequestResponse{ID: mrID, ProjectID: identity.ProjectID, IID: identity.MergeRequestIID, State: mr.State}
	current.DiffRefs.HeadSHA = mr.DiffHeadSHA
	if err := validateMergeRequest(identity, current); err != nil {
		return err
	}
	if mr.HeadPipeline == nil || mr.HeadPipeline.ID != fmt.Sprintf("gid://gitlab/Ci::Pipeline/%d", pipeline.ID) ||
		!strings.EqualFold(mr.HeadPipeline.SHA, pipeline.SHA) {
		return failure.Retry("merge_request_pipeline_identity_unresolved", 0)
	}
	return nil
}
