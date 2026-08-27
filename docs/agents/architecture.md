# Architecture

## System Model

The application is a self-hosted GitLab merge request reviewer. Each installation serves one team with independent credentials, configuration, and SQLite state. Runtime memory and disposable repository workspaces remain installation-local. Installations share no database or control plane.

One installation runs as one process and replica because SQLite and local repository caches are not distributed coordination mechanisms.

## Stack

- Go with the standard library HTTP server unless a concrete requirement proves it insufficient
- One OCI image with persistent SQLite storage
- SQLite through `github.com/mattn/go-sqlite3`, built with CGO, as the only application database
- Gemini through `google.golang.org/genai`, using the Developer API directly or a configured Gemini Developer API-compatible endpoint, as the only model backend
- A small explicit Gemini function-calling loop with no general agent framework or automatic tool execution

A narrow Gemini client interface is used as a test seam, not as a provider abstraction. SQLite compatibility has a hard stop at [v0.28.0](https://github.com/aminvakil/wormtamer/releases/tag/v0.28.0); state created by earlier releases is unsupported. The Gemini model is an explicit required configuration value. Review output and resource limits remain application-owned; the review thinking level is deployment-configurable.

## Compatibility Baseline

Compatibility with Gemini versions earlier than 3 is explicitly out of scope. Do not add fallbacks, tests, or review findings for those versions. Otherwise, investigate model-version compatibility only for a concrete observed failure or an explicit task.

No release or production deployment compatibility baseline exists yet. Until one is explicitly established, changes do not need to preserve configuration formats, application interfaces, or SQLite state created by earlier development builds; recreating development configuration or state is acceptable when it keeps the current design simpler and correct. This does not relax correctness, durability, or recovery requirements for a running version.

Establish an upgrade and compatibility policy before the first production deployment.

## Deployment Configuration

The process starts with an explicit JSON configuration path, for example `wormtamer -config ./config.json`, and fails startup when the file or a required value is missing or invalid. Configuration decoding rejects unknown fields. Relative data and review-workspace paths resolve from the configuration file's directory. The configuration defines the listen address, SQLite path, disposable review-workspace path, log level, HTTP or HTTPS GitLab base URL, webhook secret, GitLab personal access token, model API key, optional Gemini Developer API-compatible base URL, model, review thinking level, authorized internal repositories, and whether reviews receive every other authorized repository as related context; required values must be non-empty and repository entries must be well-formed and unique. The review-workspace path must be outside service-private configuration and database directories. `log_level` accepts `debug`, `info`, `warn`, or `error` and defaults to `info` when omitted. When `gemini.base_url` is omitted, the SDK uses the Gemini Developer API. When set, it is the validated HTTP or HTTPS base URL used for both reviews and feedback evaluation. The endpoint must accept the Gemini Developer API request path, authentication header, function-calling, structured-output, and thinking configuration used by Wormtamer and return native Gemini responses. OpenAI-compatible endpoints serving Gemini models are not sufficient. `gemini.thinking_level` defaults to `default`, which leaves the SDK thinking configuration unset. Any other non-empty value is passed through without a local allowlist so model-specific support is decided by the endpoint. The validated GitLab and configured model endpoint URLs are canonicalized. The review worker is always enabled, so its credentials are required at startup without external credential or scope validation.

```json
{
  "listen_address": "127.0.0.1:8080",
  "database_path": "data/wormtamer.db",
  "review_workspace_path": "/var/lib/wormtamer-reviews",
  "log_level": "info",
  "gitlab": {
    "base_url": "https://gitlab.example",
    "webhook_secret": "replace-me",
    "personal_access_token": "replace-me"
  },
  "gemini": {
    "api_key": "replace-me",
    "base_url": "",
    "model": "replace-me",
    "thinking_level": "default"
  },
  "authorized_repositories": ["group/project", "group/shared-contracts"],
  "share_all_authorized_repositories": false
}
```

Authorized repositories are identified by exact GitLab namespace paths such as `group/project`. The same list authorizes webhook ingress and bounds which internal repositories can be disclosed to and inspected by the model. Authorization remains independent from repository sharing, so an unlisted path is never eligible for trusted preparation.

`share_all_authorized_repositories` defaults to `false`. When `false`, a review prepares only its current repository. When `true`, every other authorized repository is available as related context; the current repository is excluded and the related set is ordered deterministically. Enabling it is an operator assertion that every authorized repository has the same review audience, not a request to inspect repositories eagerly or exhaustively. Keep it disabled when repository audiences differ.

Authorization by path intentionally fails after a project rename until configuration is updated; durable review identity still uses the numeric project ID supplied by GitLab.

Plain HTTP is supported for local self-hosted operation. `GET /healthcheck` is an unauthenticated liveness check that returns success after startup; it does not report job state or GitLab connectivity. The same listener serves the [read-only web panel](#read-only-web-panel). Failed-job mutation remains limited to the local commands described in [Reliability](reliability.md#jobs-and-retries).

## Components

```text
GitLab -> webhook ingress -> SQLite review jobs -> review worker -> review agent
                         |                            -> Git workspace preparation -> read/bash subprocesses
                         |                            -> publication broker -> GitLab
                         +-> SQLite feedback jobs -> feedback evaluator -> runtime memory
Periodic reconciler -----+
Read-only web panel ---------------------------------------> SQLite
                   \----------------------------------------> process-local diagnostics buffers
```

### Webhook ingress

Validates and durably records merge request webhooks. Ready openings create idempotent review jobs. Close and merge actions create at most one feedback synthesis job for the merge request when a completed, locally validated and published Wormtamer review is available. Eligible work is acknowledged only after the applicable transaction commits; Note Hooks create no work.

### Review worker

Claims jobs with leases, retries recoverable failures, and completes generated work only after publication is reconciled. For a job without a locally validated result, it checks the deterministic GitLab publication marker before loading review evidence or invoking Gemini; an existing current publication completes as external-only recovery without reconstructing structured review data.

After that exact-head recovery check, the worker validates the GitLab diff version for the current head. Within retained SQLite state, a new head whose available `patch_id_sha` matches the newest completed canonical job for the same merge request completes as equivalent without invoking Gemini, preparing repositories, or publishing another note. The canonical job must have both a local validated result and a durable publication and cannot itself be equivalent. Head SHA remains the revision, repository, review-target, finding, and publication identity; patch identity only suppresses redundant work.

### Review agent

The worker starts with bounded merge request metadata and changed-file diffs, prepares a disposable Git review root, then runs a small explicit Gemini function-calling loop. Every ordinary generation declares exactly `read` and `bash`; application code dispatches each call under the credential-free review-tool identity. The loop continues until Gemini returns a structured result, the cumulative 16 MiB serialized function-response allowance forces one final-only generation, or the review deadline ends. Application code still requires a final summary and findings whose paths match fetched changed files before persistence.

A finding is a discrete, actionable correctness, security, or reliability defect introduced by the changed diff or made newly reachable or materially worse by it. It must identify concrete affected behavior and a realistic failure scenario without relying on unstated assumptions. Pre-existing issues unaffected by the change, style preferences, generic best practices, speculative risks, and missing tests or documentation without an independent concrete defect are not findings. Attributed tool context may establish impact, but each finding remains attached to an exact changed-file `new_path`. Findings with the same root cause are consolidated and explanations state the changed behavior, trigger, and impact concisely before recommending the smallest relevant correction.

Findings use ordered priorities `P0` through `P3`. `P0` is an immediate deployment or operations blocker, or catastrophic security or data-loss impact in a realistic supported scenario. `P1` is an urgent serious defect that should be fixed before merge. `P2` is a normal concrete defect that should be fixed. `P3` is a limited but real defect, not a style preference or optional improvement.

### Local review agent

The model-facing foundation is exactly two tools. `read` reads relative or absolute file paths as bounded text. `bash` executes an unrestricted Bash command in the current Git working directory with Pi-compatible tail truncation and review-local full-output files. There is no command allowlist, path confinement, network broker, or sandbox. Both tools run as the dedicated review-tool UID/GID with a minimal environment and no service credentials; the application process retains final result validation and GitLab publication. The authoritative trust and credential decision is in [Security](security.md#local-review-agent).

Current-project runtime memory is materialized as an ordinary, provenance-bearing JSON file outside repository-controlled paths in the review root. Trusted code fixes its GitLab instance and numeric project scope. All exposed memory versions are recorded at the successful review-result checkpoint.

### Repository workspace

Before the first generation, trusted application code clones the current project and, when repository sharing is enabled, every other authorized repository in service-private staging. It fetches all branch refs and complete branch histories; the current checkout is detached at the exact merge-request ref SHA, while each related checkout starts detached at its default-branch head. The setup removes and validates retained Git credential, proxy, and helper configuration before recursively transferring ownership and atomically exposing the complete root to the review-tool identity. The current repository is Bash's working directory, related repositories have deterministic sibling paths, and `.git` metadata and refs remain mutable.

The complete review root, including repositories, memory, shell-output files, worktrees, and model-created files, is removed after the attempt and at process startup and shutdown. There is no cross-review repository cache or application workspace-size quota.

### Publication broker

Validates findings, assigns each ordered finding a deterministic application-owned identifier under the immutable review identity, and posts one summary note per review identity using a stable hidden marker. Finding identifiers are persisted with the validated result and rendered for human reference. The broker reconciles an existing marked note before review generation and again before posting, owns GitLab write access, and remains outside repository workspaces.

### Feedback evaluator

The feedback worker runs only after an authorized merge request close or merge webhook created a job. It transiently fetches the terminal head's bounded diff and current comments, excludes internal, system, and Wormtamer-authored notes, and combines them with the bound locally persisted Wormtamer review. Gemini decides whether that complete evidence supports one concise, reusable, project-specific review lesson. It may decline to create memory; empty diffs receive no application-specific handling.

Diffs, comments, summaries, findings, and model output remain untrusted evidence. Closing or merging is a trigger, not proof that a comment or the Wormtamer review was correct. Trusted code validates input bounds, secret exclusion, the structured one-or-none result, and fixed repository scope before atomically completing the job and optionally storing one lesson. Diff and comment text are not persisted, and later merge request or comment activity does not update or deactivate the memory.

### Model usage diagnostics

Before each application-level Gemini request, the trusted review or feedback path creates a durable generation record correlated to its workflow job and attempt; review generations also record their turn and final-only state. A returned response checkpoints bounded model and usage metadata, latency, finish and structured-validation outcomes, and sanitized declared tool names. A request error checkpoints a failed outcome without inferred usage. Hidden SDK HTTP retries remain one application-level record because the SDK exposes only the final response.

The SDK distinguishes a missing usage-metadata object but represents omitted numeric fields as zero. Wormtamer follows that contract rather than intercepting raw response bodies. Non-negative counts are retained when present; internally inconsistent totals remain visible but cannot produce a catalog-derived estimate. Records contain no prompts, candidates, tool arguments, tool results, repository content, comments, memory lessons, credentials, or logs.

For direct Gemini Developer API use, startup retrieves the configured Gemini model's USD token rates from LiteLLM's bounded public pricing catalog; retrieval failure does not block review service and leaves costs unavailable. The catalog is application data, not model evidence or authority. Standard and above-200k rates are converted to integer picos and applied to consistent observed token categories. For a custom endpoint, Wormtamer does not assume upstream Gemini prices. It instead accepts LiteLLM's validated USD `x-litellm-response-cost` response header when present and otherwise leaves cost unavailable. Only the source category and resulting per-generation cost are persisted, so later catalog changes do not rewrite history.

### Process-local diagnostics

A concurrency-safe in-memory recorder observes enabled structured `slog` events and application-visible review and feedback conversations. It is additional to normal JSON stderr logging and performs no file, container, or external logging-service reads. Conversation content is captured only when `log_level` is `debug`; at other levels the recorder observes only generation metadata from the durable usage-recording path. One review conversation covers one workflow attempt and all of its generations; a feedback attempt has one generation. Any contained generation ID resolves to that conversation, with the first generation ID used for canonical navigation.

Conversation buffering is limited to 64 records, 4 MiB per record, and 32 MiB total. Log buffering is limited to 2,000 events, 16 KiB per event, 8 MiB total, and 64 attributes per event. Logs evict the oldest event; conversations evict the oldest completed record before any record whose latest generation is still started. If an impossible all-active workload reaches a hard ceiling, the newest active conversation is omitted so the ceiling remains enforced. An individually oversized record retains bounded identity or event metadata with an explicit `limit_exceeded` marker rather than partial private content. Buffer start time and eviction counts remain visible, and every buffer resets on process restart.

Known configured credentials are replaced using whole-value diagnostic redaction before content enters either buffer. This does not detect unknown secrets or remove private source. Conversation events contain the system instruction, initial prompt, accepted non-thought model turns and their declared function calls, admitted tool responses, fixed application denial responses, and validated feedback decisions in application-visible order. Thought text, thought signatures, and other SDK protocol data are excluded. Content-bearing stderr debug events are represented in the panel log buffer by a bounded reference to the correlated conversation rather than a duplicate prompt, argument, result, or response.

Recorder operations perform only bounded memory work. Readers receive snapshots and do not hold the recorder lock while HTML is rendered. Recorder omission or eviction never changes durable generation checkpoints, workflow retries, tool dispatch, publication, feedback decisions, logging delivery, or shutdown.

### Reconciler

The GitLab integration supports GitLab 17 and newer. The reconciler scans each authorized project immediately after startup and five minutes after each completed cycle. It lists bounded pages of open merge requests, skips drafts and work-in-progress entries as observed, and idempotently enqueues missing review identities. Scans have no durable cursor or schedule; restart repeats the scan safely.

### Read-only web panel

The built-in panel provides server-rendered HTML at `GET /`, with bounded history and detail views at `GET /reviews`, `GET /reviews/{job-id}`, `GET /feedback`, `GET /feedback/{job-id}`, `GET /memory`, `GET /usage`, `GET /usage/{generation-id}`, `GET /diagnostics/conversations`, `GET /diagnostics/conversations/{generation-id}`, and `GET /diagnostics/logs`. It shows committed review and feedback job state, patch-ID availability, validated review results and finding identities, publication status, memory-retrieval identities, feedback-derived memory, an explicit non-secret configuration summary including the effective current-only or all-authorized sharing mode, and current-process diagnostic buffers. Equivalent jobs link to their canonical review instead of presenting its result, findings, publication, or feedback synthesis as their own. A publication recovered externally without a local result is labeled external-only rather than reconstructed. A reconciled job without an associated webhook path is shown by numeric project ID.

Usage reporting covers rolling 24-hour, 7-day, and 30-day windows with validated request-kind, configured-model, resolved-model, and numeric-project filters. It shows observed token-category totals, model, repository, and request-kind breakdowns, generation histories, and aggregate estimated cost in USD. It never presents an estimate as provider billing and does not expose per-generation cost, formulas, or pricing rates.

Durable panel handlers query SQLite through fixed-size cursor pagination and bounded aggregate groups. Diagnostic handlers read bounded in-memory snapshots and use SQLite only for correlated durable generation and workflow metadata. No panel handler makes GitLab, Gemini, repository, file, container, or external logging-service requests. The panel exposes no state-changing methods and cannot retry work, create reviews, change logging, delete or export diagnostics, or edit configuration or memory. It requires no presentation-only persistent state. Panel access and traffic controls deliberately remain at the deployment boundary described in [Security](security.md#read-only-web-panel).

## Context and State

The model conversation begins with bounded changed-file diffs, relevant metadata, the current Git working directory and exact reviewed head, deterministic paths and initial revisions for prepared related repositories, the review-memory file path and advisory authority, the structured response schema, and exactly the `read` and `bash` declarations. The system instruction contributes only Pi's minimal tool list and two file-operation guidelines; it does not teach shell or Git command recipes. Function responses are added in same-turn call order. Conversations and command output are not persisted in SQLite.

SQLite stores webhook, job, publication, patch-equivalence, merge request progress, and structured model-generation diagnostic records. A review job may originate from a webhook event or from reconciliation without an event. Each newly generated result records either its validated GitLab patch ID or an explicit unavailable outcome. An equivalent job records the same patch ID and its canonical job ID but owns no result, findings, memory-retrieval audit, or publication. Existing and externally recovered rows without locally validated results retain unknown patch identity. GitLab remains the source of truth for merge requests and published discussions.

Git patch IDs intentionally ignore whitespace and can remain equal after a target-branch update changes surrounding repository context. Wormtamer accepts both consequences when suppressing equivalent reviews. Equivalence is installation-local and is not reconstructed from GitLab notes: after SQLite state loss, exact-head markers still recover unchanged revisions, but a rebased head is reviewed and published normally.

Runtime memory is installation-specific and separate from workflow state. A record preserves its repository scope, source merge request and bound feedback job, model-selected lesson, source URL, and creation time. Diff and comment text and arbitrary model conversation are not persisted.

For a newly generated successful review, SQLite records each unique memory identity and version returned to Gemini and its retrieval time in the same checkpoint as the validated result. This establishes which memory was exposed, not which memory affected model reasoning. The audit shares the review result's lifetime and stores no query, lesson copy, prompt, tool response, or failed-attempt history. An external-only publication recovered after local state loss stores its marker and GitLab note identity without a fabricated review result or memory audit and is therefore unavailable to feedback evaluation.

Gemini may create one repository-scoped lesson after assessing the terminal diff, current comments, and bound Wormtamer review. Stored lessons remain eligible for future bounded retrieval without later source reconciliation and are advisory rather than trusted policy: current code and explicit project policy override them.

Runtime review memory is separate from contributor guidance in `AGENTS.md` and `docs/agents/`.

## Excluded Until Required

Do not add multi-tenant logic, a central service, another database or queue, a distributed worker fleet, eager indexing, model training, a provider abstraction, a sandbox, or structured wrappers around ordinary shell and Git operations without a concrete approved requirement.

See [Reliability](reliability.md) for workflow guarantees and [Security](security.md) for trust boundaries.
