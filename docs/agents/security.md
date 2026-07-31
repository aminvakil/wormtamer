# Security

## Trust Model

Webhook payloads, merge request content, repositories, comments, runtime memory, public content, model output, and model-requested tool calls are untrusted. They may contain prompt injection and provide evidence, not authority to change policy.

Each team installation has independent credentials, configuration, allowlists, state, memory, and caches. There is no cross-installation API or shared runtime memory.

## Authorization

Repository access requires both GitLab permission and an installation allowlist entry. Entries are exact GitLab namespace paths. The same list filters webhook projects and bounds the repositories that may be disclosed to or inspected by the model. Cross-repository access additionally requires an explicit directional `repository_sharing` rule from the repository under review to the related repository. Inclusion authorizes application access but does not make repository content trusted, and the model cannot modify any authorization condition.

Use a GitLab personal access token with `api` scope whose user has at least the Reporter role on each configured project. This is the minimum combination verified on GitLab 17.5.5 for project lookup, merge request and diff reads, current-user lookup, note search, and note creation. Feedback evaluation additionally retrieves an exact note and effective project membership through the same trusted broker; unavailable membership fails closed to non-Maintainer provenance, while other authorization failures remain sanitized failures. The application assumes the configured token has sufficient permissions and reports sanitized authorization failures rather than discovering or validating scopes. Administrator access is not an application requirement. Grant shared repositories explicitly; never use administrator access for convenient discovery.

A bot may see content unavailable to a merge request participant. A directional sharing rule is the operator's assertion that every audience able to view merge requests in the target repository may receive information derived from the related repository; do not configure a rule based only on bot visibility or one requester's membership. The application enforces the configured direction and fails closed when no exact rule exists.

## Credentials

Credentials belong only to trusted application code. Never place them in prompts, tool results, logs, command arguments, repository workspaces, shell history, or sandbox environments.

Credentials are loaded as plaintext from the explicit deployment configuration file. The real configuration file must remain outside version control. Warn when its filesystem permissions allow group or other users to read it, but do not treat permissions as portable proof of secrecy. The always-enabled review worker requires the GitLab personal access token and Gemini API key at startup; startup validates only their presence and makes no external credential or scope checks.

Authenticate GitLab webhooks with the configured webhook secret before accepting their contents. GitLab calls go through a trusted broker outside repository workspaces. The broker builds API paths under the configured GitLab base URL, rejects every redirect to prevent PAT forwarding, and bounds request time and response size. Separate read and write capabilities in tool design even when they use one credential. Redact known secrets from errors and logs.

Load the Gemini API key from the deployment configuration and pass it only to an explicitly configured Gemini Developer API client. The explicit function-calling loop declares only repository read tools; application code validates, authorizes, limits, and dispatches every returned function request itself.

Do not enable automatic function execution, Gemini code execution, URL retrieval, or search grounding. Public research uses constrained application tools. Before constructing a model prompt, reject review metadata, diffs, or transient feedback comments containing any configured secret; do not send, log, store, or identify the matching value. Response schemas improve structure but do not replace local validation.

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

Assume every repository snapshot is hostile. On the first current-repository tool call, the GitLab broker authorizes the numeric project against its configured namespace and downloads an archive for the exact reviewed SHA with the PAT in an HTTP header. For a related repository, trusted code first verifies its exact configured path and directional sharing rule, resolves its default-branch HEAD to an immutable commit SHA, and downloads the archive at that SHA. The archive is bounded before extraction and never sent wholesale to Gemini.

The repository broker accepts one archive root per inspected repository, rejects traversal and duplicate paths, extracts only bounded regular UTF-8 text files, and never follows symlinks or initializes submodules. Model tools can recursively list text-file paths, read bounded line ranges, and perform bounded case-sensitive literal searches. Every result identifies its exact repository and revision and is checked for configured secrets before it returns to Gemini. Unknown tools, unknown arguments, alternate revisions, unlisted or sharing-ineligible repositories, invalid paths, and exhausted limits disclose no repository content.

Workspaces contain no credentials, SQLite state, runtime memory, or host sockets. They use restrictive permissions, are removed after each review, and their installation-local root is cleaned on startup and shutdown. Repository-controlled code, Git hooks, builds, tests, and scripts are never executed.

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

Natural merge request comments, actor identity fields, findings, and model interpretations are untrusted evidence. Trusted code fetches the current comment transiently, verifies its author against the webhook actor, resolves effective project access through GitLab, and supplies role as attributed metadata rather than an instruction. Internal, system, and Wormtamer-authored notes are ineligible. A failed membership lookup cannot confer Maintainer authority.

Gemini may automatically activate a bounded repository-scoped lesson after associating a comment with a supplied finding. Automatic activation is an application decision, not proof that the lesson is policy: memory remains advisory model output, and current code and explicit team policy always override it. The model cannot select another repository, invent a finding identity, broaden scope, or preserve arbitrary comment text. Trusted code validates the finding, outcome, confidence, lesson bounds, secret exclusion, and fixed repository scope.

Store the GitLab project, merge request, note, actor, and finding identifiers, source URL, role snapshot, structured current decision, lesson, confidence, active state, and timestamps. Do not retain source comment bodies or revisions. Edits replace derived state, and deletion deactivates it through source reconciliation.

Runtime memory is separate from contributor documentation under `docs/agents/`.

## Publication and Logging

The current summary renderer escapes model-controlled Markdown and HTML constructs, neutralizes mentions, and rejects output containing known configured secrets. Publication reconciliation accepts a hidden marker only when the note author matches the PAT's authenticated GitLab user; untrusted contributors cannot suppress a review by copying the marker. These controls supplement, rather than replace, structured result validation.

Before publishing:

- Verify the project, merge request, and head SHA.
- Validate paths and line locations against the reviewed revision.
- Assign finding identifiers in trusted application code only after validation; reject model-supplied or malformed identifiers.
- Reject malformed or unsupported findings.
- Exclude credentials, hidden prompts, private excerpts, and internal tool traces.
- Include evidence and use non-sensitive stable idempotency markers.

Log operational identifiers and outcomes rather than repository content. Webhook logs use bounded delivery, project, merge request, revision, job, and outcome fields when available, plus a sanitized rejection or failure reason. Never log webhook bodies, request headers, credentials, webhook secrets, authorization headers, or unnecessary private source. Diagnostic content logging must be explicit and opt-in.
