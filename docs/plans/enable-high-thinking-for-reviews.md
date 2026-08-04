# Enable high thinking for merge request reviews

Status: proposed

Depends on:

- [Evaluate review generation settings on current merge requests](evaluate-review-generation-settings.md)

## Goal

Apply the generation settings selected by the current-MR evaluation so merge request reviews use Gemini's high thinking level with a sufficient bounded output budget.

## Scope

- Set the application-owned thinking level for merge request review generations to `high`.
- Keep thought text excluded from responses and logs while preserving any model-required thought signatures across function-calling turns.
- Use the smallest output-token ceiling accepted by the evaluation plan.
- Handle incomplete candidate finish reasons according to the evaluated response behavior.
- Add focused deterministic tests for the owned generation configuration and response handling.

Apply high thinking only to merge request review generation. Do not change feedback evaluation, add another review or verification pass, request or retain thought text, expose thinking or token settings as deployment configuration, enable automatic tools, add candidates, change sampling settings, or weaken local result and tool validation.

## Approach

Configure the review `GenerateContent` calls with `ThinkingLevelHigh` and thought inclusion disabled. Keep generation settings application-owned as required by the architecture.

Retain model-required thought signatures in the existing conversation when returning brokered function results, but continue excluding thought text from parsed output, diagnostics, persistence, and publication.

Adopt the evaluated per-generation output ceiling without changing the 64 KiB validated-result limit. Reject or classify an incomplete finish such as `MAX_TOKENS` before accepting a function call or final structured result. Optional usage metadata may support diagnostics but must not become required for successful Gemini API responses.

## Risks and Open Questions

High thinking consumes more free-tier quota and time. The preceding evaluation must show that the selected settings remain within the existing review deadline and produce suitable findings on the observed cases.

SDK usage fields may be absent even when generation succeeds. Finish reason and valid structured completion remain authoritative; missing optional accounting must not fail a review.

## Verification

- Every merge request review generation requests high thinking and does not request returned thought text.
- Function-calling conversations preserve model-required thought signatures without logging or persisting thought content.
- Feedback evaluation retains its current generation settings.
- The configured output ceiling exactly matches the smallest setting accepted by the evaluation plan.
- `MAX_TOKENS` and other incomplete finishes cannot be accepted as completed tool or final-result turns.
- Missing optional token-usage metadata does not fail an otherwise valid response.
- Existing two-minute review deadlines, structured-output bounds, configured-secret checks, tool authorization and limits, final-path validation, persistence, and publication behavior remain unchanged.
