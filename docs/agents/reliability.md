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

Only an `open` action that is not marked draft or work-in-progress creates a review job directly from webhook ingress. Authenticated and authorized draft openings and other merge request actions are persisted as ignored events without jobs. Periodic reconciliation discovers an open merge request if it is ready during a later scan.

Reject a missing or invalid webhook secret with `401`, an unlisted project namespace with `403`, malformed input with `400`, an oversized request with `413`, and authenticated overload with retryable `503` and `Retry-After` responses. A persistence failure returns a server error rather than acknowledging the event. Rejections log bounded operational identifiers and reasons without logging secrets or payload bodies. Graceful shutdown stops accepting new requests and gives active ingress transactions a bounded opportunity to finish.

## Feedback Ingestion

An authenticated `Note Hook` create or update for an authorized, non-system, non-internal merge request comment is eligible when that merge request already has a durably published Wormtamer review backed by its locally validated structured result. An external publication recovered after local state loss is not eligible because Wormtamer cannot safely reconstruct that result from rendered Markdown. In one transaction, insert a delivery-deduplicated feedback event containing only bounded source identifiers and create or update one feedback job per GitLab note. A note's first eligible event binds the job to the exact latest published review; later updates retain that immutable target. Do not persist the note body. An event without an eligible locally backed review is retained as ignored. A newer source update requeues evaluation and immediately deactivates prior derived memory; a stale update is retained as ignored.

Every eligible comment is evaluated, including comments bound to zero-finding reviews and ordinary discussion that may produce no structured decision. Feedback jobs fetch the current note after webhook commit, so processing intentionally evaluates the latest GitLab text rather than preserving comment revisions. Each job has at most five claims, a renewable three-minute lease, and the same retry categories and bounded exponential backoff used by review work. The structured current evaluation and active memory replace prior derived state transactionally. A crash may repeat GitLab reads and Gemini evaluation but cannot create duplicate current memory.

GitLab emits comment webhooks for creation and updates, not deletion. Every five minutes, the feedback worker checks the source of each active comment-derived memory. A missing note or a source timestamp changed without a matching webhook deactivates that memory; a transient check failure defers another check without treating the source as deleted or changed.

## Review Identity

Identify work by at least:

```text
GitLab instance + project ID + merge request IID + head SHA
```

The numeric project ID from the webhook remains the durable identity even though configuration authorizes repositories by namespace path. Deduplicate events for the same head SHA. Before publication, confirm that the reviewed SHA remains current; obsolete findings must not be presented as current.

## Review Feedback Target Identity

The first-class overall-review target is `WT-R-` followed by the unpadded uppercase base32 encoding of the first 128 bits of a SHA-256 digest. Hash a `wormtamer:review:v1` domain separator, canonical GitLab instance, numeric project ID, merge request IID, and lowercase head SHA as UTF-8 fields separated and terminated by zero bytes. The target is supplied by trusted application code and remains distinct from finding identities.

A newly derived typed memory identity is `WT-M-` plus the same bounded digest encoding over a `wormtamer:memory:v2` domain separator, canonical GitLab instance, numeric project ID, note ID, target type, and target identity. Including the target type prevents review and finding targets from aliasing.

## Finding Identity

After local model-output validation, assign each ordered finding an application-owned identifier under its immutable review identity. The identifier is `WT-F-` followed by the unpadded uppercase base32 encoding of the first 128 bits of a SHA-256 digest. Hash a `wormtamer:finding:v1` domain separator, canonical GitLab instance, numeric project ID, merge request IID, lowercase head SHA, and one-based finding ordinal as UTF-8 fields separated and terminated by zero bytes.

Persist the identifiers and zero-based finding positions in the same transaction as the validated review result. The identifier and `(job, position)` are both unique. A collision or malformed identifier fails persistence rather than aliasing another finding. Retries and publication reconciliation reuse the persisted identifiers; the model cannot provide or change them.

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

Use at most five claims per job. Network failures, HTTP request timeouts including status `408`, rate limits, and server failures are retryable; credential, authorization, and other rejected requests are permanent. Locally calculated exponential retry delays start at five seconds and cap at five minutes. A valid GitLab `Retry-After` is instead a minimum delay and may defer work for up to 24 hours; a longer requested delay fails under the stable `retry_after_exceeds_limit` category rather than retrying early. The shared GitLab client also delays all subsequent worker and reconciliation requests until a supported `Retry-After` expires; a `429` without a valid value applies a five-minute process-local delay. This application-wide gate is not durable across restart. Record the bounded error category and message and next attempt time. Distinguish retryable failures, permanent configuration or authorization failures, and obsolete work. A recognized merge request state other than `opened`, including `closed` or `merged`, and a changed head SHA are obsolete; renamed or unauthorized projects and malformed states fail. Failed review and feedback jobs remain inspectable and may be retried after correction through local operational commands, without an administration HTTP surface. `wormtamer -config <path> jobs list-failed` returns one JSON document containing at most 100 failures ordered by newest update time, with a truncation indicator and only the job kind and ID, attempt count, last error category, update time, numeric project and merge request identifiers, and the review head SHA or feedback note ID. It does not return stored error messages or workflow, repository, comment, result, memory, or credential content.

`wormtamer -config <path> jobs retry review <job-id>` and `jobs retry feedback <job-id>` conditionally move exactly one currently failed job to `queued`, reset its attempt count, make it immediately due, and clear its failure and lease fields. Missing jobs and jobs no longer in `failed` fail distinctly without mutation, so concurrent commands cannot reset newly active work. Retry preserves review identity, source events, validated results, publication records, immutable feedback review bindings, evaluations, and derived memory. The ordinary claim path moves a retried review with a validated result directly to `publishing` rather than invoking Gemini again. Operational commands open only configured SQLite state and do not initialize external clients, background components, or the HTTP listener.

The single worker polls once per second, leases one job for two minutes, renews every 30 seconds, and processes one job at a time. On shutdown it allows active work ten seconds to reach a checkpoint. At that deadline it cancels the job and returns without waiting for an uncooperative operation; unfinished work remains recoverable through lease expiry.

## Idempotent Publication

GitLab publication and its local record cannot be committed atomically. The current worker gives its one summary note a stable hidden marker derived from review identity, for example:

```html
<!-- wormtamer:review=<review-identity-hash> -->
```

For a claimed job without a locally validated result, search the newest notes before loading review evidence or invoking Gemini, examining at most 1,000 notes across ten pages for the exact marker on a note authored by the PAT's authenticated GitLab user. Fail closed if absence cannot be established. When a matching note exists, confirm that the merge request remains open at the exact head SHA, then atomically store its marker and GitLab note ID and complete the job without fabricating or regenerating a structured result. This external-only recovery suppresses duplicate model work and publication but remains ineligible for feedback evaluation.

When no matching note exists, perform the review normally. Before posting, search again to cover an existing publication or a lost response, and reconcile GitLab and SQLite before creating another note. After posting, store the marker and GitLab note ID. Limit the rendered note to 64 KiB and complete a job only after the marked note exists and its publication record is durable. Future separate finding discussions require their own stable finding identities.

## Reconciliation

Reconciliation runs immediately after startup and five minutes after each completed cycle. It sequentially resolves every configured project by exact namespace path and lists up to ten pages of 100 open merge requests. Ready revisions are inserted page by page through the same unique review identity used by webhook ingress; drafts and work-in-progress entries are skipped as observed. Existing jobs in any state are not reset.

Project failures do not terminate the process or roll back jobs committed from earlier pages. A later cursor-free scan starts the project again from its first page, and uniqueness makes repetition safe. Rate limiting or a supported `Retry-After` stops the current cycle while the shared GitLab request gate delays subsequent application requests. Scheduling and backpressure are process-local, so restart triggers an immediate scan.

Reconciliation is read-only at GitLab and does not invoke Gemini or publish. Webhooks provide low latency; reconciliation recovers deliveries lost before reaching the application.

## Durable State

SQLite stores the locally validated structured review result before publication and reuses that checkpoint on retry; it does not persist prompts, diffs, raw model responses, conversations, public responses, or public repository snapshots. SQLite state must represent:

- Durable merge request webhook events and processing status
- Bounded feedback delivery and source identifiers without comment bodies
- Review and feedback job state, leases, attempts, scheduling, and errors
- Publication markers and GitLab object IDs; a publication may lack a local review result only when recovered from an existing exact marker before generation
- Application-owned finding identifiers and their ordered positions under validated review results
- Feedback jobs bound to immutable published reviews, plus current typed review- or finding-target decisions and repository-scoped active memories
- Versioned identities of memory returned during each successfully checkpointed review, without queries or lesson copies
- Last-seen and successfully reviewed merge request revisions

Webhook event insertion and its job creation occur in one transaction. Webhook events retain the bounded raw JSON payload and delivery, project, merge request, revision, action, and outcome metadata needed for later inspection. Events and jobs are separate records with database-enforced delivery and review uniqueness. A reconciled job has no source event; a later webhook for the same review identity is persisted as a duplicate review without creating another job.

Keep the database and WAL on persistent storage with reliable file locking. Enable foreign keys, configure bounded lock waiting, and choose durability settings and backup procedures appropriate to the deployment platform. The application owns and checks the SQLite schema version at startup. Run one replica per installation.

There is no merge request webhook-event or payload retention policy yet; ignored events and stored merge request payloads accumulate until operational requirements establish one. Feedback events intentionally retain no comment payload or body.

Repository workspaces are disposable and rebuilt when a review attempt restarts. The GitLab broker limits requests to 30 seconds, metadata responses to 256 KiB, each diff or note page to 2 MiB, changed diffs to five pages, 100 files, 512 KiB of aggregate diff content, and each repository archive to 32 MiB compressed. Archive extraction accepts at most 20,000 entries and 128 MiB uncompressed per repository, exposing at most 10,000 UTF-8 text files of at most 2 MiB each. One application-owned limit permits at most eight internal repository tool calls and eight distinct internal repositories per review; because each first repository access consumes a call, it is not charged again against a separate budget. One immutable revision is retained for each inspected repository.

Each public HTTP request has a ten-second deadline, permits at most five redirects, and accepts a URL of at most 2,048 bytes. A direct web response contains at most 48 KiB of supported UTF-8 text. GitHub metadata responses contain at most 256 KiB, and public repository archives contain at most 32 MiB compressed before the same extraction limits used for internal archives. A review permits at most eight public-source calls, including web fetches and public repository list or read operations. Public GitHub rate limits, timeouts, network failures, and server failures retry the review; rejected requests and security-boundary failures fail closed.

Memory retrieval accepts queries of at most 256 bytes, searches at most 100 newest active records already filtered to the exact current GitLab instance and project, and returns at most five matches per call. A review permits at most eight memory calls independently of the eight internal repository and eight public-source category ceilings, with at most 16 calls combined across all tools and a one-second deadline for each memory query. Tool results are limited to 256 KiB in aggregate per review and 64 KiB each, with narrower listing, read, search, path, URL, and query limits owned by application code.

When a valid internal repository listing, read, or search request exceeds its per-call output limit, the tool loop returns only the `repository_tool_output_limit_exceeded` category to Gemini. When an internal recursive search exceeds its scan limit, the loop similarly returns only `repository_search_limit_exceeded`. The failed request still consumes one internal repository and combined tool call, and Gemini may spend another call on narrower arguments. The application does not truncate the result, disclose partial content, choose narrower arguments, or retry the request itself. Recovery is classified by both failure category and requested tool: the output-limit category applies only to internal listing, reading, and searching, and the scan-limit category applies only to internal search. Public URL and GitHub repository tools do not inherit this recovery even if their broker surfaces a repository-prefixed failure. Other exhausted limits and broker failures retain their existing failure semantics.

The complete Gemini function-calling loop is limited to two minutes; each review generation allows 16,384 output tokens, and the final result allows 20 findings and 64 KiB of validated JSON. Each generation request makes at most five HTTP attempts with bounded exponential backoff for HTTP `408`, `429`, `500`, `502`, `503`, and `504` responses and transport errors so a transient response does not immediately discard the in-memory tool conversation; exhausting those attempts defers and later restarts the review. Only a candidate with finish reason `STOP` can supply a tool call or final result; token exhaustion, safety stops, malformed function calls, unexpected tool calls, and other incomplete finishes retry without parsing candidate content. Optional model-version and token-usage metadata may be absent without failing an otherwise complete response. Inputs that exceed a bound fail rather than being silently truncated. Download and model operations must never be the only record that review work exists.
