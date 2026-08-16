# Add a read-only web panel

Status: proposed

## Goal

Provide a small built-in web panel for understanding Wormtamer's current workload, completed reviews, feedback processing, runtime memory, and effective non-secret configuration without requiring direct SQLite access or reconstructing state from logs.

The panel is an observation surface only. It must not change workflow state, configuration, GitLab content, runtime memory, or any other application data.

## Scope

This plan includes server-rendered HTML views for:

- An overview with review and feedback job counts by state, the oldest queued work, recent review and feedback activity, active-memory count, and effective non-secret configuration.
- A bounded, paginated review list showing numeric project and merge request identity, project path when available from the source event, short head SHA, source type, state, attempt count, timestamps, finding count, publication status, and last error category.
- A review detail view showing the immutable review identity, complete locally validated summary and findings, application-owned finding identifiers, publication status, and the identities and timestamps of memory retrieved for that review.
- A bounded, paginated feedback-job list showing its bound review, numeric project, merge request and note identities, state, attempt count, timestamps, decision count, active-lesson count, and last error category.
- A bounded, paginated memory view showing active and inactive records, repository scope, target type and identity, outcome, confidence, lesson when present, source role and URL, and update time.

The configuration section may show the GitLab base URL, configured Gemini endpoint and model, thinking level, log level, authorized repositories, effective repository-sharing mode, and approved public sources. It must be built from an explicit display type that cannot contain credentials, filesystem paths, or other omitted configuration fields.

The panel is served by the existing process and listener. It is always available, requires no new configuration, uses the standard library HTTP server and templates, and performs no external GitLab, Gemini, repository, or public-source requests while rendering a page.

The panel has these permanent constraints:

- It never implements POST, PUT, PATCH, DELETE, or any other behavior that mutates application or external state.
- It never retries, cancels, or requeues jobs or creates reviews manually.
- It never edits configuration or runtime memory.
- It never adds a SQLite schema migration solely for presentation. Reconciled jobs without an associated source path display their numeric project ID rather than fetching or inventing a path.

This plan does not include:

- Raw webhook payloads, comment bodies, stored error messages, publication markers, arbitrary repository browsing, or credentials.
- Live component heartbeats, connectivity claims, or reconciliation history.
- A JSON API, client-side application framework, JavaScript, charts, or a new frontend build process.

Model usage and cost reporting and bounded diagnostic conversation and log views are separate dependent plans because they require data that the core panel does not currently retain.

## Approach

### HTTP and presentation

Add a focused `internal/panel` package with a narrow read-only store interface. It owns embedded `html/template` files and a small embedded stylesheet. The application composes its routes with the existing healthcheck and webhook routes rather than coupling presentation to webhook ingress.

Use these routes:

- `GET /` for the overview.
- `GET /reviews` and `GET /reviews/{job-id}` for review history and detail.
- `GET /feedback` for feedback processing history.
- `GET /memory` for feedback decisions and runtime lessons.
- `GET /assets/panel.css` for the embedded stylesheet.

All collection routes use fixed maximum page sizes and validated pagination and filter values. Useful filters remain limited to values already represented deterministically in SQLite, such as job state and active-memory state. Invalid query values return `400`, unknown detail identities return `404`, and internal failures return a generic `500` without exposing stored data or query details.

Render review text, findings, paths, lessons, source metadata, and all other persisted or model-derived values as untrusted text through `html/template`; do not interpret them as HTML or Markdown. Only emit links that are constructed from or validated against the configured GitLab base URL. Responses containing application state use `Cache-Control: no-store` and restrictive content-type, framing, referrer, and content security headers. The panel uses no inline script or third-party assets.

### Read model

Add explicit store query methods and display-oriented records for:

- Overview counts and oldest queued timestamps.
- Recent and paginated review jobs with aggregate finding and publication information.
- One review with its decoded stored result, ordered finding IDs, publication record, and memory-retrieval audit.
- Recent and paginated feedback jobs with aggregate decision and active-lesson information.
- Paginated review-memory records with their repository and evaluated-source context.

Keep every query read-only, context-aware, bounded in returned rows, and free of raw payload and error-message columns. Do not hold a transaction while rendering. Decode and validate stored review JSON through the existing review package; distinguish a valid external-only publication, which has no local structured result, from corrupt or inconsistent local state.

Use source-event presence to label a review as webhook-originated or reconciled. Show the source event's project path when one is directly associated; otherwise fall back to the durable numeric project ID. Page rendering must not trigger network lookup to fill presentation gaps.

### Integration and documentation

Construct the panel after configuration and SQLite startup, pass only its narrow store dependency and explicit non-secret configuration view, and mount it on the existing HTTP server. Keep `/healthcheck` and `/webhooks/gitlab` behavior and limits unchanged.

After implementation, update the focused architecture, reliability, security, deployment, and top-level documentation to describe the read-only views, their data sources and exclusions, and the rule that operational mutations remain outside the panel. Remove the completed plan after those durable decisions have authoritative homes.

## Risks and Open Questions

- Review summaries, findings, lessons, repository names, and source metadata may contain private or hostile text. Plain text rendering, automatic escaping, restrictive response headers, and the exclusion of raw payloads and debug material are required boundaries rather than presentation details.
- Dashboard counts and history queries share SQLite with webhook and worker traffic. Queries must remain short and bounded, and the implementation should prefer simple indexed state and identity lookups over expensive analytics.
- Existing state does not reliably retain a project path for every reconciled job. Numeric-ID fallback is intentionally preferable to adding persistent state or synchronous GitLab reads for this outcome.
- The panel reflects durable state, not guaranteed current external connectivity. Labels must not imply that GitLab, Gemini, reconciliation, or a worker is healthy merely because the process can render a page.

## Verification

- The overview accurately reflects persisted review, feedback, publication, finding, and active-memory state while an empty database still renders a useful page.
- Review lists and details distinguish queued, running, publishing, completed, failed, obsolete, and external-only recovered reviews and show only the documented fields.
- Feedback and memory views represent completed evaluations, decisions without lessons, active lessons, and deactivated lessons without retrieving source comment bodies.
- Pagination and supported filters are deterministic and bounded; malformed values fail without executing broad fallback queries.
- Every panel route is observational: requests cannot retry jobs, mutate SQLite, publish to GitLab, call Gemini, fetch repositories, or perform public network access.
- Hostile model output and lessons containing HTML, Markdown, mentions, malformed links, or control-like text remain inert visible text in the rendered response.
- Credentials, raw webhook payloads, stored error messages, publication markers, filesystem paths, prompts, tool traces, and debug logs never appear in panel responses.
- Existing healthcheck and webhook route behavior, request limits, persistence ordering, and worker operation remain unchanged when panel routes are added.
