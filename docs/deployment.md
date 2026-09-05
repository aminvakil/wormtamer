# Container deployment

Wormtamer runs as one process and one replica. The container image includes the application, its CGO runtime libraries, public CA certificates, Bash, Git, ripgrep (`rg`), fd (`fd`), and curl. Configuration, credentials, SQLite state, and disposable review workspaces remain outside the image.

## Image

Pull the published image from GitHub Container Registry:

```sh
docker pull ghcr.io/aminvakil/wormtamer:latest
```

Each release updates `ghcr.io/aminvakil/wormtamer:latest`. Verify the model's expected commands in an image with:

```sh
docker run --rm --entrypoint /bin/bash ghcr.io/aminvakil/wormtamer:latest \
  -c 'command -v bash git rg fd curl'
```

## Configure

Copy `config.example.json` to a location outside the repository and image, then replace every placeholder with the installation's GitLab URL, credentials, Gemini model, and authorized repositories. `log_level` accepts `debug`, `info`, `warn`, or `error` and defaults to `info` if omitted. Wormtamer does not support Gemini models earlier than version 3; see the project [compatibility baseline](agents/architecture.md#compatibility-baseline). The example already uses the container listen address and persistent database path:

```sh
cp config.example.json /secure/path/config.json
```

Leave `gemini.base_url` empty or omit it to use the Gemini Developer API directly. Set it to an HTTP or HTTPS Gemini Developer API-compatible base URL to use a custom endpoint for both reviews and feedback evaluation. Wormtamer appends `/v1beta/models/<model>:generateContent` and sends `gemini.api_key` in the `x-goog-api-key` header.

A custom endpoint must accept and return the native Gemini Developer API behavior Wormtamer uses, including function calling, structured JSON response schemas, and thinking configuration. An OpenAI-compatible endpoint serving a Gemini model is not sufficient. The endpoint receives private merge request content and tool results and must serve the configured Gemini 3 or newer model. Wormtamer rejects model endpoint redirects and base URLs containing credentials, queries, or fragments. Use HTTPS unless the endpoint is confined to an appropriate local network.

Set `grace_period` to control the initial delay before reviewing each newly observed MR revision; see [Review grace period](agents/reliability.md#review-grace-period) for its format, default, and supersession behavior.

Use a GitLab personal access token with `api` scope whose user has at least the Reporter role on every authorized project.

`share_all_authorized_repositories` defaults to `false`, which prepares only the repository under review. Enabling it makes every other authorized repository available as related context in every review; the current repository is never duplicated as related context. **Enable it only when every person able to view merge requests in any authorized repository may receive information derived from every other authorized repository.** Keep it disabled when repository audiences differ. The setting does not authorize unlisted repositories or request eager or exhaustive model inspection.

`review_workspace_path` identifies disposable storage exposed only to the credential-free review-tool identity; the example uses `/var/lib/wormtamer-reviews`. Keep it outside the configuration and SQLite directories. Model-directed Bash has ordinary network access and can use curl without an application domain allowlist. The accepted trust boundary is documented under [Local review agent](agents/security.md#local-review-agent).

Configure each authorized GitLab project to send merge request webhooks to:

```text
https://wormtamer.example/webhooks/gitlab
```

The webhook secret in GitLab must equal `gitlab.webhook_secret`. Close and merge events let Wormtamer evaluate the terminal diff, current comments, and its locally persisted review. Note Hook deliveries are ignored.

The configuration contains plaintext credentials. Keep it outside version control, set it to mode `0600` or stricter, restrict its parent directory, and mount it read-only. The service process runs as root only so it can own private state and launch tools as fixed UID/GID `65532`; the model never receives a root process. Startup fails if the review-tool identity can access the configuration or SQLite paths. Do not pass credentials through command arguments or bake them into another image.

## Store state and disposable workspaces

Use one persistent volume for `/var/lib/wormtamer`. The SQLite database, WAL, and shared-memory files must remain together on storage that provides reliable filesystem locking. Use a separate disposable volume or filesystem for `/var/lib/wormtamer-reviews` so review activity and disk exhaustion cannot starve persistent SQLite storage.

Docker-managed volumes are the simplest option:

```sh
docker volume create wormtamer-data
docker volume create wormtamer-workspaces
```

The image initializes the state directory as root-owned mode `0700` and the review-workspace parent as root-owned mode `0711`; individual exposed review roots become owned by UID/GID `65532`. If bind mounts are used, preserve those ownership and traversal properties. Never run multiple Wormtamer containers against either volume.

Size and monitor disposable capacity for complete Git histories of the current and every prepared related repository, bounded command-output spools, model-created Git worktrees, and arbitrary files created or copied by Bash. Wormtamer intentionally has no workspace-size quota. Filesystem allocation or write failure fails the active setup or tool call; it does not trigger a cleanup-and-continue fallback.

## Run

```sh
docker run --detach \
  --name wormtamer \
  --restart unless-stopped \
  --stop-timeout 20 \
  --publish 8080:8080 \
  --mount type=bind,src=/absolute/path/config.json,dst=/etc/wormtamer/config.json,readonly \
  --mount type=volume,src=wormtamer-data,dst=/var/lib/wormtamer \
  --mount type=volume,src=wormtamer-workspaces,dst=/var/lib/wormtamer-reviews \
  ghcr.io/aminvakil/wormtamer:latest
```

Replace the configuration source with its absolute host path. Publishing `8080:8080` accepts webhook traffic on the host's network interfaces. Wormtamer itself serves HTTP, so expose it only on a trusted network or place it behind an operator-managed TLS reverse proxy. A reverse proxy running on the same host may instead publish Wormtamer on loopback.

The minimal panel does not authenticate requests or impose panel-specific request and concurrency limits. Configure the reverse proxy to restrict and limit the panel routes as required for the deployment. The webhook endpoint retains its separate application-owned secret verification and ingress limit; see the [panel security boundary](agents/security.md#read-only-web-panel).

The executable is the image entrypoint, so `SIGTERM` reaches it directly. Keep the stop timeout at 20 seconds or longer; the process uses a bounded ten-second graceful-shutdown period.

## Operate

Probe liveness from outside the container:

```sh
curl --fail --silent --show-error http://127.0.0.1:8080/healthcheck
```

A successful response confirms that startup completed and the HTTP server is live. It does not check GitLab, Gemini, or queued jobs.

Open `http://127.0.0.1:8080/` to view the built-in read-only panel. It shows current persisted job counts, review history and findings, terminal feedback processing, runtime memory, and non-secret effective configuration. Review, feedback, and memory history use bounded pagination. The panel reflects local SQLite state and does not probe GitLab, Gemini, repositories, files, containers, or logging services while rendering.

The panel also exposes `/reviews`, `/feedback`, and `/memory`, with review and feedback detail routes; it has no controls or request methods for changing application state or logging. Review summaries and memory lessons may contain private project information.

Use container logs for operational failures:

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

For model diagnostics, set `"log_level": "debug"` and restart the container. Structured stderr events include the complete model system instruction and prompt, each requested tool and its arguments, each admitted bounded tool result, the validated final review response, and the validated memory decision. These logs can contain private merge request data, repository and command output, comments, memory, model output, and secrets unknown to Wormtamer. Restrict container-log access and retention, and restore `"log_level": "info"` after diagnosis. Wormtamer replaces a complete diagnostic value when it contains a configured GitLab or Gemini credential, including a JSON-escaped form. At the default `info` level, this content is not logged.

Restarting the container with the same volume preserves workflow and runtime-memory state. Before workers start, Wormtamer requeues interrupted running review and feedback jobs that have attempts remaining and fails interrupted jobs whose fifth claim was exhausted. A review with a persisted validated result resumes publication without another Gemini review; other interrupted work may repeat. Stop the existing container before starting a replacement so only one replica accesses SQLite.

## Back up and restore

The operator owns backup and restore. For a filesystem-level backup, stop Wormtamer and back up the entire persistent state volume so the database and any WAL state stay together. The disposable review-workspace volume is excluded and may be cleared while Wormtamer is stopped. An online backup must use a SQLite-aware backup mechanism; copying only the live database file is unsafe.

Restore only while Wormtamer is stopped, restore the complete persistent state to root-owned mode-`0700` storage, and then start one container with that volume. Protect backups as sensitive data because stored webhook payloads, review results, and memory lessons may contain private project information.
