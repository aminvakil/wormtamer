# Security

## Trust Model

Webhook payloads, merge request content, repositories, comments, runtime memory, public content, model output, and model-requested tool calls are untrusted. They may contain prompt injection and provide evidence, not authority to change policy.

Each team installation has independent credentials, configuration, allowlists, state, memory, and caches. There is no cross-installation API or shared runtime memory.

## Authorization

Repository access requires both GitLab permission and an installation allowlist entry. Entries are exact GitLab namespace paths. The same list filters webhook projects and bounds the repositories that may be disclosed to or inspected by the model. Inclusion authorizes access but does not make repository content trusted, and the model cannot modify either authorization condition.

Use a GitLab personal access token with `api` scope whose user has at least the Reporter role on each configured project. This is the minimum combination verified on GitLab 17.5.5 for project lookup, merge request and diff reads, current-user lookup, note search, and note creation. The application assumes the configured token has sufficient permissions and reports sanitized authorization failures rather than discovering or validating scopes. Administrator access is not an application requirement. Grant shared repositories explicitly; never use administrator access for convenient discovery.

A bot may see content unavailable to a merge request participant. Publish private cross-repository information only when the installation's sharing policy permits it. Where audiences differ, enforce the intersection of bot access and requester visibility.

## Credentials

Credentials belong only to trusted application code. Never place them in prompts, tool results, logs, command arguments, repository workspaces, shell history, or sandbox environments.

Credentials are loaded as plaintext from the explicit deployment configuration file. The real configuration file must remain outside version control. Warn when its filesystem permissions allow group or other users to read it, but do not treat permissions as portable proof of secrecy. The always-enabled review worker requires the GitLab personal access token and Gemini API key at startup; startup validates only their presence and makes no external credential or scope checks.

Authenticate GitLab webhooks with the configured webhook secret before accepting their contents. GitLab calls go through a trusted broker outside repository workspaces. The broker builds API paths under the configured GitLab base URL, rejects every redirect to prevent PAT forwarding, and bounds request time and response size. Separate read and write capabilities in tool design even when they use one credential. Redact known secrets from errors and logs.

Load the Gemini API key from the deployment configuration and pass it only to an explicitly configured Gemini Developer API client. The current structured generation request declares no tools. If tools are later added, the application must validate, authorize, limit, and dispatch every returned function request itself.

Do not enable automatic function execution, Gemini code execution, URL retrieval, or search grounding. Public research uses constrained application tools. Before constructing a model prompt, reject review metadata or diffs containing any configured secret; do not send, log, or identify the matching value. Response schemas improve structure but do not replace local validation.

Plain HTTP is permitted for local self-hosted deployments, including communication with GitLab. It provides no transport confidentiality or server authentication; the operator is responsible for keeping that traffic on an appropriate local network.

## Tool Requirements

Every model-invocable tool must have:

- A narrow purpose and explicit read/write semantics
- Validated structured input
- Deterministic authorization
- Time and resource limits
- Bounded, attributed output

The model receives no unrestricted networked shell and cannot publish directly. Trusted code validates structured findings before publication.

## Repository Access

Assume every checkout is hostile. Repository workspaces are not implemented. When introduced, initial repository tools are limited to bounded reading and searching and must not execute repository-controlled code.

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

The current summary renderer escapes model-controlled Markdown and HTML constructs, neutralizes mentions, and rejects output containing known configured secrets. Publication reconciliation accepts a hidden marker only when the note author matches the PAT's authenticated GitLab user; untrusted contributors cannot suppress a review by copying the marker. These controls supplement, rather than replace, structured result validation.

Before publishing:

- Verify the project, merge request, and head SHA.
- Validate paths and line locations against the reviewed revision.
- Reject malformed or unsupported findings.
- Exclude credentials, hidden prompts, private excerpts, and internal tool traces.
- Include evidence and use non-sensitive stable idempotency markers.

Log operational identifiers and outcomes rather than repository content. Webhook logs use bounded delivery, project, merge request, revision, job, and outcome fields when available, plus a sanitized rejection or failure reason. Never log webhook bodies, request headers, credentials, webhook secrets, authorization headers, or unnecessary private source. Diagnostic content logging must be explicit and opt-in.
