package review

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"

	"github.com/aminvakil/wormtamer/internal/diagnostics"
	"github.com/aminvakil/wormtamer/internal/failure"
	"github.com/aminvakil/wormtamer/internal/repository"
	"google.golang.org/genai"
)

type toolEvidenceBudget struct {
	mu        sync.Mutex
	used      int
	exhausted bool
}

func (b *toolEvidenceBudget) admit(response *genai.FunctionResponse) error {
	encoded, err := json.Marshal(response)
	if err != nil {
		return failure.Retry("tool_result_encoding_failed", 0)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.exhausted || b.used+len(encoded) > maxToolResultBytes {
		b.exhausted = true
		return repository.ErrToolEvidenceLimit
	}
	b.used += len(encoded)
	return nil
}

func (b *toolEvidenceBudget) available() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return !b.exhausted
}

func (r *GeminiReviewer) callTool(ctx context.Context, broker repository.ToolBroker, call *genai.FunctionCall,
	budget *toolEvidenceBudget, logger *slog.Logger, turn int) (repository.ToolResult, error) {
	if call.Name != repository.ToolCodemode {
		return broker.Call(ctx, call.Name, call.Args)
	}
	runner, ok := broker.(repository.CodemodeBroker)
	if !ok {
		return repository.ToolResult{}, failure.Retry("review_tools_unavailable", 0)
	}
	logger = logger.With("parent_tool_call_id", boundedDiagnosticValue(call.ID, r.forbidden, 256))
	return runner.RunCode(ctx, call.Args, func(ctx context.Context, id int64, name string, args map[string]any) (map[string]any, error) {
		if name != repository.ToolRead && name != repository.ToolBash {
			return nil, failure.Retry("model_requested_undeclared_tool", 0)
		}
		if !budget.available() {
			return nil, repository.ErrToolEvidenceLimit
		}
		nested := &genai.FunctionCall{ID: fmt.Sprintf("nested-%d", id), Name: name, Args: args}
		if logger.Enabled(ctx, slog.LevelDebug) {
			logger.DebugContext(ctx, "Gemini review tool call", "turn", turn,
				"tool_call_id", nested.ID, "tool", name, "arguments", diagnosticJSON(args, r.forbidden, 4096))
		}
		result, err := broker.Call(ctx, name, args)
		if err != nil {
			return nil, err
		}
		response, encoded, err := functionResponse(nested, repository.ToolResult{Response: result.ScriptResponse()}, r.forbidden)
		if err != nil {
			return nil, err
		}
		if err := budget.admit(response); err != nil {
			return nil, err
		}
		logger.InfoContext(ctx, "Gemini review tool completed", "turn", turn, "tool", name, "outcome", "completed")
		if logger.Enabled(ctx, slog.LevelDebug) {
			logger.DebugContext(ctx, "Gemini review tool result", "turn", turn,
				"tool_call_id", nested.ID, "tool", name, "result", diagnostics.Redact(string(encoded), r.forbidden))
		}
		return response.Response, nil
	})
}
