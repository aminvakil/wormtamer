package usage

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFetchGeminiPricingUsesLiteLLMGeminiEntry(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet {
			t.Fatalf("method = %s", request.Method)
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{
  "gemini/gemini-3.1-pro-preview": {
    "litellm_provider": "gemini",
    "mode": "chat",
    "input_cost_per_token": 2e-6,
    "cache_read_input_token_cost": 2e-7,
    "output_cost_per_token": 1.2e-5,
    "input_cost_per_token_above_200k_tokens": 4e-6,
    "cache_read_input_token_cost_above_200k_tokens": 4e-7,
    "output_cost_per_token_above_200k_tokens": 1.8e-5
  }
}`))
	}))
	defer server.Close()

	pricing, err := fetchGeminiPricing(context.Background(), server.Client(), server.URL, "models/gemini-3.1-pro-preview")
	if err != nil {
		t.Fatalf("fetchGeminiPricing() error = %v", err)
	}
	if pricing.Standard.InputPicos != 2_000_000 ||
		pricing.Standard.CachedInputPicos == nil || *pricing.Standard.CachedInputPicos != 200_000 ||
		pricing.Standard.OutputPicos != 12_000_000 || pricing.Standard.ReasoningPicos != 12_000_000 ||
		pricing.Above200K == nil || pricing.Above200K.InputPicos != 4_000_000 ||
		pricing.Above200K.CachedInputPicos == nil || *pricing.Above200K.CachedInputPicos != 400_000 ||
		pricing.Above200K.OutputPicos != 18_000_000 || pricing.Above200K.ReasoningPicos != 18_000_000 {
		t.Fatalf("pricing = %+v", pricing)
	}
}

func TestFetchGeminiPricingRejectsNonGeminiAndMissingRates(t *testing.T) {
	for _, body := range []string{
		`{"gemini/gemini-test":{"litellm_provider":"vertex_ai","mode":"chat","input_cost_per_token":1e-6,"output_cost_per_token":2e-6}}`,
		`{"gemini/gemini-test":{"litellm_provider":"gemini","mode":"chat","input_cost_per_token":1e-6}}`,
	} {
		server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
			_, _ = response.Write([]byte(body))
		}))
		_, err := fetchGeminiPricing(context.Background(), server.Client(), server.URL, "gemini-test")
		server.Close()
		if err == nil {
			t.Fatalf("fetchGeminiPricing accepted %s", body)
		}
	}
}

func TestEstimateCostUsesGeminiHighContextRates(t *testing.T) {
	cached := int64(2)
	highCached := int64(20)
	pricing := Pricing{
		Standard:  PricingRates{InputPicos: 1, CachedInputPicos: &cached, OutputPicos: 3, ReasoningPicos: 4},
		Above200K: &PricingRates{InputPicos: 10, CachedInputPicos: &highCached, OutputPicos: 30, ReasoningPicos: 40},
	}
	low, ok := estimateCostPicos(TokenCounts{Prompt: 100, Cached: 10, ToolUsePrompt: 5, Candidates: 3, Thoughts: 2}, pricing)
	if !ok || low != 90+20+5+9+8 {
		t.Fatalf("low estimate = %d, %v", low, ok)
	}
	high, ok := estimateCostPicos(TokenCounts{Prompt: 200_000, Cached: 10, ToolUsePrompt: 1, Candidates: 3, Thoughts: 2}, pricing)
	if !ok || high != 199_990*10+10*20+10+3*30+2*40 {
		t.Fatalf("high estimate = %d, %v", high, ok)
	}
}

func TestLiteLLMResponseCostPicos(t *testing.T) {
	tests := []struct {
		value string
		want  *int64
	}{
		{value: "0.000604", want: int64Pointer(604_000_000)},
		{value: "2e-6", want: int64Pointer(2_000_000)},
		{value: "0", want: int64Pointer(0)},
		{value: "-1", want: nil},
		{value: "0.0000000000001", want: nil},
		{value: "not-a-cost", want: nil},
		{value: "1e", want: nil},
		{value: "1ee2", want: nil},
	}
	for _, test := range tests {
		headers := http.Header{}
		headers.Set("X-Litellm-Response-Cost", test.value)
		got := LiteLLMResponseCostPicos(headers)
		if (got == nil) != (test.want == nil) || (got != nil && *got != *test.want) {
			t.Errorf("LiteLLMResponseCostPicos(%q) = %v, want %v", test.value, got, test.want)
		}
	}
	if got := LiteLLMResponseCostPicos(http.Header{}); got != nil {
		t.Fatalf("missing cost = %d", *got)
	}
}

func int64Pointer(value int64) *int64 {
	return &value
}
