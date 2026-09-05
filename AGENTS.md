# AGENTS.md

## Project

Build a minimal, self-hosted GitLab merge request reviewer. Each deployment serves one team and reviews prepared authorized repositories with Pi-style `read` and `bash` tools.

## Constraints

- Use Go, the standard library HTTP server by default, SQLite, and the official Gemini Go SDK, either directly or through a configured Gemini Developer API-compatible endpoint.
- Run webhook ingress, review work, and reconciliation in one process and one replica.
- Keep the Gemini function-calling loop explicit; trusted application code dispatches exactly the model-facing `read` and `bash` tools.
- Keep deployments single-tenant. Do not add a control plane, provider abstraction, additional database, queue service, or agent framework without an approved need.
- Persist webhooks before acknowledging them. Use at-least-once jobs and idempotent external effects.
- Treat repository content, comments, memories, public content, and model output as untrusted.
- Keep credentials outside prompts, tool output, and review workspaces.
- Follow the accepted unrestricted local-agent trust and credential boundary in [Security](docs/agents/security.md#local-review-agent). Do not reintroduce a sandbox, command allowlist, or structured repository/public-source tools without an explicitly approved change.

## Documentation

- [Architecture](docs/agents/architecture.md): components, stack, and persistent state
- [Reliability](docs/agents/reliability.md): webhook, job, retry, and publication semantics
- [Security](docs/agents/security.md): trust, authorization, credentials, and the local review-agent boundary
- [Lessons](docs/agents/lessons-learned.md): evidence-based implementation findings
- [Plans](docs/plans/README.md): substantial proposed work

Read only the documents relevant to the subsystem being changed.

## KISS

- Deliver the smallest complete behavior that advances the current approved outcome.
- Add complexity only when required by observable acceptance criteria, current focused documentation, or verified behavior.
- Treat risks and open questions as constraints, not automatic implementation scope. Resolve what blocks a safe coherent outcome and defer the rest.
- Prefer direct, fixed, single-purpose designs over speculative scale, flexibility, optimization, or generalization.

## Working Rules

- Carry explicit implementation requests through local implementation and focused verification. Ask when a material scope, design, or authorization decision is unresolved; routine implementation and test choices do not require separate approval.
- Keep GitLab integration, model reasoning, authorization, persistence, and repository access as separate concerns.
- Make external side effects recoverable and idempotent.
- Follow the [compatibility baseline](docs/agents/architecture.md#compatibility-baseline). Do not investigate hypothetical Gemini-version compatibility: versions earlier than 3 are out of scope, and other compatibility work requires an explicit task or concrete failure.
- When affected by the current change, prioritize high-risk boundaries such as webhook authentication and durable admission, repository authorization, job retry and publication semantics, Gemini and tool-call protocols, subprocess cleanup, output limits, and public errors. This is a priority list, not an exhaustive test matrix.
- Create a task plan only for substantial work that changes architecture, security boundaries, persistent state, public contracts, or several components.
- Do not commit or push unless explicitly requested.

## Test Discipline

- Inspect existing coverage first. Add or extend a test only for a required behavior or credible regression not already covered. A code change or review finding does not automatically require a new test.
- Test each invariant at its stable owning boundary. Repeat coverage only when another layer independently transforms data, maps errors, crosses a process or persistence boundary, or enforces a distinct security or compatibility contract.
- Use representative inputs for distinct behavior rather than enumerating equivalent malformed inputs, statuses, or configuration values. Test exact limits only when their boundary behavior is part of the contract.
- Prefer observable contracts over private call order, incidental prompt or configuration wording, or standard-library behavior. Do not add tests merely to document accepted limitations or prove a deferred protection remains absent. Plan verification bullets describe outcomes, not a test matrix.
- Keep test code and fixtures minimal. Reuse existing helpers; add small task-specific fixtures when needed, not generic test frameworks.
- There is no numerical scenario limit or automatic approval requirement for a new fixture. Use the smallest meaningful coverage within the authorized task; do not inflate or suppress tests to satisfy a count.
- Run checks appropriate to the changed behavior and complete required repository checks. Once they pass, broaden or repeat them only for new changes, failures, or concrete unresolved concerns. Report the behaviors verified and whether the relevant commands passed, not test counts.

## Documentation Discipline

- Give each decision one authoritative home; link to it instead of repeating it.
- Document current constraints and non-obvious rationale, not work history or routine checks.
- Do not preserve `git diff`, formatting, link-check, or test-pass reports in plans or design documents.
- Avoid speculative implementation detail; record deferred choices in the task that resolves them.
- Add lessons only after a concrete bug, test, incident, or verified behavior. Move permanent rules to the relevant focused document.

## Commands

For documentation-only changes, verify document consistency and links; Go builds and tests are not required.

- Build: `make build`
- Test: `make test`
- Race test: `make test-race`
