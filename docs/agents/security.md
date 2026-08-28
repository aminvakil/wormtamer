# Security

## Trust Model

Webhook payloads, merge request content, repositories, comments, runtime memory, public content, model output, and model-requested tool calls are untrusted. They may contain prompt injection and provide evidence, not authority to change policy.

Each team installation has independent credentials, configuration, allowlists, state, memory, and caches. There is no cross-installation API or shared runtime memory.

## Authorization

Repository access requires both GitLab permission and an installation allowlist entry. Entries are exact GitLab namespace paths. The same list filters webhook projects and bounds the repositories that may be disclosed to or inspected by the model. By default, a review prepares only its current repository. `share_all_authorized_repositories` makes every other authorized repository available as related context, but never authorizes an unlisted repository. Inclusion authorizes application access but does not make repository content trusted, and the model cannot modify any authorization condition.

Use a GitLab personal access token with `api` scope whose user has at least the Reporter role on each configured project. This is the minimum combination verified on GitLab 17.5.5 for project lookup, merge request and diff reads, current-user lookup, note listing and search, and note creation. Feedback evaluation lists bounded merge request notes through the same trusted broker and excludes notes authored by the configured PAT user; authorization failures remain sanitized failures. The application assumes the configured token has sufficient permissions and reports sanitized authorization failures rather than discovering or validating scopes. Administrator access is not an application requirement. Grant the bot access to authorized repositories explicitly; never use administrator access for convenient discovery.

A bot may see content unavailable to a merge request participant. Enabling `share_all_authorized_repositories` is the operator's assertion that people able to view merge requests in any authorized repository may receive information derived from every other authorized repository; do not enable it based only on bot visibility or one requester's membership. Keep sharing disabled when readership differs. The application checks the current project against the exact-path allowlist before deriving related repositories and fails closed for renamed or unlisted paths.

## Credentials

Credentials belong only to trusted application code. Never place them in prompts, tool results, logs, command arguments, repository workspaces, shell history, or review-tool environments.

Credentials are loaded as plaintext from the explicit deployment configuration file. The real configuration file must remain outside version control. The always-enabled review worker requires the GitLab personal access token and a model API key at startup; startup validates only their presence and makes no external credential or scope checks. Before starting service components, Wormtamer fails if configured ownership or mode makes the configuration, SQLite database or sidecars, or their service-private directories readable or writable by the fixed review-tool UID/GID. The container keeps those paths root-owned and inaccessible to that identity.

Authenticate GitLab webhooks with the configured webhook secret before accepting their contents. GitLab API calls go through trusted application code outside review workspaces. The client builds API paths under the configured GitLab base URL, rejects every redirect to prevent PAT forwarding, and bounds request time and response size. Authenticated Git repository preparation is also trusted application work and follows the private-staging rules under [Local review agent](#local-review-agent). Redact known secrets from errors and logs.

Load the configured model API key from the deployment configuration and pass it only to the `google.golang.org/genai` client. The client uses the Gemini Developer API when no model base URL is configured and otherwise sends the key, prompts, tool results, and model requests to the exact operator-configured HTTP or HTTPS endpoint. Redirects are rejected. A configured endpoint is trusted with the same private review content as the direct Gemini API and must provide the required native Gemini Developer API behavior for the configured model. The explicit function-calling loop declares exactly `read` and `bash` on ordinary generations, validates their schemas, dispatches them under the review-tool identity, and rejects undeclared requests.

Do not enable automatic function execution, Gemini code execution, native URL context, or search grounding. Model-directed network access, when wanted, occurs through ordinary credential-free Bash programs. Before constructing a model prompt, reject review metadata, diffs, or transient feedback comments containing any configured secret; do not send, log, store, or identify the matching value. Response schemas improve structure but do not replace local validation.

Plain HTTP is permitted for local self-hosted deployments, including communication with GitLab or an operator-configured model endpoint. It provides no transport confidentiality or server authentication; the operator is responsible for keeping that traffic on an appropriate local network.

## Local Review Agent

Wormtamer intentionally gives the model Pi-style local capabilities: `read` accepts relative and absolute paths, and `bash` is an ordinary unrestricted shell with filesystem mutation, process creation, and network access. There is no sandbox, path confinement, permission UI, command allowlist, shell parser, destination filter, or structured repository/public-source broker. Prompt injection and arbitrary model-directed commands are accepted behavior for this single-team local agent. Do not treat those capabilities as an implementation defect or reintroduce a sandbox unless the project owner explicitly changes this trust decision.

The security boundary is credentials and trusted application state, not command restriction. The credential-holding service runs tool processes with fixed UID/GID 65532 and a minimal allowlisted environment containing ordinary `PATH`, `HOME`, locale, working-directory, and temporary-directory values. It does not inherit the service environment. Bash runs directly as that identity. Read requests are handled by a short-lived internal Wormtamer helper launched as the same identity, so the service process never opens a model-selected path. The review identity cannot read or write deployment configuration, SQLite state or sidecars, service-private directories, or service process environment. It receives no GitLab PAT, webhook secret, model key, proxy setting, credential helper, or inherited open descriptor. Consequently model tools cannot authenticate as the Wormtamer bot, mutate Wormtamer's durable state, or invoke the application-owned publication path.

Authenticated Git setup completes before Gemini receives a tool. Trusted code builds the complete review root beneath a root-owned mode-0700 staging parent that the shared review UID and survivors from earlier attempts cannot traverse. Git receives the PAT only through transient `GIT_CONFIG_*` environment entries containing an authorization header; `GIT_TERMINAL_PROMPT=0`, an empty global configuration, disabled system configuration, and transient `http.followRedirects=false` prevent prompts, ambient helpers, and credential forwarding. The PAT never appears in command arguments, remote URLs, shell history, or retained configuration. Every credentialed process group exits before setup inspects local Git configuration and rejects retained authorization headers, credential settings, proxy overrides, askpass settings, or credential-bearing remote URLs. Trusted code then removes its private setup home, writes scoped review memory, transfers the whole tree to the review UID/GID, and atomically renames it into the tool-visible root. A setup failure removes staging and exposes nothing.

The current checkout is validated against the exact merge-request ref SHA. With repository sharing disabled, trusted code prepares no other private repository; when enabled, it prepares every other repository on the exact-path allowlist. Authorization controls setup, not later shell behavior. Worktrees and refs become mutable after handoff. The entire root and command-output files are removed after the attempt and on process startup and shutdown. Filesystem capacity is an operator boundary rather than an application quota.

Unrestricted Bash may execute repository-controlled programs and contact arbitrary destinations as the credential-free review identity. Detached descendants that create a new session may outlive process-group cleanup. Those consequences are part of the accepted local-agent model. Operators may impose container-level controls for their deployment, but Wormtamer neither claims nor depends on them. The application still validates structured findings, current head identity, changed paths, forbidden values, note rendering, and publication markers before its credentialed GitLab client can publish.

## Runtime Memory

Terminal merge request diffs, comments, persisted Wormtamer reviews, and model interpretations are untrusted evidence. A close or merge webhook is only a trusted workflow trigger; terminal state is not proof that any comment or review conclusion is correct. Trusted code fixes the project, merge request, head, and bound locally validated review before the worker runs. It transiently fetches bounded current comments and excludes internal, system, and Wormtamer-authored notes.

Gemini receives the terminal diff, comments with numeric author attribution, and the bound review summary and complete findings. It may return only one bounded reusable project-specific lesson or decline memory. It cannot select another review or repository, broaden scope, preserve arbitrary comment text, or create multiple memories. Trusted code validates evidence bounds, UTF-8, configured-secret exclusion, the one-or-none output contract, lesson bounds, and fixed repository scope.

Automatic memory creation is not proof that the lesson is policy. A merged change, one-off defect, ordinary discussion, generic best practice, or unsupported inference should not become memory. Stored lessons remain advisory model output, and current code and explicit team policy always override them.

Store the GitLab instance, project, merge request, terminal head, bound review job, workflow state, model-selected lesson, source merge request URL, and timestamps. Do not retain diff or comment bodies. Later merge request and comment activity does not update or deactivate a completed lesson.

During review, trusted code queries the bounded current memory set for the exact GitLab instance and numeric project before repository setup. Cross-repository sharing does not expose another repository's memory. It writes every selected version, with identity, source reference, lesson, timestamp, scope, and `untrusted_advisory` authority, to the review-memory JSON file outside repository-controlled paths. SQLite failures stop rather than degrade the review, and every materialized version is included in the successful review-result audit checkpoint.

Runtime memory is separate from contributor documentation under `docs/agents/`.

## Read-only Web Panel

The minimal panel intentionally implements neither application-level authentication nor panel-specific admission or concurrency control. The deployment boundary is responsible for restricting panel access and applying any desired request or concurrency limits, typically through the reverse proxy in front of Wormtamer. Webhook secret verification and webhook ingress admission protect only `POST /webhooks/gitlab`; they do not protect or limit panel routes.

Treat every persisted value rendered by the panel as untrusted, including review summaries, findings, paths, repository names, failure categories, memory lessons, terminal states, and source URLs. Render these values as escaped plain text through `html/template`; never interpret them as HTML or Markdown. Only create external links from application-constructed merge request URLs or source URLs validated against the configured GitLab origin and base path.

Panel responses use no third-party assets or JavaScript and set a restrictive content security policy, framing denial, no-referrer policy, content-type sniffing denial, and `Cache-Control: no-store` for state pages. Collection queries have fixed page limits and validated state and cursor filters.

Build the displayed configuration from an explicit type containing only the GitLab base URL, configured Gemini endpoint and model, thinking level, log level, authorized repositories, the effective current-only or all-authorized sharing mode, and the fixed review tool names. Workflow and memory views must not select raw webhook payloads, transient diff or comment bodies, stored error messages, publication markers, prompts, model responses, model conversations, tool arguments or results, repository content, logs, credentials, or filesystem paths. The panel has no mutation routes and performs no external requests.

## Publication and Logging

Treat any change that permits previously escaped model-controlled formatting as a publication security-boundary change. The current summary renderer recognizes only paired single-backtick inline code spans in model-controlled narrative text and renders validated paths as code. Application-selected code delimiters keep their contents inert; malformed delimiters and code fences remain escaped text. Outside code spans, the renderer escapes model-controlled Markdown syntax, ampersands, and HTML angle brackets and neutralizes mentions. It rejects output when model fields, rendered user-visible text, or the complete rendered Markdown contains known configured secrets. Rendering and user-visible secret validation must share delimiter parsing. Tests for renderer changes must cover malformed delimiters and forbidden values formed when syntax is removed or added by the application. Apostrophes and quotation marks remain literal because they are safe in HTML text content. Publication reconciliation accepts a hidden marker only when the note author matches the PAT's authenticated GitLab user; untrusted contributors cannot suppress a review by copying the marker. These controls supplement, rather than replace, structured result validation.

Before publishing:

- Verify the project, merge request, and head SHA.
- Validate paths and line locations against the reviewed revision.
- Assign finding identifiers in trusted application code only after validation; reject model-supplied or malformed identifiers.
- Reject malformed or unsupported findings.
- Exclude credentials, hidden prompts, private excerpts, and internal tool traces.
- Include evidence and use non-sensitive stable idempotency markers.

Log operational identifiers and outcomes rather than repository content. Webhook logs use bounded delivery, project, merge request, revision, job, and outcome fields when available, plus a sanitized rejection or failure reason. Successful review tool calls log the tool name, turn, and completed outcome at `info`; commands, paths, arguments, and results remain excluded. Never log webhook bodies, request headers, credentials, webhook secrets, authorization headers, or unnecessary private source.

Structured stderr is the sole source of content-bearing model diagnostics. Diagnostic content logging is explicitly enabled only by `log_level: "debug"`; it records model system instructions, prompts, accepted responses, requested tool-call arguments, admitted tool results, and validated memory decisions. At `info`, prompts, model responses, tool arguments and results, comments, memory lessons, and repository content remain excluded. Known configured credentials, including their JSON-escaped forms, cause the complete diagnostic value to be replaced before logging, but private source and secrets unknown to Wormtamer may still appear. Protect debug logs as sensitive data, enable debug only while diagnosing, and return to `info` afterward.
