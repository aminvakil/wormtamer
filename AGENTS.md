# AGENTS.md

## Project

Build a minimal, self-hosted GitLab merge request reviewer. Each deployment serves one team and may inspect authorized repositories, runtime review memory, and public sources through constrained tools.

## Constraints

- Use Go, the standard library HTTP server by default, SQLite, and the official Gemini Go SDK.
- Run webhook ingress, review work, and reconciliation in one process and one replica.
- Keep the Gemini function-calling loop explicit; trusted application brokers validate and dispatch every tool call.
- Keep deployments single-tenant. Do not add a control plane, provider abstraction, additional database, queue service, or agent framework without an approved need.
- Persist webhooks before acknowledging them. Use at-least-once jobs and idempotent external effects.
- Treat repository content, comments, memories, public content, and model output as untrusted.
- Keep credentials outside prompts, tool output, repository workspaces, and sandboxes.
- Do not execute repository-controlled code until a sandbox design is approved.

## Documentation

- [Architecture](docs/agents/architecture.md): components, stack, and persistent state
- [Reliability](docs/agents/reliability.md): webhook, job, retry, and publication semantics
- [Security](docs/agents/security.md): trust, authorization, credentials, sandboxing, and network access
- [Lessons](docs/agents/lessons-learned.md): evidence-based implementation findings
- [Plans](docs/plans/README.md): substantial proposed work

Read only the documents relevant to the subsystem being changed.

## Working Rules

- Prefer the smallest change that satisfies the documented requirements.
- Keep GitLab integration, model reasoning, authorization, persistence, and repository access as separate concerns.
- Make external side effects recoverable and idempotent.
- Create a task plan only for substantial work that changes architecture, security boundaries, persistent state, public contracts, or several components.
- Do not commit or push unless explicitly requested.

## Documentation Discipline

- Give each decision one authoritative home; link to it instead of repeating it.
- Document current constraints and non-obvious rationale, not work history or routine checks.
- Do not preserve `git diff`, formatting, link-check, or test-pass reports in plans or design documents.
- Avoid speculative implementation detail; record deferred choices in the task that resolves them.
- Add lessons only after a concrete bug, test, incident, or verified behavior. Move permanent rules to the relevant focused document.

## Commands

Add canonical build and test commands here when the Go module is initialized.
