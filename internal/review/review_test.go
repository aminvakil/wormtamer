package review

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aminvakil/wormtamer/internal/diagnostics"
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
	readSchema := declarations[0].ParametersJsonSchema.(map[string]any)
	readProperties := readSchema["properties"].(map[string]any)
	if readSchema["type"] != "object" || readSchema["additionalProperties"] != false ||
		!slices.Equal(readSchema["required"].([]string), []string{"path"}) || len(readProperties) != 3 ||
		readProperties["path"].(map[string]any)["type"] != "string" ||
		readProperties["offset"].(map[string]any)["type"] != "integer" || readProperties["offset"].(map[string]any)["minimum"] != 1 ||
		readProperties["limit"].(map[string]any)["type"] != "integer" || readProperties["limit"].(map[string]any)["minimum"] != 1 {
		t.Fatalf("read schema = %+v", readSchema)
	}
	bashSchema := declarations[1].ParametersJsonSchema.(map[string]any)
	bashProperties := bashSchema["properties"].(map[string]any)
	if bashSchema["type"] != "object" || bashSchema["additionalProperties"] != false ||
		!slices.Equal(bashSchema["required"].([]string), []string{"command"}) || len(bashProperties) != 2 ||
		bashProperties["command"].(map[string]any)["type"] != "string" ||
		bashProperties["timeout"].(map[string]any)["type"] != "number" || bashProperties["timeout"].(map[string]any)["exclusiveMinimum"] != 0 {
		t.Fatalf("bash schema = %+v", bashSchema)
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

func TestSystemInstructionDefinesReviewPolicy(t *testing.T) {
	for _, policy := range []string{
		"untrusted evidence", "changed-file diff", "path must exactly match",
		"initial working directory", "Prepared related repositories", "advisory review memory",
	} {
		if !strings.Contains(systemInstruction, policy) {
			t.Fatalf("system instruction lacks policy %q", policy)
		}
	}
}

func TestReviewPromptIdentifiesWorkingDirectoryRelatedRepositoriesAndMemory(t *testing.T) {
	prompt, err := reviewPrompt(testSnapshot())
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"untrusted merge request evidence", "<merge_request_json>",
		`"working_directory":"/reviews/current"`, `"reviewed_head":"0123456789abcdef0123456789abcdef01234567"`,
		`"repository":"group/related"`, `"path":"/reviews/related/group/related"`,
		`"review_memory":{"path":"/reviews/review-memory.json","authority":"untrusted_advisory"}`,
		`"changed_files":[{"old_path":"main.go","new_path":"main.go","diff":"+changed"`,
	} {
		if !strings.Contains(prompt, expected) {
			t.Fatalf("prompt lacks %s: %s", expected, prompt)
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

func TestReviewDiagnosticsRespectLogLevelAndRedactCredentials(t *testing.T) {
	secret := "configured\nsecret"
	for _, test := range []struct {
		name  string
		level slog.Level
		debug bool
	}{
		{name: "info", level: slog.LevelInfo},
		{name: "debug", level: slog.LevelDebug, debug: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var logs bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: test.level}))
			generator := &fakeGenerator{generations: []Generation{
				toolGeneration(&genai.FunctionCall{ID: secret, Name: repository.ToolRead, Args: map[string]any{"path": secret}}),
				textGeneration(`{"summary":"diagnostic response","findings":[]}`),
			}}
			broker := &fakeToolBroker{result: repository.ToolResult{Response: map[string]any{"output": "private tool result"}}}
			reviewer := newGeminiReviewer(generator, "gemini-test", []string{secret}, logger)
			if _, _, err := reviewer.Review(context.Background(), testSnapshot(), broker); err != nil {
				t.Fatal(err)
			}
			output := logs.String()
			privateValues := []string{"+changed", "private tool result", "diagnostic response"}
			if !test.debug {
				for _, value := range privateValues {
					if strings.Contains(output, value) {
						t.Fatalf("info logs contain %q: %s", value, output)
					}
				}
				if strings.Contains(output, `configured\nsecret`) {
					t.Fatalf("info logs contain JSON-escaped credential: %s", output)
				}
				return
			}
			for _, value := range append(privateValues,
				"Gemini review prompt", "Gemini review tool call", "Gemini review tool result", "Gemini review response",
				diagnostics.RedactedSensitiveContent) {
				if !strings.Contains(output, value) {
					t.Fatalf("debug logs lack %q: %s", value, output)
				}
			}
			if strings.Contains(output, `configured\nsecret`) {
				t.Fatalf("debug logs contain JSON-escaped credential: %s", output)
			}
		})
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
