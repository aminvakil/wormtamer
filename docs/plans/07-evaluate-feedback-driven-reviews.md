# Evaluate feedback-driven reviews

Status: proposed

Depends on:

- [Capture explicit review feedback](04-capture-explicit-review-feedback.md)
- [Retrieve approved review memory](06-retrieve-review-memory.md)

## Goal

Establish whether approved feedback and memory improve later reviews instead of merely accumulating data or reinforcing repeated false positives.

## Scope

- Preserve the bounded link between a review, retrieved memory identities, published finding identities, and subsequent explicit feedback.
- Report installation-local measures such as supported findings, rejected findings, repeated corrected patterns, and outcomes associated with memory use.
- Provide a reproducible offline evaluation path using explicitly selected historical records without re-publishing GitLab notes.
- Make evaluation output aggregate or identifier-based by default.

Do not create a centralized analytics service, compare teams, export private source, retrain Gemini, score individual contributors, or treat missing feedback as approval.

## Approach

Use existing durable finding, feedback, and memory identities to form evaluation records. Keep evaluation separate from live job decisions: it may show that a lesson is harmful or irrelevant, but memory changes still go through explicit correction or supersession.

Begin with deterministic counts and a small operator-invoked replay against approved, non-sensitive cases. Add semantic or model-based judging only if human feedback cannot answer a concrete quality question, and never let a model judge its own output without attributed human evidence.

## Risks and Open Questions

- Feedback is selective and missing-not-at-random, so aggregate rates are evidence rather than unbiased accuracy estimates.
- Repository evolution can make historical cases obsolete.
- Replaying private diffs through Gemini may require renewed operator consent and cost controls.
- Minimum sample sizes and the first release's useful measures need definition from actual feedback volume.

## Verification

- Operators can determine whether a finding received explicit supported, rejected, corrected, conflicting, or no feedback without treating the last category as positive.
- A later finding can be associated with the approved memory records actually retrieved for its review.
- Evaluation runs do not publish, change memory approval, reset jobs, or expose repository content in default output.
- A harmful lesson can be identified through evidence and superseded through the normal memory workflow with the audit trail intact.
