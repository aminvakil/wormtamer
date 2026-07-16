# Recover missed merge request revisions through reconciliation

Status: proposed

## Goal

Recover reviewable merge request revisions that webhooks did not deliver. A periodic reconciler in the existing Wormtamer process scans every authorized GitLab project and idempotently creates review jobs using the same review identity as webhook ingress.

The first scan runs immediately after startup. A successful scan queues existing open merge requests that are ready at the time they are observed, allowing a new deployment to discover work that predates it and later scans to discover draft-to-ready transitions.

## Scope

Include:

- One periodic reconciler in the existing process, with no overlapping scans
- Exact configured-project authorization and numeric project identity validation
- Bounded pagination of open merge requests in every authorized project
- Basic validation of merge request identity, current head SHA, and readiness from GitLab 17 or newer responses
- Idempotent review job creation without synthetic webhook events
- A schema migration allowing reconciled jobs to have no source webhook event
- An immediate startup scan followed by a fixed five-minute interval
- Application-wide, process-local GitLab `Retry-After` backpressure
- Bounded logs, request sizes, request times, and shutdown
- Automated tests using temporary SQLite databases and `httptest` GitLab servers

Do not change the Gemini prompt, response schema, generation settings, summary-note format, publication identity, or existing review retry policy. Do not add draft lifecycle states, durable reconciliation scheduling, a listing cursor, repository checkout, model tools, runtime memory, public research, inline discussions, a status API, reconciliation configuration, manual retry controls, another service, queue, or database.

## Persistence Contract

Migrate `review_jobs` so `source_event_id` is nullable while preserving existing job IDs, review identities, results, publications, attempts, and states:

- A non-null `source_event_id` identifies a job created by webhook ingress.
- A null `source_event_id` identifies a job created by reconciliation.

Preserve the unique review identity constraint:

```text
GitLab instance + project ID + merge request IID + head SHA
```

Add one store operation that atomically creates or finds a reconciled job by review identity and reports whether it was newly queued. Concurrent webhook ingestion and reconciliation for the same identity must still produce one job:

- If webhook ingress creates the job first, reconciliation observes the existing job.
- If reconciliation creates the job first, a later webhook event is persisted and records the existing review as a duplicate.
- Reconciliation never resets or duplicates an existing queued, running, publishing, completed, failed, or obsolete job.

A reconciliation scan has no durable cursor or scheduler record. A crash may leave a partial scan and the next scan may repeat earlier work; database uniqueness makes both cases safe.

## GitLab Reconciliation

Support GitLab 17 and newer. For each exact path in `authorized_repositories`:

1. Resolve the project by its URL-encoded namespace path beneath the configured `/api/v4` endpoint.
2. Confirm the returned `path_with_namespace` exactly matches the configured path and retain the positive numeric project ID.
3. List merge requests with `state=opened`, `page`, and `per_page=100`.
4. For each entry, require the expected project ID, a positive IID, the documented opened state, and a valid current head SHA.
5. Skip entries marked draft or work-in-progress when observed.
6. Create or find a review job for every other entry.

Use the GitLab 17 merge request response fields for current head SHA and readiness. Normalize the head SHA in the same way as webhook ingress.

Process validated entries page by page. Jobs committed before a later page or project failure remain valid. Duplicate entries are harmless because the database review identity is unique. A later cycle retries the project from its first page.

Use the existing application-owned GitLab HTTP client, PAT boundary, 30-second request timeout, redirect rejection, status classification, response decoding, and bounded `Retry-After` parsing. Reconciliation is read-only at GitLab; it never invokes Gemini and never publishes.

A renamed project fails exact-path authorization until configuration is updated. The existing worker remains responsible for confirming project authorization, open state, and current head SHA before publication. Reconciliation does not add draft checks to the worker: readiness is only considered when the reconciler observes the merge request.

## Bounds and Scheduling

Use fixed application constants rather than new configuration fields:

- Run the first cycle immediately after the HTTP server and worker start.
- Start the next cycle five minutes after the previous cycle finishes.
- Never overlap reconciliation cycles.
- Scan configured projects sequentially.
- Request at most ten pages and 1,000 merge requests per project.
- Accept at most 100 entries and 2 MiB per response page.
- Stop the current project on malformed responses, malformed pagination, a limit violation, or a request failure, then continue with the next project unless application-wide backpressure applies.
- On shutdown, stop starting projects and cancel the active request.

A project exceeding the initial limits is logged and left for a later design change. These limits are intended for the small-team deployment model.

Normal scheduling is in memory. Restarting the process causes another immediate scan.

## Application-wide Backpressure

Add a process-local not-before gate to the shared GitLab client. All worker and reconciliation GitLab requests pass through this gate.

When any GitLab request receives a retryable response with a valid supported `Retry-After`, update the gate so future GitLab requests wait until that time. A `429` without a valid `Retry-After` applies a five-minute gate. Concurrent requests already in flight are not canceled.

When reconciliation receives either response, stop the current cycle; the normal scheduler may start a later cycle, whose requests will also wait at the shared gate if necessary. Worker jobs continue using their existing retry scheduling in addition to the shared gate.

Keep the existing maximum supported `Retry-After` of 24 hours and the existing `retry_after_exceeds_limit` failure category. Do not add a durable blocked state or operator-unblock mechanism. Restarting the process clears the shared gate.

## Logging and Process Integration

Initialize the reconciler only after configuration and schema migration succeed. Run webhook ingress, the review worker, and reconciliation in the same process.

Cycle and project logs contain bounded project path, numeric project ID when known, page and merge request counts, newly queued count, existing count, duration, outcome, and stable failure category. Never log PATs, request headers, response bodies, merge request titles or descriptions, source branches, diffs, or raw external errors.

Project failures do not terminate the process. Persistence failures do not claim that an entry was queued; already committed jobs remain valid and are found again on a later scan.

## Risks

- The first successful scan may enqueue many existing merge requests and incur Gemini usage. This is intentional for the small-team deployment model.
- Full scans make more GitLab requests than incremental reconciliation but avoid cursor state and recovery logic.
- A merge request may change draft state after reconciliation observes it. The worker may still review it; this plan intentionally accepts that race.
- A partial project scan may create some jobs before a later page fails. Repeated scans and review identity uniqueness make this safe.
- Projects with more than 1,000 open merge requests are unsupported by this milestone.
- The process-local backpressure gate is lost on restart. This is an accepted simplification.
- Fake GitLab tests cannot establish endpoint compatibility or PAT scopes. Verify the integration against GitLab 17 or newer before deployment.

## Verification

- On an empty database, a successful immediate scan queues each authorized open merge request that is neither draft nor work-in-progress.
- A draft merge request is skipped; if it is ready during a later scan, that scan creates exactly one job for its current head.
- A missed webhook revision is queued by a later scan and follows the existing worker, Gemini, and publication path unchanged.
- Repeated scans, partial scans, restarts, duplicate list entries, and concurrent webhook ingress produce one job per review identity without resetting existing jobs.
- A new head SHA creates a new job; an existing identity in any state is left unchanged.
- Existing webhook jobs retain their event relationship through migration; reconciled jobs have a null `source_event_id` and no synthetic event.
- Renamed or mismatched projects and malformed entries do not create jobs for the rejected data and do not terminate the process.
- Page, entry, response-size, and request-time limits are enforced.
- A supported `Retry-After` observed by either the worker or reconciler delays future GitLab requests from both. A `429` without a valid value applies the five-minute gate.
- Restart triggers an immediate scan and clears process-local scheduling and backpressure state.
- Shutdown cancels reconciliation without affecting already committed jobs.
- Reconciliation never invokes Gemini, publishes notes, or exposes configured secrets in logs or request paths.
