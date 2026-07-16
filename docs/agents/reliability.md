# Reliability

## Guarantee

The system provides at-least-once review execution with idempotent external effects, not exactly-once execution. A crash may repeat work but must not silently lose a merge request revision.

## Webhook Ingestion

Authenticate and bound the request before parsing it, then authorize its exact project namespace against the installation configuration. Handle an authenticated, authorized merge request webhook in this order:

1. Begin a SQLite transaction.
2. Insert the event using a stable delivery identifier.
3. Create or confirm its review job when the event is eligible, or record why it was ignored.
4. Commit.
5. Return success.

Never acknowledge an accepted merge request event before commit. Duplicate delivery must resolve to the same event or job and return success without creating another job. If GitLab supplies no suitable event identifier, derive one deterministically from stable delivery data.

Initially, only an `open` action that is not marked draft or work-in-progress creates a review job. An MR opened as draft remains unreviewed even if it later becomes ready; that transition may be added when required. Authenticated and authorized draft openings and other merge request actions are persisted as ignored events without jobs.

Reject a missing or invalid webhook secret with `401`, an unlisted project namespace with `403`, malformed input with `400`, and an oversized request with `413`. A persistence failure returns a server error rather than acknowledging the event. Rejections log bounded operational identifiers and reasons without logging secrets or payload bodies.

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

Use bounded backoff and record the last error and next attempt time. Distinguish retryable failures, permanent configuration or authorization failures, and obsolete work. Failed jobs remain inspectable and may be retried after correction.

## Idempotent Publication

GitLab publication and its local record cannot be committed atomically. Give each artifact a stable hidden marker derived from review and finding identity, for example:

```html
<!-- ai-reviewer:review=<review-id>:finding=<finding-id> -->
```

Before posting, search existing discussions for the marker. After posting, store the marker and GitLab object ID. On retry, reconcile GitLab and SQLite before creating an artifact.

Derive finding identity from stable evidence such as revision, path, line range, category, and normalized content—not a new random value per attempt. Complete a job only after all intended publication effects are reconciled.

## Reconciliation

Periodically:

1. List recently updated open merge requests in configured projects.
2. Read each current head SHA.
3. Compare it with local last-seen and last-reviewed state.
4. Enqueue missing revisions through the normal idempotent path.
5. Reconcile incomplete publications.

Webhooks provide low latency; reconciliation recovers deliveries lost before reaching the application.

## Durable State

SQLite state must represent:

- Durable webhook events and processing status
- Review identity, job state, leases, attempts, scheduling, and errors
- Publication markers and GitLab object IDs
- Last-seen and successfully reviewed merge request revisions

Event insertion and job creation occur in one transaction.

Keep the database and WAL on persistent storage with reliable file locking. Enable foreign keys, configure bounded lock waiting, and choose durability settings and backup procedures appropriate to the deployment platform. Run one replica per installation.

Repository caches are disposable. Clone, download, and model operations require explicit time, size, and concurrency limits and must never be the only record that review work exists.
