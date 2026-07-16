# Validate and package a GitLab 17 deployment

Status: proposed

## Goal

Produce a minimally deployable Wormtamer image and validate the current end-to-end reviewer against a real GitLab 17 or newer instance. Resolve concrete compatibility issues found by the validation and document the least-privileged GitLab PAT and project role that support the required read and note-publication operations.

This milestone proves the existing vertical slice before adding repository workspaces, model tools, runtime memory, or other review capabilities.

## Scope

Include:

- An operator-run smoke test using a dedicated, non-sensitive GitLab project and real Gemini credentials
- Validation of webhook ingestion, startup reconciliation, Gemini review, publication, restart idempotency, draft-to-ready discovery, encoded namespace paths, and GitLab pagination behavior
- Discovery of the minimum working GitLab PAT scope and project role
- Compatibility fixes limited to supported GitLab 17-or-newer APIs
- A minimal OCI image that builds the existing CGO and SQLite FTS5 binary and runs as a non-root user
- Operator documentation for external configuration, persistent SQLite storage, one-replica operation, health checking, startup, and graceful shutdown
- Automated regression tests for every compatibility defect found during the smoke test

Do not add model tools, repository checkout, runtime memory, a status API, another database or queue, orchestrator-specific manifests, registry automation, automatic credential discovery, or repository-controlled code execution.

## Approach

### Real GitLab validation

Use a dedicated project whose contents and merge requests are safe to send to Gemini. Keep the real configuration and all credentials outside the repository, image, command arguments, and captured test output.

Exercise the deployed process against GitLab 17 or newer:

1. Create an eligible merge request before Wormtamer starts and confirm the immediate reconciliation scan discovers its current head.
2. Confirm the worker obtains the diff, invokes Gemini, and publishes exactly one marked note authored by the configured PAT user.
3. Restart the process with the same persistent database and confirm reconciliation creates neither a duplicate job nor a duplicate note.
4. Create an MR through the normal webhook path and confirm it follows the same durable review and publication flow.
5. Create a draft MR, confirm reconciliation skips it as observed, mark it ready, and confirm a later scan creates one review job.
6. Exercise a nested namespace path and GitLab pagination headers used by the reconciler. Use draft entries or a read-only probe when additional pages are needed so pagination validation does not trigger unnecessary Gemini reviews.
7. Repeat the required GitLab operations with progressively narrower PAT scope and project membership until the minimum working combination is established. Do not add startup scope discovery to the application.

Any API mismatch must remain fail-closed and retain existing authorization, response-size, redirect, timeout, logging, and idempotency boundaries. Add an `httptest` regression before changing behavior.

### OCI packaging

Add the smallest multi-stage image build that:

- Builds `cmd/wormtamer` with CGO and the `sqlite_fts5` build tag
- Contains only the runtime libraries and certificates required by the binary
- Runs as a dedicated non-root user
- Contains no deployment configuration, credentials, database, or build cache
- Accepts an explicit external configuration path
- Supports a writable persistent data mount for the SQLite database and WAL files
- Preserves signal delivery to the Wormtamer process for graceful shutdown

Add concise operator documentation with an example invocation and mount layout. State that deployments use one process and one replica, configuration remains outside version control, SQLite and its WAL share persistent storage with working file locks, and transport security is the operator's responsibility when plain HTTP is used.

## Risks and Open Questions

- GitLab minor releases or self-hosted configuration may differ in merge request fields, pagination headers, permissions, or note behavior. Support only verified GitLab 17-or-newer behavior and fail visibly elsewhere.
- The minimum PAT scope may still be broad because publishing notes is a write operation. Record the verified minimum rather than weakening application authorization checks.
- A real smoke test sends bounded project metadata and diffs to Gemini and may publish visible notes. Use only the dedicated test project and remove test artifacts according to operator policy.
- Container base-image and runtime-library choices should favor a small, maintainable build over a static binary or custom SQLite toolchain.
- Backup, restore, TLS termination, and orchestrator-specific deployment remain operator concerns unless validation establishes a concrete missing requirement.

## Verification

- A clean deployment from the OCI image starts from an external configuration file, writes SQLite state to persistent storage, serves `GET /healthcheck`, and shuts down cleanly on the normal container stop signal.
- An MR that predates startup is reviewed and receives one marked note; restarting with the same database does not duplicate the job, Gemini review, or note.
- A webhook-created MR is persisted before acknowledgement and completes through the same review and publication path.
- A draft MR is skipped when observed and is reviewed once after becoming ready during a later scan.
- Nested project paths resolve correctly, and the pagination contract used by reconciliation is confirmed against GitLab 17 or newer.
- The documented PAT scope and project role are the narrowest combination verified to support project lookup, MR metadata and diff reads, current-user lookup, note search, and note creation.
- Compatibility fixes have automated regression coverage and do not expose credentials, response bodies, diffs, or raw external errors in logs.
- Operator documentation is sufficient to reproduce the deployment without embedding secrets or mutable state in the image.
