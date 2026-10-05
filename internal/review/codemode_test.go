package review

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/aminvakil/wormtamer/internal/failure"
	"github.com/aminvakil/wormtamer/internal/repository"
	"google.golang.org/genai"
)

type codeBroker struct {
	fakeToolBroker
	run func(context.Context, repository.NestedToolCall) (repository.ToolResult, error)
}

func (b *codeBroker) RunCode(ctx context.Context, _ map[string]any, call repository.NestedToolCall) (repository.ToolResult, error) {
	return b.run(ctx, call)
}

func TestCodemodeExposesOnlySelectedOutputAndLogsNestedCalls(t *testing.T) {
	for _, level := range []slog.Level{slog.LevelInfo, slog.LevelDebug} {
		t.Run(level.String(), func(t *testing.T) {
			var logs bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: level}))
			generator := &fakeGenerator{generations: []Generation{
				toolGeneration(&genai.FunctionCall{ID: "script", Name: repository.ToolCodemode, Args: map[string]any{"code": "private script"}}),
				textGeneration(`{"summary":"ok","findings":[]}`),
			}}
			broker := &codeBroker{fakeToolBroker: fakeToolBroker{result: repository.ToolResult{
				Response:    map[string]any{"output": "direct presentation"},
				ScriptValue: repository.BashScriptResult{Output: "private intermediate", ExitCode: 7},
			}}}
			broker.run = func(ctx context.Context, call repository.NestedToolCall) (repository.ToolResult, error) {
				response, err := call(ctx, 1, repository.ToolBash, map[string]any{"command": "private command"})
				if err != nil {
					return repository.ToolResult{}, err
				}
				value := response["output"].(repository.BashScriptResult)
				if value.Output != "private intermediate" || value.ExitCode != 7 {
					t.Fatalf("script result = %+v", response)
				}
				return repository.ToolResult{Response: map[string]any{"output": "selected evidence"}}, nil
			}
			_, _, err := newGeminiReviewer(generator, "gemini-test", nil, logger).Review(context.Background(), testSnapshot(), broker)
			if err != nil {
				t.Fatal(err)
			}
			responses := functionResponses(generator.requests[1].contents)
			if len(responses) != 1 || responses[0].Name != repository.ToolCodemode || responses[0].Response["output"] != "selected evidence" {
				t.Fatalf("model responses = %+v", responses)
			}
			if !strings.Contains(logs.String(), `"parent_tool_call_id":"script"`) || !strings.Contains(logs.String(), `"tool":"bash"`) {
				t.Fatalf("missing nested diagnostics: %s", logs.String())
			}
			for _, content := range []string{"private script", "private intermediate", "private command"} {
				if strings.Contains(logs.String(), content) != (level == slog.LevelDebug) {
					t.Fatalf("unexpected diagnostic content at %s: %s", level, logs.String())
				}
			}
		})
	}
}

func TestCodemodeChecksStructuredEvidenceBeforeScriptReceivesIt(t *testing.T) {
	secret := "configured-secret"
	generator := &fakeGenerator{generations: []Generation{
		toolGeneration(&genai.FunctionCall{ID: "script", Name: repository.ToolCodemode, Args: map[string]any{"code": "ignored"}}),
	}}
	broker := &codeBroker{fakeToolBroker: fakeToolBroker{result: repository.ToolResult{
		Response:    map[string]any{"output": "safe formatted tail"},
		ScriptValue: repository.BashScriptResult{Output: secret + "\nsafe formatted tail"},
	}}}
	broker.run = func(ctx context.Context, call repository.NestedToolCall) (repository.ToolResult, error) {
		response, err := call(ctx, 1, repository.ToolBash, map[string]any{"command": "anything"})
		if response != nil {
			t.Fatal("sensitive content was supplied to script")
		}
		return repository.ToolResult{}, err
	}
	_, _, err := newGeminiReviewer(generator, "gemini-test", []string{secret}, nil).Review(context.Background(), testSnapshot(), broker)
	var failureError *failure.Error
	if !errors.As(err, &failureError) || failureError.Category != "sensitive_tool_content" || failureError.Retryable {
		t.Fatalf("credential failure = %v", err)
	}
}

func TestCodemodeNestedEvidenceExhaustsSharedBudget(t *testing.T) {
	generator := &fakeGenerator{generations: []Generation{
		toolGeneration(
			&genai.FunctionCall{ID: "direct", Name: repository.ToolRead, Args: map[string]any{"path": "a"}},
			&genai.FunctionCall{ID: "script", Name: repository.ToolCodemode, Args: map[string]any{"code": "ignored"}},
			&genai.FunctionCall{ID: "later", Name: repository.ToolRead, Args: map[string]any{"path": "b"}},
		),
		textGeneration(`{"summary":"bounded","findings":[]}`),
	}}
	broker := &codeBroker{fakeToolBroker: fakeToolBroker{result: repository.ToolResult{Response: map[string]any{"output": strings.Repeat("x", 9<<20)}}}}
	broker.run = func(ctx context.Context, call repository.NestedToolCall) (repository.ToolResult, error) {
		_, err := call(ctx, 1, repository.ToolRead, map[string]any{"path": "nested"})
		return repository.ToolResult{}, err
	}
	result, _, err := newGeminiReviewer(generator, "gemini-test", nil, nil).Review(context.Background(), testSnapshot(), broker)
	if err != nil || result.Summary != "bounded" || broker.calls != 2 {
		t.Fatalf("budget result = %+v, %v; calls=%d", result, err, broker.calls)
	}
	responses := functionResponses(generator.requests[1].contents)
	if len(responses) != 3 || responses[1].Response["error"] != "tool_result_limit_exceeded" || responses[2].Response["error"] != "tool_result_limit_exceeded" {
		t.Fatalf("budget responses = %+v", responses)
	}
	config := generator.requests[1].config
	if config.ToolConfig.FunctionCallingConfig.Mode != genai.FunctionCallingConfigModeNone || len(config.Tools) != 0 {
		t.Fatalf("codemode remained available after exhaustion: %+v", config)
	}
}
