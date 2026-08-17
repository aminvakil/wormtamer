package usage

import (
	"context"
	"errors"
	"math"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	RequestReview   = "review"
	RequestFeedback = "feedback"

	CompletionStarted  = "started"
	CompletionResponse = "response"
	CompletionFailed   = "failed"
	CompletionUnknown  = "unknown"

	CostSourceCatalog  = "litellm_catalog"
	CostSourceEndpoint = "endpoint_response"

	maxTokenCount               = math.MaxInt32
	maxRatePicos                = int64(1_000_000_000_000)
	geminiHighRateThreshold     = int64(200_000)
	generationCheckpointTimeout = 5 * time.Second
)

type Scope struct {
	RequestKind   string
	ReviewJobID   int64
	FeedbackJobID int64
	Attempt       int
}

type scopeKey struct{}

func NewCheckpointContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), generationCheckpointTimeout)
}

func WithScope(ctx context.Context, scope Scope) context.Context {
	return context.WithValue(ctx, scopeKey{}, scope)
}

func ScopeFromContext(ctx context.Context) (Scope, bool) {
	scope, ok := ctx.Value(scopeKey{}).(Scope)
	return scope, ok && scope.valid()
}

func (s Scope) valid() bool {
	if s.Attempt <= 0 {
		return false
	}
	return (s.RequestKind == RequestReview && s.ReviewJobID > 0 && s.FeedbackJobID == 0) ||
		(s.RequestKind == RequestFeedback && s.FeedbackJobID > 0 && s.ReviewJobID == 0)
}

type Pricing struct {
	Standard  PricingRates
	Above200K *PricingRates
}

type PricingRates struct {
	InputPicos       int64
	CachedInputPicos *int64
	OutputPicos      int64
	ReasoningPicos   int64
}

type GenerationStart struct {
	Scope
	Turn            *int
	ConfiguredModel string
	FinalOnly       bool
	StartedAt       time.Time
}

type TokenCounts struct {
	Prompt        int64
	Cached        int64
	ToolUsePrompt int64
	Candidates    int64
	Thoughts      int64
	Total         int64
}

type GenerationCompletion struct {
	State                  string
	CompletedAt            time.Time
	Latency                time.Duration
	ResolvedModel          string
	FinishReason           string
	StructuredValidation   string
	ToolCallsAvailable     bool
	ToolNames              []string
	UsageMetadataAvailable bool
	Tokens                 TokenCounts
	EndpointCostPicos      *int64
}

type StoredCompletion struct {
	GenerationCompletion
	UsageMetadataValid bool
	StoreTokenCounts   bool
	CostSource         string
	EstimatedCostPicos *int64
}

type Persistence interface {
	CreateModelGeneration(context.Context, GenerationStart) (int64, error)
	CompleteModelGeneration(context.Context, int64, StoredCompletion) error
}

type GenerationRecorder interface {
	Start(context.Context, GenerationStart) (int64, error)
	Complete(context.Context, int64, GenerationCompletion) error
}

type Recorder struct {
	storage   Persistence
	pricing   *Pricing
	forbidden []string
}

func NewRecorder(storage Persistence, pricing *Pricing, forbidden []string) (*Recorder, error) {
	if storage == nil {
		return nil, errors.New("usage persistence is required")
	}
	if pricing != nil && (!validPricingRates(pricing.Standard) ||
		(pricing.Above200K != nil && !validPricingRates(*pricing.Above200K))) {
		return nil, errors.New("invalid usage pricing")
	}
	var snapshot *Pricing
	if pricing != nil {
		snapshot = clonePricing(pricing)
	}
	return &Recorder{storage: storage, pricing: snapshot, forbidden: append([]string(nil), forbidden...)}, nil
}

func (r *Recorder) Start(ctx context.Context, start GenerationStart) (int64, error) {
	if !start.Scope.valid() || start.ConfiguredModel == "" || start.StartedAt.IsZero() ||
		(start.Turn != nil && (*start.Turn < 0 || start.RequestKind != RequestReview)) ||
		(start.RequestKind == RequestFeedback && start.Turn != nil) {
		return 0, errors.New("invalid model generation start")
	}
	if containsForbiddenLabel(start.ConfiguredModel, r.forbidden) {
		start.ConfiguredModel = "[redacted sensitive content]"
	}
	return r.storage.CreateModelGeneration(ctx, start)
}

func (r *Recorder) Complete(ctx context.Context, generationID int64, completion GenerationCompletion) error {
	if generationID <= 0 || completion.CompletedAt.IsZero() || completion.Latency < 0 ||
		(completion.State != CompletionResponse && completion.State != CompletionFailed) {
		return errors.New("invalid model generation completion")
	}
	if completion.State == CompletionFailed && (completion.ResolvedModel != "" || completion.FinishReason != "" ||
		completion.ToolCallsAvailable || len(completion.ToolNames) > 0 || completion.UsageMetadataAvailable ||
		completion.Tokens != (TokenCounts{}) || completion.EndpointCostPicos != nil) {
		return errors.New("failed model generation contains response metadata")
	}
	if completion.EndpointCostPicos != nil && *completion.EndpointCostPicos < 0 {
		return errors.New("invalid endpoint response cost")
	}
	stored := StoredCompletion{GenerationCompletion: completion}
	stored.EndpointCostPicos = nil
	stored.ToolNames = append([]string(nil), completion.ToolNames...)
	for index, name := range stored.ToolNames {
		if containsForbiddenLabel(name, r.forbidden) {
			stored.ToolNames[index] = "[redacted sensitive content]"
		}
	}
	if containsForbiddenLabel(stored.ResolvedModel, r.forbidden) {
		stored.ResolvedModel = "[redacted sensitive content]"
	}
	if containsForbiddenLabel(stored.FinishReason, r.forbidden) {
		stored.FinishReason = "[redacted sensitive content]"
	}
	if !validLabel(stored.ResolvedModel, 256) {
		stored.ResolvedModel = ""
	}
	if !validLabel(stored.FinishReason, 128) {
		stored.FinishReason = ""
	}
	if completion.State == CompletionResponse && completion.UsageMetadataAvailable {
		stored.StoreTokenCounts = validTokenRanges(completion.Tokens)
		if stored.StoreTokenCounts {
			stored.UsageMetadataValid = internallyConsistent(completion.Tokens)
		}
	}
	if completion.EndpointCostPicos != nil && completion.State == CompletionResponse {
		cost := *completion.EndpointCostPicos
		stored.CostSource = CostSourceEndpoint
		stored.EstimatedCostPicos = &cost
	} else if r.pricing != nil && completion.State == CompletionResponse {
		stored.CostSource = CostSourceCatalog
		if stored.UsageMetadataValid {
			if estimate, ok := estimateCostPicos(completion.Tokens, *r.pricing); ok {
				stored.EstimatedCostPicos = &estimate
			}
		}
	}
	return r.storage.CompleteModelGeneration(ctx, generationID, stored)
}

func containsForbiddenLabel(value string, forbidden []string) bool {
	for _, secret := range forbidden {
		if secret != "" && strings.Contains(value, secret) {
			return true
		}
	}
	return false
}

func validLabel(value string, limit int) bool {
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

func validTokenRanges(tokens TokenCounts) bool {
	return tokens.Prompt >= 0 && tokens.Prompt <= maxTokenCount &&
		tokens.Cached >= 0 && tokens.Cached <= maxTokenCount &&
		tokens.ToolUsePrompt >= 0 && tokens.ToolUsePrompt <= maxTokenCount &&
		tokens.Candidates >= 0 && tokens.Candidates <= maxTokenCount &&
		tokens.Thoughts >= 0 && tokens.Thoughts <= maxTokenCount &&
		tokens.Total >= 0 && tokens.Total <= maxTokenCount
}

func internallyConsistent(tokens TokenCounts) bool {
	return tokens.Prompt > 0 && tokens.Total > 0 && tokens.Cached <= tokens.Prompt &&
		tokens.Total == tokens.Prompt+tokens.ToolUsePrompt+tokens.Candidates+tokens.Thoughts
}

func clonePricing(pricing *Pricing) *Pricing {
	copy := *pricing
	if pricing.Standard.CachedInputPicos != nil {
		cached := *pricing.Standard.CachedInputPicos
		copy.Standard.CachedInputPicos = &cached
	}
	if pricing.Above200K != nil {
		high := *pricing.Above200K
		if pricing.Above200K.CachedInputPicos != nil {
			cached := *pricing.Above200K.CachedInputPicos
			high.CachedInputPicos = &cached
		}
		copy.Above200K = &high
	}
	return &copy
}

func validPricingRates(rates PricingRates) bool {
	if rates.InputPicos < 0 || rates.InputPicos > maxRatePicos ||
		rates.OutputPicos < 0 || rates.OutputPicos > maxRatePicos ||
		rates.ReasoningPicos < 0 || rates.ReasoningPicos > maxRatePicos {
		return false
	}
	return rates.CachedInputPicos == nil || (*rates.CachedInputPicos >= 0 && *rates.CachedInputPicos <= maxRatePicos)
}

func estimateCostPicos(tokens TokenCounts, pricing Pricing) (int64, bool) {
	rates := pricing.Standard
	if pricing.Above200K != nil && tokens.Prompt+tokens.ToolUsePrompt > geminiHighRateThreshold {
		rates = *pricing.Above200K
	}
	if tokens.Cached > 0 && rates.CachedInputPicos == nil {
		return 0, false
	}
	cachedRate := int64(0)
	if rates.CachedInputPicos != nil {
		cachedRate = *rates.CachedInputPicos
	}
	parts := []struct {
		tokens int64
		rate   int64
	}{
		{tokens: tokens.Prompt - tokens.Cached, rate: rates.InputPicos},
		{tokens: tokens.Cached, rate: cachedRate},
		{tokens: tokens.ToolUsePrompt, rate: rates.InputPicos},
		{tokens: tokens.Candidates, rate: rates.OutputPicos},
		{tokens: tokens.Thoughts, rate: rates.ReasoningPicos},
	}
	var total int64
	for _, part := range parts {
		if part.tokens != 0 && part.rate > math.MaxInt64/part.tokens {
			return 0, false
		}
		value := part.tokens * part.rate
		if value > math.MaxInt64-total {
			return 0, false
		}
		total += value
	}
	return total, true
}
