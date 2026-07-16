# Select Go and Gemini implementation stack

Status: completed

Summary: Record the initial implementation stack as a minimal Go service using SQLite and only the Gemini Developer API, with model tool calls kept behind trusted application brokers.

## Context

The architecture already requires a single-replica, single-tenant service with durable SQLite job state, at-least-once execution, idempotent publication, and deterministic tool authorization. The programming language, model integration, and initial runtime-memory retrieval mechanism were previously undecided.

The deployment will use a Gemini API key and does not need other model providers. The application is primarily an HTTP ingress, durable workflow, and security broker rather than an in-process machine-learning system.

## Goal

Make the stack sufficiently explicit for subsequent implementation plans: Go, standard-library HTTP by default, one process, SQLite-backed jobs and memory, the official Gemini Go SDK, and a manual model function-calling loop.

## Non-goals

- Initialize a Go module or implement application code.
- Select a SQLite driver or migration library before durability tests can be planned.
- Select a specific Gemini model or generation settings.
- Design a sandbox for executing repository-controlled code.
- Add support for other model providers.

## Approach

Document the smallest stack that fits the existing architecture. Keep webhook ingress, worker execution, and reconciliation in one Go process. Call the Gemini Developer API through `google.golang.org/genai`, but manually validate and dispatch requested tools through application brokers. Store initial runtime memory in dedicated SQLite tables and use scoped FTS5 retrieval rather than introducing a vector database.

Do not execute repository-controlled code in the initial implementation. Public research remains behind constrained application tools rather than Gemini built-in retrieval capabilities.

## Expected Changes

- `AGENTS.md`: state the selected stack and update the current-phase and verification guidance.
- `docs/agents/architecture.md`: record the implementation stack, Gemini agent loop, SQLite memory retrieval, and remaining undecided details.
- `docs/agents/security.md`: define Gemini credential, function-calling, built-in tool, and initial repository-execution boundaries.
- `docs/plans/select-go-gemini-stack.md`: preserve the decision context, trade-offs, and verification.

## Reliability and Security

The decision preserves SQLite as the durable source of job state and does not add another queue or database. A single process and replica avoid unsupported distributed coordination.

The Gemini API key remains in the trusted application layer. Model function requests and structured responses remain untrusted and are validated locally. SDK automatic function execution, built-in code execution, URL retrieval, and search grounding are initially disabled because they could bypass deterministic authorization, network controls, and source attribution.

## Trade-offs

Go provides simple deployment, explicit concurrency, subprocess control, and strong support for a stateful service. Python has a broader AI ecosystem, but that advantage is limited when the sole backend has an official Go SDK; it also adds packaging and async/runtime coordination concerns. TypeScript has strong JSON tooling but introduces event-loop and native SQLite module considerations. Rust offers stronger compile-time guarantees at a higher implementation cost.

A custom function-calling loop requires maintaining a small amount of integration code, but it is easier to audit than a general agent framework. FTS5 may eventually be insufficient for semantic memory retrieval; embeddings should be introduced only after retrieval evaluation demonstrates that need.

## Dependencies

None.

## Verification

- Confirm all documentation links resolve.
- Confirm the focused architecture and security documents agree with `AGENTS.md`.
- Confirm previously undecided stack entries are removed or narrowed.
- Inspect `git diff` for unrelated changes.

## Completion Record

- Result: Recorded Go, SQLite, the Gemini Developer API, the manual function-calling boundary, initial FTS5 memory retrieval, and the no-repository-execution boundary in contributor and focused design documentation.
- Deviations: None.
- Verification performed: `git diff --check` passed; all local Markdown links resolve; stack references were checked for consistency across `AGENTS.md`, architecture, security, and this plan.
