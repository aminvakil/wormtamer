# Recover jobs without leases

Status: approved

## Goal

Replace lease ownership and renewal with startup recovery that matches the supported one-process, one-replica deployment. Preserve at-least-once review and memory execution and idempotent publication with less coordination state and no renewal goroutines.

## Scope

Apply the same single-process recovery model to review and feedback jobs. Keep one review worker and one feedback worker; they may continue processing their separate job kinds concurrently.

Remove lease owners, lease expirations, worker owner identifiers, lease renewal methods, and renewal goroutines. Collapse review `publishing` into `running`: a persisted validated result, rather than a separate transient state, determines whether a claimed review resumes publication or invokes Gemini.

Retain:

- atomic due-job claims
- attempt counts, bounded retry delays, five-attempt exhaustion, and manual failed-job retry
- patch-ID pending deferral and patch-equivalent canonical completion
- exact-head publication-marker recovery
- atomic feedback completion and optional runtime-memory insertion
- bounded graceful shutdown and the one-replica deployment requirement

This plan does not add multi-replica coordination, a queue service, or a generic worker framework.

## Approach

- At service startup, recover interrupted `running` review and feedback jobs before workers start. Requeue jobs with attempts remaining and fail jobs whose attempt budget is exhausted.
- Claim one due queued row atomically per worker, move it to `running`, and increment its attempt count. No other supported process may claim the same job.
- Guard result checkpoints, equivalent completion, publication completion, retries, and terminal transitions by job identity and expected `running` state rather than lease owner and time.
- When a review already has a validated result, skip repository preparation and Gemini and continue publication reconciliation. A running review without a result retains existing marker-first recovery.
- Leave cancellation-interrupted work recoverable by the startup transition. Do not introduce an in-process timeout claimant to replace leases.
- Remove lease columns and lease-oriented due indexes from the rebaselined schema and update reliability and deployment documentation to describe startup recovery.

## Verification

- A process interruption before review-result persistence causes the claimed revision to be reviewed again after startup.
- An interruption after result persistence resumes publication without another Gemini review or repository preparation.
- An interruption after GitLab accepts a note but before local completion reconciles the existing marker and does not publish a duplicate.
- Patch-ID pending retries preserve their retry reserve, and equivalent heads still complete against an eligible canonical review without generation or publication.
- An interrupted feedback job may repeat evidence loading and memory evaluation, while feedback completion and optional memory insertion remain atomic and cannot create duplicate memory.
- Startup does not reclaim completed, failed, obsolete, or already queued jobs, and it fails exhausted interrupted jobs deterministically.
- Retry scheduling, failure categories, manual retry, current-head checks, and graceful shutdown retain their observable behavior.
- Concurrent webhook ingestion, reconciliation, and operational retry cannot cause two claims of the same job within the supported single process.
