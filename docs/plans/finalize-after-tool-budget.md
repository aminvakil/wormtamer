# Finalize reviews after tool budgets are exhausted

Status: proposed

## Goal

Preserve the in-memory Gemini conversation and produce the best supported final review when a tool category or the combined tool budget is exhausted. A model request beyond a limit must never execute the excess tool or restart the entire review solely because of that request.

## Scope

- Derive the tools declared on each generation from the remaining internal-repository, memory, public-source, and combined call budgets.
- Remove only an exhausted category while other categories and the combined budget remain available.
- Disable function calling and require a structured final response when the combined budget is exhausted.
- Handle same-turn batches deterministically: dispatch only calls that fit the remaining budgets and return bounded limit responses for excess calls.
- If Gemini requests a tool that is no longer available, do not dispatch it and make the following generation final-only.
- Preserve the existing local output validation, broker authorization, per-call result limits, correctable tool-failure accounting, and two-minute review deadline.

Do not raise tool limits, persist model conversations, add automatic tool execution, or weaken retries for genuinely incomplete, invalid, or timed-out final responses.

## Approach

1. Pass the currently available tool categories into generation configuration instead of declaring every tool on every turn.
2. Track remaining category and combined budgets in the trusted review loop. Decrement them only for calls admitted for dispatch; existing dispatched calls that return correctable limit failures remain charged.
3. Process calls in model response order. Return a function response containing only the stable applicable limit category for each denied excess call, without invoking its broker.
4. After any request for an unavailable tool, issue one generation with function calling disabled so Gemini must synthesize from evidence already collected. When all combined calls have been admitted, enter the same final-only mode directly.
5. Accept only a locally validated structured result from final-only mode. Continue to retry the job for timeout, incomplete finish, malformed content, or invalid structured output.
6. Emit bounded operational telemetry when final-only mode is entered, without logging tool arguments or results at info level.

## Risks and Open Questions

- A model may return a function call despite function calling being disabled. Treat this as an invalid final response rather than executing it or reopening tools.
- A same-turn batch can cross more than one category limit. Responses must retain the original call IDs and order so the Gemini conversation remains well formed.
- Dynamic declarations and function-calling mode `NONE` rely on the native Gemini behavior already required by the compatibility baseline; no compatibility fallback is in scope.

## Verification

- After eight admitted internal-repository calls, subsequent generations no longer declare internal-repository tools while eligible memory or public-source tools remain declared.
- After sixteen admitted combined calls, the next generation declares no callable tools and can return a valid final review.
- A ninth internal-repository request is not dispatched, does not discard prior tool evidence, and can be followed by a valid final result in the same review attempt.
- A same-turn batch never dispatches more calls than any category or combined budget allows, and every function call receives an ordered response.
- Invalid or incomplete final-only output retains existing retry behavior.
