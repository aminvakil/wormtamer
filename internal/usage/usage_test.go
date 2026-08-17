package usage

import (
	"context"
	"testing"
	"time"
)

func TestRecorderCalculatesStableFixedPointEstimate(t *testing.T) {
	storage := &fakePersistence{}
	cachedRate := int64(200_000)
	pricing := &Pricing{Standard: PricingRates{
		InputPicos: 2_000_000, CachedInputPicos: &cachedRate,
		OutputPicos: 12_000_000, ReasoningPicos: 12_000_000,
	}}
	recorder, err := NewRecorder(storage, pricing, nil)
	if err != nil {
		t.Fatal(err)
	}
	turn := 2
	startedAt := time.Now().UTC()
	id, err := recorder.Start(context.Background(), GenerationStart{
		Scope: Scope{RequestKind: RequestReview, ReviewJobID: 7, Attempt: 2},
		Turn:  &turn, ConfiguredModel: "gemini-test", FinalOnly: true, StartedAt: startedAt,
	})
	if err != nil || id != 41 {
		t.Fatalf("Start() = %d, %v", id, err)
	}
	pricing.Standard.OutputPicos = 1
	cachedRate = 1
	completion := GenerationCompletion{
		State: CompletionResponse, CompletedAt: startedAt.Add(time.Second), Latency: time.Second,
		UsageMetadataAvailable: true,
		Tokens:                 TokenCounts{Prompt: 100, Cached: 20, ToolUsePrompt: 10, Candidates: 30, Thoughts: 5, Total: 145},
	}
	if err := recorder.Complete(context.Background(), id, completion); err != nil {
		t.Fatal(err)
	}
	got := storage.completion
	if !got.UsageMetadataValid || got.EstimatedCostPicos == nil {
		t.Fatalf("stored completion = %+v", got)
	}
	want := int64(80*2_000_000 + 20*200_000 + 10*2_000_000 + 35*12_000_000)
	if *got.EstimatedCostPicos != want || got.CostSource != CostSourceCatalog {
		t.Fatalf("estimate = %v, source=%q, want %d", got.EstimatedCostPicos, got.CostSource, want)
	}
}

func TestRecorderKeepsInconsistentUsageWithoutCost(t *testing.T) {
	storage := &fakePersistence{}
	cachedRate := int64(1)
	recorder, err := NewRecorder(storage, &Pricing{Standard: PricingRates{
		InputPicos: 1, CachedInputPicos: &cachedRate, OutputPicos: 1, ReasoningPicos: 1,
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	completion := GenerationCompletion{
		State: CompletionResponse, CompletedAt: time.Now().UTC(), UsageMetadataAvailable: true,
		Tokens: TokenCounts{Prompt: 100, Candidates: 20, Total: 999},
	}
	if err := recorder.Complete(context.Background(), 1, completion); err != nil {
		t.Fatal(err)
	}
	if !storage.completion.StoreTokenCounts || storage.completion.UsageMetadataValid || storage.completion.EstimatedCostPicos != nil ||
		storage.completion.CostSource != CostSourceCatalog {
		t.Fatalf("stored completion = %+v", storage.completion)
	}
}

func TestRecorderKeepsConfiguredCredentialsOutOfMetadata(t *testing.T) {
	storage := &fakePersistence{}
	recorder, err := NewRecorder(storage, nil, []string{"configured-secret"})
	if err != nil {
		t.Fatal(err)
	}
	turn := 0
	id, err := recorder.Start(context.Background(), GenerationStart{
		Scope: Scope{RequestKind: RequestReview, ReviewJobID: 7, Attempt: 1}, Turn: &turn,
		ConfiguredModel: "model-configured-secret", StartedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := recorder.Complete(context.Background(), id, GenerationCompletion{
		State: CompletionResponse, CompletedAt: time.Now().UTC(), ResolvedModel: "resolved-configured-secret",
		FinishReason: "configured-secret", StructuredValidation: "valid",
	}); err != nil {
		t.Fatal(err)
	}
	if storage.start.ConfiguredModel != "[redacted sensitive content]" ||
		storage.completion.ResolvedModel != "[redacted sensitive content]" ||
		storage.completion.FinishReason != "[redacted sensitive content]" {
		t.Fatalf("start=%+v completion=%+v", storage.start, storage.completion)
	}
}

func TestRecorderMarksMissingAndInvalidUsageUnavailable(t *testing.T) {
	tests := []GenerationCompletion{
		{State: CompletionResponse, CompletedAt: time.Now().UTC()},
		{State: CompletionResponse, CompletedAt: time.Now().UTC(), UsageMetadataAvailable: true, Tokens: TokenCounts{Prompt: -1}},
	}
	for index, completion := range tests {
		storage := &fakePersistence{}
		recorder, err := NewRecorder(storage, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := recorder.Complete(context.Background(), int64(index+1), completion); err != nil {
			t.Fatal(err)
		}
		if storage.completion.UsageMetadataValid || storage.completion.StoreTokenCounts || storage.completion.EstimatedCostPicos != nil {
			t.Fatalf("case %d stored completion = %+v", index, storage.completion)
		}
	}
}

func TestRecorderPrefersEndpointResponseCostWithoutTokenMetadata(t *testing.T) {
	storage := &fakePersistence{}
	pricing := &Pricing{Standard: PricingRates{InputPicos: 1, OutputPicos: 1, ReasoningPicos: 1}}
	recorder, err := NewRecorder(storage, pricing, nil)
	if err != nil {
		t.Fatal(err)
	}
	cost := int64(987_654_321)
	completion := GenerationCompletion{
		State: CompletionResponse, CompletedAt: time.Now().UTC(), EndpointCostPicos: &cost,
	}
	if err := recorder.Complete(context.Background(), 1, completion); err != nil {
		t.Fatal(err)
	}
	if storage.completion.EstimatedCostPicos == nil || *storage.completion.EstimatedCostPicos != cost ||
		storage.completion.CostSource != CostSourceEndpoint {
		t.Fatalf("stored completion = %+v", storage.completion)
	}
}

type fakePersistence struct {
	start      GenerationStart
	completion StoredCompletion
}

func (s *fakePersistence) CreateModelGeneration(_ context.Context, start GenerationStart) (int64, error) {
	s.start = start
	return 41, nil
}

func (s *fakePersistence) CompleteModelGeneration(_ context.Context, _ int64, completion StoredCompletion) error {
	s.completion = completion
	return nil
}
