# Automate dependency maintenance and vulnerability scanning

Status: proposed

## Goal

Make Go module and container base-image updates visible as reviewed pull requests, and reject changes with known reachable Go vulnerabilities in CI.

## Scope

- Extend Dependabot from GitHub Actions to the repository's Go module and Dockerfile dependencies.
- Track `golang.org/x/vuln/cmd/govulncheck` as a versioned Go tool and run it against `./...` in CI.
- Pin both Dockerfile base images by digest so Docker dependency changes are explicit and reviewable.

Keep dependency updates as ordinary pull requests subject to existing CI. Do not add automatic merging, automatic releases, a third-party vulnerability service, SARIF publishing, scheduled image rebuilding, or broad supply-chain policy beyond these dependencies and the vulnerability check.

## Approach

- Add `gomod` and `docker` entries for `/` to `.github/dependabot.yml`, on a weekly schedule. Retain the existing GitHub Actions entry and avoid grouping unrelated updates until update volume demonstrates a need.
- Add `govulncheck` through Go's `tool` directive with an exact released `golang.org/x/vuln` version. Invoke it as `go tool govulncheck ./...` so local and CI execution use the version recorded in `go.mod` and `go.sum`, and Go-module Dependabot can propose scanner updates.
- Add a separate CI job using the Go version from `go.mod`. Run `govulncheck` in its default text mode so findings that affect reachable code fail the job; do not use JSON or SARIF modes that can report findings with a successful exit status.
- Pin `golang:1.26-trixie` and `debian:trixie-slim` to their current manifest digests while preserving the readable tags. Dependabot's Docker ecosystem then proposes digest or supported tag changes instead of allowing mutable base-image changes to enter unrelated builds silently.
- Keep the existing Docker build and healthcheck job as the acceptance check for base-image updates.

The vulnerability database is necessarily fetched during CI. A database or network outage should fail the dedicated job visibly rather than silently skipping the scan.

## Verification

- Dependabot recognizes the Go module, Dockerfile, and existing GitHub Actions ecosystems and can propose updates for each independently.
- `go tool govulncheck ./...` resolves the committed scanner version and succeeds when no reachable known vulnerability is reported.
- A controlled fixture or documented test invocation with a reachable known-vulnerable Go call causes the vulnerability job to fail; informational vulnerabilities outside reachable code do not create a false successful scan.
- Both digest-pinned base images build successfully and the resulting container passes the existing healthcheck and shutdown test.
- Dependency pull requests receive the same tests and Docker build checks as other changes and are never merged or released automatically.
