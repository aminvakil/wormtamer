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

- Prefer the smallest change that satisfies the documented requirements.
- Keep GitLab integration, model reasoning, authorization, persistence, and repository access as separate concerns.
- Make external side effects recoverable and idempotent.
- Follow the [compatibility baseline](docs/agents/architecture.md#compatibility-baseline). Do not investigate hypothetical Gemini-version compatibility: versions earlier than 3 are out of scope, and other compatibility work requires an explicit task or concrete failure.
- When affected by the current change, prioritize high-risk boundaries such as webhook authentication and durable admission, repository authorization, job retry and publication semantics, Gemini and tool-call protocols, subprocess cleanup, output limits, and public errors. This is a priority list, not an exhaustive test matrix.
- Create a task plan only for substantial work that changes architecture, security boundaries, persistent state, public contracts, or several components.
- Do not commit or push unless explicitly requested.

## Test Discipline

- Test code and fixtures follow the same KISS and focused-change rules as production code. Add the smallest set that proves the requested behavior or reproduces a concrete regression.
- Before adding tests, inspect the existing suite. For each new behavioral scenario, identify the requirement or regression, the stable boundary that owns it, existing coverage, and the distinct failure it would catch. If no distinct failure exists, do not add it.
- Test each invariant once at its owning boundary. Repeat it at another layer only when that layer independently transforms data, maps errors, crosses a process or persistence boundary, or enforces a distinct security or compatibility contract.
- Use one representative per equivalence class or code branch. Test exact limits only when inclusive or exclusive behavior is an explicit contract. Do not enumerate equivalent malformed inputs, every non-success status, or pass-through configuration values.
- A plan's Verification section defines observable outcomes, not one executable test per bullet. Review findings also do not automatically require new tests; update or consolidate existing coverage first.
- Prefer observable behavior over implementation details. Avoid tests of private call order, copied prompt or configuration prose, standard-library behavior, or accepted limitations and deferred protections unless they are explicit external contracts. Never add a test whose purpose is to prove that a deferred protection remains absent.
- For a bug fix, add one focused regression test by default. For a feature, start with one happy path and add only independently meaningful failure scenarios.
- Reuse focused test support and do not duplicate identical non-trivial helpers or subprocess fixtures. Do not create a generic test framework.
- Do not add tests solely to increase coverage, and do not present test or subtest counts as evidence of quality. Report the distinct behaviors verified and whether the relevant commands passed.
- If a change appears to require more than five new behavioral scenarios or a new subprocess fixture, stop before implementation and request approval with a short test matrix. Parameterized cases and subtests count as scenarios.

## Documentation Discipline

- Give each decision one authoritative home; link to it instead of repeating it.
- Document current constraints and non-obvious rationale, not work history or routine checks.
- Do not preserve `git diff`, formatting, link-check, or test-pass reports in plans or design documents.
- Avoid speculative implementation detail; record deferred choices in the task that resolves them.
- Add lessons only after a concrete bug, test, incident, or verified behavior. Move permanent rules to the relevant focused document.

## Commands

- Build: `make build`
- Test: `make test`
- Race test: `make test-race`
