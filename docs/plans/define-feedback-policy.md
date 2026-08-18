# Define feedback classification and memory policy

Status: proposed

## Goal

Define one deterministic, documented meaning for attributed natural-language feedback decisions and the conditions under which the combined feedback for one reviewed revision may create active repository-scoped memory. The policy must keep comments, diffs, roles, and model interpretations as untrusted evidence while preventing independently evaluated comments from leaving superseded or conflicting lessons active.

## Agreed Direction

- Keep every eligible comment as an attributed feedback source with its own current typed decisions and immutable binding to the effective canonical review that existed when the source was first accepted.
- Create active memory from one current synthesis per canonical review identity, not directly from each comment and not from the MR IID alone. Different reviewed heads have different diffs, findings, targets, and syntheses.
- Evaluate all accepted eligible source versions bound to that review together with the persisted review summary and findings, application-owned targets, the exact bounded reviewed diff, and application-verified role provenance for every source.
- Exclude system, internal, and Wormtamer-authored notes. Fetch source bodies and the diff transiently without retaining them in SQLite or ordinary diagnostics.
- Let a synthesis produce zero or more reusable lessons. Atomically replace its complete prior lesson set and retain validated provenance from each lesson to supplied source notes and review or finding targets.
- Supply the exact reviewed diff directly. Do not add feedback repository tools or permit access to another revision, repository, or public source.
- Treat authenticated create and update Note Hooks as the sole source-change signal. An accepted event that advances the source-set version immediately makes prior synthesis ineligible for retrieval, before recomputation.
- Do not reconcile comments periodically. Operators own webhook configuration and delivery, and GitLab's lack of deletion Note Hooks means deleting a comment does not automatically change or deactivate synthesized memory.

## Scope

- Define `supports`, `rejects`, and `corrects` for overall-review and finding targets when each source is interpreted in the bounded aggregate context.
- Define when feedback targets the overall review rather than one or more specific findings.
- Define the complete eligible source set, deterministic ordering, count and byte bounds, and behavior when exact comments or the exact reviewed diff are unavailable.
- Define how an explicit same-author correction supersedes earlier wording and how conflicting statements from different actors or roles remain represented.
- Define whether confidence represents interpretation certainty, factual confidence, synthesis confidence, or a smaller useful subset, and whether low-confidence decisions remain valid.
- Decide whether actor role, confidence, outcome, explicit wording, unresolved conflict, or activation timing imposes a hard memory-activation threshold.
- Define whether role provenance is fixed when each source version is accepted or re-resolved when the aggregate is synthesized.
- Define the required form of each reusable lesson, including standalone scope, conditions, source and target provenance, and prohibited one-off or copied content.
- Specify the required feedback prompt, response schema, trusted validation, persistence invariants, and focused architecture, reliability, and security documentation for the dependent implementation.

Do not implement synthesis in this policy task. Do not add analytics, evaluation replay, contributor scoring, repository tools, persisted comment or diff content, a memory-management surface, or cross-revision aggregation. The dependent [review feedback synthesis](synthesize-review-feedback.md) plan owns implementation, and the separate deferred [feedback-driven review evaluation](evaluate-feedback-driven-reviews.md) remains out of scope.

## Approach

1. Establish representative aggregate cases for explicit support, rejection, material correction, ambiguous discussion, generic acknowledgement, multiple independent findings, zero-finding reviews, same-author corrections, different-role conflicts, accepted source edits, missed-delivery and deletion limitations, and comments on different reviewed heads of one MR.
2. Define the exact source set and evidence bounds. Include only accepted eligible source versions bound to the same effective canonical review and require the exact reviewed diff rather than a newer MR diff.
3. Choose mutually exclusive per-source outcome and target-selection rules that produce no decision when target or stance is ambiguous, while preserving conflicting source decisions rather than flattening them silently.
4. Define correction, conflict, role, confidence, and activation-timing rules. Enforce every selected hard boundary in trusted code rather than relying only on model instructions.
5. Require every active lesson to be standalone, narrowly repository-specific, conditional where needed, supported by validated supplied provenance, and free of copied comments, identities, one-off code state, and generic programming advice.
6. Record the approved policy and its required model contract, trusted validation, and persistence invariants in the focused architecture, reliability, and security documents before implementing the dependent synthesis plan.

## Risks and Open Questions

- A Maintainer or Owner is stronger provenance but can still be mistaken; a hard role threshold reduces poisoning risk but may discard useful contributor knowledge.
- An exact diff provides stronger evidence than the review result alone but cannot establish runtime usage, unstated team policy, unchanged repository context, or external facts.
- Aggregating comments increases prompt-injection exposure and input size. Bounds must fail without silently dropping a source whose absence could change the synthesis.
- Immediate activation can expose a provisional lesson between an initial statement and a later correction. Delayed activation can withhold useful guidance; the policy must choose observable timing semantics.
- Missed create or update webhook delivery and comment deletion are intentionally not discovered by polling, so operator documentation must make the resulting stale-source risk explicit.
- `low` confidence currently coexists with the rule that ambiguous comments produce no decision; decide whether it has a distinct useful meaning or should be removed.
- Clarify whether “fixed”, “done”, or “good catch” supports a finding, and reserve correction for feedback that materially changes its factual basis, scope, priority, or recommendation.

## Verification

- Every eligible source maps to one deterministic supplied target and outcome or to no decision, and specific-finding feedback does not also create an overall-review decision without explicit overall-review content.
- The synthesis considers the complete bounded accepted source set for exactly one canonical reviewed revision; comments bound to another head of the same MR are excluded.
- A later explicit correction removes contradicted guidance from the replacement synthesis, while unresolved statements from different actors remain visible as conflict under the selected policy.
- Ambiguous comments and ordinary discussion create neither source decisions nor memory, but an accepted later edit still advances the complete source set.
- Accepting a create or update event immediately makes every synthesis for the prior source-set version ineligible; retrieval never serves that stale synthesis while replacement is queued or failed.
- One synthesis may create no lesson or several independent lessons, and every active lesson satisfies the selected role, confidence, timing, conflict, provenance, and content rules through trusted validation.
- Feedback evaluation receives only the exact bounded reviewed diff and cannot broaden repository scope, inspect other repository content, use tools, publish, or treat evidence as application instructions.
- Comment bodies and diffs remain transient and are absent from durable feedback, memory, and ordinary diagnostic state.
- No periodic comment reconciliation is introduced. The policy explicitly assigns webhook configuration and delivery to operators and states that deleting a comment does not automatically deactivate synthesized memory.
