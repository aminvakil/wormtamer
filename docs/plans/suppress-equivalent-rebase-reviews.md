# Suppress reviews for equivalent rebases

Status: proposed

## Goal

Avoid repeated Gemini reviews and GitLab comments when rebasing an open merge request changes its head SHA but GitLab reports the same normalized patch through `patch_id_sha`.

Keep the exact head SHA as revision, repository-access, review-target, finding, and publication identity. Patch equivalence suppresses redundant work only while the SQLite state connecting the revisions remains available; it does not redefine which exact head produced a review.

The motivating observed case had different pre- and post-rebase heads, while both GitLab diff versions had the same `patch_id_sha` and contained the same effective change.

## Scope

- Read and validate the `patch_id_sha` availability for the GitLab merge request diff version corresponding to the claimed head SHA.
- Persist the patch ID on each newly generated review job when GitLab supplies one; retain an explicit unavailable outcome when review must proceed without one.
- For a new head in the same GitLab instance, project, and merge request, reuse the newest completed canonical job with the same patch ID only when that job has both a locally validated result and a durable publication.
- Complete the new job as equivalent to that canonical job without invoking Gemini, loading repository archives, or creating another GitLab note.
- Preserve the existing head-based review and finding identities and `wormtamer:review` publication marker. Do not edit existing notes or add a marker for an equivalent head.
- Resolve feedback through the newest eligible completed job: a direct review binds to itself, while an equivalent job binds through `equivalent_to_job_id` to its canonical published review and original head-based review and finding targets.
- Show the persisted patch ID and canonical-job relationship in the read-only review views so equivalent completion is distinguishable from an unpublished or external-only review.

Git patch IDs ignore whitespace. Under this policy, a whitespace-only update with the same GitLab patch ID is intentionally treated as already reviewed. Patch equality can also survive a target-branch update whose surrounding repository context changed; accepting that possible stale-context risk is part of suppressing reviews after equivalent rebases.

This plan does not:

- Change webhook admission, reconciliation, or head-SHA job uniqueness.
- Deduplicate patches across merge requests or projects.
- Compute an application-owned strict diff fingerprint or compare repository trees.
- Backfill patch IDs for existing jobs whose SQLite rows predate this behavior.
- Add patch-based GitLab note markers or recover patch equivalence after SQLite state loss.

Consequently, an existing completed row without a patch ID is not reusable and may cause one more review. If SQLite is removed, a clean Wormtamer instance still recovers an exact unchanged head through its existing head-based note marker, but after a rebase it does not infer patch equivalence from the old note and reviews the new head normally.

## Approach

### GitLab diff-version metadata

Extend bounded review loading to obtain the current merge request diff-version metadata from GitLab. Accept a patch ID only from a finalized version whose project, merge request, and `head_commit_sha` match the claimed review identity. Normalize the validated hexadecimal value before returning it with the review snapshot.

Represent patch identity availability explicitly rather than treating every null value alike:

- **Available:** the matching finalized version contains a valid non-null patch ID. It is eligible for equivalence lookup.
- **Pending:** GitLab has not exposed the matching current version yet or reports that preparation is incomplete. Defer once under the stable `merge_request_patch_id_pending` category only when enough ordinary claims remain for the actual review.
- **Terminally unavailable:** GitLab reports a finalized zero-file or no-files version, another documented terminal state without a patch ID, the job already used its one pending deferral, or deferring would consume the reserved review retry budget. Continue through the normal review path without equivalence lookup and record no patch ID; this optional deduplication signal must not strand an otherwise reviewable revision.

A finalized non-empty version with a null patch ID is ambiguous for imported, legacy, or delayed GitLab data. Treat the first observation as pending when retry budget permits, then terminally unavailable on the next claim. Patch-ID waiting therefore consumes at most one claim and leaves at least three of the current five claims, including the active claim, for normal review and publication. Reconciliation needs no reset exception because the same unique head-SHA job is requeued once and then proceeds without patch equivalence if GitLab still has no usable ID.

Transport failures, rate limits, response decoding errors, unknown diff-version states, malformed patch IDs, and identity mismatches are not translated into unavailable success: they retain existing stable retry, failure, backpressure, or obsolete handling. Keep the exact existing-head publication lookup before review loading so external recovery for the same head remains unchanged and does not require diff-version availability.

### Durable equivalence state

Add a sequential SQLite migration with `review_jobs.patch_id_status`, nullable `review_jobs.patch_id_sha`, and nullable `review_jobs.equivalent_to_job_id`, where the latter references a canonical review job. The bounded status values are `unknown`, `pending`, `available`, and `unavailable`; existing rows migrate to `unknown`. Enforce that only `available` has a valid non-null patch ID and that equivalent jobs have an available patch ID. Add a lookup index scoped by GitLab instance, project ID, merge request IID, status, and patch ID. Patch ID is not unique because multiple head-SHA jobs can represent one patch.

Checkpoint `pending` in the same lease-checked operation that schedules the single patch-ID retry, making the deferral durable across restart. Persist `available` and its patch ID, or `unavailable` and a null patch ID, atomically with a newly validated review result. A normal review completed under the explicit unavailable outcome cannot become a source for equivalence. Existing externally recovered publications have no local result and retain `unknown` because they cannot be canonical sources for equivalence. Existing rows also remain `unknown` rather than triggering network backfill during migration or startup.

Add a lease-checked transactional store operation that completes a running job as equivalent. It must verify that the selected canonical job:

- Is a different job in the exact same GitLab instance, project, and merge request.
- Has the same non-null patch ID.
- Is completed with a local review result and publication.
- Is itself canonical rather than another equivalence, preventing alias chains.

The equivalent job becomes completed, records the patch ID and canonical job ID, clears its lease and error fields, and creates no review result, finding, memory-retrieval audit, or publication of its own. Completed-job invariants and panel labels must distinguish direct publication, external-only publication, and equivalent completion.

### Worker decision

For a claimed job without a stored result or exact existing head marker:

1. Load the current snapshot and classify its patch identity as available, pending, or terminally unavailable from validated GitLab fields.
2. If this is the first pending observation and at least three claims can remain for review including the next claim, atomically checkpoint `pending` and requeue once without invoking Gemini or publishing. If the job was already pending or lacks that reserve, classify the patch identity as unavailable and continue now.
3. If available, look up the newest eligible canonical review for the same merge request and patch ID. If one exists, confirm immediately before completion that the claimed head remains the open current head, then atomically complete the job as equivalent and log only bounded job identities and the canonical job ID.
4. If no canonical match exists, or patch identity is terminally unavailable, run the existing review flow and checkpoint the available patch ID or explicit null with the validated result before publication.

A changed head during this sequence remains obsolete under the existing rules. Retries with an already stored result continue directly to publication and retain the patch availability checkpointed with that result. Exact-marker recovery, publication reconciliation, marker generation, finding identifiers, and model prompts otherwise remain unchanged.

The read-only panel should display equivalent jobs as references to their canonical job rather than pretending they own the canonical result or publication. It should distinguish a terminally unavailable patch identity from a value that has not yet been fetched.

For a new feedback job, select the newest eligible completed review job for the exact merge request and resolve its effective review job as `COALESCE(equivalent_to_job_id, id)`. Require the resolved canonical job to have the local validated result and publication used by existing feedback evaluation. Bind the feedback job immutably to that canonical ID. This makes X → Y → equivalent-X feedback target X, while direct Y still targets Y; it does not copy results, publications, or target identities onto the equivalent job. Updates to an existing feedback note retain their original immutable binding as today.

After implementation, update the focused architecture and reliability documents and the README to describe one generated review per distinct GitLab patch within retained installation state, the accepted whitespace and changed-base-context semantics, equivalent job persistence, and clean-database behavior.

## Verification

- An initial revision with a finalized non-null patch ID invokes Gemini once, publishes one head-based marked note, and stores its patch ID with the validated result.
- A later head of the same merge request with the same GitLab patch ID completes as equivalent to the canonical job without invoking Gemini, loading a repository archive, rendering a note, or calling GitLab publication.
- Equivalent completion survives process restart because both the patch ID and canonical relationship are durable.
- A different patch ID, a match from another merge request or project, an external-only publication, an unpublished result, a failed job, and a pre-migration null patch ID do not suppress review generation.
- A pending current patch ID requeues at most once under its stable category without model or publication work; if it remains unavailable, the next claim performs a normal review without a patch ID and preserves at least three claims for review and publication rather than failing only because the optional identity was delayed.
- Finalized zero-file and no-files versions proceed directly through normal review without equivalence, while imported or legacy finalized null patch IDs use the bounded pending-then-unavailable behavior.
- Transport, decoding, malformed-value, unsupported-state, authorization, and identity errors retain stable failure handling and never silently become a successful unavailable classification.
- Lease loss or a head change cannot leave an equivalent job completed by a stale worker.
- In an X → Y → equivalent-X sequence, a new eligible feedback comment binds to canonical X; a comment first bound while Y is the newest eligible completed job remains immutably bound to Y after later updates.
- Existing head-based marker recovery and lost-publication-response reconciliation retain their current behavior.
- Removing SQLite and then rebasing an MR with only an old head-based Wormtamer note results in a new review and comment, because no patch equivalence is reconstructed from GitLab notes.
- Review list and detail views identify available, pending, terminally unavailable, and equivalent patch state and link equivalent jobs to the canonical job without duplicating its result, findings, publication, or feedback targets.
