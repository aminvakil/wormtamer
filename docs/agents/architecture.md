# Architecture

## System Model

The application is a self-hosted GitLab merge request reviewer. Each installation serves one team with independent credentials, configuration, and SQLite state. Future runtime memory and repository caches remain installation-local. Installations share no database or control plane.

One installation runs as one process and replica because SQLite and local repository caches are not distributed coordination mechanisms.

## Stack

- Go with the standard library HTTP server unless a concrete requirement proves it insufficient
- One OCI image with persistent SQLite storage
- SQLite through `github.com/mattn/go-sqlite3`, built with CGO, as the only application database
- The Gemini Developer API through `google.golang.org/genai` as the only model backend
- A small explicit Gemini function-calling loop with no general agent framework or automatic tool execution

A narrow Gemini client interface is used as a test seam, not as a provider abstraction. SQLite migrations advance sequentially through `PRAGMA user_version`. The Gemini model is an explicit required configuration value; generation settings remain application-owned rather than deployment-configurable.

## Compatibility Baseline

Compatibility with Gemini versions earlier than 3 is explicitly out of scope. Do not add fallbacks, tests, or review findings for those versions. Otherwise, investigate model-version compatibility only for a concrete observed failure or an explicit task.

No release or production deployment compatibility baseline exists yet. Until one is explicitly established, changes do not need to preserve configuration formats, application interfaces, or SQLite state created by earlier development builds; recreating development configuration or state is acceptable when it keeps the current design simpler and correct. This does not relax correctness, durability, or recovery requirements for a running version.

Establish an upgrade and compatibility policy before the first production deployment.

## Deployment Configuration

The process starts with an explicit JSON configuration path, for example `wormtamer -config ./config.json`, and fails startup when the file or a required value is missing or invalid. Configuration decoding rejects unknown fields. Relative data paths resolve from the configuration file's directory. The configuration defines the listen address, SQLite path, HTTP or HTTPS GitLab base URL, webhook secret, GitLab personal access token, Gemini API key and model, and authorized repositories; required values must be non-empty and repository entries must be well-formed and unique. The validated GitLab URL is canonicalized before it participates in durable identity. The review worker is always enabled, so its credentials are required at startup without making external validation calls.

```json
{
  "listen_address": "127.0.0.1:8080",
  "database_path": "data/wormtamer.db",
  "gitlab": {
    "base_url": "https://gitlab.example",
    "webhook_secret": "replace-me",
    "personal_access_token": "replace-me"
  },
  "gemini": {
    "api_key": "replace-me",
    "model": "replace-me"
  },
  "authorized_repositories": ["group/project", "group/shared-contracts"],
  "repository_sharing": {
    "group/project": ["group/shared-contracts"]
  }
}
```

Authorized repositories are identified by exact GitLab namespace paths such as `group/project`. The same list authorizes webhook ingress and bounds which internal repositories can be disclosed to and inspected by the model. `repository_sharing` adds directional rules from a repository under review to related repositories whose information may be disclosed to that review's audience. Both sides must be authorized, self-sharing and duplicate entries are rejected, and a missing rule denies cross-repository access. Authorization by path intentionally fails after a project rename until configuration is updated; durable review identity still uses the numeric project ID supplied by GitLab.

Plain HTTP is supported for local self-hosted operation. `GET /healthcheck` is an unauthenticated liveness check that returns success after startup; it does not report job state or GitLab connectivity. Operational job visibility is through bounded logs rather than a status API.

## Components

```text
GitLab -> webhook ingress -> SQLite jobs -> review worker -> review agent
                                      |                    -> future tool brokers
Periodic reconciler ------------------+                    -> publication broker -> GitLab
```

### Webhook ingress

Validates and durably records webhooks, creates idempotent review jobs, and acknowledges only after the transaction commits.

### Review worker

Claims jobs with leases, retries recoverable failures, and completes work only after publication is reconciled.

### Review agent

The worker starts with bounded merge request metadata and changed-file diffs, then runs a small explicit Gemini function-calling loop. Gemini may request bounded context from the current repository or directionally shared related repositories through declared read-only tools. Application code dispatches each request and still requires a final summary and findings whose paths match fetched changed files before persistence.

### Tool brokers

Model-invocable tool brokers enforce repository allowlists, directional sharing rules, credential and network boundaries, resource limits, and read/write permissions. Model intent cannot override broker policy. The repository broker provides bounded file listing, text-file range reads, and case-sensitive literal search in the current repository and sharing-eligible related repositories. Every request names an exact repository exposed in the review input, and every result identifies its repository and immutable revision. Other brokers remain deferred.

Tools may provide bounded, attributed access to:

- The current repository and authorized, directionally shared internal repositories (implemented)
- Runtime review memory
- Public documentation and repositories
- Structured finding submission

### Repository workspace

When Gemini first requests current-repository context, the GitLab broker downloads a bounded repository archive at the exact reviewed head SHA. On first access to a sharing-eligible related repository, the broker resolves that repository's default-branch HEAD, pins its immutable commit SHA, and downloads an archive at that SHA. Trusted application code extracts validated regular UTF-8 text files into installation-local disposable workspaces; it does not invoke Git, follow symlinks, initialize submodules, or expose binary and oversized files. Workspaces are removed after each review, and their dedicated root is cleaned at startup and shutdown.

Repository tools may list, read, and search these snapshots but cannot execute repository-controlled code. A review may make at most eight repository tool calls and inspect at most eight distinct repositories under one shared application-owned ceiling. Related snapshots are fetched lazily, one revision is retained per repository during the review, and there is no cross-review repository cache.

### Publication broker

Validates findings, assigns each ordered finding a deterministic application-owned identifier under the immutable review identity, and posts one summary note per review identity using a stable hidden marker. Finding identifiers are persisted with the validated result and rendered for human reference. The broker reconciles an existing marked note before posting, owns GitLab write access, and remains outside repository workspaces.

### Reconciler

The GitLab integration supports GitLab 17 and newer. The reconciler scans each authorized project immediately after startup and five minutes after each completed cycle. It lists bounded pages of open merge requests, skips drafts and work-in-progress entries as observed, and idempotently enqueues missing review identities. Scans have no durable cursor or schedule; restart repeats the scan safely.

## Context and State

The model conversation begins with bounded changed-file diffs, relevant metadata, the exact current and sharing-eligible repository paths, the structured response schema, declared repository tools, and application-owned limits. Only validated, attributed tool results are added on later turns; conversations are not persisted. Authorization and limits remain deterministic regardless of model intent.

SQLite stores webhook, job, publication, and merge request progress records. A review job may originate from a webhook event or from reconciliation without an event. GitLab remains the source of truth for merge requests and published discussions.

Runtime memory is not implemented. If introduced, it remains installation-specific and separate from workflow state. Records must preserve scope, evidence, confidence, approval status, and timestamps, and are not trusted merely because a model generated them.

Runtime review memory is separate from contributor guidance in `AGENTS.md` and `docs/agents/`.

## Excluded Until Required

Do not add multi-tenant logic, a central service, another database or queue, a distributed worker fleet, eager indexing of all repositories, model training, a provider abstraction, or repository-controlled code execution without a concrete approved requirement.

See [Reliability](reliability.md) for workflow guarantees and [Security](security.md) for trust boundaries.
