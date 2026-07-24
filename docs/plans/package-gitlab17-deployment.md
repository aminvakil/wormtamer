# Package a GitLab 17 deployment

Status: proposed

## Goal

Produce a minimal OCI image and operator documentation for the validated Wormtamer process.

## Scope

- Build `cmd/wormtamer` with CGO in a multi-stage image using a restricted build context and narrowly scoped copies.
- Include only required runtime libraries and CA certificates, and run as a dedicated non-root user.
- Keep configuration, credentials, SQLite state, local artifacts, and build caches outside every image stage.
- Document read-only external configuration, persistent SQLite and WAL storage, one-replica operation, health checking, startup, graceful shutdown, and operator-owned TLS termination.

Do not add orchestrator-specific manifests, registry automation, another database or queue, or repository-controlled code execution.

## Approach

Use a restrictive `.dockerignore` and copy only the source and module files required to build Wormtamer. Use compatible maintained builder and runtime bases rather than a static binary or custom SQLite toolchain.

Run Wormtamer directly as the image entrypoint under a dedicated numeric UID and GID. Use `/etc/wormtamer/config.json` as the read-only configuration path and `/var/lib/wormtamer` as the writable persistent data directory. The documented configuration uses the absolute database path `/var/lib/wormtamer/wormtamer.db` so the database, WAL, and shared-memory files remain on the same mount, and listens on `0.0.0.0` so published container ports are reachable.

Keep the root filesystem read-only apart from the persistent data mount. Preserve direct `SIGTERM` delivery and document a container stop grace period longer than Wormtamer's ten-second shutdown deadline. Health probes query `GET /healthcheck` from outside the image rather than adding an HTTP client solely for an image-local probe.

## Risks and Open Questions

- The runtime image must contain the dynamic libraries required by the CGO SQLite binary; compatible builder and runtime bases reduce, but do not replace, verification of the final binary.
- SQLite, its WAL, and shared-memory files require one persistent directory with working file locks and ownership writable by the image UID.
- Configuration contains plaintext credentials and must be mounted read-only with restrictive operator-managed permissions.
- Backup, restore, and TLS termination remain operator responsibilities.

## Verification

- The build context excludes `.git`, ignored local artifacts, configuration, credentials, databases, and build outputs; image stages copy only required tracked inputs.
- A clean image build produces a working binary whose dynamic dependencies and CA trust are present in the runtime image, without configuration, credentials, SQLite state, or build cache in the final image.
- The container runs as its dedicated non-root UID with a read-only root filesystem, starts from the read-only mounted configuration, and writes the database, WAL, and shared-memory files only under the persistent data mount.
- With the documented listen address and port publication, an external probe receives success from `GET /healthcheck`.
- An exec-form entrypoint receives `SIGTERM`, and a container stop grace period of at least 20 seconds allows Wormtamer to complete its bounded graceful shutdown.
- Restarting with the same persistent data directory preserves durable state and publication idempotency.
- Operator documentation is sufficient to reproduce a one-process, one-replica deployment behind an optional TLS reverse proxy and explains configuration permissions, volume ownership and locking, health checking, startup, shutdown, backup responsibility, and restore responsibility.
