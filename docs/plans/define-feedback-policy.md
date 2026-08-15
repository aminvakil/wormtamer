# Define feedback classification and memory policy

Status: proposed

## Goal

Define one deterministic, documented meaning for natural-language feedback decisions and the conditions under which a decision may create active review memory. The policy must keep comments and model interpretations untrusted while producing useful repository-scoped guidance.

## Scope

- Define `supports`, `rejects`, and `corrects` for overall-review and finding targets.
- Define when feedback targets the overall review rather than one or more specific findings.
- Define whether confidence represents interpretation certainty, factual confidence, or both, and whether low-confidence decisions are valid.
- Decide whether actor role, confidence, outcome, or explicit wording imposes a hard memory-activation threshold.
- Define the required form of a reusable lesson, including scope, conditions, and prohibited one-off or copied content.
- Align the feedback prompt, response schema, trusted validation, SQLite constraints, and focused architecture and security documentation with the chosen policy.

Do not add analytics, evaluation replay, contributor scoring, repository tools for the feedback evaluator, or a new memory-management surface. The separate deferred [feedback-driven review evaluation](evaluate-feedback-driven-reviews.md) remains out of scope.

## Approach

1. Establish representative comment cases for explicit support, rejection, material correction, ambiguous discussion, generic acknowledgement, comments spanning several findings, and feedback about a zero-finding review.
2. Choose mutually exclusive outcome and target-selection rules that produce no decision when target or stance is ambiguous.
3. Define confidence in terms that trusted code can validate. If role or confidence is a policy boundary for memory activation, enforce it in application code rather than relying only on model instructions.
4. Require each active lesson to be standalone, narrowly repository-specific, conditional where needed, and free of comment text, identities, one-off code state, and generic programming advice.
5. Update the model contract and local validation together, then document the durable policy in the focused architecture or security document.

## Risks and Open Questions

- A Maintainer or Owner is stronger provenance but can still be mistaken; a hard role threshold reduces poisoning risk but may discard useful contributor knowledge.
- The evaluator does not inspect current repository code, so it cannot verify whether a comment's project-specific claim is factually correct.
- `low` confidence currently coexists with the rule that ambiguous comments produce no decision; decide whether it has a distinct useful meaning or should be removed.
- Clarify whether “fixed”, “done”, or “good catch” supports a finding, and reserve correction for comments that materially change its factual basis, scope, priority, or recommendation.

## Verification

- Representative comments map to one deterministic target and outcome or to no decision.
- Specific-finding feedback does not also create an overall-review decision without explicit overall-review content.
- Ambiguous comments and ordinary discussion create neither decisions nor memory.
- Every active lesson satisfies the selected role, confidence, and content policy through trusted validation.
- Feedback evaluation remains unable to broaden repository scope, inspect code, publish, or treat comments as application instructions.
