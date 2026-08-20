# Round LiteLLM response costs

Status: proposed

## Goal

Preserve valid LiteLLM-reported generation costs when binary floating-point serialization adds insignificant digits beyond Wormtamer's integer-picodollar storage precision.

LiteLLM can return values such as `0.0035887500000000004`. The current exact decimal conversion rejects the entire value, leaving that generation unpriced and causing aggregate estimates to omit real spend. Round valid endpoint-reported costs to the nearest picodollar instead.

## Scope

Change only parsing of the non-negative USD `x-litellm-response-cost` header used with a custom model endpoint.

Keep exact conversion for LiteLLM catalog token rates so this fix does not silently alter configured pricing inputs. Continue rejecting missing or repeated headers, malformed numbers, negative values, unsupported exponents, and values outside the `int64` picodollar range.

Use deterministic nearest-picodollar rounding, with half-picodollar ties rounded up because accepted costs are non-negative. The maximum representation error is therefore one half picodollar per generation.

This plan does not include:

- Schema changes or historical backfill. SQLite does not retain rejected header values, so prior unavailable costs cannot be reconstructed locally.
- LiteLLM log or billing API integration, invoice reconciliation, or changes to displayed estimate semantics.
- Local repricing of custom-endpoint requests from token counts when no valid response-cost header is available.

## Approach

Keep the existing exact decimal parser for catalog rates. Add the smallest separate conversion path needed by `LiteLLMResponseCostPicos` to parse bounded decimal or scientific notation without binary floating-point arithmetic, scale it to picodollars, and round discarded decimal digits to the nearest integer picodollar.

Pass the rounded value through the existing endpoint-cost recording path. It remains persisted as `cost_source = 'endpoint_response'` and participates in existing aggregates without database or panel changes.

Document nearest-picodollar normalization alongside the custom-endpoint response-cost contract in the focused reliability documentation.

## Verification

- Observed LiteLLM values such as `0.0035887500000000004` and `0.0049949999999999994` become `3,588,750,000` and `4,995,000,000` picodollars rather than unavailable costs.
- Existing exactly representable decimal and scientific-notation headers retain their current values.
- Values immediately below, at, and above a half-picodollar boundary demonstrate the documented rounding rule.
- Missing, repeated, malformed, negative, unsupported-exponent, and overflowing headers remain unavailable.
- LiteLLM catalog rates with non-zero precision beyond one picodollar remain rejected rather than rounded.
- A returned generation with a rounded endpoint cost persists that value and includes it in the existing aggregate estimate; failed requests and responses without a valid header remain unpriced.
