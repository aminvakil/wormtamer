# Improve model-facing instructions

Status: proposed

Depends on:

- [Recover from broad internal repository searches](recover-from-broad-internal-repository-searches.md)
- [Learn from natural review feedback](learn-from-natural-review-feedback.md)

## Goal

Review and improve every current Gemini-facing instruction and structured contract so the model receives concise, consistent guidance about its task, evidence hierarchy, tool selection, security boundaries, and required output. Reduce avoidable broad repository requests when exact or path-scoped context is already known without weakening broker enforcement or adding persistent model hints.

## Scope

Cover both current model workflows:

- Merge request review: the system instruction, JSON-delimited user prompt, repository, memory, and public-source function declarations, and final response schema.
- Feedback evaluation: the system instruction, JSON-delimited user prompt, and decision response schema.

For each workflow:

- Compare model-facing guidance with the current architecture, security, reliability, and broker behavior.
- Remove material ambiguity, contradiction, or misplaced guidance while keeping each concern in the narrowest useful location.
- State tool-selection guidance where it can affect the model before its first call: read an exact known file directly, scope recursive listing or search to a known relevant directory, and avoid broad repository traversal when narrower evidence can answer the review question.
- Keep untrusted-data delimiters, evidence attribution, memory subordination, secret handling, repository and public-source boundaries, and structured-output requirements explicit.
- Keep function descriptions and schemas aligned with actual accepted arguments, recursion behavior, bounds, and correction opportunities.
- Add focused tests for model-facing requirements that application code owns.

Do not add persistent repository hints, global review memory, prompt configurability, a prompt framework, another model call, live-model tests, automatic prompt optimization, provider-specific fallback text, or a semantic evaluation service. Do not change authorization, tool capabilities, output schemas, resource limits, or feedback policy unless the review finds a concrete mismatch that prevents the existing contract from being expressed correctly.

## Approach

Treat the model interface as four layers with separate responsibilities:

1. System instructions define the task, trust hierarchy, enduring policy, and completion rules.
2. User prompts delimit the current untrusted input and state the immediate request without repeating the full policy.
3. Function declarations explain when to use each tool, its actual selection semantics, and how to choose bounded arguments.
4. Response schemas define machine-enforced structure; prose should not restate schema details unless needed for correct meaning.

Inventory every instruction, prompt builder, function declaration, and response schema used by `internal/review` and `internal/memory`. Check each statement against trusted validation and broker code. Change wording only when it maps to current behavior, a documented constraint, or an observed failure mode; do not rewrite clear text for style alone.

For repository access, add general pre-call guidance that prefers the smallest direct request capable of answering the question. An exact path from the diff should normally be read directly; a known directory should be supplied to recursive listing or search. Keep root operations valid when the model lacks narrower context. Coordinate this wording with the search-limit recovery plan so recursion, bounded failures, and narrower retries are described once in the most useful model-facing locations.

For feedback evaluation, verify that the instruction, input framing, schema, and local validation consistently express natural overall-review and finding feedback without user-facing identifier syntax, role as provenance rather than authority, optional reusable project-specific lessons, and no decision for unrelated or ambiguous comments. Preserve the absence of model tools in this workflow.

Use focused assertions for required semantics and generated declarations or schemas rather than snapshotting entire prompts. Existing local validation remains authoritative even when equivalent guidance appears in prose.

## Risks and Open Questions

Prompt wording can influence but cannot guarantee a particular tool sequence. Gemini may still choose a broad request; deterministic brokers, error-only correction, and call ceilings remain the enforcement and recovery mechanisms. Verification should therefore test the model-visible contract and bounded application behavior, not require identical live-model output on every run.

Adding every operational detail to the system instruction could make the prompt repetitive and obscure higher-priority policy. Keep tool-specific selection advice in function declarations where possible and use the system instruction only for cross-tool decision rules needed before the first call.

## Verification

- The review accounts for every Gemini system instruction, user prompt builder, function declaration, and response schema currently used by merge request review and feedback evaluation.
- Before any tool call, the review workflow tells Gemini to prefer direct reads for exact known paths and path-scoped recursive operations when a relevant directory is known, while retaining valid root operations when needed.
- Internal listing and search declarations accurately describe recursion, bounded behavior, optional paths, and narrower request guidance; read declarations accurately describe bounded line ranges.
- Repository, memory, and public-source declarations remain consistent with their distinct authorization and scope rules and do not imply unavailable tools or automatic access.
- Untrusted merge request, repository, memory, public, and feedback content remains clearly subordinate to application policy, current code, and explicit project policy.
- Feedback instructions, schema, and validation consistently enforce the supplied finding- and review-level target rules, treat role only as provenance, and reject unrelated or ambiguous feedback and unsuitable lessons.
- Response schemas and local validators retain their current required fields, bounds, and authority; prompt changes do not substitute for deterministic validation.
- Representative unit tests demonstrate the required model-visible guidance and unchanged security, tool, and structured-output contracts without calling a live model.
