# Add durable GitLab webhook ingress

Status: proposed

## Goal

Establish the first executable milestone: a single Wormtamer process accepts authenticated GitLab merge request webhooks, durably records them in SQLite, and creates one idempotent queued job for each newly opened, ready merge request from an authorized repository.

## Scope

Include:

- A Go command started with an explicit JSON configuration path such as `wormtamer -config ./config.json`
- Standard-library HTTP serving with `POST /webhooks/gitlab` and `GET /healthcheck`
- GitLab webhook-secret authentication and exact namespace-path authorization
- Durable webhook events and queued review jobs in SQLite
- Idempotency by delivery identity and review identity
- Structured, bounded operational logs
- Graceful process startup and shutdown

Do not include a review worker, GitLab API calls or a personal access token, Gemini, repository checkout, publication, reconciliation, runtime memory, public research, or a job-status API.

## Configuration Contract

The initial JSON configuration contains:

```json
{
  "listen_address": ":8080",
  "database_path": "./wormtamer.db",
  "gitlab": {
    "base_url": "http://gitlab.internal",
    "webhook_secret": "replace-me"
  },
  "authorized_repositories": [
    "group/project"
  ]
}
```

Require the configuration path and every listed value. Resolve relative data paths from the configuration file's directory. Reject unknown fields, empty secrets, invalid GitLab URLs, malformed repository paths, and duplicate repository entries. Permit HTTP GitLab URLs. Warn, without refusing startup, when the configuration file is readable by group or other users.

Do not accept or require credentials for capabilities outside this milestone. A real secret-bearing configuration must remain outside version control; a future example file contains placeholders only.

## Ingress Contract

`POST /webhooks/gitlab` authenticates the GitLab token, limits the body before parsing it, accepts merge request hook payloads, and compares `project.path_with_namespace` exactly with the configured repository list.

Only an `open` merge request action not marked draft or work-in-progress creates a job. Draft openings and other authenticated, authorized merge request actions are persisted with an ignored outcome but create no job. A later draft-to-ready transition does not create a job in this milestone.

Use GitLab's event UUID when available and a deterministic digest of stable delivery data otherwise. Use the configured GitLab instance, numeric project ID, merge request IID, and head SHA as the review identity. Event insertion and eligible job creation occur in one transaction. A duplicate event or review identity returns success without creating another job.

Return:

- `200` after an accepted event transaction commits, including duplicate and ignored MR events
- `401` for a missing or invalid webhook secret
- `403` for an authenticated event from an unlisted namespace path
- `400` for malformed supported input
- `413` when the request body exceeds the 1 MiB ingress limit
- `500` when durable acceptance cannot commit

`GET /healthcheck` returns `200` with `ok\n` after successful startup. It is a liveness check only and exposes no job state.

## Persistence Approach

Use SQLite through `github.com/mattn/go-sqlite3` with CGO and FTS5 enabled. Keep event and job records distinct, enforce review and delivery uniqueness in the database, enable foreign keys, configure bounded lock waiting, and use WAL on persistent storage. Apply the initial schema at startup with an application-owned schema version so later changes can fail or migrate explicitly.

Store enough event metadata and payload data to inspect accepted deliveries without relying on logs. Treat stored payloads as untrusted. Jobs remain queued because worker execution is outside this milestone.

Do not pin a CI runner, build distribution, or libc version in this task. Packaging constraints may be introduced when an observed compatibility requirement justifies them.

## Security and Operational Behavior

Compare webhook secrets without timing-dependent string comparison. Bound request size and server timeouts. Never log request bodies, headers, secrets, or repository content.

Logs provide the only job-level operational visibility and include bounded identifiers when available: delivery ID, project ID and namespace, merge request IID, head SHA, job ID, outcome, and sanitized rejection or failure reason. A renamed namespace is expected to receive `403` until configuration is updated.

Graceful shutdown stops accepting new requests and allows active ingress transactions a bounded opportunity to finish.

## Risks

Plain HTTP and plaintext configuration provide no transport or at-rest secret encryption. This is accepted for the initial local deployment; filesystem and network access remain operator responsibilities.

CGO-built standalone binaries may depend on the build environment's libc. Containers are the primary deployment path, and no distribution compatibility promise or build-image pin is part of this milestone.

Persisted ignored events and payloads grow over time. Retention and backup policy remain deferred until operational requirements are known.

## Verification

- Starting without `-config`, with invalid configuration, or with an unavailable SQLite path fails before the server accepts traffic and does not expose secret values.
- `/healthcheck` returns exactly the documented liveness response after successful startup.
- Invalid authentication, malformed or oversized input, and an unlisted namespace produce the documented status without creating events or jobs; rejection logs contain a reason but no payload or secret.
- A ready MR `open` webhook commits one event and one queued job with the expected GitLab instance, numeric project ID, MR IID, and head SHA before returning `200`.
- Replaying a delivery and sending another delivery for the same review identity return `200` while preserving one job.
- Draft openings and non-opening MR actions commit ignored events and no jobs.
- A forced SQLite commit failure never returns success.
- State remains present after process restart, foreign-key enforcement is active, WAL and bounded lock waiting are configured, and FTS5 is available in the CGO build.
- Shutdown does not accept new ingress work and does not leave an acknowledged event outside its committed transaction.
