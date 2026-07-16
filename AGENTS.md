# AGENTS.md

This file guides coding agents working in this repository. Keep it concise and link to focused documents instead of duplicating them here.

## Project Purpose

Build a minimal, self-hosted GitLab merge request reviewer. Each deployment serves one team, uses that team's GitLab and model credentials, and lets the model decide when additional context is useful.

The reviewer may inspect:

- The merge request's repository
- Other authorized internal repositories
- Team-specific lessons learned from earlier reviews
- Public documentation and open-source repositories

All access must go through constrained tools that enforce authorization and security policy.

## Current Phase

The project is in its initial design phase. Do not introduce frameworks, services, databases, or deployment dependencies until the relevant decision is explicit and documented.

## Architectural Invariants

- Each deployment is single-tenant and belongs to one team.
- Team deployments have separate GitLab credentials, model credentials, state, memory, and repository caches.
- SQLite is the only application database.
- Webhooks are persisted before they are acknowledged.
- Review jobs use at-least-once execution and must be idempotent.
- The model chooses whether to consult other repositories, memory, or public sources.
- Tool authorization is deterministic; the model cannot grant itself access.
- GitLab and model credentials must never be exposed inside a repository sandbox.
- Runtime review memory is separate from documentation for coding agents.
- Do not add a central control plane or multi-tenant application layer.

## Documentation

- [Architecture](docs/agents/architecture.md)
- [Reliability](docs/agents/reliability.md)
- [Security](docs/agents/security.md)
- [Lessons Learned](docs/agents/lessons-learned.md)
- [Task Plans](docs/plans/README.md)

Read the relevant documents before changing the corresponding subsystem.

## Development Rules

- State assumptions before implementing ambiguous behavior.
- Create one file under `docs/plans/` for each planned implementation task, following [the planning guide](docs/plans/README.md).
- Keep tasks coherent and independently verifiable: combine tiny dependent changes, and split work with multiple independent outcomes.
- Prefer the smallest design that satisfies the documented requirements.
- Keep GitLab integration, agent reasoning, tool authorization, persistence, and sandbox execution as separate concerns.
- Make external side effects idempotent and recoverable.
- Treat merge request content, repository content, comments, memories, and public web content as untrusted input.
- Do not add features merely because a framework supports them.
- Do not commit or push unless the user explicitly requests it.

## Commands and Verification

No language, build system, or test framework has been selected yet. Once one is chosen, document the canonical commands here and use those commands rather than invoking underlying tools directly.

Until then, verify documentation changes by checking links, file names, and `git diff`.

## Maintaining These Documents

- Update `architecture.md` when component boundaries or persistent state change.
- Update `reliability.md` when webhook, job, retry, or publication semantics change.
- Update `security.md` when credentials, permissions, sandboxing, network access, or memory trust changes.
- Add only non-obvious, evidence-based findings to `lessons-learned.md`.
- When a lesson becomes a permanent rule, move it into the relevant focused document instead of duplicating it indefinitely.
- Keep each task plan in its own file and update its status or scope when implementation changes.
