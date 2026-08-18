# Synthesize review feedback memory

Status: proposed

Depends on:

- [Feedback classification and memory policy](define-feedback-policy.md)

## Goal

Replace independently active comment-derived lessons with one current, bounded feedback synthesis for each effective canonical review revision. Preserve individual comments as attributed feedback sources while allowing their combined evidence to produce zero or more repository-scoped lessons without leaving superseded or conflicting lessons active.

## Scope

- Keep each eligible GitLab note as a durable, immutable-bound feedback source with its current typed decisions and verified actor provenance.
- Maintain one current synthesis for the canonical review identity, not merely the merge request IID, because findings and diffs are revision-specific.
- Evaluate the persisted review result and application-owned targets together with the exact bounded reviewed diff and all accepted eligible source versions bound to that review, including the application-verified role provenance selected by policy for each source.
- Allow a synthesis to produce zero or more independently retrievable lessons. Preserve validated links from every lesson to its supporting source notes and review or finding targets.
- Recompute and atomically replace the complete synthesis after an authenticated create or update Note Hook advances the accepted source set.
- In the same ingress transaction, make every prior synthesis version ineligible for retrieval before acknowledging an event that advances the source-set version.
- Keep comment bodies and diffs transient. Do not persist them in SQLite, memory records, diagnostics, or ordinary logs.
- Update bounded panel views and review-memory retrieval to expose synthesized lessons and their non-content provenance rather than independently active per-comment lessons.

Do not add repository tools, related-repository or public-source access to feedback evaluation, periodic comment or source reconciliation, cross-revision synthesis, analytics, contributor scoring, a memory mutation UI, another queue or database, or a separate worker service.

## Approach

Retain delivery-deduplicated note events and immutable source-to-review bindings. Add an application-owned synthesis identity and source-set version under the effective canonical review identity. An accepted create or update event that advances current source state also advances that version and makes every prior synthesis version ineligible in the same transaction before webhook acknowledgement. Memory retrieval requires the synthesis version to equal the current source-set version, so queued or failed recomputation cannot leave stale lessons retrievable. Concurrent events may coalesce work, but a synthesis may commit only for the complete source version it evaluated; a stale attempt cannot reactivate an older version.

For each attempt, trusted code fetches every bounded accepted source note, verifies its identity, author, and accepted source version, supplies the role provenance selected by the approved policy, and orders sources deterministically. It transiently retrieves the exact diff for the bound reviewed head and combines it with the persisted summary, findings, and application-owned targets. It must never substitute a newer MR diff or silently omit, truncate, or relabel an unavailable or over-limit source set. Existing configured-secret checks apply to every comment, diff, and model-visible field before generation.

The structured evaluator result retains typed source decisions and returns a bounded ordered lesson set. Each lesson references only supplied note and target identities. Trusted code validates those references, selected policy thresholds, lesson bounds, repository scope, and secret exclusion before replacing the previous synthesis in one transaction. Reviews retrieve only active synthesized lessons for the current source-set version; migration or state transition must not leave legacy per-comment lessons active alongside them.

Authenticated create and update Note Hooks are the sole signal that a source set changed. Do not periodically list or check comments. Deployment operators are responsible for configuring those hooks and ensuring their delivery; a missed event leaves Wormtamer unaware of the change. GitLab emits no deletion Note Hook, so deleting a comment alone does not advance the source-set version, trigger recomputation, or deactivate lessons derived from it. This limitation must be documented for operators. At-least-once claims, leases, retries, and source-version checks keep accepted events safe after crashes.

## Risks and Open Questions

- GitLab must support bounded retrieval of the exact historical reviewed diff after the MR head changes or the MR closes; unavailable exact evidence must not be replaced with current content.
- Aggregating comments increases prompt-injection exposure, model input size, membership lookups, and synthesis work, so source-count, aggregate-byte, and request limits must be application-owned.
- Role changes after a comment was written can alter current membership. The policy must specify whether synthesis uses the role snapshot from the accepted source version or re-resolves all roles on every synthesis.
- A comment may arrive while synthesis is running. Transactional source-set invalidation and version checks must prevent stale retrieval and commits.
- Missed create or update delivery and comment deletion remain invisible by design. This avoids unbounded polling but can leave accepted source state different from GitLab indefinitely.
- Multiple lessons need stable identities and versions without treating model wording or arbitrary source order as trusted identity input.

## Verification

- Two comments bound to the same review, where the later comment explicitly corrects the earlier one, produce a replacement synthesis in which the contradicted lesson is no longer active.
- Comments bound to another reviewed head are not included, even when they belong to the same MR.
- Independent reusable guidance in one review can produce multiple bounded lessons, while ordinary discussion can produce no decision or lesson.
- Conflicting comments and role provenance follow the approved deterministic activation policy rather than whichever note happened to be evaluated independently or last.
- Accepting a create or update event that advances current source state immediately makes the prior synthesis ineligible for retrieval; failed or delayed recomputation leaves no stale current synthesis.
- A stale or repeated attempt cannot commit against a newer source-set version or restore an older synthesis.
- Every active lesson references only supplied source and target identities and remains scoped to the exact GitLab project.
- Feedback evaluation receives only the exact bounded reviewed diff and has no repository, public-source, publication, or mutation tools.
- A missing, mismatched, oversized, secret-bearing, or otherwise invalid evidence set fails without committing partial synthesis or exposing private content.
- SQLite and ordinary diagnostics contain source identifiers, structured decisions, synthesis provenance, and lesson state, but no comment bodies or diffs.
- No periodic GitLab comment listing or source check is introduced; configured create and update Note Hooks are the only change signal.
- Operator documentation states that webhook delivery is required and that deleting a comment does not automatically invalidate or deactivate its synthesized lessons.
- Later reviews retrieve only active synthesized lessons whose source-set version is current; legacy independently active comment lessons are not returned alongside them.
