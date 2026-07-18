# Package a GitLab 17 deployment

Status: proposed

## Goal

Produce a minimal OCI image and operator documentation for the validated Wormtamer process.

## Scope

- Build `cmd/wormtamer` with CGO in a multi-stage image.
- Include only required runtime libraries and CA certificates, and run as a dedicated non-root user.
- Keep configuration, credentials, SQLite state, and build caches outside the image.
- Document external configuration, persistent SQLite and WAL storage, one-replica operation, health checking, startup, graceful shutdown, and operator-owned TLS termination.

Do not add orchestrator-specific manifests, registry automation, another database or queue, or repository-controlled code execution.

## Approach

Use an explicit external configuration path and a writable persistent data mount. Preserve direct signal delivery to Wormtamer. Prefer a small maintained runtime base over a static binary or custom SQLite toolchain.

## Risks and Open Questions

- The runtime image must contain the dynamic libraries required by the CGO SQLite binary.
- SQLite, its WAL, and shared-memory files require persistent storage with working file locks.
- Backup, restore, and TLS termination remain operator responsibilities.

## Verification

- A clean image build produces a working binary and contains no configuration, credentials, database, or build cache.
- The container runs as non-root, starts from a mounted configuration, writes SQLite state to persistent storage, and serves `GET /healthcheck`.
- Normal container stop reaches Wormtamer and completes graceful shutdown; restart with the same storage preserves idempotency.
- Operator documentation is sufficient to reproduce a one-process, one-replica deployment behind an optional TLS reverse proxy.

## Deferred Validation

Before closing this plan, use more than 100 safe open merge requests in a sample GitLab 17-or-newer project to confirm reconciler pagination beyond the first page without duplicate jobs. This is a manual release check, not a separate implementation project.
