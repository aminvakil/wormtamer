package review

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
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

func TestReviewDeclaresReadBashAndCodemode(t *testing.T) {
	config := generationConfig("default", true)
	if len(config.Tools) != 1 || config.ToolConfig.FunctionCallingConfig.Mode != genai.FunctionCallingConfigModeAuto {
		t.Fatalf("ordinary generation config = %+v", config)
	}
	declarations := config.Tools[0].FunctionDeclarations
	if len(declarations) != 3 || declarations[0].Name != "read" || declarations[1].Name != "bash" || declarations[2].Name != "codemode" {
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
	codeSchema := declarations[2].ParametersJsonSchema.(map[string]any)
	codeProperties := codeSchema["properties"].(map[string]any)
	if codeSchema["additionalProperties"] != false || !slices.Equal(codeSchema["required"].([]string), []string{"code"}) ||
		len(codeProperties) != 1 || codeProperties["code"].(map[string]any)["type"] != "string" {
		t.Fatalf("codemode schema = %+v", codeSchema)
	}
}

func TestReviewPromptIdentifiesWorkingDirectoryRelatedRepositoriesAndMemory(t *testing.T) {
	prompt, err := reviewPrompt(testSnapshot())
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"<merge_request_json>",
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
	reviewer := newGeminiReviewer(generator, "gemini-test", nil, nil)
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

func TestReviewLogsSDKUsageMetadataWhenAvailable(t *testing.T) {
	for _, test := range []struct {
		name       string
		usage      string
		wantPrompt float64
	}{
		{
			name:       "reported",
			usage:      `,"usageMetadata":{"promptTokenCount":321,"candidatesTokenCount":12,"thoughtsTokenCount":34}`,
			wantPrompt: 321,
		},
		{
			name:  "zero",
			usage: `,"usageMetadata":{"promptTokenCount":0,"candidatesTokenCount":12,"thoughtsTokenCount":34}`,
		},
		{name: "absent"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"{\"summary\":\"ok\",\"findings\":[]}"}],"role":"model"},"finishReason":"STOP"}]` + test.usage + "}"))
			}))
			defer server.Close()

			var logs bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&logs, nil))
			reviewer, err := NewGeminiReviewer(context.Background(), "test-key", server.URL, "gemini-test", "default", nil, logger)
			if err != nil {
				t.Fatal(err)
			}
			result, _, err := reviewer.Review(context.Background(), testSnapshot(), nil)
			if err != nil || result.Summary != "ok" {
				t.Fatalf("Review() = %+v, %v", result, err)
			}
			var event map[string]any
			if err := json.Unmarshal(logs.Bytes(), &event); err != nil {
				t.Fatal(err)
			}
			if event["msg"] != "Gemini review generation" {
				t.Fatalf("unexpected event: %v", event)
			}
			for key, want := range map[string]float64{
				"prompt_token_count": test.wantPrompt, "candidates_token_count": 12, "thinking_token_count": 34,
			} {
				got, present := event[key]
				if present != (test.usage != "") || (present && got != want) {
					t.Fatalf("%s = %v (present=%t), want %v (present=%t)", key, got, present, want, test.usage != "")
				}
			}
		})
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
			assertToolDurations(t, output, []string{repository.ToolRead})
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
	result, _, err := newGeminiReviewer(generator, "gemini-test", nil, nil).Review(context.Background(), testSnapshot(), broker)
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
}

func TestMoreThanSixteenSmallCallsRemainValid(t *testing.T) {
	calls := make([]*genai.FunctionCall, 20)
	for index := range calls {
		calls[index] = &genai.FunctionCall{ID: string(rune('a' + index)), Name: repository.ToolRead, Args: map[string]any{"path": "small"}}
	}
	generator := &fakeGenerator{generations: []Generation{toolGeneration(calls...), textGeneration(`{"summary":"many","findings":[]}`)}}
	broker := &fakeToolBroker{result: repository.ToolResult{Response: map[string]any{"output": "small"}}}
	result, _, err := newGeminiReviewer(generator, "gemini-test", nil, nil).Review(context.Background(), testSnapshot(), broker)
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
	_, _, err := newGeminiReviewer(generator, "gemini-test", nil, nil).Review(context.Background(), testSnapshot(), broker)
	if err == nil || broker.calls != 0 {
		t.Fatalf("undeclared tool error=%v calls=%d", err, broker.calls)
	}
}

func TestReviewerPreservesStructuredReviewValidation(t *testing.T) {
	generator := &fakeGenerator{generations: []Generation{textGeneration(`{"summary":"bad path","findings":[{"priority":"P2","title":"x","explanation":"x","recommendation":"x","path":"unchanged.go"}]}`)}}
	_, _, err := newGeminiReviewer(generator, "gemini-test", nil, nil).Review(context.Background(), testSnapshot(), &fakeToolBroker{})
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
	_, _, err := newGeminiReviewer(generator, "gemini-test", nil, nil).Review(ctx, testSnapshot(), broker)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v", err)
	}
}

func TestReviewerPropagatesGenerationCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := newGeminiReviewer(deadlineGenerator{}, "gemini-test", nil, nil).Review(ctx, testSnapshot(), &fakeToolBroker{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v", err)
	}
}

func TestReviewerClassifiesGenerationDeadline(t *testing.T) {
	reviewer := newGeminiReviewer(deadlineGenerator{}, "gemini-test", nil, nil)
	reviewer.generationTimeout = 10 * time.Millisecond
	reviewer.reviewTimeout = time.Minute
	_, _, err := reviewer.Review(context.Background(), testSnapshot(), &fakeToolBroker{})
	var failureError *failure.Error
	if !errors.As(err, &failureError) || failureError.Category != "gemini_timeout" || !failureError.Retryable {
		t.Fatalf("generation deadline error = %v", err)
	}
}

func TestReviewerWrapsUpAtReviewDeadline(t *testing.T) {
	for _, duringTool := range []bool{false, true} {
		name := "generation"
		if duringTool {
			name = "tool_batch"
		}
		t.Run(name, func(t *testing.T) {
			calls := []*genai.FunctionCall{{ID: "read", Name: repository.ToolRead, Args: map[string]any{"path": "main.go"}}}
			if duringTool {
				calls = append(calls,
					&genai.FunctionCall{ID: "interrupted", Name: repository.ToolBash, Args: map[string]any{"command": "sleep"}},
					&genai.FunctionCall{ID: "skipped", Name: repository.ToolRead, Args: map[string]any{"path": "other.go"}})
			}
			first := toolGeneration(calls...)
			first.Content.Parts[0].ThoughtSignature = []byte("signature")
			var requests []generationRequest
			generator := generatorFunc(func(ctx context.Context, _ string, contents []*genai.Content, config *genai.GenerateContentConfig) (Generation, error) {
				requests = append(requests, generationRequest{contents: append([]*genai.Content(nil), contents...), config: config})
				if config.ToolConfig.FunctionCallingConfig.Mode == genai.FunctionCallingConfigModeNone {
					deadline, ok := ctx.Deadline()
					if ctx.Err() != nil || !ok || time.Until(deadline) <= 0 || time.Until(deadline) > time.Second || len(config.Tools) != 0 {
						t.Fatalf("wrap-up context/config = %v, %+v", ctx.Err(), config)
					}
					return textGeneration(`{"summary":"Partial assessment.","findings":[{"priority":"P2","title":"Check error","explanation":"An error is ignored.","recommendation":"Handle it.","path":"main.go"}]}`), nil
				}
				if len(requests) == 1 {
					return first, nil
				}
				return deadlineGenerator{}.Generate(ctx, "", contents, config)
			})
			toolCalls := 0
			broker := toolBrokerFunc(func(ctx context.Context, _ string, _ map[string]any) (repository.ToolResult, error) {
				toolCalls++
				if toolCalls == 2 {
					<-ctx.Done()
					return repository.ToolResult{}, ctx.Err()
				}
				return repository.ToolResult{Response: map[string]any{"output": "collected evidence"}}, nil
			})
			reviewer := newGeminiReviewer(generator, "gemini-test", nil, nil)
			reviewer.generationTimeout = time.Second
			reviewer.reviewTimeout = 20 * time.Millisecond
			result, encoded, err := reviewer.Review(context.Background(), testSnapshot(), broker)
			if err != nil || result.Summary != incompleteReviewNotice+"\n\nPartial assessment." || len(result.Findings) != 1 {
				t.Fatalf("Review() = %+v, %v", result, err)
			}
			stored, err := DecodeStored(encoded)
			if err != nil || stored.Summary != result.Summary || len(stored.Findings) != 1 {
				t.Fatalf("stored result = %+v, %v", stored, err)
			}
			wantRequests, wantTools := 3, 1
			if duringTool {
				wantRequests, wantTools = 2, 2
			}
			if len(requests) != wantRequests || toolCalls != wantTools {
				t.Fatalf("generations=%d tools=%d", len(requests), toolCalls)
			}
			contents := requests[len(requests)-1].contents
			if contents[1] != first.Content || string(contents[1].Parts[0].ThoughtSignature) != "signature" {
				t.Fatal("wrap-up lost the original model content")
			}
			responses := functionResponses(contents)
			if len(responses) != len(calls) || responses[0].Response["output"] != "collected evidence" {
				t.Fatalf("wrap-up evidence = %+v", responses)
			}
			for i, response := range responses {
				if response.ID != calls[i].ID || response.Name != calls[i].Name || (i > 0 && response.Response["error"] != "review_time_limit_exceeded") {
					t.Fatalf("response %d = %+v", i, response)
				}
			}
		})
	}
}

func TestReviewerUsesNoticeWhenWrapUpCannotFinish(t *testing.T) {
	for _, name := range []string{"timeout", "server_error", "credentials", "incomplete_finish", "tool_call", "invalid_path"} {
		t.Run(name, func(t *testing.T) {
			calls := 0
			generator := generatorFunc(func(ctx context.Context, _ string, _ []*genai.Content, _ *genai.GenerateContentConfig) (Generation, error) {
				calls++
				switch name {
				case "timeout":
					<-ctx.Done()
					return Generation{}, ctx.Err()
				case "server_error":
					return Generation{}, genai.APIError{Code: http.StatusServiceUnavailable}
				case "credentials":
					return Generation{}, genai.APIError{Code: http.StatusUnauthorized}
				case "incomplete_finish":
					return Generation{FinishReason: genai.FinishReasonMaxTokens}, nil
				case "tool_call":
					return toolGeneration(&genai.FunctionCall{ID: "unexpected", Name: repository.ToolBash}), nil
				default:
					return textGeneration(`{"summary":"invalid","findings":[{"priority":"P2","title":"x","explanation":"x","recommendation":"x","path":"unchanged.go"}]}`), nil
				}
			})
			reviewer := newGeminiReviewer(generator, "gemini-test", nil, nil)
			reviewer.reviewTimeout = -1
			reviewer.generationTimeout = 10 * time.Millisecond
			broker := &fakeToolBroker{}
			result, encoded, err := reviewer.Review(context.Background(), testSnapshot(), broker)
			if name == "credentials" {
				var credentialFailure *failure.Error
				if !errors.As(err, &credentialFailure) || credentialFailure.Category != "gemini_invalid_credentials" || credentialFailure.Retryable {
					t.Fatalf("credential failure = %v", err)
				}
				return
			}
			if err != nil || !strings.HasPrefix(result.Summary, incompleteReviewNotice+"\n\n") || len(result.Findings) != 0 || calls != 1 || broker.calls != 0 {
				t.Fatalf("Review() = %+v, %v; generations=%d tools=%d", result, err, calls, broker.calls)
			}
			if _, err := DecodeStored(encoded); err != nil {
				t.Fatalf("invalid fallback checkpoint: %v", err)
			}
		})
	}
}

func TestReviewerPropagatesWrapUpCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	generator := generatorFunc(func(_ context.Context, _ string, _ []*genai.Content, _ *genai.GenerateContentConfig) (Generation, error) {
		cancel()
		return textGeneration(`{"summary":"partial","findings":[]}`), nil
	})
	reviewer := newGeminiReviewer(generator, "gemini-test", nil, nil)
	reviewer.reviewTimeout = -1
	_, _, err := reviewer.Review(ctx, testSnapshot(), nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("wrap-up cancellation = %v", err)
	}
}

func TestReviewerPreservesToolFailureAtReviewDeadline(t *testing.T) {
	generator := &fakeGenerator{generations: []Generation{toolGeneration(&genai.FunctionCall{ID: "read", Name: repository.ToolRead})}}
	broker := toolBrokerFunc(func(ctx context.Context, _ string, _ map[string]any) (repository.ToolResult, error) {
		<-ctx.Done()
		return repository.ToolResult{}, errors.Join(failure.Failed("sensitive_tool_content"), ctx.Err())
	})
	reviewer := newGeminiReviewer(generator, "gemini-test", nil, nil)
	reviewer.reviewTimeout = 10 * time.Millisecond
	_, _, err := reviewer.Review(context.Background(), testSnapshot(), broker)
	var toolFailure *failure.Error
	if !errors.As(err, &toolFailure) || toolFailure.Category != "sensitive_tool_content" || len(generator.requests) != 1 {
		t.Fatalf("tool failure = %v; generations=%d", err, len(generator.requests))
	}
}

func TestReviewerRejectsSensitiveWrapUp(t *testing.T) {
	generator := &fakeGenerator{generations: []Generation{textGeneration(`{"summary":"configured-secret","findings":[]}`)}}
	reviewer := newGeminiReviewer(generator, "gemini-test", []string{"configured-secret"}, nil)
	reviewer.reviewTimeout = -1
	_, _, err := reviewer.Review(context.Background(), testSnapshot(), nil)
	var outputFailure *failure.Error
	if !errors.As(err, &outputFailure) || outputFailure.Category != "sensitive_model_output" {
		t.Fatalf("wrap-up credential failure = %v", err)
	}
}

func assertToolDurations(t *testing.T, output string, wantTools []string) {
	t.Helper()
	var tools []string
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		var event struct {
			Message  string `json:"msg"`
			Tool     string `json:"tool"`
			Duration *int64 `json:"duration_ms"`
		}
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		if event.Message != "Gemini review tool completed" {
			continue
		}
		if event.Duration == nil || *event.Duration < 0 {
			t.Fatalf("missing or negative tool duration: %s", line)
		}
		tools = append(tools, event.Tool)
	}
	if !slices.Equal(tools, wantTools) {
		t.Fatalf("completed tools = %v, want %v", tools, wantTools)
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

type generatorFunc func(context.Context, string, []*genai.Content, *genai.GenerateContentConfig) (Generation, error)

func (g generatorFunc) Generate(ctx context.Context, model string, contents []*genai.Content, config *genai.GenerateContentConfig) (Generation, error) {
	return g(ctx, model, contents, config)
}

type toolBrokerFunc func(context.Context, string, map[string]any) (repository.ToolResult, error)

func (b toolBrokerFunc) Call(ctx context.Context, name string, arguments map[string]any) (repository.ToolResult, error) {
	return b(ctx, name, arguments)
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
