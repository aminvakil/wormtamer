# Publish addressable review findings

Status: proposed

## Goal

Give every validated finding a stable application-owned identity so human feedback and later memory can refer to the exact finding from an exact merge request revision.

## Scope

- Assign and durably retain a stable finding identity derived from application workflow identity rather than model-provided text.
- Include a bounded human-referenceable identifier with each rendered finding while retaining one concise summary publication per reviewed revision.
- Preserve finding identity across retries, restart, publication reconciliation, and reuse of a stored review result.
- Keep review-level and finding-level identity distinct.

Do not add feedback ingestion, infer finding outcomes, publish inline discussions, or change the model into the authority for identifiers.

## Approach

Extend validated review persistence so each ordered finding is associated with an identifier under the existing review identity. The renderer exposes enough of the identifier for an explicit feedback action while keeping a stable machine representation for webhook processing. Identity assignment occurs only after model output passes local validation.

Continue to reconcile the existing marked summary note as one idempotent external effect. If separate GitLab discussions are later required, they need their own publication plan and stable external-effect reconciliation rather than being introduced here.

## Risks and Open Questions

- Content-derived identifiers can change after harmless wording changes, while ordinal identifiers apply only within one immutable review revision; the final algorithm must prioritize retry stability and unambiguous lookup.
- Visible identifiers must be usable without making the note noisy.
- Contributor copies of identifiers are untrusted and cannot themselves authenticate feedback.

## Verification

- Every published finding has one unique identifier scoped to the reviewed GitLab instance, project, merge request, and head SHA.
- Reprocessing the same stored result after crashes or lost GitLab responses produces the same finding identifiers and no duplicate publication.
- A malformed or model-supplied identifier cannot enter durable state or rendered output.
- An identifier resolves to exactly one durable finding and never aliases a finding from another revision or repository.
