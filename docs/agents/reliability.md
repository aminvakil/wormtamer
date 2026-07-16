# Reliability

## Guarantee

The system targets **at-least-once review execution with idempotent external effects**. It does not claim exactly-once execution.

A process crash may cause work to run again, but it must not cause a merge request or revision to be silently forgotten.

## Durable Webhook Ingestion

Webhook handling follows this order:

1. Validate the webhook secret and required identifiers.
2. Begin a SQLite transaction.
3. Insert the webhook into the durable inbox using a stable event identifier.
4. Create or confirm the corresponding review job.
5. Commit the transaction.
6. Return a successful HTTP response.

Never acknowledge a webhook before the transaction commits. Duplicate deliveries must resolve to the same stored event or job rather than creating duplicate work.

If GitLab does not provide a suitable stable event identifier, derive one deterministically from the relevant delivery metadata and payload.

## Review Identity

A review job is tied to an immutable merge request revision, identified at minimum by:

```text
GitLab instance + project ID + merge request IID + head SHA
```

A new head SHA is new review work. Events for the same head SHA are deduplicated.

Before publishing, verify that the reviewed SHA is still the merge request head. Do not present findings from an obsolete revision as current findings.

## Job States and Leases

The conceptual lifecycle is:

```text
queued -> running -> publishing -> completed
            |            |
            +-> retry <---+

unrecoverable or exhausted retries -> failed
```

A worker atomically claims a queued job and records:

- Lease owner
- Lease expiry
- Attempt count
- Start time

Long-running work renews its lease. After a crash, expired leases become eligible for retry. On graceful shutdown, the process stops claiming jobs and either finishes active work or lets its leases expire.

Restarting a review from the beginning is preferred over persisting and resuming an arbitrary model conversation. Durable checkpoints are needed around external effects, not every model token.

## Retry Policy

Retries must be bounded and use backoff. Record the last error and next eligible attempt time.

Distinguish at least:

- Retryable failures: timeouts, transient GitLab/model errors, process interruption
- Permanent failures: invalid credentials, revoked repository access, malformed configuration
- Obsolete work: merge request closed or head SHA replaced

Failed jobs must remain inspectable and eligible for explicit retry after their cause is corrected.

## Idempotent Publication

Publishing a GitLab comment and recording its returned ID cannot be one atomic transaction. The process may crash after GitLab accepts a comment but before SQLite records it.

Every published artifact therefore needs a stable hidden marker derived from the review and finding identity, for example:

```html
<!-- ai-reviewer:review=<stable-review-id>:finding=<stable-finding-id> -->
```

Before posting, search existing merge request discussions for the marker. After posting, store the GitLab object ID and marker in SQLite. On retry, reconcile GitLab and SQLite before creating anything.

Finding identifiers should come from stable evidence such as revision, file, line range, category, and normalized finding content—not from a random value generated on each attempt.

Mark a job complete only when all intended publication effects have been reconciled.

## Reconciliation

Webhooks can be lost before they reach the application. A periodic reconciliation pass must:

1. List recently updated open merge requests in configured projects.
2. Read each current head SHA.
3. Compare it with local last-seen and last-reviewed state.
4. Enqueue missing revisions using the normal idempotent job path.
5. Reconcile incomplete publication records where necessary.

Webhook ingestion provides low latency; reconciliation provides eventual recovery.

## Minimal SQLite State

The precise schema is deferred, but state should cover these concepts:

- `events`: durable webhook inbox and processing status
- `jobs`: review identity, state, lease, attempts, scheduling, and errors
- `publications`: stable markers and remote GitLab object IDs
- `mr_state`: last seen and last successfully reviewed head SHA

Event insertion and job creation must occur in one transaction.

## SQLite Operation

- Place the database and its WAL files on a persistent local or block volume.
- Use WAL mode.
- Use a durability-oriented synchronous setting.
- Enable foreign-key enforcement.
- Configure a busy timeout instead of immediately failing on short lock contention.
- Do not place the database on storage with unreliable file locking.
- Run one application replica per team installation.
- Back up persistent state consistently with its WAL requirements.

The exact pragmas and backup mechanism must be verified against the selected runtime and deployment platform.

## Resource Failures

Repository clones, public-source downloads, and model calls must have explicit time, size, and concurrency limits. A failed disposable cache can be recreated; it must never be the only record that a review is required.
