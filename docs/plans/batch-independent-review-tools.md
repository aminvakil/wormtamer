# Batch independent review tool calls

Status: proposed

## Goal

Reduce Gemini round trips by directing the model to request independent repository reads, searches, memory lookups, or public-source reads together in one function-calling turn when none of their arguments depends on another call's result.

## Scope

- Add concise model-facing guidance to batch independent tool calls in one turn.
- Clarify that dependent exploration remains sequential; for example, a path discovered by a search must not be guessed in the same batch.
- Verify that multiple valid calls in one model turn are all brokered, charged individually against their category and combined limits, and returned together before the next generation.
- Use existing `tool_call_count` and `tool_names` generation telemetry to assess deployed behavior.

Do not parallelize broker execution, increase tool budgets, proactively choose tools in application code, add repository caching, or treat batching as permission to broaden searches.

## Approach

1. Extend the review system instruction with one direct rule: batch independent calls whose complete arguments are already known, while keeping calls that depend on earlier results sequential.
2. Keep broker dispatch ordered and sequential. Batching saves model generations without introducing concurrent workspace initialization, result-order ambiguity, or new resource contention.
3. Add a review-loop contract test in which one model turn requests multiple independent valid tools, all calls are dispatched and charged, their function responses are returned in the same order, and the next turn supplies the final review.
4. Retain the existing preference for exact reads and narrowly scoped searches; batching changes turn grouping, not evidence scope.

## Risks and Open Questions

- Prompting influences but cannot guarantee model behavior. Existing telemetry is sufficient to determine whether deployed reviews begin producing multi-call turns.
- Batched output can consume the aggregate tool-result budget more quickly, but every call remains subject to existing per-call and aggregate limits.
- A broker failure in one batched call retains the review loop's established failure semantics; this plan does not introduce partial retry or parallel cancellation behavior.

## Verification

- The model contract explicitly distinguishes independent calls that should be batched from dependent calls that must remain sequential.
- Multiple calls returned in one turn preserve call IDs and order through their function responses.
- Every batched call is authorized, dispatched, and charged independently under existing limits.
- No broker call runs concurrently and no tool argument is inferred from another result in the same batch by application code.
- Generation logs continue to expose bounded per-turn call counts and declared tool names so batching effectiveness can be observed without debug content.
