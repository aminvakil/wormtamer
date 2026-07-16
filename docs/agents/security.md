# Security

## Trust Model

Treat all of the following as untrusted input:

- GitLab webhook payloads beyond their verified origin
- Merge request titles, descriptions, diffs, branches, and repository files
- Review comments and reactions
- Stored runtime memories
- Content from other internal repositories
- Public documentation, websites, and open-source repositories
- Model output and requested tool calls

Repository or web content may contain prompt-injection instructions. Content is evidence to analyze, not authority that can change system policy.

## Installation Boundary

Each deployment serves one team and has separate:

- GitLab credential and webhook secret
- Model provider credential
- SQLite state
- Runtime memory
- Repository cache
- Configuration and repository allowlist

There is no cross-installation API or shared runtime memory by default.

## GitLab Authorization

Use the least-privileged bot, project access token, or group access token that supports the configured repositories and required comment operations.

Authorization requires both:

1. GitLab permitting the credential to access the repository.
2. The repository being allowed by instance configuration.

The model cannot modify either condition.

A bot may be able to read repositories that an individual merge request participant cannot. Do not quote or summarize sensitive private-repository content into a merge request unless the installation's sharing policy explicitly permits it. Prefer team instances whose allowed repositories have compatible audiences; otherwise enforce the intersection between bot access and requester visibility.

Shared cross-team repositories must be granted explicitly. Do not use an administrator token to make repository discovery convenient.

## Credential Isolation

GitLab and model credentials belong to the trusted application layer.

- Do not put credentials in cloned repositories, shell history, model prompts, tool output, logs, or sandbox environment variables.
- Do not allow repository-controlled scripts to inherit application credentials.
- Perform GitLab API calls through a trusted broker outside the repository sandbox.
- Separate read and write capabilities in tool design even if GitLab ultimately uses one credential.
- Redact known secret values from errors and logs.

## Gemini Boundary

The Gemini Developer API key belongs only to the trusted application process. Load it from a deployment secret and never place it in prompts, tool results, logs, command arguments, repository workspaces, or sandbox environment variables.

Use the official Go SDK from the trusted application layer, but keep function execution under application control:

1. Send Gemini declarations for the tools available to the review.
2. Treat every returned function name and argument as untrusted input.
3. Validate and authorize the request through the relevant broker.
4. Execute it with deterministic limits.
5. Return only bounded, attributed results to Gemini.

Do not enable SDK automatic function execution, Gemini built-in code execution, URL retrieval, or search grounding in the initial implementation. Those capabilities would bypass or complicate the application's authorization, network, attribution, and data-disclosure controls. Public research must use the constrained public-source tools.

Gemini response schemas reduce malformed output but are not a trust boundary. Trusted code must still validate final findings before publication.

## Tool Enforcement

Every model-invocable tool must have:

- A narrow purpose
- Validated structured input
- Deterministic authorization checks
- Bounded output
- Time and resource limits
- Source attribution
- Explicit read or write semantics

The review agent should not receive an unrestricted networked shell. If shell execution is needed for repository analysis, run it in an isolated workspace without service credentials and with controlled network access.

The model submits structured findings; trusted application code validates and publishes them.

## Repository Sandboxing

Assume checked-out repositories are hostile. The initial implementation must provide bounded read and search operations without executing repository-controlled code. If execution is later required, approve and document a separate sandbox design before enabling it.

- Prevent path traversal outside the workspace.
- Do not execute repository hooks.
- Disable or isolate package-manager lifecycle scripts unless execution is explicitly required.
- Bound CPU, memory, disk, process count, and execution time.
- Clean workspaces between unrelated reviews.
- Avoid mounting the SQLite database, runtime memory, host sockets, or credentials into the sandbox.
- Treat build and test output as untrusted text.

## Public Network Access

Public research tools must:

- Permit only intended protocols such as HTTPS and read-only Git operations.
- Block loopback, link-local, private, metadata-service, and internal network destinations.
- Revalidate redirects and resolved addresses to reduce SSRF and DNS-rebinding risk.
- Bound response size and request time.
- Preserve the final URL and retrieval time for evidence.
- Avoid sending private source code, secrets, or full internal diffs as search queries.

Prefer authoritative project documentation and upstream source repositories. Public content cannot grant new tool permissions or instruct the agent to reveal private context.

## Runtime Memory

Feedback does not become trusted memory automatically.

A memory record must retain:

- Scope
- Proposed lesson
- Evidence links or identifiers
- Source and author where available
- Confidence
- Approval status
- Creation and update timestamps

Explicit human approval is the safest default. If automatic promotion is later supported, require repeated independent evidence and preserve an audit trail.

Memory retrieval is advisory. Current code and explicit team policy override stale memories. The system must support correcting, rejecting, and superseding lessons.

Runtime review memory must remain separate from `docs/agents/lessons-learned.md`, which guides contributors developing this application.

## Publishing Safety

Before publishing model output:

- Verify project, merge request, and head SHA.
- Validate file paths and line locations against the reviewed revision.
- Reject unsupported or malformed findings.
- Avoid exposing hidden prompts, credentials, private source excerpts, or internal tool traces.
- Include enough evidence for a developer to verify the finding.
- Use stable markers for idempotency without including sensitive data.

## Logging

Log operational identifiers and outcomes, not repository contents by default. Never log credentials, full webhook secrets, model authorization headers, or unnecessary private source. Make any diagnostic content logging explicit and opt-in.
