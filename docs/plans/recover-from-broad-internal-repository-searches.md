# Recover from broad internal repository searches

Status: proposed

## Goal

Let Gemini retry an internal repository search with a narrower path when the search exceeds its bounded scan limit, while ensuring repository output-limit recovery remains restricted to internal repository tools.

## Scope

- Treat `repository_search_limit_exceeded` as model-correctable only for `search_repository`.
- Treat `repository_tool_output_limit_exceeded` as model-correctable only for internal repository listing, reading, and searching.
- Return only the existing bounded `{ "error": <category> }` function response and let Gemini choose whether and how to retry.
- Clarify that internal repository search is recursive and bounded and should use the narrowest relevant `path` when known.
- Continue charging every failed request against the internal repository and combined tool-call ceilings.
- Preserve all current scan, output, result, repository, call, and time limits.

Do not truncate results, select a narrower path in application code, increase limits, or retry automatically. Do not add model-correction behavior for public URL or public GitHub repository size limits, invalid requests, redirects, or blocked destinations. Keep authorization, secret detection, archive validation, aggregate tool-result, persistence, infrastructure, and security-boundary failures under their existing semantics.

## Approach

Make correction classification consider both the requested tool and the failure category. Allow the repository output-limit category only for `list_repository_files`, `read_repository_file`, and `search_repository`; allow the search scan-limit category only for `search_repository`. Keep the existing correction behavior for argument, path, repository-selection, memory, and approved public-source failures unchanged.

Tool identity is required because public GitHub list and read operations reuse the repository workspace and can surface repository-prefixed failures. A shared category must not silently broaden recovery across the public-source boundary.

Update the `search_repository` declaration to explain that search recursively scans bounded text content and that Gemini should supply the narrowest relevant path when known. Keep `path` optional because root searches remain valid for repositories within the scan bound.

The review loop already charges calls before broker dispatch. Reuse that accounting and the existing error-only function response so repeated broad searches remain bounded without another retry mechanism or new state.

## Risks and Open Questions

Gemini may repeat a broad search or finish without the missing context. Existing internal and combined call ceilings bound this behavior.

A narrower path can avoid the scan-byte limit, but there may be no useful subdirectory known to the model. Do not expose scanned paths, byte counts, partial matches, or limit values in the error response to guide it.

## Verification

- A root `search_repository` request that exceeds the scan limit returns only `repository_search_limit_exceeded` to Gemini instead of terminating the review.
- Gemini can retry the search under a narrower path and complete the review using the attributed result.
- The broad and narrowed searches each consume one internal repository and combined tool call; repeated broad searches still stop at the existing ceiling.
- No partial matches, paths, or repository content from the failed search are returned or logged as a tool result.
- Internal repository listing, reading, and searching continue to recover from `repository_tool_output_limit_exceeded` with the existing bounded response.
- A public URL or public GitHub repository call that surfaces an output or response limit does not inherit internal repository recovery.
- `repository_search_limit_exceeded` from any tool other than `search_repository` remains terminal.
- Authorization, secret detection, archive validation, aggregate tool-result limits, retryable broker failures, and all other security-boundary failures retain their current behavior.
