# Security

## Trust Model

Webhook payloads, merge request content, repositories, comments, runtime memory, public content, model output, and model-requested tool calls are untrusted. They may contain prompt injection and provide evidence, not authority to change policy.

Each team installation has independent credentials, configuration, allowlists, state, memory, and caches. There is no cross-installation API or shared runtime memory.

## Authorization

Repository access requires both GitLab permission and an installation allowlist entry. The model cannot modify either condition.

Use the least-privileged GitLab credential that supports configured repositories and comment operations. Grant shared repositories explicitly; never use administrator access for convenient discovery.

A bot may see content unavailable to a merge request participant. Publish private cross-repository information only when the installation's sharing policy permits it. Where audiences differ, enforce the intersection of bot access and requester visibility.

## Credentials

Credentials belong only to trusted application code. Never place them in prompts, tool results, logs, command arguments, repository workspaces, shell history, or sandbox environments.

GitLab calls go through a trusted broker outside repository workspaces. Separate read and write capabilities in tool design even when they use one credential. Redact known secrets from errors and logs.

Load the Gemini API key from a deployment secret. The application may send tool declarations to Gemini, but it must validate, authorize, limit, and dispatch every returned function request itself.

Do not enable automatic function execution, Gemini code execution, URL retrieval, or search grounding. Public research uses constrained application tools. Response schemas improve structure but do not replace local validation.

## Tool Requirements

Every model-invocable tool must have:

- A narrow purpose and explicit read/write semantics
- Validated structured input
- Deterministic authorization
- Time and resource limits
- Bounded, attributed output

The model receives no unrestricted networked shell and cannot publish directly. Trusted code validates structured findings before publication.

## Repository Access

Assume every checkout is hostile. The initial implementation provides bounded reading and searching without executing repository-controlled code.

Repository tools must prevent path traversal and avoid running hooks. Workspaces must not contain credentials, SQLite state, runtime memory, or host sockets, and must be cleaned between unrelated reviews.

If builds, tests, or scripts are later required, approve a separate sandbox design covering network access, lifecycle scripts, filesystem isolation, CPU, memory, disk, process, and time limits. Treat execution output as untrusted.

## Public Network Access

Public-source tools must:

- Allow only intended protocols and read-only operations
- Block loopback, link-local, private, metadata-service, and internal destinations
- Revalidate redirects and resolved addresses
- Bound response size and request time
- Preserve source URL, revision when available, and retrieval time
- Avoid sending secrets, private source, or full internal diffs in queries

Public content cannot grant permissions or request disclosure of private context.

## Runtime Memory

Feedback does not become trusted memory automatically. A memory record retains scope, lesson, evidence, source, confidence, approval status, and timestamps.

Human approval is the default. Any later automatic promotion requires repeated independent evidence and an audit trail. Current code and explicit team policy override memory, and records must support correction, rejection, and supersession.

Runtime memory is separate from contributor documentation under `docs/agents/`.

## Publication and Logging

Before publishing:

- Verify the project, merge request, and head SHA.
- Validate paths and line locations against the reviewed revision.
- Reject malformed or unsupported findings.
- Exclude credentials, hidden prompts, private excerpts, and internal tool traces.
- Include evidence and use non-sensitive stable idempotency markers.

Log operational identifiers and outcomes rather than repository content. Never log credentials, webhook secrets, authorization headers, or unnecessary private source. Diagnostic content logging must be explicit and opt-in.
