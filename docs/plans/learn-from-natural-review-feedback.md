# Learn from natural review feedback

Status: proposed

## Goal

Let people give Wormtamer feedback through ordinary GitLab merge request comments without copying finding identifiers or using special syntax. For every eligible comment after a published Wormtamer review, let Gemini decide whether the comment is unrelated discussion, feedback about the overall review, or feedback about one or more supplied findings, and retain only validated reusable repository guidance.

## Scope

- Queue every eligible comment against a published Wormtamer review whether that review has zero, one, or many findings.
- Bind a source note's first eligible event to the exact latest published review; later note updates retain that target.
- Give the feedback evaluator the bound review's immutable identity, head SHA, persisted summary, complete persisted findings with application-owned identifiers, transient comment text, and verified actor-role provenance.
- Let Gemini return no decision for ordinary discussion, requests for a coworker to review, unrelated comments, or ambiguous remarks.
- Let Gemini classify explicit natural-language feedback as supporting, rejecting, or correcting either the overall review or one or more supplied findings. Users never need to mention an internal review or finding identifier.
- Represent the overall review as a first-class target for every review, not only reviews with no findings.
- Retain an optional lesson only when the evaluator selects concise, reusable, project-specific guidance that can improve later reviews. A feedback decision does not automatically create memory.
- Preserve repository scoping, actor-role attribution, edit replacement, deletion deactivation, secret rejection, no-comment-body retention, and advisory-memory treatment.

This work does not republish or amend the original review, create a finding from a comment, rerun a review at the same head SHA, infer approval from missing feedback, reconcile comments missed without a Note Hook, fetch surrounding discussion as evaluator context, or add a manual feedback API.

## Approach

### Bind comments to immutable reviews

When a new source note is first accepted, select the latest review for the exact GitLab instance, numeric project, and merge request that has both a persisted result and durable publication. Store that review job identity on the feedback job in the same transaction that queues it. A comment update must reuse the existing binding rather than selecting a newer review.

Check for an existing feedback job before selecting a review so edits preserve their original target. If no feedback job exists and no published review exists, persist the event as ignored without creating a job. Do not require findings for eligibility and never fall back from a newer zero-finding review to findings from an older revision.

### Delegate semantic classification, not authority

Send each eligible comment to Gemini even when it appears to be normal merge request discussion; do not require keywords, finding identifiers, mentions, or other application-owned syntax. Supply the exact bound review as untrusted context and let Gemini return zero or more structured decisions:

- no decisions when the comment is unrelated or too ambiguous;
- a review-target decision for feedback about the review as a whole, including a missed issue after a no-finding review;
- finding-target decisions when natural language can be associated with one or more supplied findings.

Use a unified target type and application-owned target identity in the response contract. The application supplies the only valid review and finding targets and rejects invented, mismatched, duplicate, or malformed targets. Actor role remains provenance rather than authority, and comments, findings, summaries, and model output remain untrusted evidence.

Explicit overall approval may produce a supporting review decision but no active lesson. Generic phrases such as "looks good" should produce no decision when their relationship to Wormtamer is ambiguous. A concrete one-off missed defect may produce a correcting review decision without memory. A concrete correction that establishes reusable project-specific guidance may additionally create memory.

### Persist typed decisions and bounded memory

Create an application-owned stable review target derived from the immutable review identity. Extend durable feedback decisions and review memory to identify either that review target or a supplied finding target. Include target type and identity in deterministic memory identity; do not use a sentinel or synthetic finding identifier.

Memory retrieval must expose target type and provenance while retaining the exact GitLab-instance and numeric-project scope. Existing finding-specific outcomes and active memories retain their behavior, but users no longer need to identify findings explicitly.

Keep the feedback worker's at-least-once, lease, retry, source-check, and transactional replacement semantics. An edit reevaluates the current source text against the originally bound review and replaces prior decisions. Source deletion or an unobserved update deactivates derived memory as it does today. Persist structured decisions and source and target identifiers, but never the source comment body or arbitrary model conversation.

## Risks and Open Questions

Every eligible post-review comment now incurs a Gemini evaluation, including ordinary team discussion. This is the deliberate cost of accepting natural feedback without brittle keyword or identifier gates; existing request, retry, input, and secret bounds remain in force.

A single comment and the bound review may not provide enough context to resolve pronouns or references to surrounding discussion. Return no decision when the evidence is insufficient rather than fetching and retaining broader conversation context in this release.

A top-level merge request comment does not identify a Wormtamer review explicitly. Binding its first eligible event to the latest published review is deterministic, but feedback intended for an older revision may be judged unrelated. Do not add selection syntax unless observed usage proves it necessary.

Natural review-level feedback is easier to overgeneralize than feedback anchored to a concrete finding. Persist a correction without memory when the comment identifies a one-off defect, generic dissatisfaction, or a claim without enough reusable project-specific guidance.

## Verification

- A person can give useful feedback in natural language without copying or mentioning a `WT-F-...` or review identifier.
- Every eligible comment after a published review creates and completes a feedback job, including comments after zero-finding reviews.
- Ordinary implementation discussion, a request for a coworker to review, unrelated comments, and ambiguous approval produce no decisions or active memory.
- Explicit feedback about the review as a whole produces a typed review-target decision whether or not the review had findings.
- Natural-language feedback about a supplied finding is associated with that finding without requiring its identifier in the comment.
- Concrete feedback describing missed reusable project-specific guidance can create active advisory memory scoped to that repository, and a later review can retrieve it.
- A concrete one-off missed defect can be recorded as a review correction without creating lasting memory.
- Trusted validation rejects model-invented review or finding targets and prevents scope changes.
- When an older review has findings and the latest published review has none, a new comment binds to the latest review and does not silently use older findings.
- Editing a source comment reevaluates it against the same bound review and replaces current decisions; deletion or an unobserved update deactivates derived memory.
- Persisted state contains review, target, and source identifiers plus structured decisions, but no source comment body or synthetic finding.
