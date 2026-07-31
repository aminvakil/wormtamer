# Evaluate feedback-driven reviews

Status: proposed

Depends on:

- [Runtime memory](../agents/architecture.md#context-and-state)
- [Review memory retrieval](../agents/architecture.md#tool-brokers)

## Goal

Establish whether comment-derived decisions and active memory improve later reviews instead of merely accumulating data or reinforcing repeated false positives.

## Scope

- Preserve the bounded link between a review, retrieved memory identities, published finding identities, and subsequent structured feedback decisions.
- Report installation-local measures such as supported findings, rejected findings, repeated corrected patterns, and outcomes associated with memory use.
- Provide a reproducible offline evaluation path using explicitly selected historical records without re-publishing GitLab notes.
- Make evaluation output aggregate or identifier-based by default.

Do not create a centralized analytics service, compare teams, export private source, retrain Gemini, score individual contributors, or treat missing feedback as approval.

## Approach

Use existing durable finding, feedback-source, and memory identities to form evaluation records. Keep evaluation separate from live job decisions: it may show that a lesson is harmful or irrelevant, but evaluation does not itself change active memory state.

Begin with deterministic counts and a small operator-invoked replay against active, non-sensitive cases. Add semantic or model-based judging only if human feedback cannot answer a concrete quality question, and never let a model judge its own output without attributed human evidence.

## Risks and Open Questions

- Feedback is selective and missing-not-at-random, so aggregate rates are evidence rather than unbiased accuracy estimates.
- Repository evolution can make historical cases obsolete.
- Replaying private diffs through Gemini may require renewed operator consent and cost controls.
- Minimum sample sizes and the first release's useful measures need definition from actual feedback volume.

## Verification

- Operators can determine whether a finding received structured supported, rejected, corrected, conflicting, or no feedback without treating the last category as positive.
- A later finding can be associated with the active memory records actually retrieved for its review.
- Evaluation runs do not publish, change active memory state, reset jobs, or expose repository content in default output.
- A harmful lesson can be identified through evidence without the evaluation run silently changing live memory.
