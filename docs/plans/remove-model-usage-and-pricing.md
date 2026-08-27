# Remove model usage and pricing persistence

Status: approved

## Goal

Remove durable billing-like model diagnostics, including public pricing lookup, cost estimation, generation records, and usage reporting. Model calls should depend only on review or memory workflow state, not on a secondary diagnostic checkpoint.

## Scope

Remove:

- LiteLLM public pricing-catalog retrieval at startup
- custom-endpoint response-cost parsing
- fixed-point price and cost calculations
- durable model-generation records and token aggregates
- `/usage` and generation-detail panel routes

The read-only panel remains with overview, review, feedback, and runtime-memory views. Review and memory workflows remain enabled for direct Gemini Developer API use and configured compatible endpoints. Existing bounded operational generation logs remain where they do not require new persistence or a replacement telemetry subsystem.

Loss of persisted usage and cost history is accepted. The project has no production state-compatibility baseline, so this plan requires a schema rebaseline rather than a migration or preservation of obsolete diagnostic rows.

## Approach

- Remove pricing retrieval and recorder construction from startup, including interrupted-generation reconciliation.
- Remove usage scope, start/completion checkpoints, cost-source handling, and generation-recorder dependencies from both review generation and memory evaluation.
- Simplify SDK response adaptation by retaining only response fields needed for model validation and ordinary structured logging.
- Remove generation persistence, aggregate queries, panel view models, filters, detail sections, templates, and schema objects.
- Remove pricing, billing-estimate, generation-retention, and usage-panel requirements from focused documentation without weakening workflow persistence requirements.
- Coordinate the final schema rebaseline with the lease-removal plan when both are implemented in the same unreleased sequence.

## Verification

- Service startup makes no LiteLLM pricing-catalog request.
- A review or memory Gemini request is not prevented or retried because a diagnostic generation row cannot be created or completed.
- Returned model responses and request failures retain their existing review and memory workflow validation, retry, and terminal-failure behavior.
- Direct Gemini and configured compatible endpoints continue to work without interpreting a response-cost header.
- The database contains no model-generation, token-usage, pricing, or estimated-cost state.
- `/usage` and generation-detail routes return `404`; overview, review, feedback, and memory panel routes remain available.
