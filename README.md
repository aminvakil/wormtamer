# The simplest self-hosted AI reviewer for GitLab

**Wormtamer** reviews GitLab merge requests with Gemini and publishes one concise summary with actionable findings. It runs as a single Go process, stores its state in SQLite, and needs no external database, queue, or agent framework.

## What it does

- Accepts authenticated GitLab merge request webhooks
- Reconciles open merge requests periodically so missed webhooks do not lose reviews
- Sends bounded merge request metadata and changed-file diffs to Gemini
- Validates model output before publishing it
- Posts one idempotent review note for each merge request revision
- Persists webhook, job, result, and publication state in SQLite

Wormtamer supports GitLab 17 and newer. Each deployment serves one team and runs as one process and one replica.

## Deliberately small

Wormtamer does not require PostgreSQL, Redis, a queue service, multiple workers, or a control plane. It does not clone repositories or execute repository-controlled code. The current reviewer uses only the merge request metadata and diffs fetched through the GitLab API.

## Requirements

- A GitLab personal access token with `api` scope and at least the Reporter role on each configured project
- A GitLab webhook secret
- A Gemini Developer API key and model name
- Docker for the recommended deployment, or Go 1.26 with CGO and a C compiler for local builds

## Quick start

### 1. Configure

Copy `config.example.json` and replace its placeholders.

    cp config.example.json config.json

### 2. Create persistent storage

    mkdir -p wormtamer_db
    sudo chown 65534:65534 wormtamer_db

### 3. Run

    docker run --detach \
      --name wormtamer \
      --restart unless-stopped \
      --stop-timeout 20 \
      --publish 8080:8080 \
      --volume ./config.json:/etc/wormtamer/config.json:ro \
      --volume ./wormtamer_db/:/var/lib/wormtamer/ \
      ghcr.io/aminvakil/wormtamer:latest

### 4. Add the webhook

Configure GitLab to send merge request webhooks to:

    https://wormtamer.example/webhooks/gitlab

See [Container deployment](docs/deployment.md) for configuration permissions, TLS termination, health checking, shutdown, persistence, backup, and restore guidance.

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
