package store

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/aminvakil/wormtamer/internal/usage"
)

func TestModelGenerationLifecyclePersistsMetadataWithoutContent(t *testing.T) {
	storage := openTestStore(t)
	defer storage.Close()
	accepted, err := storage.AcceptEvent(context.Background(), readyEvent("usage-review"))
	if err != nil {
		t.Fatal(err)
	}
	turn := 1
	started := time.Now().UTC()
	generationID, err := storage.CreateModelGeneration(context.Background(), usage.GenerationStart{
		Scope: usage.Scope{RequestKind: usage.RequestReview, ReviewJobID: accepted.JobID, Attempt: 2},
		Turn:  &turn, ConfiguredModel: "gemini-test", FinalOnly: true, StartedAt: started,
	})
	if err != nil {
		t.Fatal(err)
	}
	estimate := int64(123456)
	completion := usage.StoredCompletion{
		GenerationCompletion: usage.GenerationCompletion{
			State: usage.CompletionResponse, CompletedAt: started.Add(1500 * time.Millisecond), Latency: 1500 * time.Millisecond,
			ResolvedModel: "gemini-resolved", FinishReason: "STOP", StructuredValidation: "valid",
			ToolCallsAvailable: true, ToolNames: []string{"read_repository_file"}, UsageMetadataAvailable: true,
			Tokens: usage.TokenCounts{Prompt: 100, Cached: 20, ToolUsePrompt: 10, Candidates: 30, Thoughts: 5, Total: 145},
		},
		StoreTokenCounts: true, UsageMetadataValid: true,
		CostSource:         usage.CostSourceCatalog,
		EstimatedCostPicos: &estimate,
	}
	if err := storage.CompleteModelGeneration(context.Background(), generationID, completion); err != nil {
		t.Fatal(err)
	}

	var state, model, resolved, validation, toolNames string
	var latency, prompt, cached, toolPrompt, candidates, thoughts, total, cost int64
	if err := storage.db.QueryRow(`
SELECT completion_state, configured_model, resolved_model, latency_ms, structured_validation,
       tool_names_json, prompt_tokens, cached_tokens, tool_use_prompt_tokens,
       candidate_tokens, thought_tokens, total_tokens, estimated_cost_picos
FROM model_generations WHERE id = ?`, generationID).Scan(
		&state, &model, &resolved, &latency, &validation, &toolNames,
		&prompt, &cached, &toolPrompt, &candidates, &thoughts, &total, &cost); err != nil {
		t.Fatal(err)
	}
	if state != usage.CompletionResponse || model != "gemini-test" || resolved != "gemini-resolved" || latency != 1500 ||
		validation != "valid" || toolNames != `["read_repository_file"]` || prompt != 100 || cached != 20 ||
		toolPrompt != 10 || candidates != 30 || thoughts != 5 || total != 145 || cost != estimate {
		t.Fatalf("stored generation state=%q model=%q resolved=%q latency=%d validation=%q tools=%q tokens=%d/%d/%d/%d/%d/%d cost=%d",
			state, model, resolved, latency, validation, toolNames, prompt, cached, toolPrompt, candidates, thoughts, total, cost)
	}
	report, err := storage.ReadUsageReport(context.Background(), UsageQuery{
		Since: started.Add(-time.Hour), Until: started.Add(time.Hour), Limit: 50,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.GenerationCount != 1 || report.ResponseCount != 1 || report.UsageAvailableCount != 1 ||
		report.PricedCount != 1 || report.Tokens.Input != 110 || report.Tokens.Output != 35 || report.Tokens.Prompt != 100 || report.Tokens.UncachedInput != 80 ||
		report.Tokens.CachedInput != 20 || report.Tokens.ToolUseInput != 10 || report.Tokens.Candidate != 30 ||
		report.Tokens.Thought != 5 || report.Tokens.Total != 145 || len(report.Costs) != 1 ||
		report.Costs[0].EstimatedCostPicos != estimate ||
		len(report.Generations.Records) != 1 || report.Generations.Records[0].ID != generationID {
		t.Fatalf("usage report = %+v", report)
	}
	detail, err := storage.GetGenerationRecord(context.Background(), generationID)
	if err != nil || !detail.TokenCountsAvailable || len(detail.ToolNames) != 1 || detail.ToolNames[0] != "read_repository_file" {
		t.Fatalf("GetGenerationRecord() = %+v, %v", detail, err)
	}
	reviewDetail, err := storage.GetReviewRecord(context.Background(), accepted.JobID)
	if err != nil || len(reviewDetail.Generations) != 1 || reviewDetail.Generations[0].ID != generationID {
		t.Fatalf("GetReviewRecord() generations = %+v, %v", reviewDetail.Generations, err)
	}
}

func TestModelGenerationStoresEndpointCostWithoutUsageMetadata(t *testing.T) {
	storage := openTestStore(t)
	defer storage.Close()
	accepted, err := storage.AcceptEvent(context.Background(), readyEvent("usage-endpoint-cost"))
	if err != nil {
		t.Fatal(err)
	}
	turn := 0
	started := time.Now().UTC()
	generationID, err := storage.CreateModelGeneration(context.Background(), usage.GenerationStart{
		Scope: usage.Scope{RequestKind: usage.RequestReview, ReviewJobID: accepted.JobID, Attempt: 1},
		Turn:  &turn, ConfiguredModel: "gemini-proxy", StartedAt: started,
	})
	if err != nil {
		t.Fatal(err)
	}
	cost := int64(123_000_000)
	if err := storage.CompleteModelGeneration(context.Background(), generationID, usage.StoredCompletion{
		GenerationCompletion: usage.GenerationCompletion{
			State: usage.CompletionResponse, CompletedAt: started.Add(time.Second), StructuredValidation: "valid",
		},
		CostSource: usage.CostSourceEndpoint, EstimatedCostPicos: &cost,
	}); err != nil {
		t.Fatal(err)
	}
	var source string
	var storedCost int64
	if err := storage.db.QueryRow(`SELECT cost_source, estimated_cost_picos FROM model_generations WHERE id = ?`, generationID).
		Scan(&source, &storedCost); err != nil {
		t.Fatal(err)
	}
	if source != usage.CostSourceEndpoint || storedCost != cost {
		t.Fatalf("stored cost = %q %d", source, storedCost)
	}
}

func TestUsageReportFiltersUnavailableResolvedModel(t *testing.T) {
	storage := openTestStore(t)
	defer storage.Close()
	accepted, err := storage.AcceptEvent(context.Background(), readyEvent("usage-resolved-model-filter"))
	if err != nil {
		t.Fatal(err)
	}
	turn := 0
	started := time.Now().UTC()
	for index, resolved := range []string{"", "gemini-resolved"} {
		generationID, err := storage.CreateModelGeneration(context.Background(), usage.GenerationStart{
			Scope: usage.Scope{RequestKind: usage.RequestReview, ReviewJobID: accepted.JobID, Attempt: 1},
			Turn:  &turn, ConfiguredModel: "gemini-test", StartedAt: started.Add(time.Duration(index) * time.Second),
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := storage.CompleteModelGeneration(context.Background(), generationID, usage.StoredCompletion{GenerationCompletion: usage.GenerationCompletion{
			State: usage.CompletionResponse, CompletedAt: started.Add(time.Duration(index+1) * time.Second),
			ResolvedModel: resolved, FinishReason: "STOP", StructuredValidation: "valid",
		}}); err != nil {
			t.Fatal(err)
		}
	}
	report, err := storage.ReadUsageReport(context.Background(), UsageQuery{
		Since: started.Add(-time.Hour), Until: started.Add(time.Hour), ResolvedModelUnavailable: true, Limit: 50,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.GenerationCount != 1 || len(report.Generations.Records) != 1 || report.Generations.Records[0].ResolvedModel != "" {
		t.Fatalf("unavailable resolved-model report = %+v", report)
	}
	if _, err := storage.ReadUsageReport(context.Background(), UsageQuery{
		Since: started.Add(-time.Hour), Until: started.Add(time.Hour),
		ResolvedModel: "gemini-resolved", ResolvedModelUnavailable: true, Limit: 50,
	}); err == nil {
		t.Fatal("ReadUsageReport accepted conflicting resolved-model filters")
	}
}

func TestModelGenerationFailureAndInterruptionRemainExplicit(t *testing.T) {
	storage := openTestStore(t)
	defer storage.Close()
	accepted, err := storage.AcceptEvent(context.Background(), readyEvent("usage-outcomes"))
	if err != nil {
		t.Fatal(err)
	}
	turn := 0
	start := usage.GenerationStart{
		Scope: usage.Scope{RequestKind: usage.RequestReview, ReviewJobID: accepted.JobID, Attempt: 1},
		Turn:  &turn, ConfiguredModel: "gemini-test", StartedAt: time.Now().UTC(),
	}
	failedID, err := storage.CreateModelGeneration(context.Background(), start)
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.CompleteModelGeneration(context.Background(), failedID, usage.StoredCompletion{GenerationCompletion: usage.GenerationCompletion{
		State: usage.CompletionFailed, CompletedAt: start.StartedAt.Add(time.Second), Latency: time.Second,
		StructuredValidation: "request_failed",
	}}); err != nil {
		t.Fatal(err)
	}
	unknownID, err := storage.CreateModelGeneration(context.Background(), start)
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.MarkStartedModelGenerationsUnknown(context.Background()); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[int64]string{failedID: usage.CompletionFailed, unknownID: usage.CompletionUnknown} {
		var state string
		var completed sql.NullString
		if err := storage.db.QueryRow(`SELECT completion_state, completed_at FROM model_generations WHERE id = ?`, id).Scan(&state, &completed); err != nil {
			t.Fatal(err)
		}
		if state != want || (want == usage.CompletionUnknown && completed.Valid) {
			t.Fatalf("generation %d state=%q completed=%+v, want %q", id, state, completed, want)
		}
	}
}
