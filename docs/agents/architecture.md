# Architecture

## Purpose

The project is a minimal, self-hosted GitLab merge request reviewer. It performs autonomous, tool-driven investigation while keeping repository access and external side effects under deterministic application control.

## Deployment Model

Each installation serves exactly one team.

```text
Infrastructure GitLab group
        -> infrastructure reviewer instance
        -> infrastructure GitLab credential
        -> model API key 1
        -> infrastructure state and memory

Android GitLab group
        -> Android reviewer instance
        -> Android GitLab credential
        -> model API key 2
        -> Android state and memory
```

All installations use the same application artifact but have independent configuration and persistent storage. They do not share a database or runtime memory.

This is different from running multiple replicas of one installation. A team installation initially runs as one replica because local SQLite and filesystem state are not a distributed coordination mechanism.

## Implementation Stack

The application is implemented in Go and packaged as one OCI image. One process hosts the HTTP ingress, SQLite-backed worker loop, and periodic reconciler. The standard library HTTP server is the default; a web framework requires a demonstrated need.

SQLite is accessed with ordinary SQL and explicit transactions. The exact Go SQLite driver and migration mechanism remain implementation decisions that must be validated against the durability requirements.

The only model backend is the Gemini Developer API, accessed through the official `google.golang.org/genai` SDK. The application owns a small manual function-calling loop and does not use a general agent framework or a provider-neutral compatibility layer. A narrow internal client interface is permitted as a test seam, not as a speculative multi-provider abstraction.

## Component Boundaries

```text
GitLab
  -> webhook ingress
  -> SQLite durable inbox and job state
  -> review worker
  -> review agent
       -> current-repository tools
       -> authorized internal-repository tools
       -> runtime-memory tools
       -> public web and repository tools
  -> GitLab publication broker

Periodic reconciler
  -> GitLab
  -> SQLite job state
```

### Webhook ingress

Validates the webhook, durably records it, creates an idempotent review job, and acknowledges the request only after the SQLite transaction commits.

### Review worker

Claims queued jobs with a lease, runs reviews, renews active leases, retries recoverable failures, and marks jobs complete only after publication has been reconciled.

### Review agent

Receives the merge request diff and metadata plus a set of constrained tools. Gemini decides whether more context is needed and which tools to call. Trusted application code manually dispatches each requested function call. The model must produce structured findings with evidence rather than directly calling GitLab, executing functions automatically, or accessing credentials.

### Tool broker

Enforces repository allowlists, credential boundaries, network policy, resource limits, and read/write permissions. Model intent never overrides broker policy.

### Repository workspace

Contains a temporary checkout of the merge request repository. Other repositories are discovered and fetched lazily when requested. Repository caches are disposable and are not authoritative state.

The initial implementation performs bounded read and search operations but does not execute repository-controlled code. Builds, tests, and other repository code execution require a separate sandbox decision and implementation.

### GitLab publication broker

Posts or updates review comments using stable identifiers. It is trusted code outside the repository sandbox and owns the GitLab write credential.

### Reconciler

Periodically compares recently updated open merge requests with local review state. It recovers work that was missed because a webhook was lost before reaching the instance.

## Agent Tools

The exact API is not yet fixed, but tools should cover these capabilities:

- Read and search the current repository
- List and search authorized internal repositories
- Lazily inspect a selected internal repository
- Search applicable runtime lessons
- Propose a new or updated lesson with evidence
- Search public documentation
- Inspect an explicitly selected public repository
- Submit structured review findings

Tools should return bounded, attributable results. Internal repository results must retain source repository and revision information; public results must retain URLs and revisions where available.

## Context Selection

The model, rather than a fixed preprocessing pipeline, decides whether to use additional context. The initial prompt should contain the merge request diff, relevant metadata, tool descriptions, and resource limits.

Model-directed selection is intentionally non-deterministic. Authorization, time limits, clone limits, network restrictions, and output validation remain deterministic.

## Persistent State

SQLite is the only application database. It stores durable webhook, job, publication, and merge request progress state. See [Reliability](reliability.md).

Runtime review memory is persistent per installation but is conceptually separate from event state. It is stored in dedicated SQLite tables and initially retrieved with scope filters and FTS5 text search. It must be accessible only through memory tools and must preserve scope, evidence, confidence, approval status, and timestamps. Embeddings or a separate retrieval system require evidence that this approach is insufficient.

Repository checkouts and public-source caches are disposable. GitLab remains the source of truth for merge requests, discussions, reactions, and published comments.

## Memory Scopes

A lesson may apply to:

- The whole team installation
- One repository
- A directory or path pattern
- A language, framework, or technology

Memories are retrieved on demand by the model. They are not model fine-tuning and must not silently become trusted merely because an LLM generated them.

Developer guidance in `AGENTS.md` and `docs/agents/` is not runtime review memory. See [Lessons Learned](lessons-learned.md).

## Deliberate Non-Goals

Until requirements prove otherwise, do not add:

- Multi-tenant application logic
- A central management service
- PostgreSQL, MongoDB, Redis, or a separate vector database
- A dashboard, billing, or organization management UI
- Eager cloning or indexing of every accessible repository
- A distributed worker fleet
- Model training or fine-tuning
- A general agent framework or model-provider abstraction
- A separate queue service
- Repository-controlled code execution in the initial implementation

## Undecided Details

The following choices should be made only when implementation begins and requirements are concrete:

- Go SQLite driver and migration mechanism
- Specific Gemini model and generation settings
- Sandbox implementation if repository code execution is later approved
- GitLab webhook installation mechanism for different GitLab editions
- Public web search provider
