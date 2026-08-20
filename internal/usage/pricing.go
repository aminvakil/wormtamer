package usage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	liteLLMPriceCatalogURL = "https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json"
	maxPriceCatalogBytes   = 4 << 20
	priceCatalogTimeout    = 5 * time.Second
)

type catalogModelPricing struct {
	Provider             string      `json:"litellm_provider"`
	Mode                 string      `json:"mode"`
	Input                json.Number `json:"input_cost_per_token"`
	CachedInput          json.Number `json:"cache_read_input_token_cost"`
	Output               json.Number `json:"output_cost_per_token"`
	Reasoning            json.Number `json:"output_cost_per_reasoning_token"`
	InputAbove200K       json.Number `json:"input_cost_per_token_above_200k_tokens"`
	CachedInputAbove200K json.Number `json:"cache_read_input_token_cost_above_200k_tokens"`
	OutputAbove200K      json.Number `json:"output_cost_per_token_above_200k_tokens"`
	ReasoningAbove200K   json.Number `json:"output_cost_per_reasoning_token_above_200k_tokens"`
}

func FetchGeminiPricing(ctx context.Context, model string) (*Pricing, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	client := &http.Client{
		Timeout:   priceCatalogTimeout,
		Transport: transport,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return fetchGeminiPricing(ctx, client, liteLLMPriceCatalogURL, model)
}

func fetchGeminiPricing(ctx context.Context, client *http.Client, catalogURL, model string) (*Pricing, error) {
	model = strings.TrimPrefix(strings.TrimSpace(model), "models/")
	if model == "" {
		return nil, errors.New("Gemini model is required for pricing lookup")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, catalogURL, nil)
	if err != nil {
		return nil, errors.New("create LiteLLM pricing request")
	}
	response, err := client.Do(req)
	if err != nil {
		return nil, errors.New("fetch LiteLLM pricing catalog")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch LiteLLM pricing catalog: HTTP %d", response.StatusCode)
	}
	if response.ContentLength > maxPriceCatalogBytes {
		return nil, errors.New("LiteLLM pricing catalog exceeds size limit")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxPriceCatalogBytes+1))
	if err != nil {
		return nil, errors.New("read LiteLLM pricing catalog")
	}
	if len(body) > maxPriceCatalogBytes {
		return nil, errors.New("LiteLLM pricing catalog exceeds size limit")
	}
	var catalog map[string]json.RawMessage
	if err := json.Unmarshal(body, &catalog); err != nil {
		return nil, errors.New("decode LiteLLM pricing catalog")
	}
	entry, ok := catalog["gemini/"+model]
	if !ok {
		return nil, errors.New("configured Gemini model is absent from LiteLLM pricing catalog")
	}
	var remote catalogModelPricing
	if err := json.Unmarshal(entry, &remote); err != nil {
		return nil, errors.New("decode selected Gemini catalog entry")
	}
	if remote.Provider != "gemini" || remote.Mode != "chat" {
		return nil, errors.New("selected Gemini model has incompatible LiteLLM pricing metadata")
	}
	standard, err := catalogRates(remote.Input, remote.CachedInput, remote.Output, remote.Reasoning)
	if err != nil {
		return nil, err
	}
	pricing := &Pricing{Standard: standard}
	if remote.InputAbove200K != "" || remote.CachedInputAbove200K != "" || remote.OutputAbove200K != "" || remote.ReasoningAbove200K != "" {
		high, err := catalogRates(remote.InputAbove200K, remote.CachedInputAbove200K, remote.OutputAbove200K, remote.ReasoningAbove200K)
		if err != nil {
			return nil, errors.New("invalid high-context Gemini pricing in LiteLLM catalog")
		}
		pricing.Above200K = &high
	}
	return pricing, nil
}

func catalogRates(input, cached, output, reasoning json.Number) (PricingRates, error) {
	inputPicos, ok := decimalPicos(string(input))
	if !ok {
		return PricingRates{}, errors.New("invalid Gemini input pricing in LiteLLM catalog")
	}
	outputPicos, ok := decimalPicos(string(output))
	if !ok {
		return PricingRates{}, errors.New("invalid Gemini output pricing in LiteLLM catalog")
	}
	rates := PricingRates{InputPicos: inputPicos, OutputPicos: outputPicos, ReasoningPicos: outputPicos}
	if cached != "" {
		cachedPicos, ok := decimalPicos(string(cached))
		if !ok {
			return PricingRates{}, errors.New("invalid Gemini cached-input pricing in LiteLLM catalog")
		}
		rates.CachedInputPicos = &cachedPicos
	}
	if reasoning != "" {
		reasoningPicos, ok := decimalPicos(string(reasoning))
		if !ok {
			return PricingRates{}, errors.New("invalid Gemini reasoning pricing in LiteLLM catalog")
		}
		rates.ReasoningPicos = reasoningPicos
	}
	if !validPricingRates(rates) {
		return PricingRates{}, errors.New("Gemini pricing in LiteLLM catalog is out of range")
	}
	return rates, nil
}

func LiteLLMResponseCostPicos(headers http.Header) *int64 {
	values := headers.Values("X-Litellm-Response-Cost")
	if len(values) != 1 {
		return nil
	}
	cost, ok := roundedDecimalPicos(values[0])
	if !ok {
		return nil
	}
	return &cost
}

func decimalPicos(value string) (int64, bool) {
	return convertDecimalPicos(value, false)
}

func roundedDecimalPicos(value string) (int64, bool) {
	return convertDecimalPicos(value, true)
}

func convertDecimalPicos(value string, round bool) (int64, bool) {
	if value == "" || len(value) > 64 || strings.TrimSpace(value) != value || strings.HasPrefix(value, "+") || strings.HasPrefix(value, "-") {
		return 0, false
	}
	mantissaValue, exponentValue := value, ""
	if exponentIndex := strings.IndexAny(value, "eE"); exponentIndex >= 0 {
		if exponentIndex == 0 || exponentIndex == len(value)-1 || strings.IndexAny(value[exponentIndex+1:], "eE") >= 0 {
			return 0, false
		}
		mantissaValue, exponentValue = value[:exponentIndex], value[exponentIndex+1:]
	}
	exponent := 0
	if exponentValue != "" {
		if len(exponentValue) > 4 {
			return 0, false
		}
		parsed, err := strconv.Atoi(exponentValue)
		if err != nil || parsed < -100 || parsed > 100 {
			return 0, false
		}
		exponent = parsed
	}
	mantissa := strings.Split(mantissaValue, ".")
	if len(mantissa) > 2 || mantissa[0] == "" || (len(mantissa) == 2 && mantissa[1] == "") {
		return 0, false
	}
	fractionDigits := 0
	if len(mantissa) == 2 {
		fractionDigits = len(mantissa[1])
	}
	digits := strings.Join(mantissa, "")
	for _, character := range digits {
		if character < '0' || character > '9' {
			return 0, false
		}
	}
	digits = strings.TrimLeft(digits, "0")
	if digits == "" {
		return 0, true
	}
	scale := 12 + exponent - fractionDigits
	roundUp := false
	if scale < 0 {
		remove := -scale
		if !round {
			if remove >= len(digits) || strings.TrimRight(digits[len(digits)-remove:], "0") != "" {
				return 0, false
			}
			digits = digits[:len(digits)-remove]
		} else if remove > len(digits) {
			digits = ""
		} else {
			cutoff := len(digits) - remove
			roundUp = digits[cutoff] >= '5'
			digits = digits[:cutoff]
		}
		scale = 0
	}
	if digits == "" {
		if roundUp {
			return 1, true
		}
		return 0, true
	}
	if len(digits)+scale > 19 {
		return 0, false
	}
	digits += strings.Repeat("0", scale)
	parsed, err := strconv.ParseInt(digits, 10, 64)
	if err != nil || (roundUp && parsed == math.MaxInt64) {
		return 0, false
	}
	if roundUp {
		parsed++
	}
	return parsed, true
}
