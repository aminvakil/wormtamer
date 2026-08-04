# Recover from oversized repository tool requests

Status: proposed

## Goal

Allow a review to continue when a valid internal repository tool request exceeds its bounded output limit and Gemini can retry with a narrower path, query, or line range. A broad but authorized request should consume bounded resources without permanently failing the review.

## Scope

- Treat the non-retryable `repository_tool_output_limit_exceeded` category as model-correctable in the explicit Gemini tool loop.
- Return the existing bounded error-only function response so Gemini may issue a narrower request on a later turn.
- Clarify the internal repository listing tool description to prefer a relevant subdirectory when one is known.
- Continue counting the failed request against repository and total tool-call limits.
- Keep all existing per-call, per-review, file-count, byte, time, and repository limits unchanged.

Do not truncate tool results, automatically choose a path, increase limits, retry the same request in application code, or broaden this behavior to public-source, aggregate-result, archive, authorization, secret, or persistence failures.

## Approach

Add `repository_tool_output_limit_exceeded` to the existing allowlist of failures that are returned to Gemini as a structured `{ "error": <category> }` function response. Reuse the current correction path rather than adding a second retry mechanism. The failed tool call has already been charged before dispatch, so repeated broad requests remain bounded by the existing eight internal-repository calls and combined tool-call ceiling.

Update the `list_repository_files` declaration to state that listing is recursive and bounded and that Gemini should supply the narrowest relevant `path` when known. Keep `path` optional because small repository-root listings remain valid and useful.

The output-limit category can arise from listing, reading, or searching internal repository content. Returning only its category discloses no partial content; Gemini may narrow the applicable path, query, or line range or finish without that context. Keep `repository_search_limit_exceeded` and every security-boundary or broker failure outside the model-correctable allowlist.

## Risks and Open Questions

Gemini may spend additional calls repeating an oversized request or may finish without verifying the desired context. Existing call ceilings bound that behavior, and preserving model choice is preferable to silent truncation or application-selected context.

The shared output-limit category does not identify which narrower argument is needed. Tool declarations provide the available arguments, and the concrete category is sufficient for the smallest implementation; do not add paths, counts, limits, or private repository metadata to error responses without demonstrated need.

## Verification

- An oversized root `list_repository_files` request returns only `repository_tool_output_limit_exceeded` to Gemini instead of terminating the review.
- Gemini can follow that response with a listing scoped to a small subdirectory and complete the review using the attributed result.
- The oversized request and corrected request each consume one internal-repository tool call.
- Repeated oversized requests still stop at the existing tool-call ceiling.
- No partial paths or repository content from the oversized result are returned or logged as a tool result.
- Archive validation, repository authorization, secret detection, aggregate tool-result limits, persistence failures, and retryable broker failures continue to terminate or defer the review under their existing semantics.
