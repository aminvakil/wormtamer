# Add model usage and cost reporting

Status: proposed

Depends on:

- [Read-only web panel](../agents/architecture.md#read-only-web-panel)

## Goal

Record and display installation-local Gemini usage for review and feedback generation so an operator can understand token consumption, model behavior, and estimated cost by request, job, repository, and time period.

This is application-observed diagnostic reporting, not an authoritative provider invoice. The views remain strictly read-only and cannot alter jobs, model behavior, pricing, or retained records.

## Scope

Capture one structured diagnostic record for every application-level Gemini generation request made by review or feedback processing, including generations whose response is incomplete, fails local validation, or belongs to a workflow attempt that is later retried.

When supplied by the configured endpoint, record:

- Prompt, cached-content, tool-use prompt, candidate, thought, and total token counts.
- Configured model, resolved model version, request kind, workflow job and attempt, review turn, and request time.
- Latency, finish reason, structured-validation outcome, declared tool-call count and names, and whether final-only mode applied.
- Completion state distinguishing a returned response, a failed request without usage metadata, and a request whose outcome became unknown during interruption.

Add read-only panel views that provide:

- Totals for review and feedback generation over bounded day, week, and month windows.
- Breakdown by configured or resolved model, repository, request kind, and token category.
- Per-review and per-feedback-job generation history, including failed workflow attempts that consumed observable usage.
- Estimated cost totals and per-generation estimates when explicit pricing is configured.
- Clear unavailable or incomplete labels when the endpoint omits metadata, the application did not observe a completed response, or no applicable pricing exists.

Cost estimates use explicit configuration rather than hard-coded or remotely fetched model prices. Pricing defines a currency and per-million-token rates for uncached input, cached input, and output. Tool-use prompt tokens use the input rate; thought tokens use the output rate. Persist the pricing snapshot applied to each estimate so changing configuration does not silently rewrite historical estimates. When cached-token metadata is present, subtract it from total prompt tokens before applying the uncached input rate.

This plan does not include:

- Provider billing API access, invoice reconciliation, budgets, alerts, quotas, or spend controls.
- Inferred usage for failed HTTP attempts or SDK-internal retries that return no usage metadata.
- Model prompts, responses, tool arguments, tool results, or application logs; those belong to the separate diagnostic-content plan.
- Contributor scoring, cross-installation reporting, or a general analytics platform.
- Panel controls for changing pricing or deleting usage history.

## Approach

### Durable generation records

Add a focused diagnostic store contract and a sequential SQLite migration for model-generation records. This migration represents new operational evidence that Wormtamer does not currently retain; it must not denormalize existing workflow state merely to simplify presentation.

Create a generation record before each application-level SDK call and complete it after the call returns or fails. Associate it with the review or feedback job, workflow attempt, and review turn using application-owned identity. Repeated work is recorded as repeated usage rather than deduplicated under review identity, because retries can incur real additional consumption.

A started record that is never completed remains an explicit unknown outcome after a crash. The SDK may retry HTTP internally; because it exposes only the final response metadata, one application-level record must not pretend to enumerate or cost hidden transport attempts.

Capture every available token field from Gemini usage metadata rather than the narrower subset currently written to logs. Extend feedback generation to preserve the same metadata through its existing narrow Gemini test seam. Validate token counts as non-negative bounded integers and preserve absence separately from a reported zero.

Diagnostic persistence happens outside the validated review-result and publication transactions. A failure to create the pre-request record prevents the external model request. A failure to checkpoint a returned response is a persistence failure and leaves the pre-request record visibly incomplete; it must not fabricate usage from candidate text.

### Pricing and estimates

Extend strict deployment configuration with one optional explicit pricing block for the configured model endpoint. Validate non-negative decimal rates and a bounded currency label. Convert rates to an integer fixed-point representation rather than accumulating binary floating-point values.

Calculate and persist each estimate from observed token categories and the current pricing snapshot when the generation response is checkpointed. If required categories are absent or internally inconsistent, retain the observed counts but mark the cost unavailable. Display the formula and rate snapshot on generation detail so an estimate is reproducible.

### Panel integration

Add a `GET /usage` aggregate view and generation history sections on review and feedback pages. Queries use bounded time windows, validated filters, fixed page sizes, and aggregate only structured diagnostic columns. They never load model content.

Use project path only when existing durable workflow state provides it; otherwise display the numeric project ID. Aggregate labels must state that totals cover only observed application records and may differ from endpoint billing.

Update focused architecture, reliability, security, deployment, and configuration documentation with the diagnostic state, its incomplete-record semantics, pricing source, and reporting limitations. Keep the durable rule that no panel route mutates this state.

## Risks and Open Questions

- A custom endpoint may omit fields, define token categories differently, or bill differently from Gemini. Explicit rates and unavailable labels limit false precision but cannot make the report authoritative.
- A process or storage failure between the external response and its checkpoint can leave usage unknown. Pre-request records expose this gap but cannot recover metadata that was never durably observed.
- Usage records grow with every generation, including failed review attempts. A retention policy is not yet established; define one before implementation if unbounded installation-local history is unacceptable.

## Verification

- Every application-level review and feedback generation has a durable started record before the SDK call, and returned metadata completes the same record.
- Retried workflow attempts and multiple tool turns appear as separate generation usage rather than being collapsed into the final published review.
- All endpoint-provided token categories, resolved model information, latency, finish reason, validation outcome, and tool-call metadata round-trip without model content.
- Missing metadata, interrupted requests, failed SDK calls, and inconsistent totals render as explicit unavailable or incomplete states and never as zero cost.
- Cost estimates use only the persisted rate snapshot and observed token categories, remain stable after configuration changes, and are identified as estimates.
- Usage pages and review or feedback detail sections are bounded, read-only, and perform no external requests.
- Credentials, prompts, responses, tool arguments and results, repository content, comments, and logs cannot enter generation usage records or responses.
