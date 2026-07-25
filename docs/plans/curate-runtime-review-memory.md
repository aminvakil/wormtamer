# Curate runtime review memory

Status: proposed

Depends on: [Capture explicit review feedback](capture-explicit-review-feedback.md)

## Goal

Turn supported review feedback into installation-local memory that humans can approve, correct, reject, and supersede without treating model output or a single comment as trusted policy.

## Scope

- Add durable memory records with scope, lesson, evidence references, source, confidence, approval status, and timestamps.
- Keep an audit trail for proposal, approval, rejection, correction, and supersession.
- Require human approval before a memory record is eligible for review-time retrieval.
- Enforce installation and repository scope independently from review workflow state.

Do not train or fine-tune a model, automatically promote feedback, mutate contributor documentation, share memory between installations, or retrieve memory into reviews yet.

## Approach

Create memory as a separate SQLite domain linked to immutable finding and feedback evidence by identifiers, not by copied raw repository content. A proposed lesson is untrusted until an authorized human performs an explicit approval action. Current code and explicit team policy always override memory.

The approval interaction and minimum approving role must be decided before this plan is approved. Prefer extending the explicit GitLab feedback contract over adding a status API or control plane, provided approval and correction remain unambiguous and auditable.

Memory scope must be no broader than its evidence supports. At minimum, the model must distinguish installation-wide and repository-specific lessons; narrower path or component scope should be added only when concrete feedback requires it.

## Risks and Open Questions

- Automatically generated lesson text may misstate human feedback; approval must show the exact proposed lesson and evidence.
- A single team's conventions may conflict across repositories, making overly broad scope harmful.
- Deletion, privacy, and retention requirements for feedback and memory are not yet defined.
- Approval authorization must remain meaningful when GitLab membership later changes.

## Verification

- Explicit feedback can produce a proposed memory record that is ineligible for retrieval until approved.
- Authorized humans can approve, reject, correct, and supersede records without erasing prior states or evidence references.
- Unapproved, rejected, and superseded records are distinguishable through deterministic queries.
- Memory from one installation or repository scope cannot be silently widened or exposed to another.
- Stored memory contains no credentials, hidden prompts, raw model conversation, or unnecessary source excerpts.
