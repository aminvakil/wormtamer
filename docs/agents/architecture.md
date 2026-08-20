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

The process starts with an explicit JSON configuration path, for example `wormtamer -config ./config.json`, and fails startup when the file or a required value is missing or invalid. Configuration decoding rejects unknown fields. Relative data paths resolve from the configuration file's directory. The configuration defines the listen address, SQLite path, log level, HTTP or HTTPS GitLab base URL, webhook secret, GitLab personal access token, model API key, optional Gemini Developer API-compatible base URL, model, review thinking level, authorized internal repositories, approved public domains, and exact public GitHub repositories; required values must be non-empty and repository entries must be well-formed and unique. `log_level` accepts `debug`, `info`, `warn`, or `error` and defaults to `info` when omitted. When `gemini.base_url` is omitted, the SDK uses the Gemini Developer API. When set, it is the validated HTTP or HTTPS base URL used for both reviews and feedback evaluation. The endpoint must accept the Gemini Developer API request path, authentication header, function-calling, structured-output, and thinking configuration used by Wormtamer and return native Gemini responses. OpenAI-compatible endpoints serving Gemini models are not sufficient. `gemini.thinking_level` defaults to `default`, which leaves the SDK thinking configuration unset. Any other non-empty value is passed through without a local allowlist so model-specific support is decided by the endpoint. The validated GitLab and configured model endpoint URLs are canonicalized. The review worker is always enabled, so its credentials are required at startup without external credential or scope validation.

```json
{
  "listen_address": "127.0.0.1:8080",
  "database_path": "data/wormtamer.db",
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
  "public_sources": {
    "allowed_domains": ["github.com", "openbao.org", "syncthing.net"],
    "github_repositories": ["nginx/nginx"]
  },
  "authorized_repositories": ["group/project", "group/shared-contracts"],
  "share_all_authorized_repositories": false,
  "repository_sharing": {
    "group/project": ["group/shared-contracts"]
  }
}
```

Authorized repositories are identified by exact GitLab namespace paths such as `group/project`. The same list authorizes webhook ingress and bounds which internal repositories can be disclosed to and inspected by the model. `repository_sharing` adds directional rules from a repository under review to related repositories whose information may be disclosed to that review's audience. Both sides must be authorized, self-sharing and duplicate entries are rejected, and a missing rule denies cross-repository access.

`share_all_authorized_repositories` defaults to `false`. When `true`, it derives a directional rule from every authorized repository to every other authorized repository; the current repository is excluded. It cannot be combined with a non-empty `repository_sharing` map. This is an operator assertion that every authorized repository has the same review audience, not a request to inspect repositories eagerly or exhaustively. Directional rules remain recommended when repository audiences differ.

Authorization by path intentionally fails after a project rename until configuration is updated; durable review identity still uses the numeric project ID supplied by GitLab.

`public_sources.allowed_domains` must include `github.com`; each canonical entry authorizes that domain and dot-boundary subdomains for bounded direct HTTPS retrieval. `public_sources.github_repositories` contains exact `<owner>/<repository>` slugs and authorizes snapshot tools only for those repositories. These installation-wide lists are disclosed to every review. Public GitHub access is unauthenticated.

Plain HTTP is supported for local self-hosted operation. `GET /healthcheck` is an unauthenticated liveness check that returns success after startup; it does not report job state or GitLab connectivity. The same listener serves the [read-only web panel](#read-only-web-panel). Failed-job mutation remains limited to the local commands described in [Reliability](reliability.md#jobs-and-retries).

## Components

```text
GitLab -> webhook ingress -> SQLite review jobs -> review worker -> review agent
                         |                            -> repository, memory, and public-source brokers
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

After that exact-head recovery check, the worker validates the GitLab diff version for the current head. Within retained SQLite state, a new head whose available `patch_id_sha` matches the newest completed canonical job for the same merge request completes as equivalent without invoking Gemini, loading a repository archive, or publishing another note. The canonical job must have both a local validated result and a durable publication and cannot itself be equivalent. Head SHA remains the revision, repository, review-target, finding, and publication identity; patch identity only suppresses redundant work.

### Review agent

The worker starts with bounded merge request metadata and changed-file diffs, then runs a small explicit Gemini function-calling loop. Gemini may request bounded context from the current repository, directionally shared related repositories, active advisory memory for the exact current repository, approved public websites, and exact configured public GitHub repositories through declared read-only tools. Tool declarations narrow as application-owned budgets are consumed, and the loop requires a structured final-only generation after a denied excess call or exhaustion of the combined budget. Application code dispatches each admitted request and still requires a final summary and findings whose paths match fetched changed files before persistence.

A finding is a discrete, actionable correctness, security, or reliability defect introduced by the changed diff or made newly reachable or materially worse by it. It must identify concrete affected behavior and a realistic failure scenario without relying on unstated assumptions. Pre-existing issues unaffected by the change, style preferences, generic best practices, speculative risks, and missing tests or documentation without an independent concrete defect are not findings. Attributed tool context may establish impact, but each finding remains attached to an exact changed-file `new_path`. Findings with the same root cause are consolidated and explanations state the changed behavior, trigger, and impact concisely before recommending the smallest relevant correction.

Findings use ordered priorities `P0` through `P3`. `P0` is an immediate deployment or operations blocker, or catastrophic security or data-loss impact in a realistic supported scenario. `P1` is an urgent serious defect that should be fixed before merge. `P2` is a normal concrete defect that should be fixed. `P3` is a limited but real defect, not a style preference or optional improvement.

### Tool brokers

Model-invocable tool brokers enforce repository allowlists, directional sharing rules, credential and network boundaries, resource limits, and read/write permissions. Model intent cannot override broker policy. The repository broker provides bounded file listing, text-file range reads, and case-sensitive literal search in the current repository and sharing-eligible related repositories. Every request names an exact repository exposed in the review input, and every result identifies its repository and immutable revision.

The memory broker provides bounded lexical search over merge-request-derived lessons for the exact GitLab instance and numeric project under review. The model supplies only a query, not scope. Directional repository sharing does not broaden memory access. Results identify their repository scope, memory identity, merge request source reference, lesson, and creation time, and label lessons as untrusted advisory guidance.

The public-source broker fetches one independently authorized HTTPS text URL at a time and provides file listing and range reads from exact configured GitHub repositories. It performs no search or automatic crawling. A GitHub repository's default-branch HEAD is resolved and pinned on first access in each review; content is extracted under the same hostile-archive rules as internal repositories. Results are labeled as untrusted public evidence and attributed with a final URL and retrieval time or an exact repository and commit.

Tools may provide bounded, attributed access to:

- The current repository and authorized, directionally shared internal repositories (implemented)
- Runtime review memory (implemented)
- Public documentation and exact configured GitHub repositories (implemented)
- Structured finding submission

### Repository workspace

When Gemini first requests current-repository context, the GitLab broker downloads a bounded repository archive at the exact reviewed head SHA. On first access to a sharing-eligible related repository, the broker resolves that repository's default-branch HEAD, pins its immutable commit SHA, and downloads an archive at that SHA. Trusted application code extracts validated regular UTF-8 text files into installation-local disposable workspaces; it does not invoke Git, follow symlinks, initialize submodules, or expose binary and oversized files. Workspaces are removed after each review, and their dedicated root is cleaned at startup and shutdown.

Internal repository tools may list, read, and search these snapshots but cannot execute repository-controlled code. Public GitHub snapshot tools may list and read but do not search. A review may make at most eight internal repository tool calls and inspect at most eight distinct internal repositories under one shared application-owned ceiling. Public repository access has a separate eight-call ceiling. Related and public snapshots are fetched lazily, one revision is retained per repository during the review, and there is no cross-review repository cache.

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

The built-in panel provides server-rendered HTML at `GET /`, with bounded history and detail views at `GET /reviews`, `GET /reviews/{job-id}`, `GET /feedback`, `GET /feedback/{job-id}`, `GET /memory`, `GET /usage`, `GET /usage/{generation-id}`, `GET /diagnostics/conversations`, `GET /diagnostics/conversations/{generation-id}`, and `GET /diagnostics/logs`. It shows committed review and feedback job state, patch-ID availability, validated review results and finding identities, publication status, memory-retrieval identities, feedback-derived memory, an explicit non-secret configuration summary, and current-process diagnostic buffers. Equivalent jobs link to their canonical review instead of presenting its result, findings, publication, or feedback synthesis as their own. A publication recovered externally without a local result is labeled external-only rather than reconstructed. A reconciled job without an associated webhook path is shown by numeric project ID.

Usage reporting covers rolling 24-hour, 7-day, and 30-day windows with validated request-kind, configured-model, resolved-model, and numeric-project filters. It shows observed token-category totals, model, repository, and request-kind breakdowns, generation histories, and aggregate estimated cost in USD. It never presents an estimate as provider billing and does not expose per-generation cost, formulas, or pricing rates.

Durable panel handlers query SQLite through fixed-size cursor pagination and bounded aggregate groups. Diagnostic handlers read bounded in-memory snapshots and use SQLite only for correlated durable generation and workflow metadata. No panel handler makes GitLab, Gemini, repository, public-source, file, container, or external logging-service requests. The panel exposes no state-changing methods and cannot retry work, create reviews, change logging, delete or export diagnostics, or edit configuration or memory. It requires no presentation-only persistent state. Panel access and traffic controls deliberately remain at the deployment boundary described in [Security](security.md#read-only-web-panel).

## Context and State

The model conversation begins with bounded changed-file diffs, relevant metadata, the exact current and sharing-eligible repository paths, approved public domains and exact GitHub repository slugs, the structured response schema, declared tools, and application-owned limits. Model-facing guidance prefers a direct read when an exact file path is known and path-scoped recursive listing or search when a relevant directory is known; root operations remain valid when no narrower context is available. It requests independent calls together when their complete arguments are already known and keeps calls whose arguments depend on earlier results sequential. Only validated, attributed tool results are added on later turns; conversations and retrieved public content are not persisted. Authorization and limits remain deterministic regardless of model intent.

SQLite stores webhook, job, publication, patch-equivalence, merge request progress, and structured model-generation diagnostic records. A review job may originate from a webhook event or from reconciliation without an event. Each newly generated result records either its validated GitLab patch ID or an explicit unavailable outcome. An equivalent job records the same patch ID and its canonical job ID but owns no result, findings, memory-retrieval audit, or publication. Existing and externally recovered rows without locally validated results retain unknown patch identity. GitLab remains the source of truth for merge requests and published discussions.

Git patch IDs intentionally ignore whitespace and can remain equal after a target-branch update changes surrounding repository context. Wormtamer accepts both consequences when suppressing equivalent reviews. Equivalence is installation-local and is not reconstructed from GitLab notes: after SQLite state loss, exact-head markers still recover unchanged revisions, but a rebased head is reviewed and published normally.

Runtime memory is installation-specific and separate from workflow state. A record preserves its repository scope, source merge request and bound feedback job, model-selected lesson, source URL, and creation time. Diff and comment text and arbitrary model conversation are not persisted.

For a newly generated successful review, SQLite records each unique memory identity and version returned to Gemini and its retrieval time in the same checkpoint as the validated result. This establishes which memory was exposed, not which memory affected model reasoning. The audit shares the review result's lifetime and stores no query, lesson copy, prompt, tool response, or failed-attempt history. An external-only publication recovered after local state loss stores its marker and GitLab note identity without a fabricated review result or memory audit and is therefore unavailable to feedback evaluation.

Gemini may create one repository-scoped lesson after assessing the terminal diff, current comments, and bound Wormtamer review. Stored lessons remain eligible for future bounded retrieval without later source reconciliation and are advisory rather than trusted policy: current code and explicit project policy override them.

Runtime review memory is separate from contributor guidance in `AGENTS.md` and `docs/agents/`.

## Excluded Until Required

Do not add multi-tenant logic, a central service, another database or queue, a distributed worker fleet, eager indexing of repositories or public websites, public-source search, model training, a provider abstraction, or repository-controlled code execution without a concrete approved requirement.

See [Reliability](reliability.md) for workflow guarantees and [Security](security.md) for trust boundaries.
