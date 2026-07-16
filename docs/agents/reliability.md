# Reliability

## Guarantee

The system provides at-least-once review execution with idempotent external effects, not exactly-once execution. A crash may repeat work but must not silently lose a merge request revision.

## Webhook Ingestion

`POST /webhooks/gitlab` authenticates the request before reading it, admits at most four concurrent webhook requests, limits each admitted body to 1 MiB before parsing, and then authorizes its exact project namespace against the installation configuration. Handle an authenticated, authorized merge request webhook in this order:

1. Begin a SQLite transaction.
2. Insert the event using a stable delivery identifier.
3. Create or confirm its review job when the event is eligible, or record why it was ignored.
4. Commit.
5. Return success.

Never acknowledge an accepted merge request event before commit. Duplicate delivery must resolve to the same event or job and return success without creating another job. Use `X-Gitlab-Event-UUID` as the delivery identifier when available; otherwise derive a deterministic SHA-256 identifier from the GitLab instance and raw bounded payload.

Initially, only an `open` action that is not marked draft or work-in-progress creates a review job. An MR opened as draft remains unreviewed even if it later becomes ready; that transition may be added when required. Authenticated and authorized draft openings and other merge request actions are persisted as ignored events without jobs.

Reject a missing or invalid webhook secret with `401`, an unlisted project namespace with `403`, malformed input with `400`, an oversized request with `413`, and authenticated overload with retryable `503` and `Retry-After` responses. A persistence failure returns a server error rather than acknowledging the event. Rejections log bounded operational identifiers and reasons without logging secrets or payload bodies. Graceful shutdown stops accepting new requests and gives active ingress transactions a bounded opportunity to finish.

## Review Identity

Identify work by at least:

```text
GitLab instance + project ID + merge request IID + head SHA
```

The numeric project ID from the webhook remains the durable identity even though configuration authorizes repositories by namespace path. Deduplicate events for the same head SHA. Before publication, confirm that the reviewed SHA remains current; obsolete findings must not be presented as current.

## Jobs and Retries

The conceptual lifecycle is:

```text
queued -> running -> publishing -> completed
            |            |
            +-> retry <---+

unrecoverable or exhausted retries -> failed
```

A worker atomically claims a job with a lease owner, expiry, attempt count, and start time. Long work renews its lease; expired leases become retryable after a crash. Graceful shutdown stops new claims and either finishes work or lets leases expire.

Restart the review rather than persisting an arbitrary model conversation. Persist checkpoints around external effects.

Use at most five claims per job. Network failures, HTTP request timeouts including status `408`, rate limits, and server failures are retryable; credential, authorization, and other rejected requests are permanent. Locally calculated exponential retry delays start at five seconds and cap at five minutes. A valid GitLab `Retry-After` is instead a minimum delay and may defer work for up to 24 hours; a longer requested delay fails under the stable `retry_after_exceeds_limit` category rather than retrying early. Record the bounded error category and message and next attempt time. Distinguish retryable failures, permanent configuration or authorization failures, and obsolete work. A recognized merge request state other than `opened`, including `closed` or `merged`, and a changed head SHA are obsolete; renamed or unauthorized projects and malformed states fail. Failed jobs remain inspectable and may be retried after correction.

The single worker polls once per second, leases one job for two minutes, renews every 30 seconds, and processes one job at a time. On shutdown it allows active work ten seconds to reach a checkpoint. At that deadline it cancels the job and returns without waiting for an uncooperative operation; unfinished work remains recoverable through lease expiry.

## Idempotent Publication

GitLab publication and its local record cannot be committed atomically. The current worker gives its one summary note a stable hidden marker derived from review identity, for example:

```html
<!-- wormtamer:review=<review-identity-hash> -->
```

Before posting, search the newest notes first, examining at most 1,000 notes across ten pages for the exact marker on a note authored by the PAT's authenticated GitLab user, and fail closed if absence cannot be established. After posting, store the marker and GitLab note ID. On retry, reconcile GitLab and SQLite before creating another note. Limit the rendered note to 64 KiB and complete a job only after the marked note exists and its publication record is durable. Future separate finding discussions require their own stable finding identities.

## Reconciliation

Reconciliation is deferred in the current implementation. When implemented, it will periodically:

1. List recently updated open merge requests in configured projects.
2. Read each current head SHA.
3. Compare it with local last-seen and last-reviewed state.
4. Enqueue missing revisions through the normal idempotent path.
5. Reconcile incomplete publications.

Webhooks provide low latency; reconciliation recovers deliveries lost before reaching the application.

## Durable State

SQLite stores the locally validated structured review result before publication and reuses that checkpoint on retry; it does not persist prompts, diffs, raw model responses, or conversations. SQLite state must represent:

- Durable webhook events and processing status
- Review identity, job state, leases, attempts, scheduling, and errors
- Publication markers and GitLab object IDs
- Last-seen and successfully reviewed merge request revisions

Event insertion and job creation occur in one transaction. Webhook events retain the bounded raw JSON payload and delivery, project, merge request, revision, action, and outcome metadata needed for later inspection. Events and jobs are separate records with database-enforced delivery and review uniqueness.

Keep the database and WAL on persistent storage with reliable file locking. Enable foreign keys, configure bounded lock waiting, and choose durability settings and backup procedures appropriate to the deployment platform. The application owns and checks the SQLite schema version at startup. Run one replica per installation.

There is no webhook-event or payload retention policy yet; ignored events and stored payloads accumulate until operational requirements establish one.

Repository caches are disposable. The current GitLab broker limits requests to 30 seconds, metadata responses to 256 KiB, each diff or note page to 2 MiB, changed diffs to five pages, 100 files, and 512 KiB of aggregate diff content. Gemini generation is limited to two minutes, 8,192 output tokens, 20 findings, and 64 KiB of validated result JSON. Inputs that exceed a bound fail rather than being silently truncated. Clone, download, and model operations require explicit time, size, and concurrency limits and must never be the only record that review work exists.
