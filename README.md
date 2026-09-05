# The simplest self-hosted AI reviewer for GitLab

**Wormtamer** reviews GitLab merge requests with Gemini and publishes one concise summary with prioritized, actionable findings. It runs as a single Go process, stores its state in SQLite, and needs no external database, queue, or agent framework.

> **Development status:** Wormtamer is under active development. Behavior, configuration, and persisted state may change between releases, including breaking changes.

## What it does

- Accepts authenticated GitLab merge request webhooks
- Reconciles open merge requests periodically so missed webhooks do not lose reviews
- Delays new revisions by a configurable [grace period](docs/agents/reliability.md#review-grace-period) to avoid reviewing quickly superseded heads
- Starts reviews with bounded merge request metadata and changed-file diffs
- Prepares disposable Git working directories for the current repository and, when enabled, every other authorized repository
- Gives Gemini Pi-style `read` and unrestricted `bash` tools under a credential-free review identity
- After an MR closes or merges, lets Gemini derive at most one repository-scoped advisory lesson from its diff, comments, and Wormtamer review
- Validates model output before posting an idempotent review note, while retained SQLite state suppresses another review and note for a rebased head with the same GitLab patch ID
- Persists webhook, job, patch-equivalence, result, publication, feedback, and advisory-memory state in SQLite
- Shows persisted workflow and runtime-memory state in a built-in read-only web panel

Wormtamer supports GitLab 17 and newer. Each deployment serves one team and runs as one process and one replica.

Patch equivalence uses GitLab's whitespace-insensitive patch IDs. It can therefore suppress a whitespace-only update or reuse a review after target-branch context changes. Exact head SHA remains the review and finding identity. Equivalence is retained only in SQLite: after database loss, an unchanged head can still be recovered from its marked note, but a rebased head is reviewed and published again.

## Deliberately small

Wormtamer does not require PostgreSQL, Redis, a queue service, multiple workers, or a control plane. Reviews run in disposable Git working directories with ordinary local `read` and `bash` capabilities. Credentials and SQLite state remain inaccessible to the dedicated review-tool identity; final validation and publication remain application-owned.

Cross-repository preparation is all-or-nothing: reviews receive either no related private repositories or every other authorized repository. Runtime memory remains untrusted, repository-scoped advice. See the [security model](docs/agents/security.md) for the complete trust and authorization boundaries.

## Requirements

- A GitLab personal access token with `api` scope and at least the Reporter role on every authorized project
- A GitLab webhook secret
- A Gemini Developer API key, or a Gemini Developer API-compatible endpoint and API key, with a Gemini 3 or newer model name
- Docker for the recommended deployment, or Go 1.27 with CGO and a C compiler for local builds

## Quick start

### 1. Configure

Copy `config.example.json`, replace its placeholders, and set the authorized repositories. Leave `share_all_authorized_repositories` disabled unless every authorized repository has the same review audience.

    cp config.example.json config.json

### 2. Create persistent storage

Create separate volumes for persistent SQLite state and disposable review workspaces:

    docker volume create wormtamer-data
    docker volume create wormtamer-workspaces

### 3. Run

    docker run --detach \
      --name wormtamer \
      --restart unless-stopped \
      --stop-timeout 20 \
      --publish 8080:8080 \
      --volume ./config.json:/etc/wormtamer/config.json:ro \
      --mount type=volume,src=wormtamer-data,dst=/var/lib/wormtamer \
      --mount type=volume,src=wormtamer-workspaces,dst=/var/lib/wormtamer-reviews \
      ghcr.io/aminvakil/wormtamer:latest

### 4. Add the webhook

Configure each authorized GitLab project to send merge request webhooks to:

    https://wormtamer.example/webhooks/gitlab

Close and merge events let Wormtamer evaluate the terminal diff, current comments, and its locally persisted review, then retain one concise advisory lesson when the combined evidence warrants memory.

### 5. View the panel

Open `https://wormtamer.example/` to inspect persisted review activity, findings, feedback processing, runtime memory, and non-secret effective configuration. The panel is read-only and does not probe external services while rendering. Panel access and request limiting are deployment concerns described in [Container deployment](docs/deployment.md); content-bearing model diagnostics are available only through stderr at debug log level.

See [Container deployment](docs/deployment.md) for complete configuration, webhook, permissions, TLS termination, health checking, shutdown, persistence, backup, and restore guidance.

## Development

    make test
    make test-race
    make build

The executable is written to `bin/wormtamer` and requires an explicit configuration path:

    ./bin/wormtamer -config /path/to/config.json

## Documentation

- [Architecture](docs/agents/architecture.md)
- [Reliability guarantees](docs/agents/reliability.md)
- [Security model](docs/agents/security.md)
- [Container deployment](docs/deployment.md)

## License

Wormtamer is licensed under the [GNU General Public License v3.0](LICENSE).
