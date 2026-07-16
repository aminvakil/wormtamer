# Architecture

## System Model

The application is a self-hosted GitLab merge request reviewer. Each installation serves one team with independent credentials, configuration, SQLite state, runtime memory, and repository caches. Installations share no database or control plane.

One installation runs as one process and replica because SQLite and local repository caches are not distributed coordination mechanisms.

## Stack

- Go with the standard library HTTP server unless a concrete requirement proves it insufficient
- One OCI image with persistent SQLite storage
- SQLite through `github.com/mattn/go-sqlite3`, built with CGO and FTS5, as the only application database
- The Gemini Developer API through `google.golang.org/genai` as the only model backend
- A small, explicit function-calling loop; no general agent framework or automatic tool execution
- Scoped SQLite FTS5 search for initial runtime-memory retrieval

A narrow Gemini client interface may be used as a test seam, not as a provider abstraction. The migration mechanism, Gemini model, and generation settings are deferred until their implementation tasks establish requirements.

## Deployment Configuration

The process starts with an explicit JSON configuration path, for example `wormtamer -config ./config.json`, and fails startup when the file or a required value is missing or invalid. Relative data paths resolve from the configuration file's directory. The initial configuration defines the listen address, SQLite path, GitLab base URL, webhook secret, and authorized repositories. Credentials that are not used by an implemented capability are not required in advance.

Authorized repositories are identified by exact GitLab namespace paths such as `group/project`. The same list authorizes webhook ingress and defines the internal repositories that may later be disclosed to and inspected by the model. Authorization by path intentionally fails after a project rename until configuration is updated; durable review identity still uses the numeric project ID supplied by GitLab.

Plain HTTP is supported for local self-hosted operation. `GET /healthcheck` is an unauthenticated liveness check that returns success after startup; it does not report job state or GitLab connectivity. Operational job visibility is through bounded logs rather than a status API.

## Components

```text
GitLab -> webhook ingress -> SQLite jobs -> review worker -> review agent
                                      |                    -> tool brokers
Periodic reconciler ------------------+                    -> publication broker -> GitLab
```

### Webhook ingress

Validates and durably records webhooks, creates idempotent review jobs, and acknowledges only after the transaction commits.

### Review worker

Claims jobs with leases, retries recoverable failures, and completes work only after publication is reconciled.

### Review agent

Receives merge request context and constrained tools. Gemini chooses whether to request more context and returns structured findings with evidence. Trusted application code validates and dispatches every tool request.

### Tool brokers

Enforce repository allowlists, credential and network boundaries, resource limits, and read/write permissions. Model intent cannot override broker policy.

Tools may provide bounded, attributed access to:

- The current repository
- Authorized internal repositories
- Runtime review memory
- Public documentation and repositories
- Structured finding submission

### Repository workspace

Holds a temporary checkout of the current repository. Other repositories are fetched lazily. Checkouts and caches are disposable and never authoritative state.

The initial implementation permits bounded reading and searching but does not execute repository-controlled code.

### Publication broker

Validates findings and posts or updates GitLab comments using stable identifiers. It owns GitLab write access and remains outside repository workspaces.

### Reconciler

Compares recently updated merge requests with local state and enqueues revisions missed because a webhook did not arrive.

## Context and State

The initial model request contains the diff, relevant metadata, tool descriptions, and resource limits. Gemini may choose additional context, but authorization and limits remain deterministic.

SQLite stores webhook, job, publication, merge request progress, and runtime-memory records. GitLab remains the source of truth for merge requests and published discussions.

Runtime memory is installation-specific and stored separately from workflow state. Records preserve scope, evidence, confidence, approval status, and timestamps. They are retrieved on demand through memory tools and are not trusted merely because a model generated them.

Runtime review memory is separate from contributor guidance in `AGENTS.md` and `docs/agents/`.

## Excluded Until Required

Do not add multi-tenant logic, a central service, another database or queue, a distributed worker fleet, eager indexing of all repositories, model training, a provider abstraction, or repository-controlled code execution without a concrete approved requirement.

See [Reliability](reliability.md) for workflow guarantees and [Security](security.md) for trust boundaries.
