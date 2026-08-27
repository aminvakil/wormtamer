package review

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aminvakil/wormtamer/internal/failure"
	"github.com/aminvakil/wormtamer/internal/gitlab"
	"github.com/aminvakil/wormtamer/internal/repository"
	"google.golang.org/genai"
)

func TestReviewDeclaresExactlyReadAndBash(t *testing.T) {
	declarations := toolDeclarations()
	if len(declarations) != 2 || declarations[0].Name != "read" || declarations[1].Name != "bash" {
		t.Fatalf("tool declarations = %+v", declarations)
	}
	if declarations[0].Description != "Read the contents of a file. Output is truncated to 2000 lines or 50KB (whichever is hit first). Use offset/limit for large files. When you need the full file, continue with offset until complete." {
		t.Fatalf("read description = %q", declarations[0].Description)
	}
	if declarations[1].Description != "Execute a bash command in the current working directory. Returns stdout and stderr. Output is truncated to last 2000 lines or 50KB (whichever is hit first). If truncated, full output is saved to a temp file. Optionally provide a timeout in seconds." {
		t.Fatalf("bash description = %q", declarations[1].Description)
	}
	for _, declaration := range declarations {
		schema := declaration.ParametersJsonSchema.(map[string]any)
		if schema["additionalProperties"] != false {
			t.Fatalf("%s schema allows unknown arguments", declaration.Name)
		}
	}
	config := generationConfig("default", true)
	if len(config.Tools) != 1 || len(config.Tools[0].FunctionDeclarations) != 2 ||
		config.ToolConfig.FunctionCallingConfig.Mode != genai.FunctionCallingConfigModeAuto {
		t.Fatalf("ordinary generation config = %+v", config)
	}
	final := generationConfig("default", false)
	if len(final.Tools) != 0 || final.ToolConfig.FunctionCallingConfig.Mode != genai.FunctionCallingConfigModeNone {
		t.Fatalf("final-only config = %+v", final)
	}
}

func TestSystemInstructionContainsExactMinimalPiGuidance(t *testing.T) {
	fragment := "Available tools:\n- read: Read file contents\n- bash: Execute bash commands (ls, grep, find, etc.)\n\nGuidelines:\n- Use bash for file operations like ls, rg, find\n- Use read to examine files instead of cat or sed."
	if !strings.Contains(systemInstruction, fragment) {
		t.Fatalf("system instruction lacks exact Pi fragment:\n%s", systemInstruction)
	}
	for _, obsolete := range []string{
		"list_repository_files", "read_repository_file", "search_repository", "search_review_memory",
		"fetch_public_url", "list_public_repository_files", "read_public_repository_file",
		"per-category", "combined tool-call limits", "git branch", "git show", "git diff", "git log", "git switch",
	} {
		if strings.Contains(systemInstruction, obsolete) {
			t.Fatalf("system instruction contains obsolete/tutorial text %q", obsolete)
		}
	}
}

func TestReviewPromptIdentifiesWorkingDirectoryRelatedRepositoriesAndMemory(t *testing.T) {
	prompt, err := reviewPrompt(testSnapshot())
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		`"working_directory":"/reviews/current"`, `"reviewed_head":"0123456789abcdef0123456789abcdef01234567"`,
		`"repository":"group/related"`, `"path":"/reviews/related/group/related"`,
		`"review_memory":{"path":"/reviews/review-memory.json","authority":"untrusted_advisory"}`,
	} {
		if !strings.Contains(prompt, expected) {
			t.Fatalf("prompt lacks %s: %s", expected, prompt)
		}
	}
	for _, obsolete := range []string{"resource_limits", "public_sources", "allowed_domains", "tool_calls"} {
		if strings.Contains(prompt, obsolete) {
			t.Fatalf("prompt contains %q", obsolete)
		}
	}
}

func TestGeminiReviewerDispatchesSameTurnCallsInOrder(t *testing.T) {
	generator := &fakeGenerator{generations: []Generation{
		toolGeneration(
			&genai.FunctionCall{ID: "one", Name: repository.ToolRead, Args: map[string]any{"path": "a.go"}},
			&genai.FunctionCall{ID: "two", Name: repository.ToolBash, Args: map[string]any{"command": "pwd"}},
		),
		textGeneration(`{"summary":"ok","findings":[]}`),
	}}
	broker := &fakeToolBroker{callFn: func(call int, name string, _ map[string]any) (repository.ToolResult, error) {
		return repository.ToolResult{Response: map[string]any{"output": name + "-result"}}, nil
	}}
	reviewer := NewGeminiReviewerWithGenerator(generator, "gemini-test", nil)
	result, _, err := reviewer.Review(context.Background(), testSnapshot(), broker)
	if err != nil || result.Summary != "ok" || !slices.Equal(broker.names, []string{"read", "bash"}) {
		t.Fatalf("Review() = %+v, %v; calls=%v", result, err, broker.names)
	}
	responses := functionResponses(generator.requests[1].contents)
	if len(responses) != 2 || responses[0].ID != "one" || responses[1].ID != "two" ||
		responses[0].Response["output"] != "read-result" || responses[1].Response["output"] != "bash-result" {
		t.Fatalf("function responses = %+v", responses)
	}
}

func TestFunctionResponseAllowanceStopsBatchAndForcesFinalOnly(t *testing.T) {
	calls := []*genai.FunctionCall{
		{ID: "one", Name: repository.ToolRead, Args: map[string]any{"path": "one"}},
		{ID: "two", Name: repository.ToolRead, Args: map[string]any{"path": "two"}},
		{ID: "three", Name: repository.ToolBash, Args: map[string]any{"command": "echo three"}},
	}
	generator := &fakeGenerator{generations: []Generation{toolGeneration(calls...), textGeneration(`{"summary":"bounded","findings":[]}`)}}
	broker := &fakeToolBroker{result: repository.ToolResult{Response: map[string]any{"output": strings.Repeat("x", 9<<20)}}}
	result, _, err := NewGeminiReviewerWithGenerator(generator, "gemini-test", nil).Review(context.Background(), testSnapshot(), broker)
	if err != nil || result.Summary != "bounded" {
		t.Fatalf("Review() = %+v, %v", result, err)
	}
	if broker.calls != 2 {
		t.Fatalf("dispatched calls = %d, want 2", broker.calls)
	}
	responses := functionResponses(generator.requests[1].contents)
	if len(responses) != 3 || responses[0].Response["output"] == nil ||
		responses[1].Response["error"] != "tool_result_limit_exceeded" || responses[2].Response["error"] != "tool_result_limit_exceeded" {
		t.Fatalf("bounded responses = %+v", responses)
	}
	if generator.requests[1].config.ToolConfig.FunctionCallingConfig.Mode != genai.FunctionCallingConfigModeNone || len(generator.requests[1].config.Tools) != 0 {
		t.Fatalf("next generation was not final-only: %+v", generator.requests[1].config)
	}
	serialized, _ := json.Marshal(responses[0])
	if len(serialized) > maxToolResultBytes {
		t.Fatalf("admitted evidence exceeded allowance: %d", len(serialized))
	}
}

func TestMoreThanSixteenSmallCallsRemainValid(t *testing.T) {
	calls := make([]*genai.FunctionCall, 20)
	for index := range calls {
		calls[index] = &genai.FunctionCall{ID: string(rune('a' + index)), Name: repository.ToolRead, Args: map[string]any{"path": "small"}}
	}
	generator := &fakeGenerator{generations: []Generation{toolGeneration(calls...), textGeneration(`{"summary":"many","findings":[]}`)}}
	broker := &fakeToolBroker{result: repository.ToolResult{Response: map[string]any{"output": "small"}}}
	result, _, err := NewGeminiReviewerWithGenerator(generator, "gemini-test", nil).Review(context.Background(), testSnapshot(), broker)
	if err != nil || result.Summary != "many" || broker.calls != 20 {
		t.Fatalf("Review() = %+v, %v; calls=%d", result, err, broker.calls)
	}
	if generator.requests[1].config.ToolConfig.FunctionCallingConfig.Mode != genai.FunctionCallingConfigModeAuto {
		t.Fatal("small calls unexpectedly forced final-only mode")
	}
}

func TestReviewerRejectsUndeclaredToolsWithoutDispatch(t *testing.T) {
	generator := &fakeGenerator{generations: []Generation{toolGeneration(&genai.FunctionCall{ID: "old", Name: "read_repository_file"})}}
	broker := &fakeToolBroker{}
	_, _, err := NewGeminiReviewerWithGenerator(generator, "gemini-test", nil).Review(context.Background(), testSnapshot(), broker)
	if err == nil || broker.calls != 0 {
		t.Fatalf("undeclared tool error=%v calls=%d", err, broker.calls)
	}
}

func TestReviewerPreservesStructuredReviewValidation(t *testing.T) {
	generator := &fakeGenerator{generations: []Generation{textGeneration(`{"summary":"bad path","findings":[{"priority":"P2","title":"x","explanation":"x","recommendation":"x","path":"unchanged.go"}]}`)}}
	_, _, err := NewGeminiReviewerWithGenerator(generator, "gemini-test", nil).Review(context.Background(), testSnapshot(), &fakeToolBroker{})
	if err == nil {
		t.Fatal("invalid changed path was accepted")
	}
}

func TestReviewerPropagatesToolCancellation(t *testing.T) {
	generator := &fakeGenerator{generations: []Generation{toolGeneration(&genai.FunctionCall{ID: "bash", Name: repository.ToolBash, Args: map[string]any{"command": "sleep"}})}}
	ctx, cancel := context.WithCancel(context.Background())
	broker := &fakeToolBroker{callFn: func(_ int, _ string, _ map[string]any) (repository.ToolResult, error) {
		cancel()
		return repository.ToolResult{}, context.Canceled
	}}
	_, _, err := NewGeminiReviewerWithGenerator(generator, "gemini-test", nil).Review(ctx, testSnapshot(), broker)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v", err)
	}
}

func TestReviewerClassifiesInternalDeadline(t *testing.T) {
	tests := []struct {
		name      string
		generator Generator
		tools     repository.ToolBroker
	}{
		{name: "generation", generator: deadlineGenerator{}, tools: &fakeToolBroker{}},
		{
			name: "tool",
			generator: &fakeGenerator{generations: []Generation{toolGeneration(
				&genai.FunctionCall{ID: "bash", Name: repository.ToolBash, Args: map[string]any{"command": "sleep"}},
			)}},
			tools: deadlineToolBroker{},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			reviewer := NewGeminiReviewerWithGenerator(test.generator, "gemini-test", nil)
			reviewer.requestTimeout = 10 * time.Millisecond
			_, _, err := reviewer.Review(context.Background(), testSnapshot(), test.tools)
			var failureError *failure.Error
			if !errors.As(err, &failureError) || failureError.Category != "review_timeout" || !failureError.Retryable {
				t.Fatalf("deadline error = %v", err)
			}
		})
	}
}

func testSnapshot() gitlab.Snapshot {
	return gitlab.Snapshot{
		Identity: gitlab.Identity{
			GitLabInstance: "https://gitlab.example", ProjectID: 42, MergeRequestIID: 7,
			HeadSHA: "0123456789abcdef0123456789abcdef01234567",
		},
		ProjectPath: "group/project", WorkingDirectory: "/reviews/current", ReviewMemoryPath: "/reviews/review-memory.json",
		PreparedRepositories: []gitlab.PreparedRepository{{
			Repository: "group/related", Path: "/reviews/related/group/related", InitialRevision: strings.Repeat("b", 40),
		}},
		Title: "Change", SourceBranch: "feature", TargetBranch: "main",
		Files: []gitlab.ChangedFile{{OldPath: "main.go", NewPath: "main.go", Diff: "+changed"}},
	}
}

func toolGeneration(calls ...*genai.FunctionCall) Generation {
	parts := make([]*genai.Part, len(calls))
	for index, call := range calls {
		parts[index] = &genai.Part{FunctionCall: call}
	}
	return Generation{FinishReason: genai.FinishReasonStop, Content: &genai.Content{Role: genai.RoleModel, Parts: parts}}
}

func textGeneration(text string) Generation {
	return Generation{FinishReason: genai.FinishReasonStop, Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: text}}}}
}

type generationRequest struct {
	contents []*genai.Content
	config   *genai.GenerateContentConfig
}

type fakeGenerator struct {
	generations []Generation
	requests    []generationRequest
}

func (g *fakeGenerator) Generate(_ context.Context, _ string, contents []*genai.Content, config *genai.GenerateContentConfig) (Generation, error) {
	g.requests = append(g.requests, generationRequest{contents: append([]*genai.Content(nil), contents...), config: config})
	if len(g.generations) == 0 {
		return Generation{}, errors.New("no generation")
	}
	generation := g.generations[0]
	g.generations = g.generations[1:]
	return generation, nil
}

type fakeToolBroker struct {
	result repository.ToolResult
	callFn func(int, string, map[string]any) (repository.ToolResult, error)
	calls  int
	names  []string
}

func (b *fakeToolBroker) Call(_ context.Context, name string, arguments map[string]any) (repository.ToolResult, error) {
	b.calls++
	b.names = append(b.names, name)
	if b.callFn != nil {
		return b.callFn(b.calls, name, arguments)
	}
	return b.result, nil
}

type deadlineGenerator struct{}

func (deadlineGenerator) Generate(ctx context.Context, _ string, _ []*genai.Content, _ *genai.GenerateContentConfig) (Generation, error) {
	<-ctx.Done()
	return Generation{}, ctx.Err()
}

type deadlineToolBroker struct{}

func (deadlineToolBroker) Call(ctx context.Context, _ string, _ map[string]any) (repository.ToolResult, error) {
	<-ctx.Done()
	return repository.ToolResult{}, ctx.Err()
}

func functionResponses(contents []*genai.Content) []*genai.FunctionResponse {
	var responses []*genai.FunctionResponse
	for _, content := range contents {
		if content == nil || content.Role != genai.RoleUser {
			continue
		}
		for _, part := range content.Parts {
			if part != nil && part.FunctionResponse != nil {
				responses = append(responses, part.FunctionResponse)
			}
		}
	}
	return responses
}
