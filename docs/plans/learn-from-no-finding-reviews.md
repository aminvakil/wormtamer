# Learn from no-finding reviews

Status: proposed

## Goal

Let eligible GitLab comments teach Wormtamer repository-scoped advisory lessons when the latest published review reported no actionable findings. This closes the current gap where feedback about a missed issue is ignored because there is no finding identifier to target.

## Scope

- Queue eligible comment feedback against the latest published review even when that review has zero findings.
- Evaluate the comment against that review's immutable identity, head SHA, and persisted summary, then retain only bounded, reusable project-specific guidance selected by the feedback evaluator.
- Represent review-level feedback directly instead of inventing a finding that Wormtamer did not publish.
- Preserve the existing actor-role attribution, repository scoping, edit replacement, deletion deactivation, secret rejection, and no-comment-body-retention rules.
- Keep finding-specific feedback behavior unchanged.

This work does not republish or amend the original review, create findings from comments, infer approval from missing feedback, reconcile comments missed without a Note Hook, or add a manual feedback API.

## Approach

When a source note's first eligible event is accepted, bind its feedback job to the exact latest published review; later note updates retain that target. Replace the current "any finding exists for this merge request" eligibility check with "a published review exists," and ensure a no-finding review never falls back to findings from an older revision.

Extend the feedback evaluator with a first-class review-level target. For a no-finding review, provide the persisted summary and immutable review metadata with an empty finding list. The evaluator may classify the comment as supporting, rejecting, or correcting the review-level conclusion, or return no decision when it is unrelated or ambiguous. A lesson remains optional and must satisfy the existing requirement for concise, reusable, project-specific guidance.

Update durable feedback decisions and review memory to identify either a finding target or a review target. Use an application-owned stable review target derived from the immutable review identity, and include that target in deterministic memory identity. Do not use a sentinel or synthetic `WT-F-...` identifier. Memory retrieval must expose the target type and provenance while retaining the same exact GitLab-instance and numeric-project scope.

Keep Note Hook ingestion and the feedback worker's at-least-once, lease, retry, source-check, and transactional replacement semantics. An edit reevaluates the same source note against its bound review; source deletion or an unobserved update deactivates any derived review-level memory as it does for finding-level memory.

## Risks and Open Questions

A top-level merge request comment does not identify a Wormtamer review explicitly. Binding its first eligible event to the latest published review is deterministic and matches current behavior, but feedback intended for an older revision may be judged unrelated. Do not add comment syntax or broader review selection unless observed usage proves that necessary.

Review-level feedback is easier to overgeneralize because there is no concrete finding to anchor it. The evaluator must return no lesson for generic approval, dissatisfaction without reusable guidance, or claims unsupported by enough context; role remains provenance rather than authority.

## Verification

- A Note Hook comment received after a published zero-finding review creates and completes a feedback job instead of producing `ignored_no_findings`.
- Concrete feedback describing a missed project-specific issue can create active advisory memory scoped to that repository, and a later review can retrieve it.
- Generic approval, unrelated discussion, and ambiguous criticism produce no active lesson.
- When an older review has findings and the latest published review has none, feedback binds to the latest review and does not silently use the older findings.
- Finding-targeted feedback continues to produce the existing finding-level decisions and memory.
- Editing a source comment replaces its current review-level decisions; deleting or changing it without a matching webhook eventually deactivates derived memory.
- Persisted state contains review and source identifiers plus structured decisions, but no source comment body or synthetic finding.
