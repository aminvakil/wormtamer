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

Receives the merge request diff and metadata plus a set of constrained tools. The model decides whether more context is needed and which tools to call. It must produce structured findings with evidence rather than directly calling GitLab or accessing credentials.

### Tool broker

Enforces repository allowlists, credential boundaries, network policy, resource limits, and read/write permissions. Model intent never overrides broker policy.

### Repository workspace

Contains a temporary checkout of the merge request repository. Other repositories are discovered and fetched lazily when requested. Repository caches are disposable and are not authoritative state.

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

Runtime review memory is persistent per installation but is conceptually separate from event state. Its on-disk representation remains undecided. It must be accessible only through memory tools and must preserve scope, evidence, confidence, approval status, and timestamps.

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

## Undecided Details

The following choices should be made only when implementation begins and requirements are concrete:

- Programming language and web framework
- Agent SDK or direct model API
- Sandbox implementation
- Runtime memory file format and retrieval strategy
- GitLab webhook installation mechanism for different GitLab editions
- Public web search provider
