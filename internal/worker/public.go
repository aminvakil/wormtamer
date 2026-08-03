package worker

import (
	"context"
	"encoding/json"
	"time"

	"github.com/aminvakil/wormtamer/internal/failure"
	"github.com/aminvakil/wormtamer/internal/publicsource"
	"github.com/aminvakil/wormtamer/internal/repository"
)

type reviewPublicSources struct {
	broker       publicsource.Broker
	workspaces   RepositoryWorkspaces
	repositories map[string]struct{}
	open         map[string]publicWorkspace
}

type publicWorkspace struct {
	workspace   repository.Workspace
	retrievedAt time.Time
}

func newReviewPublicSources(broker publicsource.Broker, workspaces RepositoryWorkspaces, repositories []string) *reviewPublicSources {
	allowed := make(map[string]struct{}, len(repositories))
	for _, repositoryURL := range repositories {
		allowed[repositoryURL] = struct{}{}
	}
	return &reviewPublicSources{
		broker: broker, workspaces: workspaces, repositories: allowed,
		open: make(map[string]publicWorkspace),
	}
}

func (p *reviewPublicSources) Call(ctx context.Context, name string, arguments map[string]any) (map[string]any, error) {
	if p.broker == nil {
		return nil, failure.Failed("public_source_unavailable")
	}
	if name == publicsource.ToolFetchURL {
		if len(arguments) != 1 {
			return nil, failure.Failed("public_source_tool_arguments_invalid")
		}
		rawURL, ok := arguments["url"].(string)
		if !ok || rawURL == "" {
			return nil, failure.Failed("public_source_tool_arguments_invalid")
		}
		result, err := p.broker.Fetch(ctx, rawURL)
		if err != nil {
			return nil, err
		}
		toolResult := result.ToolResult()
		if err := publicsource.ValidateToolResult(toolResult); err != nil {
			return nil, err
		}
		return toolResult, nil
	}
	if name != publicsource.ToolListFiles && name != publicsource.ToolReadFile {
		return nil, failure.Failed("public_source_tool_undeclared")
	}
	requested, ok := arguments["repository"].(string)
	if !ok || requested == "" {
		return nil, failure.Failed("public_source_tool_arguments_invalid")
	}
	if _, allowed := p.repositories[requested]; !allowed {
		return nil, failure.Failed("public_repository_unavailable")
	}
	opened, exists := p.open[requested]
	if !exists {
		if len(p.open) >= publicsource.MaxToolCalls {
			return nil, failure.Retry("public_repository_limit_exceeded", 0)
		}
		snapshot, err := p.broker.LoadGitHubRepository(ctx, requested)
		if err != nil {
			return nil, err
		}
		workspace, err := p.workspaces.Create(ctx, snapshot.Revision, snapshot.Archive)
		if err != nil {
			return nil, err
		}
		opened = publicWorkspace{workspace: workspace, retrievedAt: snapshot.RetrievedAt}
		p.open[requested] = opened
	}
	workspaceArguments := make(map[string]any, len(arguments)-1)
	for key, value := range arguments {
		if key != "repository" {
			workspaceArguments[key] = value
		}
	}
	workspaceTool := repository.ToolReadFile
	if name == publicsource.ToolListFiles {
		workspaceTool = repository.ToolListFiles
	}
	result, err := opened.workspace.Call(ctx, workspaceTool, workspaceArguments)
	if err != nil {
		return nil, err
	}
	result["authority"] = "untrusted_public"
	result["repository"] = requested
	result["retrieved_at"] = opened.retrievedAt.UTC().Format(time.RFC3339Nano)
	encoded, err := json.Marshal(result)
	if err != nil {
		return nil, failure.Failed("public_source_tool_output_invalid")
	}
	if len(encoded) > publicsource.MaxToolResponse {
		return nil, failure.Failed("public_source_tool_output_limit_exceeded")
	}
	return result, nil
}

func (p *reviewPublicSources) Close() error {
	var firstError error
	for _, opened := range p.open {
		if err := opened.workspace.Close(); err != nil && firstError == nil {
			firstError = err
		}
	}
	return firstError
}
