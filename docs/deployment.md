# Container deployment

Wormtamer runs as one process and one replica. The container image includes the application, its CGO runtime libraries, and public CA certificates. Configuration, credentials, and SQLite state remain outside the image.

## Image

Pull the published image from GitHub Container Registry:

```sh
docker pull ghcr.io/aminvakil/wormtamer:latest
```

Each release updates `ghcr.io/aminvakil/wormtamer:latest`.

## Configure

Copy `config.example.json` to a location outside the repository and image, then replace every placeholder with the installation's GitLab URL, credentials, Gemini model, and authorized repositories. `log_level` accepts `debug`, `info`, `warn`, or `error` and defaults to `info` if omitted. Wormtamer does not support Gemini models earlier than version 3; see the project [compatibility baseline](agents/architecture.md#compatibility-baseline). The example already uses the container listen address and persistent database path:

```sh
cp config.example.json /secure/path/config.json
```

Leave `gemini.base_url` empty or omit it to use the Gemini Developer API directly. Set it to an HTTP or HTTPS Gemini Developer API-compatible base URL to use a custom endpoint for both reviews and feedback evaluation. Wormtamer appends `/v1beta/models/<model>:generateContent` and sends `gemini.api_key` in the `x-goog-api-key` header.

A custom endpoint must accept and return the native Gemini Developer API behavior Wormtamer uses, including function calling, structured JSON response schemas, and thinking configuration. An OpenAI-compatible endpoint serving a Gemini model is not sufficient. The endpoint receives private merge request content and tool results and must serve the configured Gemini 3 or newer model. Wormtamer rejects model endpoint redirects and base URLs containing credentials, queries, or fragments. Use HTTPS unless the endpoint is confined to an appropriate local network.

Use a GitLab personal access token with `api` scope whose user has at least the Reporter role on every authorized project. `repository_sharing` is directional: each key is a repository under review and its values are related repositories the reviewer may inspect. Configure a rule only when every audience able to view the reviewed repository's merge requests may receive information derived from each related repository; the configuration is the operator's explicit assertion of that sharing policy.

`share_all_authorized_repositories` defaults to `false`. Enabling it makes every other authorized repository available as related context in every review and cannot be combined with a non-empty `repository_sharing` map. **Enable it only when every person able to view merge requests in any authorized repository may receive information derived from every other authorized repository.** Keep it disabled and use directional rules when repository audiences differ. The setting does not authorize unlisted repositories or eagerly download related repositories.

`public_sources.allowed_domains` must include `github.com`. Each entry authorizes bounded model-directed HTTPS retrieval from that exact domain and its subdomains; for example, `syncthing.net` also permits `docs.syncthing.net`. `public_sources.github_repositories` lists exact public GitHub repositories as `<owner>/<repository>` slugs, such as `nginx/nginx`, that the model may inspect through bounded snapshot file tools. These public sources are available to every review, so add only domains to which the team permits bounded request paths to be disclosed. Public access is unauthenticated, ignores environment proxy settings, and remains subject to GitHub's public rate limits. Deployment-level egress filtering is recommended in addition to application checks.

Configure each authorized GitLab project to send both merge request and comment webhooks to:

```text
https://wormtamer.example/webhooks/gitlab
```

The webhook secret in GitLab must equal `gitlab.webhook_secret`. Comment events let Wormtamer evaluate new and edited merge request comments after findings exist; internal and system notes are ignored.

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
  ghcr.io/aminvakil/wormtamer:latest
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

Inspect terminal failures and, after correcting their cause, retry them through a short-lived command in the running container:

```sh
docker exec wormtamer wormtamer -config /etc/wormtamer/config.json jobs list-failed
docker exec wormtamer wormtamer -config /etc/wormtamer/config.json jobs retry review 17
docker exec wormtamer wormtamer -config /etc/wormtamer/config.json jobs retry feedback 23
```

`list-failed` writes bounded JSON for at most 100 newest failed jobs and reports whether more exist. A retry succeeds only while that exact job remains failed; it resets the five-attempt budget and makes the job immediately eligible. Correct credentials, authorization, configuration, or other permanent causes before retrying. A review that already has a validated result resumes publication without another Gemini review.

These commands open the same SQLite volume briefly but do not start an HTTP listener, worker, reconciler, or external client. They are safe alongside the one running replica because retry is a conditional transaction; they are not a way to start a second service replica. Their output contains operational identifiers and error categories, not stored error messages, webhook payloads, review results, comments, or memory lessons.

For model diagnostics, set `"log_level": "debug"` and restart the container. Debug events include the complete model system instruction and prompt, each requested tool and its arguments, each validated tool result, and the validated final model response. Review tool-call arguments show when Gemini chooses a directionally shared repository. These logs can contain private merge request data, repository content, comments, and secrets unknown to Wormtamer. Restrict access and retention, and restore `"log_level": "info"` after diagnosis. Wormtamer replaces any diagnostic value containing a configured GitLab or Gemini credential rather than logging it.

Restarting the container with the same volume preserves all SQLite state, including feedback and runtime memory. Stop the existing container before starting a replacement so only one replica accesses SQLite.

## Back up and restore

The operator owns backup and restore. For a filesystem-level backup, stop Wormtamer and back up the entire persistent volume so the database and any WAL state stay together. An online backup must use a SQLite-aware backup mechanism; copying only the live database file is unsafe.

Restore only while Wormtamer is stopped, restore the complete persistent state to storage writable by the image's `nobody` user, and then start one container with that volume. Protect backups as sensitive data because stored webhook payloads may contain private repository metadata.
