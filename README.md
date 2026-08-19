# The simplest self-hosted AI reviewer for GitLab

**Wormtamer** reviews GitLab merge requests with Gemini and publishes one concise summary with prioritized, actionable findings. It runs as a single Go process, stores its state in SQLite, and needs no external database, queue, or agent framework.

> **Development status:** Wormtamer is under active development. Behavior, configuration, and persisted state may change between releases, including breaking changes.

## What it does

- Accepts authenticated GitLab merge request webhooks
- Reconciles open merge requests periodically so missed webhooks do not lose reviews
- Starts reviews with bounded merge request metadata and changed-file diffs
- Lets Gemini inspect bounded text snapshots from the current repository and explicitly shared related repositories
- After an MR closes or merges, lets Gemini derive at most one repository-scoped advisory lesson from its diff, comments, and Wormtamer review
- Lets Gemini retrieve bounded public text from approved domains and exact configured GitHub repositories
- Validates model output before posting an idempotent review note, while retained SQLite state suppresses another review and note for a rebased head with the same GitLab patch ID
- Persists webhook, job, patch-equivalence, result, publication, feedback, advisory-memory, and model-usage state in SQLite
- Shows persisted workflow state and bounded current-process conversations and logs in a built-in read-only web panel

Wormtamer supports GitLab 17 and newer. Each deployment serves one team and runs as one process and one replica.

Patch equivalence uses GitLab's whitespace-insensitive patch IDs. It can therefore suppress a whitespace-only update or reuse a review after target-branch context changes. Exact head SHA remains the review and finding identity. Equivalence is retained only in SQLite: after database loss, an unchanged head can still be recovered from its marked note, but a rebased head is reviewed and published again.

## Deliberately small

Wormtamer does not require PostgreSQL, Redis, a queue service, multiple workers, or a control plane. Repository, memory, and public-source access is read-only, bounded, and authorized by application code rather than model instructions. Repository snapshots are disposable, and Wormtamer never executes repository-controlled code.

Cross-repository access requires an explicit directional sharing rule. Runtime memory remains untrusted, repository-scoped advice, and public research is limited to configured sources. See the [security model](docs/agents/security.md) for the complete trust and authorization boundaries.

## Requirements

- A GitLab personal access token with `api` scope and at least the Reporter role on every authorized project
- A GitLab webhook secret
- A Gemini Developer API key, or a Gemini Developer API-compatible endpoint and API key, with a Gemini 3 or newer model name
- Docker for the recommended deployment, or Go 1.26 with CGO and a C compiler for local builds

## Quick start

### 1. Configure

Copy `config.example.json`, replace its placeholders, and set the authorized repositories. Optional directional sharing rules and public-source allowlists control which related or public content reviews may inspect.

    cp config.example.json config.json

### 2. Create persistent storage

Create a persistent Docker volume:

    docker volume create wormtamer-data

### 3. Run

    docker run --detach \
      --name wormtamer \
      --restart unless-stopped \
      --stop-timeout 20 \
      --publish 8080:8080 \
      --volume ./config.json:/etc/wormtamer/config.json:ro \
      --mount type=volume,src=wormtamer-data,dst=/var/lib/wormtamer \
      ghcr.io/aminvakil/wormtamer:latest

### 4. Add the webhook

Configure each authorized GitLab project to send merge request webhooks to:

    https://wormtamer.example/webhooks/gitlab

Close and merge events let Wormtamer evaluate the terminal diff, current comments, and its locally persisted review, then retain one concise advisory lesson when the combined evidence warrants memory.

### 5. View the panel

Open `https://wormtamer.example/` to inspect persisted review activity, findings, feedback processing, runtime memory, application-observed model usage, non-secret effective configuration, and bounded current-process diagnostics. Conversation content is available only at debug log level and may contain private source or unknown secrets. The panel is read-only and does not probe external services while rendering. Panel access and request limiting are deployment concerns described in [Container deployment](docs/deployment.md).

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
