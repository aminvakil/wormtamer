# Container deployment

Wormtamer runs as one process and one replica. The container image includes the application, its CGO runtime libraries, and public CA certificates. Configuration, credentials, and SQLite state remain outside the image.

## Build

Build the image from the repository root for the local container host architecture:

```sh
docker build -t wormtamer:local .
```

The build context includes only the Go module files and application source selected by `.dockerignore`. A tag such as `v1.2.3` publishes the `linux/amd64` image as `ghcr.io/aminvakil/wormtamer:1.2.3` and `ghcr.io/aminvakil/wormtamer:latest`.

## Configure

Copy `config.example.json` to a location outside the repository and image, then replace every placeholder with the installation's GitLab URL, credentials, Gemini model, and authorized repositories. The example already uses the container listen address and persistent database path:

```sh
cp config.example.json /secure/path/config.json
```

Use a GitLab personal access token with `api` scope whose user has at least the Reporter role on every authorized project. Configure each GitLab project to send merge request webhooks to:

```text
https://wormtamer.example/webhooks/gitlab
```

The webhook secret in GitLab must equal `gitlab.webhook_secret`.

The configuration contains plaintext credentials. Keep it outside version control, restrict host access, and mount it read-only. The process runs as the image's `nobody` user; the mounted file must be readable by that identity. Do not pass credentials through command arguments or bake them into another image.

## Store state

Use one persistent volume for `/var/lib/wormtamer`. The SQLite database, WAL, and shared-memory files must remain together on storage that provides reliable filesystem locking.

A Docker-managed volume is the simplest option:

```sh
docker volume create wormtamer-data
```

The image initializes `/var/lib/wormtamer` for its `nobody` user. If a bind mount is used instead, the operator must make its directory writable by that identity (UID and GID `65534` in the selected runtime image). Never run multiple Wormtamer containers against the volume.

## Run

```sh
docker run --detach \
  --name wormtamer \
  --restart unless-stopped \
  --stop-timeout 20 \
  --publish 8080:8080 \
  --mount type=bind,src=/absolute/path/config.json,dst=/etc/wormtamer/config.json,readonly \
  --mount type=volume,src=wormtamer-data,dst=/var/lib/wormtamer \
  wormtamer:local
```

Replace the configuration source with its absolute host path. Publishing `8080:8080` accepts webhook traffic on the host's network interfaces. Wormtamer itself serves HTTP, so expose it only on a trusted network or place it behind an operator-managed TLS reverse proxy. A reverse proxy running on the same host may instead publish Wormtamer on loopback.

The executable is the image entrypoint, so `SIGTERM` reaches it directly. Keep the stop timeout at 20 seconds or longer; the process uses a bounded ten-second graceful-shutdown period.

## Operate

Probe liveness from outside the container:

```sh
curl --fail --silent --show-error http://127.0.0.1:8080/healthcheck
```

A successful response confirms that startup completed and the HTTP server is live. It does not check GitLab, Gemini, or queued jobs. Use container logs for operational failures:

```sh
docker logs wormtamer
```

Restarting the container with the same volume preserves queued work and publication records. Stop the existing container before starting a replacement so only one replica accesses SQLite.

## Back up and restore

The operator owns backup and restore. For a filesystem-level backup, stop Wormtamer and back up the entire persistent volume so the database and any WAL state stay together. An online backup must use a SQLite-aware backup mechanism; copying only the live database file is unsafe.

Restore only while Wormtamer is stopped, restore the complete persistent state to storage writable by the image's `nobody` user, and then start one container with that volume. Protect backups as sensitive data because stored webhook payloads may contain private repository metadata.
