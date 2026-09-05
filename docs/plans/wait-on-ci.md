# Wait for successful CI before review

Status: proposed

## Goal

Avoid repository preparation and model work on MR revisions whose CI has not succeeded, without blocking other reviews or exhausting the review retry budget.

## Configuration and Behavior

Add a deployment-wide boolean alongside the existing [review grace period](../agents/reliability.md#review-grace-period):

```json
{
  "grace_period": "1m",
  "wait_on_ci": true
}
```

`wait_on_ci` defaults to `false` when omitted. Reject explicit null and non-boolean values at startup. Disabled installations retain the current review behavior without CI requests.

When enabled, apply CI eligibility after the grace deadline, before starting new repository or model work. On every eligibility check, first validate the authorized project and confirm that the MR is still open at the job's exact head. Then evaluate the latest applicable MR pipeline:

| Observation | Outcome |
| --- | --- |
| GitLab confirms no applicable pipeline | Proceed with review |
| Applicable pipeline reports `success` | Proceed with review |
| Recognized non-success status, including pending, running, failed, canceled, manual, or skipped | Remain waiting |
| MR closed/merged or head superseded | Mark the old review job obsolete |
| Request failure, malformed response, unknown status, or unresolved pipeline identity | Use normal error handling; never infer success or absence |

Waiting has no expiry and no eventual bypass. A failed pipeline is not a failed review job. A successful retry or replacement pipeline on the same revision unblocks its existing job. A newly observed revision retains its own grace deadline and review identity; duplicate events and scans do not reset either schedule.

This is a condition for starting new review work, not a continuous CI monitor. Do not interrupt an already-running review, regenerate a completed review after CI changes, or reapply CI waiting to publication of a locally saved result. Preserve exact-head publication recovery and patch-equivalence suppression. Current-head checks before publication remain mandatory.

The no-pipeline exception is intentional: the grace period reduces the pipeline-creation race but cannot exclude a pipeline appearing after review starts. Do not inspect repository CI files to guess whether a pipeline will eventually exist.

## Approach

### GitLab readiness

Extend the trusted GitLab broker with bounded CI metadata reads under the existing [authorization and credential boundary](../agents/security.md#credentials). Use GitLab's MR-associated pipeline for the current revision, not the latest pipeline in the project or every historical pipeline. Use GitLab's aggregate pipeline status rather than traversing jobs or implementing independent rules for allowed failures and optional jobs.

Before implementing selection, verify the supported GitLab API's MR/current-revision association, including branch-backed and merged-results pipelines. A merged-results pipeline can use a generated commit; blindly comparing its SHA to the source head is insufficient. Record the chosen association rule and its evidence before coding the gate. Ambiguous or stale metadata must not be classified as confirmed absence. This is a focused prerequisite, not a request to aggregate unrelated branch, child, or downstream pipelines.

Refresh the selected pipeline on each check; do not pin waiting forever to the first failed pipeline ID. Preserve the client's existing response limits, redirect rejection, request deadlines, shared rate-limit gate, and error classification.

### Durable waiting and attempt accounting

Reuse `next_attempt_at` to schedule another eligibility check one minute after a successful non-green observation. Keep the job queued and release the worker immediately after persisting that decision. Other due reviews remain eligible; do not sleep while holding a review job. Rechecks can occur later because of normal worker load or GitLab backpressure.

Separate eligibility polling from the charged review-attempt transition. The current `ClaimJob` increments attempts immediately, so routing CI waits through ordinary retries is incorrect. Prefer selecting a due identity for preflight and conditionally claiming that same identity when charged work is required. Do not claim a different queued job using the first job's eligibility decision, and do not hold a SQLite transaction open across HTTP requests.

Successful CI waiting must neither increment attempts nor erase earlier failures. A crash during an uncharged CI check must leave recoverable work without spending the last review attempt. Actual API/infrastructure errors retain bounded retry and permanent-failure handling; successful waiting between errors must not reset that budget. Keep saved-result publication recovery, marker reconciliation, and patch-ID retry semantics coherent with the adjusted attempt boundary.

Persist only the workflow metadata needed to distinguish CI waiting and retain its last validated status, alongside the next check deadline. Waiting and its deadline survive restart. Terminal transitions stop polling; fresh MR state takes precedence over CI status even when CI remains failed. Apply the current deployment setting on subsequent eligibility checks, without resetting grace or retry deadlines or restarting completed work.

Use the existing SQLite store and one worker process. Any necessary schema adjustment follows the [compatibility baseline](../agents/architecture.md#compatibility-baseline); do not introduce a separate queue, scheduler service, or generic workflow framework.

### Visibility and documentation

Show the effective `wait_on_ci` setting in the existing non-secret configuration panel. Render deferred jobs as “Waiting for CI” with the last observed status rather than a review failure. The panel continues to read persisted state only and makes no GitLab requests.

On implementation, move the accepted scheduling and retry rules into Reliability, link configuration guidance from deployment documentation, and update the example configuration. Keep each decision authoritative in one place.

## Non-Goals

- Pipeline webhooks, configurable polling intervals, CI-wait timeouts, or manual bypass controls.
- CI log ingestion, pipeline/job mutation, automatic retries of GitLab CI, or model-assisted CI diagnosis.
- Aggregating all historical, project-wide, child, or downstream pipelines independently of GitLab's selected MR pipeline status.
- Changes to feedback eligibility, the Gemini loop, repository tool permissions, or webhook acknowledgment guarantees.

## Verification

Inspect and extend the existing configuration, GitLab, store, worker, and panel coverage according to [Test Discipline](../../AGENTS.md#test-discipline). These are observable outcomes, not one test per bullet; obtain approval before implementation if the focused matrix needs more than five new behavioral scenarios.

- Configuration distinguishes disabled/default behavior from enabled CI gating and rejects invalid values rather than silently defaulting.
- Validated pipeline metadata yields the agreed eligibility decisions for the current revision; an old successful run, unknown status, or failed lookup cannot unlock review. Confirm the selected association works for supported merged-results metadata.
- A non-green revision can remain deferred across repeated checks and restart without consuming review attempts or delaying another eligible MR. Green CI on that same revision admits work without another grace period or a duplicate job.
- A closed/merged or superseded revision stops waiting before repository/model work, while genuine API errors retain their finite retry budget even when interleaved with CI waits.
- Existing saved-result publication, external-marker recovery, patch equivalence, and feedback behavior remain intact. The panel distinguishes durable CI waiting from failure and displays only bounded persisted status.
