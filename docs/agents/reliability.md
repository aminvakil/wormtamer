# Reliability

## Guarantee

The system provides at-least-once review execution with idempotent external effects, not exactly-once execution. A crash may repeat work but must not silently lose a merge request revision.

## Webhook Ingestion

`POST /webhooks/gitlab` authenticates the request before reading it, admits at most four concurrent webhook requests, and limits each admitted body to 1 MiB. Merge request hooks are then parsed and their exact project namespace is authorized against the installation configuration. Handle an authenticated, authorized merge request webhook in this order:

1. Begin a SQLite transaction.
2. Insert the event using a stable delivery identifier.
3. Create or confirm its review or feedback job when the event is eligible, or record why it was ignored.
4. Commit.
5. Return success.

Never acknowledge an accepted merge request event before commit. Duplicate delivery must resolve to the same event or job and return success without creating another job. Use `X-Gitlab-Event-UUID` as the delivery identifier when available; otherwise derive a deterministic SHA-256 identifier from the GitLab instance and raw bounded payload.

An `open` action that is not marked draft or work-in-progress creates a review job directly from webhook ingress. Eligible `close` and `merge` actions create feedback jobs as described below. Authenticated and authorized draft openings and all other merge request actions are persisted as ignored events without jobs. Periodic reconciliation discovers an open merge request if it is ready during a later scan.

Reject a missing or invalid webhook secret with `401`, an unlisted project namespace with `403`, malformed input with `400`, an oversized request with `413`, and authenticated overload with retryable `503` and `Retry-After` responses. A persistence failure returns a server error rather than acknowledging the event. Rejections log bounded operational identifiers and reasons without logging secrets or payload bodies. Graceful shutdown stops accepting new requests and gives active ingress transactions a bounded opportunity to finish.

## Feedback Ingestion

An authenticated, authorized merge request `close` or `merge` action is eligible when the merge request already has a completed review revision whose effective canonical review has both a durable publication and a locally validated structured result. Select the most recently created eligible completed revision job; a direct revision resolves to itself and an equivalent revision resolves through its canonical job. An external publication recovered after local state loss is ineligible because Wormtamer cannot safely reconstruct its structured review from rendered Markdown. In the webhook transaction, insert the delivery-deduplicated merge request event and create at most one feedback job for that merge request. A terminal event without an eligible review is retained as ignored. Note Hooks create no feedback work.

The first eligible close or merge event fixes the feedback job's terminal head and bound review. Later close, merge, reopen, update, or comment activity does not requeue, replace, or deactivate its result. The worker transiently loads the bounded diff for that head and up to 1,000 current comments, excluding internal, system, and Wormtamer-authored notes, then evaluates them with the bound structured review. Comment content is limited to 64 KiB per note and 512 KiB in aggregate. The model returns either one bounded reusable lesson or no memory; empty diffs follow the same path. Each job has at most five claims and a renewable three-minute lease. A crash may repeat GitLab reads and Gemini evaluation, but feedback completion and optional memory insertion are one transaction and cannot create duplicate memory.

## Review Identity

Identify work by at least:

```text
GitLab instance + project ID + merge request IID + head SHA
```

The numeric project ID from the webhook remains the durable identity even though configuration authorizes repositories by namespace path. Deduplicate events for the same head SHA. Before publication or equivalent completion, confirm that the reviewed SHA remains current; obsolete findings must not be presented as current.

## Patch Equivalence

For a job without a result or exact existing-head publication, load bounded GitLab diff-version metadata with the review snapshot. Accept a lowercased 40- or 64-character hexadecimal `patch_id_sha` only from a finalized version whose merge request and head SHA match the claimed identity. A matching `collected` version with a null patch ID, or the absence of a matching current version, is pending. Defer the first pending observation once under `merge_request_patch_id_pending` only when at least three later claims remain for normal review and publication. Persist that pending checkpoint with the retry. A repeated pending observation, insufficient retry reserve, or a terminal version without a patch ID proceeds through normal review with an explicit unavailable outcome. Unknown states, malformed values, identity mismatches, and GitLab request failures retain their ordinary failure handling.

An available patch ID suppresses work only when the newest canonical job by revision-job creation order in the same GitLab instance, project, and merge request is completed with its own local result and durable publication. The new exact-head job records the patch ID and canonical job relationship and completes without a result, findings, memory-retrieval audit, publication, Gemini call, or repository preparation. Canonical jobs cannot point to another canonical job, so aliases never form chains.

Patch equality deliberately inherits Git's whitespace-insensitive patch-ID semantics and can survive target-branch changes that alter surrounding repository context. These are accepted deduplication semantics, not a change to head-based review or finding identity. Existing rows without patch identity are not backfilled. If SQLite state is lost, Wormtamer does not infer equivalence from an old head-based note; a rebased head receives a new review and publication.

## Review Feedback Target Identity

The first-class overall-review target is `WT-R-` followed by the unpadded uppercase base32 encoding of the first 128 bits of a SHA-256 digest. Hash a `wormtamer:review:v1` domain separator, canonical GitLab instance, numeric project ID, merge request IID, and lowercase head SHA as UTF-8 fields separated and terminated by zero bytes. The target is supplied by trusted application code and remains distinct from finding identities.

A derived memory identity is `WT-M-` plus the same bounded digest encoding over a `wormtamer:memory:v3` domain separator, canonical GitLab instance, numeric project ID, and merge request IID. At most one memory job and memory record exist for that merge request.

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

Use at most five claims per job. Optional patch-ID waiting consumes at most one claim and preserves at least three claims, including the next active claim, for normal review and publication. Network failures, HTTP request timeouts including status `408`, rate limits, and server failures are retryable; credential, authorization, and other rejected requests are permanent. Locally calculated exponential retry delays start at five seconds and cap at five minutes. A valid GitLab `Retry-After` is instead a minimum delay and may defer work for up to 24 hours; a longer requested delay fails under the stable `retry_after_exceeds_limit` category rather than retrying early. The shared GitLab client also delays all subsequent worker and reconciliation requests until a supported `Retry-After` expires; a `429` without a valid value applies a five-minute process-local delay. This application-wide gate is not durable across restart. Record the bounded error category and message and next attempt time. Distinguish retryable failures, permanent configuration or authorization failures, and obsolete work. For review work, a recognized merge request state other than `opened`, including `closed` or `merged`, and a changed head SHA are obsolete. Feedback work requires GitLab to report `closed` or `merged` at the fixed head before evidence is sent to Gemini; an `opened` state retries, while an identity mismatch or malformed state fails. Renamed or unauthorized projects fail. Failed review and feedback jobs remain inspectable through the read-only web panel and may be retried after correction only through local operational commands. `wormtamer -config <path> jobs list-failed` returns one JSON document containing at most 100 failures ordered by newest update time, with a truncation indicator and only the job kind and ID, attempt count, last error category, update time, numeric project and merge request identifiers, and the applicable head SHA. It does not return stored error messages or workflow, repository, comment, result, memory, or credential content.

`wormtamer -config <path> jobs retry review <job-id>` and `jobs retry feedback <job-id>` conditionally move exactly one currently failed job to `queued`, reset its attempt count, make it immediately due, and clear its failure and lease fields. Missing jobs and jobs no longer in `failed` fail distinctly without mutation, so concurrent commands cannot reset newly active work. Retry preserves review identity, source events, validated results, publication records, immutable feedback review bindings, and any atomically completed memory. The ordinary claim path moves a retried review with a validated result directly to `publishing` rather than invoking Gemini again. Operational commands open only configured SQLite state and do not initialize external clients, background components, or the HTTP listener.

The review worker polls once per second, leases one job for two minutes, renews every 30 seconds, and processes one job at a time. On shutdown it allows active work ten seconds to reach a checkpoint. At that deadline it cancels the job and returns without waiting for an uncooperative operation; unfinished work remains recoverable through lease expiry.

## Model Generation Diagnostics

Create one durable diagnostic record before every application-level Gemini SDK call made by review or feedback processing. Failure to create that record prevents the model call. Complete the same record after a response or request error. The completion write uses a separate five-second context so lease loss or workflow cancellation after the SDK returns does not prevent the checkpoint; a completion-write failure remains a persistence failure and leaves the started record incomplete rather than reconstructing usage from candidate content. SDK latency covers only the SDK call and excludes the durable pre-request insert. On service startup, records still started from the previous process become `unknown`. The local jobs command does not perform this transition and cannot disturb an active service request.

Workflow retries and review tool turns create additional records because each can consume model usage. One record may include multiple hidden SDK HTTP attempts; Wormtamer neither enumerates nor infers usage for those attempts. A returned response may complete without usage metadata, while a request error completes as failed without inferred usage. Missing usage metadata and non-negative counts whose total is inconsistent with prompt, tool-use prompt, candidate, and thought counts remain explicit and cannot produce a catalog-derived estimate.

When the Gemini Developer API is used directly, startup retrieves the configured model's USD rates from LiteLLM's bounded public pricing catalog. Failure, an absent model, malformed data, or unavailable required rates disables catalog estimates without blocking service. Standard or above-200k rates are selected from total prompt and tool-use input, then exact integer fixed point charges uncached prompt and tool-use input at the input rate, cached input at the cache-read rate, candidates at the output rate, and thoughts at the reasoning rate. For a custom endpoint, a single valid non-negative USD `x-litellm-response-cost` returned through the SDK's processed response headers is converted with decimal arithmetic to the nearest integer picodollar, with half-picodollar ties rounded up. It takes precedence and does not require locally consistent token metadata; a missing, repeated, malformed, negative, unsupported-exponent, or out-of-range value means no cost rather than an assumed upstream rate. Aggregate USD costs remain stable after catalog changes. They are diagnostics, not invoice reconciliation.

## Read-only Observability

The built-in panel reads committed SQLite workflow, memory, and model-generation state through bounded queries and cursor pagination. Rendering a page performs no workflow mutation and no external request. Dashboard counts and recent lists are separate short reads rather than one long snapshot transaction, so concurrent worker commits may become visible between sections of one response.

Usage aggregates use rolling 24-hour, 7-day, or 30-day request-time windows, bounded model and project breakdowns, and fixed-size generation pagination. The panel reports durable local state, not live GitLab, Gemini, worker, reconciliation, or billing connectivity. It shows bounded failure categories but not stored error messages. External-only publication recovery is visible without fabricating a local result. Reconciled jobs with no directly associated webhook retain numeric project identity in the panel rather than triggering a lookup or adding presentation-only state.

## Idempotent Publication

GitLab publication and its local record cannot be committed atomically. The current worker gives its one summary note a stable hidden marker derived from review identity, for example:

```html
<!-- wormtamer:review=<review-identity-hash> -->
```

For a claimed job without a locally validated result, search the newest notes before loading review evidence or invoking Gemini, examining at most 1,000 notes across ten pages for the exact marker on a note authored by the PAT's authenticated GitLab user. Fail closed if absence cannot be established. When a matching note exists, confirm that the merge request remains open at the exact head SHA, then atomically store its marker and GitLab note ID and complete the job without fabricating or regenerating a structured result. This external-only recovery suppresses duplicate model work and publication but remains ineligible for feedback evaluation.

When no matching note exists, load review evidence and first apply patch equivalence. An equivalent head creates no marker or note of its own and does not edit the canonical publication. Otherwise perform the review normally. Before posting, search again to cover an existing publication or a lost response, and reconcile GitLab and SQLite before creating another note. After posting, store the marker and GitLab note ID. Limit the rendered note to 64 KiB and complete a direct job only after the marked note exists and its publication record is durable. Future separate finding discussions require their own stable finding identities.

## Reconciliation

Reconciliation runs immediately after startup and five minutes after each completed cycle. It sequentially resolves every configured project by exact namespace path and lists up to ten pages of 100 open merge requests. Ready revisions are inserted page by page through the same unique review identity used by webhook ingress; drafts and work-in-progress entries are skipped as observed. Existing jobs in any state are not reset.

Project failures do not terminate the process or roll back jobs committed from earlier pages. A later cursor-free scan starts the project again from its first page, and uniqueness makes repetition safe. Rate limiting or a supported `Retry-After` stops the current cycle while the shared GitLab request gate delays subsequent application requests. Scheduling and backpressure are process-local, so restart triggers an immediate scan.

Reconciliation is read-only at GitLab and does not invoke Gemini or publish. Webhooks provide low latency; reconciliation recovers deliveries lost before reaching the application.

## Durable State

SQLite stores the locally validated structured review result before publication and reuses that checkpoint on retry; it does not persist prompts, diffs, raw model responses, conversations, repository workspaces, or command output. SQLite state must represent:

- Durable merge request webhook events and processing status
- Terminal merge request events and one immutable feedback job per eligible merge request, without diff or comment bodies
- Review and feedback job state, leases, attempts, scheduling, and errors
- Review patch-ID status and value, plus an equivalent job's canonical job relationship
- Publication markers and GitLab object IDs; a publication may lack a local review result only when recovered from an existing exact marker before generation, while an equivalent job has neither a result nor publication of its own
- Application-owned finding identifiers and their ordered positions under validated review results
- Feedback jobs bound to immutable published reviews, plus at most one repository-scoped lesson per completed feedback job
- Versioned identities of memory materialized for each successfully checkpointed review, without lesson copies
- One structured record for every application-level review and feedback Gemini request, including workflow correlation, observed usage metadata, completion state, and an optional retrieved-cost source and fixed-point USD estimate
- Last-seen and successfully reviewed merge request revisions

Webhook event insertion and its job creation occur in one transaction. Webhook events retain the bounded raw JSON payload and delivery, project, merge request, revision, action, and outcome metadata needed for later inspection. Events and jobs are separate records with database-enforced delivery and review uniqueness. A reconciled job has no source event; a later webhook for the same review identity is persisted as a duplicate review without creating another job.

Keep the database and WAL on persistent storage with reliable file locking. Enable foreign keys, configure bounded lock waiting, and choose durability settings and backup procedures appropriate to the deployment platform. The application owns and checks the SQLite schema version at startup. Run one replica per installation.

There is no merge request webhook-event, payload, or model-generation retention policy yet; these records accumulate until operational requirements establish one. Feedback jobs retain no diff or comment body, and model-generation records retain no model or repository content.

Repository workspaces are disposable and rebuilt when a review attempt restarts. One two-minute setup deadline bounds cloning the current repository and, when sharing is enabled, every other authorized repository, fetching refs, validating the exact merge-request head, credential cleanup, ownership transfer, and atomic exposure. Internal expiry terminates active Git process groups, removes private staging, exposes no partial root, and retries as `repository_preparation_timeout`; parent cancellation performs the same cleanup and propagates its context error. Ref/head changes retain obsolete semantics, while other sanitized transport and process failures retry as `repository_preparation_failed`. The GitLab API broker continues to limit metadata and diff requests to 30 seconds, 256 KiB metadata, five diff pages, 100 files, and 512 KiB aggregate diff content.

`read` streams file contents as text from the head, limited to 2,000 lines or 50 KiB. It stops after enough input to produce the bounded result rather than loading the complete file or scanning the remainder for a total line count. A caller offset is one-indexed and an optional limit is honored before truncation. Continuation notices identify the shown range and next offset; a first line larger than 50 KiB produces an actionable Bash fallback instead of a partial line. Ordinary argument and filesystem errors are model-correctable; malformed helper framing or JSON fails the attempt.

`bash` has no tool-specific default timeout; a positive caller timeout is model-correctable, while parent cancellation propagates after process-group cleanup. Combined stdout and stderr are observed through one pipe. Model-visible output keeps the last 2,000 lines or 50 KiB and, when truncated, points to a complete review-local output file. Streaming capture permits at most 16 MiB per command and 64 MiB of cumulative full-output spool writes per review, without refund after file deletion. Exhaustion terminates and reaps the active shell process group and returns `bash_output_limit_exceeded` with bounded tail output. Spool create, write, or sync failure is an infrastructure failure. Process-group signaling does not guarantee cleanup of deliberately detached new sessions.

Every ordinary generation declares exactly `read` and `bash`; there is no call-count or category limit, so more than sixteen small calls are valid. Requested calls in one turn are prevalidated and dispatched in model order. A review admits at most 16 MiB of cumulative serialized Gemini `FunctionResponse` evidence, including IDs, names, and JSON output or error values. Before appending each response, trusted code charges its complete serialization. If it would exceed the allowance, that result is discarded, later same-batch calls are not dispatched, and the current and later calls receive ordered fixed `tool_result_limit_exceeded` bookkeeping responses. Those fixed responses are not charged, and the next generation is final-only with function calling set to `NONE`. A function call, incomplete finish, malformed content, or invalid structured result in final-only mode retains normal review retry behavior.

Info generation logs expose per-turn `tool_call_count` and bounded `tool_names` for production observation. Tool mistakes, missing or unreadable paths, non-zero shell exits, explicit shell timeouts, and shell output exhaustion return correctable function responses. Workspace creation, process startup, spool I/O, persistence failures, and parent cancellation use ordinary workflow failure and retry handling.

The complete Gemini function-calling loop is limited to two minutes; expiry of this internal deadline retries as `review_timeout`, while parent cancellation continues to propagate unchanged. Each review generation allows 16,384 output tokens, and the final result allows 20 findings and 64 KiB of validated JSON. Each generation request makes at most five HTTP attempts with bounded exponential backoff for HTTP `408`, `429`, `500`, `502`, `503`, and `504` responses and transport errors so a transient response does not immediately discard the in-memory tool conversation; exhausting those attempts defers and later restarts the review. Only a candidate with finish reason `STOP` can supply a tool call or final result; token exhaustion, safety stops, malformed function calls, unexpected tool calls, and other incomplete finishes retry without parsing candidate content. Optional model-version and token-usage metadata may be absent without failing an otherwise complete response. Inputs that exceed a bound fail rather than being silently truncated. Repository setup, tool, and model operations must never be the only record that review work exists.
