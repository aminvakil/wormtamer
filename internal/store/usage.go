package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/aminvakil/wormtamer/internal/usage"
)

const maxGenerationToolNamesBytes = 65536

func (s *Store) CreateModelGeneration(ctx context.Context, start usage.GenerationStart) (int64, error) {
	if start.ConfiguredModel == "" || !validGenerationText(start.ConfiguredModel, 256) ||
		start.Attempt <= 0 || start.StartedAt.IsZero() {
		return 0, errors.New("invalid model generation start")
	}
	var reviewJobID, feedbackJobID, turn any
	switch start.RequestKind {
	case usage.RequestReview:
		if start.ReviewJobID <= 0 || start.FeedbackJobID != 0 || start.Turn == nil || *start.Turn < 0 || *start.Turn > 1000 {
			return 0, errors.New("invalid review generation start")
		}
		reviewJobID, turn = start.ReviewJobID, *start.Turn
	case usage.RequestFeedback:
		if start.FeedbackJobID <= 0 || start.ReviewJobID != 0 || start.Turn != nil {
			return 0, errors.New("invalid feedback generation start")
		}
		feedbackJobID = start.FeedbackJobID
	default:
		return 0, errors.New("invalid model generation kind")
	}
	result, err := s.db.ExecContext(ctx, `
INSERT INTO model_generations (
    request_kind, review_job_id, feedback_job_id, workflow_attempt, review_turn,
    configured_model, request_started_at, completion_state, final_only
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		start.RequestKind, reviewJobID, feedbackJobID, start.Attempt, turn,
		start.ConfiguredModel, formatTime(start.StartedAt), usage.CompletionStarted, boolInt(start.FinalOnly))
	if err != nil {
		return 0, fmt.Errorf("create model generation: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("read model generation ID: %w", err)
	}
	return id, nil
}

func (s *Store) CompleteModelGeneration(ctx context.Context, generationID int64, completion usage.StoredCompletion) error {
	if generationID <= 0 || completion.CompletedAt.IsZero() || completion.Latency < 0 || completion.Latency > 24*time.Hour ||
		(completion.State != usage.CompletionResponse && completion.State != usage.CompletionFailed) ||
		!validGenerationText(completion.ResolvedModel, 256) || !validGenerationText(completion.FinishReason, 128) ||
		completion.StructuredValidation == "" || !validGenerationText(completion.StructuredValidation, 128) ||
		completion.EndpointCostPicos != nil {
		return errors.New("invalid model generation completion")
	}
	if completion.State == usage.CompletionFailed && (completion.ResolvedModel != "" || completion.FinishReason != "" ||
		completion.ToolCallsAvailable || len(completion.ToolNames) > 0 || completion.UsageMetadataAvailable ||
		completion.StoreTokenCounts || completion.UsageMetadataValid || completion.CostSource != "" ||
		completion.EstimatedCostPicos != nil) {
		return errors.New("failed model generation contains response metadata")
	}
	var toolCount, toolNames any
	if completion.ToolCallsAvailable {
		if len(completion.ToolNames) > 32768 {
			return errors.New("too many model generation tool names")
		}
		for _, name := range completion.ToolNames {
			if name == "" || !validGenerationText(name, 128) {
				return errors.New("invalid model generation tool name")
			}
		}
		encoded, err := json.Marshal(completion.ToolNames)
		if err != nil || len(encoded) > maxGenerationToolNamesBytes {
			return errors.New("invalid model generation tool names")
		}
		toolCount, toolNames = len(completion.ToolNames), encoded
	}
	var prompt, cached, toolPrompt, candidates, thoughts, total any
	if completion.StoreTokenCounts {
		prompt, cached = completion.Tokens.Prompt, completion.Tokens.Cached
		toolPrompt, candidates = completion.Tokens.ToolUsePrompt, completion.Tokens.Candidates
		thoughts, total = completion.Tokens.Thoughts, completion.Tokens.Total
	}
	var costSource any
	if completion.CostSource != "" {
		if completion.CostSource != usage.CostSourceCatalog && completion.CostSource != usage.CostSourceEndpoint {
			return errors.New("invalid model generation cost source")
		}
		costSource = completion.CostSource
	}
	var estimatedCost any
	if completion.EstimatedCostPicos != nil {
		if *completion.EstimatedCostPicos < 0 || completion.CostSource == "" ||
			(completion.CostSource == usage.CostSourceCatalog && !completion.UsageMetadataValid) {
			return errors.New("invalid model generation estimate")
		}
		estimatedCost = *completion.EstimatedCostPicos
	}
	var resolvedModel, finishReason any
	if completion.ResolvedModel != "" {
		resolvedModel = completion.ResolvedModel
	}
	if completion.FinishReason != "" {
		finishReason = completion.FinishReason
	}
	result, err := s.db.ExecContext(ctx, `
UPDATE model_generations
SET completed_at = ?, completion_state = ?, latency_ms = ?, resolved_model = ?, finish_reason = ?,
    structured_validation = ?, tool_calls_available = ?, tool_call_count = ?, tool_names_json = ?,
    usage_metadata_available = ?, usage_metadata_valid = ?,
    prompt_tokens = ?, cached_tokens = ?, tool_use_prompt_tokens = ?, candidate_tokens = ?, thought_tokens = ?, total_tokens = ?,
    cost_source = ?, estimated_cost_picos = ?
WHERE id = ? AND completion_state = ?`,
		formatTime(completion.CompletedAt), completion.State, completion.Latency.Milliseconds(), resolvedModel, finishReason,
		completion.StructuredValidation, boolInt(completion.ToolCallsAvailable), toolCount, toolNames,
		boolInt(completion.UsageMetadataAvailable), boolInt(completion.UsageMetadataValid),
		prompt, cached, toolPrompt, candidates, thoughts, total,
		costSource, estimatedCost,
		generationID, usage.CompletionStarted)
	if err != nil {
		return fmt.Errorf("complete model generation: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect model generation completion: %w", err)
	}
	if updated != 1 {
		return errors.New("model generation is no longer started")
	}
	return nil
}

func (s *Store) MarkStartedModelGenerationsUnknown(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `
UPDATE model_generations SET completion_state = ? WHERE completion_state = ?`,
		usage.CompletionUnknown, usage.CompletionStarted); err != nil {
		return fmt.Errorf("mark interrupted model generations unknown: %w", err)
	}
	return nil
}

func validGenerationText(value string, limit int) bool {
	if len(value) > limit || !utf8.ValidString(value) {
		return false
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
