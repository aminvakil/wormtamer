# Remove process-local diagnostic buffers

Status: approved

## Goal

Remove the second in-memory conversation and log recording system. Use the existing structured stderr logs for model diagnostics while keeping diagnostic state out of review execution and the web process.

## Scope

Remove buffered conversations, buffered structured logs, their custom `slog` observer, and the `/diagnostics/conversations` and `/diagnostics/logs` panel routes.

Retain normal JSON stderr logging. At debug level, retain the existing application-visible review and memory-evaluation diagnostics: system instructions, prompts, accepted model content, tool calls and admitted results, and validated decisions. Known configured credentials must still be replaced before those values are logged. Info-level logging remains content-free.

The read-only panel itself remains, including review, feedback, runtime-memory, and model-usage views that belong to other subsystems. Durable model-generation records remain until the separate model-usage plan is implemented. This plan does not change runtime review memory, patch equivalence, or workflow behavior.

## Approach

- Remove the concurrency-safe conversation and log buffers, size accounting, cloning, eviction policies, snapshots, and generation-observer wrapper.
- Remove the custom tee handler and panel interfaces, handlers, templates, filters, and links used only by process-local diagnostics.
- Remove conversation-recorder callbacks from both review generation and memory evaluation. They continue to emit their existing structured logs directly.
- Retain only the small shared credential-redaction behavior needed by content-bearing stderr log call sites; do not replace the removed buffers with another recorder or logging backend.
- Update architecture, security, deployment, and panel documentation so stderr is the sole source of content-bearing diagnostics.

## Verification

- Review generation and memory evaluation produce the same validated workflow outcomes without a diagnostic recorder.
- At info level, prompts, model responses, tool arguments, tool results, comments, lessons, and repository content are absent from logs.
- At debug level, the documented application-visible diagnostic content remains available through stderr and known configured credentials are redacted, including their JSON-escaped forms.
- Diagnostic recording or rendering can no longer consume bounded process-local conversation or log memory or affect a model-generation checkpoint.
- Diagnostic panel routes return `404`; the remaining panel routes continue to render with their existing security headers and escaping.
