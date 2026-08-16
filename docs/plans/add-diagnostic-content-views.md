# Add diagnostic conversation and log views

Status: proposed

Depends on:

- [Read-only web panel](add-read-only-web-panel.md)
- [Model usage and cost reporting](add-model-usage-reporting.md)

## Goal

Provide bounded read-only views of recent model conversations and structured application logs for diagnosing review and feedback behavior without requiring the panel to read container logs or create an unbounded archive of private diagnostic content.

Diagnostic content is process-local and intentionally separate from durable workflow and usage records.

## Scope

Add a bounded recent-conversations view for review and feedback generation. A review conversation may contain:

- The system instruction and initial merge request prompt sent to Gemini.
- Complete returned model turns, including final response text and declared function calls.
- Admitted tool responses and fixed application denial responses in conversation order.
- Turn, configured and resolved model, latency, finish reason, validation outcome, and correlated generation identity.

A feedback conversation may contain its system instruction, feedback prompt, returned model content, validated decision, generation metadata, and correlated feedback and review identity.

Do not expose hidden model reasoning. Wormtamer does not request textual thoughts, and thought signatures or SDK protocol data are not conversation content.

Add a bounded recent-logs view containing structured events emitted by the application at the configured log level, with timestamp, level, message, and bounded attributes. Preserve normal stderr logging; the panel sink is an additional process-local observer and does not read files, container APIs, or an external logging service.

Full conversation capture is available only when diagnostic content logging is enabled by the existing `debug` log level. At other levels, the conversation view shows only non-content generation metadata supplied by the usage records. The logs view likewise reflects only events enabled at the configured level.

The views may include private diffs, repository excerpts returned by tools, transient feedback comments included in prompts, memory lessons, public-source content, and model output. Known configured credentials must be redacted before diagnostic content enters the bounded buffer, using the existing whole-value diagnostic redaction rules. Content may still contain private data and secrets unknown to Wormtamer.

This plan also includes adjacent diagnostic navigation:

- Correlation from a conversation to its review or feedback job and structured usage records.
- Filters for level, component, job kind, numeric project, merge request, and generation identity when those fields exist.
- Explicit buffer start time, eviction indicators, and content-omitted markers so absence is not presented as proof that an event did not happen.

This plan does not include:

- Durable storage of conversations or logs, a SQLite migration for their presentation, or history from before the current process start.
- Arbitrary repository browsing, raw webhook payload views, source comment retrieval, hidden thoughts, shell access, or direct file and container log access.
- Log-level changes, capture controls, deletion, export, replay, or any other mutation from the panel.
- Full-text indexing, cross-installation aggregation, third-party log shipping, or a general observability platform.

## Approach

### Bounded process-local recorder

Add a small `internal/diagnostics` component with separate concurrency-safe buffers for structured log events and model conversations. Use fixed application-owned entry, per-record byte, and total-byte ceilings. Evict the oldest complete records when a ceiling is reached; never let diagnostic buffering block webhook persistence, worker checkpoints, shutdown, or normal stderr logging.

If one event or conversation exceeds its individual limit, retain bounded identity and generation metadata with a `content_omitted=limit_exceeded` marker rather than truncating private structured content into a misleading partial record. Buffer state resets on process restart.

Wrap the configured `slog.Handler` with a tee that keeps normal JSON stderr output and sends enabled structured events to the log buffer. Content-bearing Gemini debug events should refer to the dedicated conversation record in the panel buffer rather than duplicating large prompts and tool results into the panel log buffer; stderr behavior remains unchanged.

Instrument the explicit review tool loop and feedback evaluator through a narrow recorder interface. Record the exact application-visible conversation sequence only after existing tool authorization, bounds, and configured-secret checks have succeeded. Correlate each model turn with the durable generation identity from the usage-reporting plan. Recorder failures or evictions affect diagnostics only and never alter model dispatch, local validation, job retry, publication, or feedback decisions.

### Panel views

Add:

- `GET /diagnostics/conversations` for bounded conversation history and filters.
- `GET /diagnostics/conversations/{generation-id}` for one escaped, ordered conversation.
- `GET /diagnostics/logs` for bounded structured log history and filters.

Render all content as escaped plain text without Markdown or HTML interpretation. Reuse the panel's no-store and restrictive response headers. Do not generate links from model- or log-supplied URLs; only application-owned navigation identities become links.

A conversation page distinguishes content not captured because debug logging was disabled, content evicted by buffer limits, content omitted because one record exceeded a limit, and a generation that produced no completed response. A log page states the process-local time range and whether older entries were evicted.

Update focused architecture, security, and deployment documentation with the process-local semantics, debug-only content boundary, fixed limits, redaction behavior, and the fact that unknown secrets and private source may appear. Keep operational mutation commands and configuration changes outside the panel.

## Risks and Open Questions

- Debug conversations intentionally include highly sensitive source and comment content. Bounded process-local retention reduces lifetime but does not make that content non-sensitive.
- Extending `slog` with a buffer must preserve handler groups, attributes, level filtering, and concurrency behavior without allowing slow panel readers to block application logging.
- Conversation capture and stderr debug events can duplicate large content in memory if they are not separated carefully.
- Exact fixed entry and byte ceilings should be selected from the existing maximum prompt, response, and aggregate tool-result sizes during implementation and verified against worst-case bounded conversations.

## Verification

- With debug logging enabled, review and feedback conversations render in exact application-visible order with their generation, workflow, and usage identities.
- With debug logging disabled, no prompts, model content, tool arguments, tool results, diffs, comments, lessons, or repository excerpts enter the conversation buffer.
- Thought signatures and hidden reasoning never appear even when other diagnostic content is captured.
- Structured logs continue to reach stderr unchanged while the panel shows only events enabled at the configured level.
- Buffer entry, record-size, and aggregate-byte ceilings remain enforced under concurrent logging and model activity; eviction and omission are visible and never produce partial content presented as complete.
- A restart clears conversations and panel log history without affecting durable workflow, memory, publication, or usage records.
- Known configured credentials are redacted before buffering, and hostile HTML, Markdown, URLs, and terminal-like control text remain inert escaped text.
- Viewing or filtering diagnostics performs no SQLite mutation, external request, model call, repository access, source-comment retrieval, or logging configuration change.
