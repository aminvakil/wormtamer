# Reliability

## Guarantee

The system provides at-least-once review execution with idempotent external effects, not exactly-once execution. A crash may repeat work but must not silently lose a merge request revision.

## Webhook Ingestion

Handle a valid webhook in this order:

1. Begin a SQLite transaction.
2. Insert the event using a stable delivery identifier.
3. Create or confirm its review job.
4. Commit.
5. Return success.

Never acknowledge before commit. Duplicate delivery must resolve to the same event or job. If GitLab supplies no suitable event identifier, derive one deterministically from stable delivery data.

## Review Identity

Identify work by at least:

```text
GitLab instance + project ID + merge request IID + head SHA
```

Deduplicate events for the same head SHA. Before publication, confirm that the reviewed SHA remains current; obsolete findings must not be presented as current.

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
