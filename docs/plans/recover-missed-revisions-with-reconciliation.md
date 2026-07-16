# Recover missed merge request revisions through reconciliation

Status: proposed

## Goal

Recover reviewable merge request revisions that webhooks did not deliver. A periodic reconciler in the existing Wormtamer process scans every authorized GitLab project, observes current open merge requests, and idempotently creates review jobs through the same durable review identity used by webhook ingress.

The first scan runs immediately after startup and intentionally enqueues every existing open merge request that is neither draft nor work-in-progress. This bootstraps a deployment without silently treating pre-existing eligible work as reviewed and also discovers draft-to-ready transitions.

## Scope

Include:

- One periodic reconciler in the existing process, with no overlapping scans
- Exact configured-project authorization and numeric project identity validation
- Bounded pagination of all open merge requests in each authorized project
- Current head SHA and draft/readiness validation from GitLab responses
- Fresh readiness checks before Gemini invocation and publication
- A durable `waiting_ready` job state for revisions that become draft after being queued
- Idempotent job creation without fabricating webhook events
- A schema migration that records whether a job originated from webhook ingress or reconciliation
- Durable cycle-wide GitLab backpressure scheduling
- Immediate startup reconciliation followed by a fixed five-minute interval
- Project-isolated failures except global backpressure, bounded structured logs, and bounded shutdown
- Automated tests using real temporary SQLite databases and `httptest` GitLab servers

Do not change the Gemini prompt, response schema, generation settings, summary-note format, or publication identity. Do not include repository checkout, model tools, runtime memory, public research, inline discussions, a status API, configuration for reconciliation timing, manual retry of failed jobs, webhook retention, multiple reconcilers, or another service, queue, or database.

## Persistence Contract

Migrate `review_jobs` without changing existing job IDs, review identities, results, publications, attempts, or current states. Add `waiting_ready` as a new non-terminal state and replace the assumption that every job has a webhook event with an explicit source contract:

- `webhook`: requires its existing `source_event_id`
- `reconciliation`: has no webhook event

Preserve the unique constraint on:

```text
GitLab instance + project ID + merge request IID + head SHA
```

Add the minimum store operations to atomically create or find a reconciled job by review identity and to transition `waiting_ready` jobs. Reconciled insertion returns whether the job was newly queued. Concurrent webhook ingestion and reconciliation for the same identity must resolve to one job:

- If webhook ingress creates the job first, reconciliation observes the existing job.
- If reconciliation creates the job first, the later webhook event is still persisted and records the existing review as a duplicate.
- Existing `queued`, `running`, `publishing`, `completed`, `failed`, or `obsolete` jobs are never reset or duplicated by reconciliation.
- A `waiting_ready` job is the only reactivatable state. A complete authorized project scan may move it back to `queued` when the same IID and head SHA are ready again, clear its readiness reason and scheduling fields, and reset its attempt count. Any persisted validated result remains intact so the existing claim path resumes at publication rather than invoking Gemini again.

When a fresh worker check observes that an otherwise current open revision is draft or work-in-progress, atomically move the leased job to `waiting_ready`, clear its lease, and record the stable `merge_request_not_ready` category. This is neither failure nor obsolescence and does not consume the later retry budget. During a complete project scan, reconcile waiting jobs before creating new jobs:

- Same IID and head, still draft: leave `waiting_ready` unchanged.
- Same IID and head, now ready: reactivate the existing job as described above.
- Same IID with a different head present in the listing: mark the waiting job `obsolete`.
- IID absent from the listing: leave it waiting because a mutable paginated listing is insufficient evidence that the merge request closed.

A reconciliation scan is not authoritative workflow state and needs no durable cursor or synthetic event. A crash may repeat the scan; database uniqueness makes repetition safe. Existing jobs and their states represent observed and reviewed revisions.

Persist a singleton reconciliation control record containing the next GitLab scan not-before time and an optional blocked category. It is scheduler state, not a listing cursor. Update it before ending a backpressured cycle so process restart cannot send GitLab requests earlier than the server requested.

## GitLab Reconciliation Contract

For each exact path in `authorized_repositories`:

1. Resolve the project by its URL-encoded namespace path beneath the configured `/api/v4` endpoint.
2. Confirm the returned `path_with_namespace` exactly matches the configured path and retain the numeric project ID.
3. List merge requests with `state=opened` using explicit, deterministic pagination.
4. Validate each response has the expected project ID, positive IID, recognized `opened` state, valid current head SHA, and explicit draft/work-in-progress flags.
5. After the complete bounded project response is validated, reconcile existing `waiting_ready` jobs and enqueue each non-draft current revision through the reconciliation store operation.

Treat project and merge request responses as untrusted. Do not enqueue any merge request from a project scan if pagination exceeds the project bound, a page is oversized, pagination is malformed, or any required identity/readiness field is malformed. This avoids presenting a partial project scan as successful. A project-local failure does not prevent scanning other authorized projects; cycle-wide backpressure does.

Use the existing application-owned GitLab HTTP client, PAT boundary, 30-second request timeout, redirect rejection, status classification, response decoding, and bounded `Retry-After` rules. Reconciliation is read-only at GitLab; it never publishes and never invokes Gemini.

A retryable GitLab response with a valid `Retry-After` is cycle-wide backpressure, not a project-isolated failure. Persist an absolute not-before time, stop the current project without enqueuing its buffered listing, and make no further GitLab requests for any project in that cycle. The next scan starts no earlier than both the normal cycle schedule and the persisted server time. A `429` without a valid `Retry-After` also stops the cycle and applies the normal five-minute delay. A value beyond the existing supported bound stores the stable `retry_after_exceeds_limit` blocked category; automatic reconciliation makes no further GitLab requests across restart until operator intervention rather than converting it into an earlier retry.

A renamed project fails exact-path authorization until configuration is updated. Closed or merged merge requests are absent from the open listing. The worker must extend its existing project, state, and SHA validation to include explicit draft and work-in-progress fields whenever it fetches merge request details:

- Immediately before invoking Gemini, a non-ready revision moves to `waiting_ready` and invokes no model.
- Before reconciling an existing marker and again immediately before posting a new note, a non-ready revision moves to `waiting_ready` and publishes nothing.
- A non-open state or changed head remains `obsolete`; malformed readiness remains `failed`; authorization failures remain `failed`.

These checks close the race between reconciliation observation and worker execution. A later complete scan reactivates the same waiting revision when it becomes ready again.

## Initial Bounds and Scheduling

Use fixed application constants rather than new configuration fields:

- Run the first scan immediately after process startup, without delaying webhook serving or health checks, unless a persisted not-before or blocked state prevents it.
- Start the next cycle five minutes after the previous cycle finishes; never overlap cycles.
- Scan configured projects sequentially.
- Request at most ten merge request pages per project with at most 100 entries per page.
- Accept at most 1,000 open merge requests per project and at most 2 MiB per response page.
- Buffer and validate the bounded project listing before creating any jobs for that project.
- Continue to the next configured project after a bounded, sanitized project-local failure. Stop the cycle after rate limiting or any retryable response carrying `Retry-After`.
- On shutdown, stop starting projects, cancel the active request, and return within ten seconds even if an operation ignores cancellation. Any jobs already committed remain durable; a partial uncommitted project listing creates no jobs.

The all-open scan deliberately favors simple, cursor-free eventual recovery. If a project exceeds the bound, fail that project visibly rather than silently reviewing only a subset. A later task may add durable incremental scanning only after a concrete project requires it.

## Process and Logging Contract

Initialize the reconciler only after configuration and schema migration succeed. Run webhook ingress, the review worker, and reconciliation in the same process. Reconciliation must not block the health check, webhook admission, or active review work beyond normal bounded SQLite contention.

Cycle and project logs contain bounded project path, numeric project ID when known, page and merge request counts, newly queued count, existing count, duration, outcome, and stable failure category. Never log PATs, request headers, response bodies, merge request titles or descriptions, diffs, source branches, or raw external errors.

Ordinary project failures do not terminate the process and are retried by the next full cycle. Cycle-wide backpressure stops subsequent project requests and persists its not-before time before the scheduler waits. Persistence failures do not claim success; already committed jobs remain valid and repeated observations remain idempotent.

## Risks

- The first scan may enqueue many existing eligible merge requests and incur immediate Gemini usage. This is intentional and bounded.
- Repeated full scans trade additional GitLab reads for simpler crash recovery and no cursor correctness problem.
- Treating `Retry-After` as installation-wide may delay projects unaffected by project-specific throttling, but avoids violating token- or instance-wide backpressure.
- A revision may wait indefinitely if it remains draft; this is intentional and consumes no worker attempts while waiting. A closed merge request already in `waiting_ready` may also remain there because absence from a mutable listing is not sufficient evidence to mark it obsolete.
- A project with more than 1,000 open merge requests is unsupported by this milestone and receives no reconciled jobs until the limit or design changes.
- Merge request listings can change during pagination. Buffering and validating a bounded cycle prevents malformed partial input, while repeated complete scans provide eventual recovery for entries shifted between pages.
- Fake GitLab tests cannot establish endpoint compatibility or minimum PAT scopes. An operator-run smoke test against a dedicated project remains required.

## Verification

- On an empty database, the immediate first scan enqueues every authorized open merge request that is neither draft nor work-in-progress, including merge requests that existed before startup.
- A draft merge request with no job is skipped; when the same head later becomes ready, a subsequent scan creates exactly one job.
- A queued revision that becomes draft before review invokes neither Gemini nor publication and moves to `waiting_ready`; when the same head becomes ready, reconciliation reactivates the same job with a fresh attempt budget.
- A revision that becomes draft after result persistence publishes nothing, preserves its validated result while waiting, and resumes publication without another Gemini request after becoming ready.
- A waiting revision whose IID appears with a changed head becomes `obsolete`; absence from a mutable listing leaves it waiting and cannot prevent later reactivation.
- A missed webhook revision is queued within one completed reconciliation interval and follows the existing worker, Gemini, and publication path unchanged.
- Repeated scans, process restarts, duplicate list entries, and concurrent webhook ingestion produce one job per review identity without resetting existing state except the defined `waiting_ready` reactivation.
- A new head SHA creates a new job; an already queued, active, completed, failed, or obsolete identity is left unchanged.
- Existing webhook-origin jobs retain their event relationship through migration; reconciled jobs have explicit reconciliation provenance and no synthetic webhook event.
- Renamed or mismatched projects, malformed identities or readiness fields, oversized pages, malformed pagination, and projects beyond the page or merge request bound create no reconciled jobs for that project and do not prevent other projects from scanning.
- A project returning `429` or a retryable response with `Retry-After` prevents all later project requests in that cycle. Tests with a controllable clock verify no request occurs before the persisted not-before time, including after restart.
- Transient project-local GitLab and persistence failures are logged with stable categories and retried on a later cycle without acknowledging an incomplete scan as successful.
- Shutdown stops new project scans and returns within the bound while preserving every committed job.
- Tests verify that reconciliation sends no configured secret to logs or GitLab request paths other than the required PAT header, never invokes Gemini, and never publishes.
