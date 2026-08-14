# Inspect and retry failed jobs

Status: proposed

## Goal

Let an operator inspect terminal failed review and feedback jobs and requeue one after correcting its cause, without editing SQLite directly or adding a network administration surface.

## Scope

Add a local operational command family to the existing executable:

```text
wormtamer -config /etc/wormtamer/config.json jobs list-failed
wormtamer -config /etc/wormtamer/config.json jobs retry review <job-id>
wormtamer -config /etc/wormtamer/config.json jobs retry feedback <job-id>
```

`list-failed` returns one bounded JSON document containing at most 100 failed jobs, newest failure first, and reports whether more rows exist. Each entry includes only the job kind, numeric job ID, attempt count, last error category, update time, numeric project and merge request identifiers, and the review head SHA or feedback note ID needed to identify the work.

`retry` atomically moves exactly one currently failed job back to `queued`, resets its attempt budget and scheduling fields, and clears its prior failure and lease fields. It preserves the review identity, source event, validated review checkpoint, publication state, feedback binding, and derived-memory state. A checkpointed review therefore resumes publication rather than invoking Gemini again.

Do not add bulk retry, arbitrary state editing, deletion, retry of completed or obsolete work, automatic retries beyond the existing limit, an administration HTTP endpoint, or repository/comment/result content to command output.

## Approach

- Extend command parsing so operational commands load the configured SQLite path but do not initialize GitLab, Gemini, public-source clients, workers, reconciliation, or the HTTP listener.
- Add bounded store queries that combine failed review and feedback status into a small operator-facing record. Fetch one extra row to determine whether the 100-row response was truncated.
- Add separate transactional store operations for review and feedback retries. Each update must require `state = 'failed'`; a missing job and a job in another state must produce distinct non-success outcomes without changing data.
- Set `attempt_count` to zero and `next_attempt_at` to the command time. Clear lease ownership, lease expiry, and failure fields. Retain any validated review result so the existing claim path resumes at `publishing`.
- Emit JSON to standard output and bounded errors to standard error. Never emit webhook payloads, model results, comments, memory lessons, credentials, or stored error messages.
- Document container usage with `docker exec` against the running single replica. The command is a short SQLite client, not a second service replica, and its conditional transaction must remain safe if the worker claims the newly queued job immediately.
- Update the reliability documentation so the supported inspection and post-correction retry behavior is authoritative there.

No schema migration is expected because the required state and identifiers already exist.

## Verification

- Listing an empty database returns a successful empty, non-truncated result.
- Failed review and feedback jobs are returned in deterministic newest-first order with only the documented fields; more than 100 failures sets the truncation indicator.
- Retrying either job kind changes only the selected failed job, resets its attempt budget, and makes it claimable immediately.
- A review with a persisted validated result resumes publication without a second model review; a failed feedback job retains its immutable published-review and current source-event binding.
- Retrying a missing, queued, running, publishing, completed, or obsolete job fails without modifying it. Repeating the same retry after the first succeeds also fails without resetting active work.
- Operational commands make no network request, start no listener or background component, and do not expose private persisted content or configured credentials.
