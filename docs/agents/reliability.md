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

An `open` action or an `update` action whose state is `opened` creates a review job directly from webhook ingress when not marked draft or work-in-progress. This includes pushed and rebased heads and transitions to ready; same-head updates remain idempotent. Eligible `close` and `merge` actions create feedback jobs as described below. Draft openings and updates, updates not in `opened` state, and all other merge request actions are persisted as ignored events without jobs. Periodic reconciliation discovers an open merge request if it is ready during a later scan.

Reject a missing or invalid webhook secret with `401`, an unlisted project namespace with `403`, malformed input with `400`, an oversized request with `413`, and authenticated overload with retryable `503` and `Retry-After` responses. A persistence failure returns a server error rather than acknowledging the event. Rejections log bounded operational identifiers and reasons without logging secrets or payload bodies. Graceful shutdown stops accepting new requests and gives active ingress transactions a bounded opportunity to finish.

## Feedback Ingestion

An authenticated, authorized merge request `close` or `merge` action is eligible when the merge request already has a completed review revision whose effective canonical review has both a durable publication and a locally validated structured result. Select the most recently created eligible completed revision job; a direct revision resolves to itself and an equivalent revision resolves through its canonical job. An external publication recovered after local state loss is ineligible because Wormtamer cannot safely reconstruct its structured review from rendered Markdown. In the webhook transaction, insert the delivery-deduplicated merge request event and create at most one feedback job for that merge request. A terminal event without an eligible review is retained as ignored. Note Hooks create no feedback work.

The first eligible close or merge event fixes the feedback job's terminal head and bound review. Later close, merge, reopen, update, or comment activity does not requeue, replace, or deactivate its result. The worker transiently loads the bounded diff for that head and up to 1,000 current comments, excluding internal, system, and Wormtamer-authored notes, then evaluates them with the bound structured review. Comment content is limited to 64 KiB per note and 512 KiB in aggregate. The model returns either one bounded reusable lesson or no memory; empty diffs follow the same path. Each job has at most five claims. A crash may repeat GitLab reads and Gemini evaluation after startup recovery, but feedback completion and optional memory insertion are one transaction and cannot create duplicate memory.

## Review Identity

Identify work by at least:

```text
GitLab instance + project ID + merge request IID + head SHA
```

The numeric project ID from the webhook remains the durable identity even though configuration authorizes repositories by namespace path. Deduplicate events for the same head SHA. Before publication or equivalent completion, confirm that the reviewed SHA remains current; obsolete findings must not be presented as current.

## Review Grace Period

The deployment-wide `grace_period` is a non-negative Go duration string such as `"1m"` or `"90s"`. It defaults to `"1m"` when omitted; `"0s"` disables the initial delay. Invalid or negative durations fail startup. This delay applies independently of CI to every new review identity, whether admitted by webhook or discovered by reconciliation, measured from local job creation rather than commit or MR creation time.

Persist the initial deadline in `next_attempt_at` with job insertion, rounding positive-delay deadlines up to whole seconds. Jobs remain queued without occupying the worker or consuming claims until due. Duplicate deliveries, same-head updates, repeated scans, and restarts do not move that deadline. Configuration changes affect only newly created jobs; ordinary retries, operator retries, and recovery of previously running jobs keep their existing scheduling semantics. Feedback jobs have no review grace period.

A newly observed head receives its own full delay. At execution, fresh GitLab validation rejects an old head or a no-longer-open MR before repository preparation or Gemini. Superseded jobs remain visible as queued until this validation marks them obsolete; webhook arrival order alone must not cancel another identity because deliveries and reconciliation observations can be stale. For heads first observed at 0s and 20s with a one-minute grace period, the first is checked and discarded at or after 60s, and the second cannot be reviewed before 80s. Already-running reviews are not interrupted by new arrivals and retain the current-head checks before publication.

## Wait for CI

The deployment-wide `wait_on_ci` boolean defaults to `false` when omitted. Explicit null and non-boolean values fail startup. Disabled installations make no CI eligibility requests.

When enabled, check CI after the persisted grace or retry deadline and before starting new repository or model work. Each check first validates the authorized project and confirms that the MR remains open at the job's exact head. A closed/merged MR or superseded head makes the job obsolete before evaluating CI. Then evaluate GitLab's selected current-revision MR pipeline:

| Observation | Outcome |
| --- | --- |
| Explicitly confirmed no selected pipeline | Proceed |
| Aggregate pipeline status `success` | Proceed |
| `created`, `waiting_for_resource`, `preparing`, `pending`, `running`, `failed`, `canceling`, `canceled`, `skipped`, `manual`, `scheduled`, or `waiting_for_callback` | Wait |
| Request failure, malformed metadata, unknown status, or unresolved association | Normal bounded error handling; never infer success or absence |

A successful non-green observation keeps the job queued, records CI waiting and the last validated status, and sets `next_attempt_at` to one minute after the observation, rounded up to whole seconds. Release the worker immediately; other due reviews remain eligible. Waiting has no expiry or eventual bypass, consumes no review attempts, and preserves earlier failure counts and categories. Actual API or infrastructure errors still consume the finite review-attempt budget, including errors interleaved with successful waits. A crash or cancellation during uncharged preflight leaves queued work recoverable without consuming the last attempt.

Waiting metadata and deadlines survive restart. Duplicate events and scans do not reset schedules or review identity. Refresh pipeline selection on each check so a successful retry or replacement on the same head unblocks its existing job without another grace period. Rechecks may be later because of worker load or GitLab backpressure. Apply the current deployment's `wait_on_ci` on subsequent due checks without resetting existing grace or retry deadlines. Terminal jobs stop polling. The read-only panel shows “Waiting for CI”, the persisted last status, and the next check deadline rather than presenting non-green CI as a review failure; rendering makes no GitLab requests.

CI is only a start condition. Locally saved results and exact-head external-marker recovery bypass CI waiting, and patch-equivalence suppression remains in effect after eligibility. Do not interrupt already-running reviews, regenerate completed results after CI changes, or reapply the gate to publication recovery. Current-head publication checks remain mandatory.

The no-pipeline exception intentionally accepts the pipeline-creation race: grace reduces but cannot eliminate a pipeline appearing after work begins. Do not inspect CI files, traverse jobs, or independently implement allowed-failure or optional-job rules.

### Pipeline Association

Use the single-MR REST response's explicitly present `head_pipeline`, not the legacy `pipeline` field, a project-wide latest pipeline, or a history scan. An explicit null confirms no selected pipeline. An omitted field is malformed/inaccessible metadata, not absence. Validate positive pipeline and project IDs and a well-formed pipeline SHA. For branch-backed or detached MR pipelines, that SHA must match the exact job head.

A merged-results pipeline may run on a generated commit. When the pipeline SHA differs from the source head, query the same MR's GraphQL `headPipeline` and require matching project ID, MR ID, current diff head, pipeline ID, and pipeline SHA, with an open MR and no GraphQL errors. GitLab resolves this field through its source-SHA-aware current-head association. Null or inconsistent pipeline confirmation retries under `merge_request_pipeline_identity_unresolved`; it never confirms absence or lets an old successful pipeline unlock review. Both API paths share the broker's request deadlines, metadata response limit, redirect rejection, rate-limit gate, and sanitized HTTP errors.

Association evidence from GitLab 17.5.5:

- The [REST MR entity](https://gitlab.com/gitlab-org/gitlab/-/blob/v17.5.5-ee/lib/api/entities/merge_request.rb) exposes the raw `head_pipeline` association only with `read_pipeline` permission. The [REST pipeline entity](https://gitlab.com/gitlab-org/gitlab/-/blob/v17.5.5-ee/lib/api/entities/ci/pipeline_basic.rb) does not expose `source_sha`.
- [GraphQL `headPipeline`](https://gitlab.com/gitlab-org/gitlab/-/blob/v17.5.5-ee/app/graphql/types/merge_request_type.rb) resolves `diff_head_pipeline`. The [MR model](https://gitlab.com/gitlab-org/gitlab/-/blob/v17.5.5-ee/app/models/merge_request.rb) returns that association only when `matches_sha_or_source_sha?(diff_head_sha)` holds; the [pipeline model](https://gitlab.com/gitlab-org/gitlab/-/blob/v17.5.5-ee/app/models/ci/pipeline.rb) checks the pipeline commit or its source commit. This supports generated merged-results commits without guessing from refs or commit parents.
- GitLab's `find_diff_head_pipeline` selects MR-associated pipelines matching SHA or source SHA. Its [finder](https://gitlab.com/gitlab-org/gitlab/-/blob/v17.5.5-ee/app/finders/ci/pipelines_for_merge_request_finder.rb) includes branch-backed pipelines, prioritizes MR-event pipelines, and orders newest IDs first within that priority. Wormtamer follows GitLab's selection rather than independently aggregating historical, child, or downstream pipelines.

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
queued -> running -> completed
            |
            +-> retry

unrecoverable or exhausted retries -> failed
```

At service startup, before workers begin, one transaction recovers interrupted `running` review and feedback jobs. Jobs with claims remaining return to `queued` and become immediately due; jobs whose fifth claim was interrupted become `failed` with `attempts_exhausted`. Completed, failed, obsolete, and already queued jobs are unchanged. Recovery does not consume another claim.

The review worker selects a due queued identity for uncharged marker lookup and optional [CI eligibility](#wait-for-ci), without holding a SQLite transaction across HTTP requests. Only admitted work or a real preflight error conditionally claims that same still-due identity; it must not claim a different job using the first job's eligibility decision. The feedback worker claims due work directly. Each claim atomically moves its job to `running` and increments its attempt count. With the supported one-process, one-replica deployment, no other process may claim running work. Result checkpoints, publication completion, retries, and terminal transitions require the expected job identity and `running` state. A validated review result remains in `running`; if the process stops afterward, startup recovery requeues it and the next claim resumes publication without repository preparation or another Gemini review. A review without a result retains marker-first external recovery on its next preflight. If failure handling cannot durably transition a claimed job, the worker returns an error and stops the service so startup recovery can handle the remaining `running` job; it does not log and continue polling.

Graceful shutdown stops new claims and gives active work a bounded opportunity to finish. Cancellation-interrupted charged work remains `running` for the next startup recovery; uncharged review preflight remains queued. There is no in-process timeout claimant.

Restart the review rather than persisting an arbitrary model conversation. Persist checkpoints around external effects.

Use at most five claims per job. Optional patch-ID waiting consumes at most one claim and preserves at least three claims, including the next active claim, for normal review and publication. Network failures, HTTP request timeouts including status `408`, rate limits, and server failures are retryable; credential, authorization, and other rejected requests are permanent. Locally calculated exponential retry delays start at five seconds and cap at five minutes. A valid GitLab `Retry-After` is instead a minimum delay and may defer work for up to 24 hours; a longer requested delay fails under the stable `retry_after_exceeds_limit` category rather than retrying early. The shared GitLab client also delays all subsequent worker and reconciliation requests until a supported `Retry-After` expires; a `429` without a valid value applies a five-minute process-local delay. This application-wide gate is not durable across restart. Record the bounded error category and message and next attempt time. Distinguish retryable failures, permanent configuration or authorization failures, and obsolete work. For review work, a recognized merge request state other than `opened`, including `closed` or `merged`, and a changed head SHA are obsolete. Feedback work requires GitLab to report `closed` or `merged` at the fixed head before evidence is sent to Gemini; an `opened` state retries, while an identity mismatch or malformed state fails. Renamed or unauthorized projects fail. Failed review and feedback jobs remain inspectable through the read-only web panel and may be retried after correction only through local operational commands. `wormtamer -config <path> jobs list-failed` returns one JSON document containing at most 100 failures ordered by newest update time, with a truncation indicator and only the job kind and ID, attempt count, last error category, update time, numeric project and merge request identifiers, and the applicable head SHA. It does not return stored error messages or workflow, repository, comment, result, memory, or credential content.

`wormtamer -config <path> jobs retry review <job-id>` and `jobs retry feedback <job-id>` conditionally move exactly one currently failed job to `queued`, reset its attempt count, make it immediately due, and clear its failure fields. Missing jobs and jobs no longer in `failed` fail distinctly without mutation, so concurrent commands cannot reset newly active work. Retry preserves review identity, source events, validated results, publication records, immutable feedback review bindings, and any atomically completed memory. A retried review with a validated result resumes publication rather than invoking Gemini again. Operational commands open only configured SQLite state and do not initialize external clients, perform startup recovery, start background components, or open the HTTP listener.

The review worker polls once per second and processes one job at a time. On shutdown it allows active work ten seconds to reach a checkpoint. At that deadline it cancels the job and returns without waiting for an uncooperative operation; unfinished work remains recoverable at the next service startup.

## Read-only Observability

The built-in panel reads committed SQLite workflow and memory state through bounded queries and cursor pagination. Rendering a page performs no workflow mutation and no external request. Dashboard counts and recent lists are separate short reads rather than one long snapshot transaction, so concurrent worker commits may become visible between sections of one response. The panel reports durable local state, not live GitLab, Gemini, worker, or reconciliation connectivity. It shows bounded failure categories but not stored error messages. External-only publication recovery is visible without fabricating a local result. Reconciled jobs with no directly associated webhook retain numeric project identity in the panel rather than triggering a lookup or adding presentation-only state.

## Idempotent Publication

GitLab publication and its local record cannot be committed atomically. The current worker gives its one summary note a stable hidden marker derived from review identity, for example:

```html
<!-- wormtamer:review=<review-identity-hash> -->
```

For a selected due job without a locally validated result, search the newest notes before claiming work, checking CI, loading review evidence, or invoking Gemini, examining at most 1,000 notes across ten pages for the exact marker on a note authored by the PAT's authenticated GitLab user. Fail closed if absence cannot be established. When a matching note exists, claim that same job and confirm that the merge request remains open at the exact head SHA, then atomically store its marker and GitLab note ID and complete the job without fabricating or regenerating a structured result. This external-only recovery suppresses duplicate model work and publication but remains ineligible for feedback evaluation.

When no matching note exists, apply optional CI eligibility, claim the same job, then load review evidence and apply patch equivalence. An equivalent head creates no marker or note of its own and does not edit the canonical publication. Otherwise perform the review normally. Before posting, search again to cover an existing publication or a lost response, and reconcile GitLab and SQLite before creating another note. After posting, store the marker and GitLab note ID. Limit the rendered note to 64 KiB and complete a direct job only after the marked note exists and its publication record is durable. Future separate finding discussions require their own stable finding identities.

## Reconciliation

Reconciliation runs immediately after startup and five minutes after each completed cycle. It sequentially resolves every configured project by exact namespace path and lists up to ten pages of 100 open merge requests. Ready revisions are inserted page by page through the same unique review identity used by webhook ingress; drafts and work-in-progress entries are skipped as observed. Existing jobs in any state are not reset.

Project failures do not terminate the process or roll back jobs committed from earlier pages. A later cursor-free scan starts the project again from its first page, and uniqueness makes repetition safe. Rate limiting or a supported `Retry-After` stops the current cycle while the shared GitLab request gate delays subsequent application requests. Scheduling and backpressure are process-local, so restart triggers an immediate scan.

Reconciliation is read-only at GitLab and does not invoke Gemini or publish. Webhooks provide low latency; reconciliation recovers deliveries lost before reaching the application.

## Durable State

SQLite stores the locally validated structured review result before publication and reuses that checkpoint on retry; it does not persist prompts, diffs, raw model responses, conversations, repository workspaces, or command output. SQLite state must represent:

- Durable merge request webhook events and processing status
- Terminal merge request events and one immutable feedback job per eligible merge request, without diff or comment bodies
- Review and feedback job state, attempts, scheduling, and errors; review CI waiting and its last validated bounded status
- Review patch-ID status and value, plus an equivalent job's canonical job relationship
- Publication markers and GitLab object IDs; a publication may lack a local review result only when recovered from an existing exact marker before generation, while an equivalent job has neither a result nor publication of its own
- Application-owned finding identifiers and their ordered positions under validated review results
- Feedback jobs bound to immutable published reviews, plus at most one repository-scoped lesson per completed feedback job
- Versioned identities of memory materialized for each successfully checkpointed review, without lesson copies
- Last-seen and successfully reviewed merge request revisions

Webhook event insertion and its job creation occur in one transaction. Webhook events retain the bounded raw JSON payload and delivery, project, merge request, revision, action, and outcome metadata needed for later inspection. Events and jobs are separate records with database-enforced delivery and review uniqueness. A reconciled job has no source event; a later webhook for the same review identity is persisted as a duplicate review without creating another job.

Keep the database and WAL on persistent storage with reliable file locking. Enable foreign keys, configure bounded lock waiting, and choose durability settings and backup procedures appropriate to the deployment platform. The application owns and checks the SQLite schema version at startup. Run one replica per installation.

There is no merge request webhook-event or payload retention policy yet; these records accumulate until operational requirements establish one. Feedback jobs retain no diff or comment body.

Repository workspaces are disposable and rebuilt when a review attempt restarts. One two-minute setup deadline bounds cloning the current repository and, when sharing is enabled, every other authorized repository, fetching refs, validating the exact merge-request head, credential cleanup, ownership transfer, and atomic exposure. Internal expiry terminates active Git process groups, removes private staging, exposes no partial root, and retries as `repository_preparation_timeout`; parent cancellation performs the same cleanup and propagates its context error. Ref/head changes retain obsolete semantics, while other sanitized transport and process failures retry as `repository_preparation_failed`. The GitLab API broker continues to limit metadata and diff requests to 30 seconds, 256 KiB metadata, five diff pages, 100 files, and 512 KiB aggregate diff content.

`read` streams file contents as text from the head, limited to 2,000 lines or 50 KiB. It stops after enough input to produce the bounded result rather than loading the complete file or scanning the remainder for a total line count. A caller offset is one-indexed and an optional limit is honored before truncation. Continuation notices identify the shown range and next offset; a first line larger than 50 KiB produces an actionable Bash fallback instead of a partial line. Ordinary argument and filesystem errors are model-correctable; malformed helper framing or JSON fails the attempt.

`bash` has no tool-specific default timeout; a positive caller timeout is model-correctable, while parent cancellation propagates after process-group cleanup. Combined stdout and stderr are observed through one pipe. Model-visible output keeps the last 2,000 lines or 50 KiB and, when truncated, points to a complete review-local output file. Streaming capture permits at most 16 MiB per command and 64 MiB of cumulative full-output spool writes per review, without refund after file deletion. Exhaustion terminates and reaps the active shell process group and returns `bash_output_limit_exceeded` with bounded tail output. Spool create, write, or sync failure is an infrastructure failure. Process-group signaling does not guarantee cleanup of deliberately detached new sessions.

Every ordinary generation declares exactly `read` and `bash`; there is no call-count or category limit, so more than sixteen small calls are valid. Requested calls in one turn are prevalidated and dispatched in model order. A review admits at most 16 MiB of cumulative serialized Gemini `FunctionResponse` evidence, including IDs, names, and JSON output or error values. Before appending each response, trusted code charges its complete serialization. If it would exceed the allowance, that result is discarded, later same-batch calls are not dispatched, and the current and later calls receive ordered fixed `tool_result_limit_exceeded` bookkeeping responses. Those fixed responses are not charged, and the next generation is final-only with function calling set to `NONE`. A function call, incomplete finish, malformed content, or invalid structured result in final-only mode retains normal review retry behavior.

Info generation logs expose per-turn `tool_call_count` and bounded `tool_names` for production observation. Tool mistakes, missing or unreadable paths, non-zero shell exits, explicit shell timeouts, and shell output exhaustion return correctable function responses. Workspace creation, process startup, spool I/O, persistence failures, and parent cancellation use ordinary workflow failure and retry handling.

The complete Gemini function-calling loop is limited to five minutes. Each generation receives a fresh two-minute deadline that includes all SDK HTTP attempts and backoff, and the Gemini HTTP client independently limits each HTTP request to two minutes. The remaining whole-review deadline is always the absolute limit for generations and tool calls. Whole-review expiry retries as `review_timeout`, per-generation expiry retries as `gemini_timeout`, and parent cancellation continues to propagate unchanged. Each review generation allows 16,384 output tokens, and the final result allows 20 findings and 64 KiB of validated JSON. Each generation request makes at most five HTTP attempts with bounded exponential backoff for HTTP `408`, `429`, `500`, `502`, `503`, and `504` responses and transport errors so a transient response does not immediately discard the in-memory tool conversation; exhausting those attempts defers and later restarts the review. Only a candidate with finish reason `STOP` can supply a tool call or final result; token exhaustion, safety stops, malformed function calls, unexpected tool calls, and other incomplete finishes retry without parsing candidate content. Optional model-version and token-usage metadata may be absent without failing an otherwise complete response. Inputs that exceed a bound fail rather than being silently truncated. Repository setup, tool, and model operations must never be the only record that review work exists.
