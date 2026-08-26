# Replace review tools with Pi-style `read` and `bash`

Status: approved

## Goal

Replace Wormtamer's specialized review tools with the same minimal model-facing foundation used by Pi: one `read` tool and one unrestricted `bash` tool operating in a real Git working directory. Coding models already know how to use shell programs and Git, so Wormtamer should provide the capabilities and working-directory context without teaching command recipes or adding a new function for each repository operation.

This makes branch, history, file-discovery, search, and ad hoc inspection available through ordinary `git`, `fd`, `rg`, and shell behavior. In particular, the model can inspect any branch already present in an authorized repository clone without every application tool acquiring a separate branch parameter.

## Settled Product Decisions

These decisions are approved requirements, not open questions:

- The review model has exactly two tools: `read` and `bash`.
- The model-facing names, schemas, descriptions, truncation behavior, and minimal prompt guidance follow Pi's built-in tools as closely as the Gemini function-response protocol permits.
- Wormtamer does not add instructions explaining `fd`, `rg`, or Git commands. Models already know those tools.
- `bash` is an ordinary unrestricted shell, not a command allowlist, shell parser, permission prompt, or disguised structured repository broker.
- `read` accepts relative and absolute paths, as Pi's tool does; it is not restricted to repository-relative paths.
- No sandbox is required. Like Pi, the tools are ordinary unrestricted local operations, but they run under a dedicated credential-free review-tool OS identity rather than the credential-holding service identity. Within this project's accepted local-agent trust model, unrestricted `read`/`bash` access inside that identity is not considered a security issue or implementation blocker. The project owner accepts prompt injection, arbitrary model-directed commands, filesystem mutation, and process/network access as expected behavior for this small project. Security hardening beyond the narrow credential boundary is out of scope and must not be raised again during implementation unless the owner explicitly changes this decision or a concrete implementation failure makes the requested behavior impossible.
- Credential isolation is a required boundary, not a shell restriction: neither tool may read the deployment configuration, SQLite state, service-process environment, or GitLab/model credentials. Tool subprocesses receive a minimal credential-free environment. Authenticated repository preparation finishes in trusted application code before model-directed commands begin and leaves no credential in Git configuration, command arguments, shell history, or the prepared workspace.
- The application continues to own final structured-review validation and GitLab publication. The credential boundary must make the guarantee real: model-executed tools cannot authenticate as the Wormtamer bot, mutate Wormtamer's SQLite state, or bypass the validated publication path.
- Model-facing parity is limited to Pi's `read`/`bash` contract and minimal prompt approach. Wormtamer retains its explicit Gemini loop, durable generation records, review deadline, and review-specific final response schema; it does not copy Pi's TUI, sessions, extensions, compaction, or editing tools.
- Existing development configuration and SQLite compatibility need not be preserved, consistent with the current compatibility baseline.

Record the accepted local-agent trust posture in `docs/agents/security.md` and remove the contradictory sandbox/tool-broker requirements from `AGENTS.md` and focused documentation as part of implementation. The resulting focused security documentation is the authoritative home for this decision after this plan is completed.

## Pi Contract to Match

Pi documents its default minimal tool approach in:

- [`README.md`](https://github.com/earendil-works/pi-mono/blob/main/packages/coding-agent/README.md), which describes Pi as a minimal coding harness and identifies `read` and `bash` among its default tools.
- [`docs/quickstart.md`](https://github.com/earendil-works/pi-mono/blob/main/packages/coding-agent/docs/quickstart.md), which describes `read` simply as reading files and `bash` as running shell commands in the current working directory.
- [`docs/security.md`](https://github.com/earendil-works/pi-mono/blob/main/packages/coding-agent/docs/security.md), which states that Pi intentionally has no built-in sandbox and runs tools with the Pi process's permissions.
- Pi's built-in [`read.ts`](https://github.com/earendil-works/pi-mono/blob/main/packages/coding-agent/src/core/tools/read.ts), [`bash.ts`](https://github.com/earendil-works/pi-mono/blob/main/packages/coding-agent/src/core/tools/bash.ts), and [`system-prompt.ts`](https://github.com/earendil-works/pi-mono/blob/main/packages/coding-agent/src/core/system-prompt.ts), which are the behavioral references when prose documentation is less specific.

With only these tools active, use Pi's minimal prompt contribution verbatim:

```text
Available tools:
- read: Read file contents
- bash: Execute bash commands (ls, grep, find, etc.)

Guidelines:
- Use bash for file operations like ls, rg, find
- Use read to examine files instead of cat or sed.
```

Do not add examples such as `git branch`, `git show`, `git diff`, `git log`, `git switch`, `fd`, or `rg` invocations. Keep Wormtamer's review policy around this small tool section rather than replacing it with Pi's general coding-assistant identity.

### `read`

Declare:

```text
read(path, offset?, limit?)
```

- `path`: relative or absolute file path.
- `offset`: optional one-indexed starting line.
- `limit`: optional maximum number of lines.
- Read text files and the image formats supported by Pi: JPEG, PNG, GIF, WebP, and BMP.
- Normalize images to a Gemini-supported inline MIME type, converting formats such as BMP to PNG when required. As Pi does by default, resize images to at most 2,000×2,000 pixels and less than 4.5 MiB of base64-encoded payload, preserving aspect ratio and trying smaller dimensions/encodings until both limits hold.
- Validate dimensions, MIME type, and final encoded size after conversion. If decoding, conversion, or resizing cannot produce a valid bounded image, return Pi's explicit image-omitted note and no media part; never attach the oversized original as a fallback.
- Return successful image reads as Gemini inline function-response media parts with a short textual note. When resized, include original and displayed dimensions plus the coordinate scale, matching Pi's model guidance.
- Truncate text from the head at 2,000 lines or 50 KiB, whichever is reached first.
- When truncation or a caller-supplied limit leaves more text, report the shown line range and next offset so the model can continue.
- Match Pi's useful oversized-first-line behavior by returning an actionable Bash fallback rather than silently returning a partial line.
- Ordinary missing-path, invalid-offset, and unreadable-file errors return as tool errors the model can correct; they do not fail the whole review attempt.

Use Pi's tool description, adjusted only if Gemini terminology requires it:

```text
Read the contents of a file. Supports text files and images (jpg, png, gif, webp, bmp). Images are sent as attachments. For text files, output is truncated to 2000 lines or 50KB (whichever is hit first). Use offset/limit for large files. When you need the full file, continue with offset until complete.
```

### `bash`

Declare:

```text
bash(command, timeout?)
```

- `command`: Bash source to execute.
- `timeout`: optional positive timeout in seconds; there is no tool-specific default timeout, while the outer review deadline and cancellation still apply.
- Execute through the installed Bash in the review's current working directory.
- Return stdout and stderr in observed order.
- Treat non-zero exits and an explicit caller-selected tool timeout as model-correctable tool errors with available bounded command output rather than malformed review output.
- Parent-context cancellation—including the outer review deadline, lease loss, and worker shutdown—is not model-correctable. Kill and reap the active shell process group, then propagate the parent context error so ordinary job retry/shutdown handling remains authoritative.
- Truncate model-visible output from the tail at 2,000 lines or 50 KiB, whichever is reached first.
- While output is accumulated, persist the complete stream to a review-local temporary file whenever truncation occurs. Include that path in the truncation notice so the model can inspect it with `read` or another Bash command.
- Bound combined stdout/stderr before truncation to 16 MiB per command and allow at most 64 MiB of cumulative full-output spool writes per review, without refunding bytes when the model deletes a spool file. Enforce both limits while reading the pipes rather than buffering first. On either limit, stop accepting spool data, terminate and reap the active shell process group, and return the stable model-correctable `bash_output_limit_exceeded` error with only the bounded tail already collected.
- On cancellation, timeout, or output exhaustion, signal the active process group and wait for the shell to exit as best-effort cleanup. Deliberately detached descendants, including commands that create a new session, may survive; do not claim stronger process-tree cleanup without a separate isolation mechanism.
- Remove all command-output files with the disposable review workspace.

Use Pi's tool description, adjusted only to avoid claiming unsupported interactive streaming:

```text
Execute a bash command in the current working directory. Returns stdout and stderr. Output is truncated to last 2000 lines or 50KB (whichever is hit first). If truncated, full output is saved to a temp file. Optionally provide a timeout in seconds.
```

## Scope

Included:

- Replace the seven current review declarations—internal repository list/read/search, memory search, public URL fetch, and public repository list/read—with only `read` and `bash`.
- Replace archive-extracted text-only snapshots with disposable Git working directories that contain `.git` metadata and branch refs.
- Install Bash, Git, ripgrep, fd, and the basic command-line programs expected by the model in the runtime image. Debian's `fd-find` package must expose an `fd` command rather than only `fdfind`.
- Run model tool operations under a dedicated credential-free OS user that can mutate review workspaces but cannot read the deployment configuration, SQLite state, or service credentials.
- Preserve review memory without a model-specific function by materializing bounded current-project memory as a documented file in the review root that `read`, `rg`, or other shell commands can inspect.
- Remove the specialized repository/public-source tool brokers, their category budgets, their declarations, and model guidance that becomes obsolete.
- Remove public-source allowlist configuration and broker code made obsolete by ordinary Bash network access. Install `curl` so the generic shell retains the former ability to retrieve public documentation without a dedicated function.
- Keep related-repository authorization for deciding which private repositories are prepared in the review workspace. Authorization controls workspace setup, not what Bash may do afterward.
- Update deployment documentation, focused agent documentation, configuration examples, panel configuration summaries, diagnostics expectations, and tests to describe the new behavior, including operator responsibility for disposable workspace capacity.

Non-goals:

- `write`, `edit`, `grep`, `find`, or `ls` function declarations; Bash already supplies those capabilities.
- A shell command allowlist or separate structured wrappers around `git`, `fd`, or `rg`.
- A sandbox, permission UI, path confinement, network filtering, or command-policy language.
- Application-enforced repository, workspace, or model-created-file size quotas; disposable storage capacity is an operator responsibility.
- Teaching the model common shell or Git commands.
- Reproducing Pi features unrelated to the two tool contracts.

## Approach

### 1. Establish the credential boundary

In the OCI deployment, keep the credential-holding Go service under a service identity that can launch each tool subprocess with a fixed, unprivileged review-tool UID/GID. The simplest container implementation may keep the service as root solely for this UID/GID transition and root-owned application state; the model never receives a root process. Do not introduce a long-running helper daemon or another application replica.

Apply the identity consistently:

- Execute Bash directly as the review-tool UID/GID with a small allowlisted environment containing only ordinary shell/runtime values such as `PATH`, `HOME`, locale, and the review working directory.
- Perform `read` through a short-lived internal helper mode of the Wormtamer binary launched under the same review-tool UID/GID. Do not open model-selected paths in the credential-holding process and then pass their contents across the boundary.
- Keep the deployment configuration, SQLite database and WAL, service-private directories, and service process environment inaccessible to the review-tool identity. Fail startup when configured file ownership or mode would make known credential/state paths readable or writable by that identity; a warning is insufficient for this deployment contract.
- Keep repository staging owned by and accessible only to the service identity throughout authenticated Git operations. A shared review-tool UID—and any detached process left by an earlier review—must be unable to traverse or modify the staging parent.
- Run authenticated Git setup as a trusted, pre-generation service subprocess and wait for its complete process group to exit before Gemini can request a tool. Pass the PAT only through transient `GIT_CONFIG_*` environment entries, never through command arguments or retained Git configuration. In the same transient configuration set `http.followRedirects=false`; any redirect fails the setup without a follow-up request.
- After every credentialed process has exited, remove and validate the absence of credential headers, credential helpers, proxy overrides, and credential-bearing remote URLs in the local Git configuration. Only then recursively transfer the completed tree to the review-tool UID/GID and atomically rename it from the service-private staging parent into the tool-visible review root.
- Give the review-tool identity ownership of only the exposed disposable review root and its command-output files. Put tool-visible workspaces outside the private database directory so parent-directory traversal permissions do not expose SQLite state.

This is deliberately only a credential and state boundary. It does not confine model-selected paths beyond normal Unix permissions, filter commands, restrict network destinations, or otherwise turn Bash into a sandbox.

### 2. Prepare one Pi-like review working directory

Create a disposable review root before the first Gemini generation and make the current repository checkout the Bash working directory. Build the complete root under the service-private staging parent and expose it to the review-tool identity only after successful credential cleanup and ownership transfer. The initial review input identifies the exposed current working directory, the reviewed immutable head, local paths for prepared related repositories, and the review-memory file. It no longer describes specialized tools, category budgets, allowed public domains, or public GitHub slugs.

Bound all repository preparation, including current and related repository fetches, checkout, validation, credential cleanup, ownership transfer, and publication of the tool-visible root, by one two-minute setup deadline. An internal setup-deadline expiry terminates and reaps active Git/helper process groups, removes private staging, and returns retryable `repository_preparation_timeout`. Parent cancellation performs the same cleanup and propagates the parent context error. Ref/head mismatches retain their existing permanent or obsolete semantics; other sanitized Git transport/process failures return retryable `repository_preparation_failed`. No setup failure may expose a partial root.

Use trusted application setup to create Git repositories before model commands run:

- Fetch the current project's branch refs and merge-request head ref, verify the merge-request ref resolves to the review identity's exact head SHA, and check out that SHA initially in detached-head state.
- Fetch every branch ref for each directionally authorized related repository and initially check out its resolved default-branch head.
- Place related repositories at deterministic sibling paths under the review root and include those paths and initial revisions in review input.
- Perform authenticated GitLab fetches in service-private staging with `GIT_TERMINAL_PROMPT=0`, the PAT header supplied through transient `GIT_CONFIG_*`, and `http.followRedirects=false` supplied the same way. The resulting repository configuration must not embed the PAT, proxy overrides, or credential helpers in a remote URL or retained Git configuration.
- Leave local refs and worktrees mutable after credential cleanup and handoff so ordinary Git commands can switch branches, create worktrees, inspect history, or alter files.
- Clean the entire review root on attempt completion, failure, cancellation, process startup, and process shutdown using the existing disposable-workspace lifecycle.

This intentionally trades the current lazy archive loading and bounded text extraction for an ordinary local Git environment. Do not recreate archive-era per-file, text-only, symlink, branch, or repository-tool restrictions around Bash.

Repository preparation and unrestricted Bash have no application-enforced workspace-size ceiling. Operators must provision and monitor sufficient disposable storage for complete Git histories, every prepared related repository, bounded command-output spools, Git worktrees, and arbitrary files created or copied by model-directed commands. Disk exhaustion is a deployment-capacity failure, not an application quota feature; deployment guidance must account for peak review workspace use and prevent the disposable workspace filesystem from starving persistent SQLite storage. Filesystem allocation/write failures fail the active setup or tool call directly rather than triggering cleanup-and-continue fallback behavior.

### 3. Materialize review memory as ordinary context

Before generation, write the bounded active memories already scoped by trusted application code to the current GitLab instance and project into a JSON file outside repository-controlled paths but inside the review root. Include provenance fields already exposed by the current memory tool. State only the file path and advisory authority in the initial review input; do not teach search commands.

Because every materialized record is available to the model, treat all included memory versions as retrieved for the existing successful-review audit checkpoint. Keep feedback synthesis and memory persistence otherwise unchanged.

### 4. Implement Pi-compatible tool operations

Replace the current repository `ToolBroker` result shape with a small generic tool result capable of carrying:

- a JSON `output` or `error` value for Gemini function responses;
- optional inline media parts for image reads;
- bounded diagnostic text without thoughts or provider protocol fields.

Maintain a 16 MiB cumulative function-response allowance per review across all turns. Charge the final serialized bytes of each complete Gemini `FunctionResponse`, including IDs/names, JSON output or errors, inline-media metadata, and the base64 expansion of every `FunctionResponse.Parts` blob. Check the charge before appending a response to conversation history. If a response would exceed the allowance, discard that result, do not dispatch later calls in the same batch, return fixed small `tool_result_limit_exceeded` responses for that call and every undispatched later call in original order so every function call receives a response, and make the next generation final-only. These fixed bookkeeping responses are not charged to the exhausted evidence allowance. This byte allowance replaces the old 256 KiB aggregate while the call-count ceiling remains removed, so more than sixteen small calls remain valid.

Implement `read` and `bash` against the prepared review root/current working directory through the review-tool identity. Share Pi-equivalent 2,000-line and 50 KiB truncation helpers, using head truncation for reads and tail truncation for shell output. Keep full Bash output in review-local temporary files only when needed. Decode the short-lived `read` helper's framed result strictly; malformed JSON, unexpected fields, invalid base64, or mismatched media metadata is an infrastructure failure with context, never a text fallback.

Stream combined command output through a tail accumulator and bounded spool writer. Charge bytes before writing, stop at 16 MiB for one command or 64 MiB of cumulative spool writes in one review, do not refund deleted-file bytes, terminate and reap the active shell process group on exhaustion, and return `bash_output_limit_exceeded` without partial unbounded data or fallback execution. A spool creation/write/fsync failure is an infrastructure failure and propagates with context; it is not translated into a successful or model-correctable response.

Tool argument mistakes, filesystem errors, non-zero commands, explicit tool timeouts, and output exhaustion are model-correctable tool responses. Workspace creation failures, inability to start Bash, spool I/O failures, persistence failures, and parent-context cancellation retain normal job failure/retry handling. Parent cancellation first signals and reaps the active process group and then returns the original context error; it never creates another Gemini function response.

### 5. Simplify the Gemini loop

Declare exactly `read` and `bash` on every ordinary generation. Remove category classification, per-category call counters, the 16-call combined ceiling, and category/call-budget denial responses. Continue the explicit loop until the model returns a valid structured review, the 16 MiB cumulative function-response allowance forces one final-only generation, or the existing outer review deadline/cancellation ends the attempt.

Keep same-turn dispatch and function-response ordering deterministic. Use Gemini's `FunctionResponse.Response["output"]`/`["error"]` convention and `FunctionResponse.Parts` for supported images. Preserve function-call IDs, generation diagnostics, debug conversation recording, finish-reason checks, and local final review validation.

Remove specialized system-instruction text about exact repository tool arguments, memory queries, public-source destinations, batching based on dependent arguments, and category budgets. Add only the Pi prompt fragment above plus concise review-specific context describing the working directory and available local repositories/memory.

### 6. Remove obsolete specialized infrastructure

Delete code and tests used only by:

- archive extraction and text-only list/read/search workspaces;
- `search_review_memory` dispatch;
- `fetch_public_url`, public repository snapshots, and their URL/domain/repository allowlists;
- repository/memory/public-source tool categories and limits;
- public-source startup client construction and public-source deployment configuration;
- tool-specific recoverable error categories that no longer have callers.

Retain GitLab authorization, review loading, publication, feedback evaluation, memory storage, review result validation, and usage/diagnostic persistence. Remove imports, interfaces, panel fields, and configuration values made unused by this change rather than leaving compatibility shims.

### 7. Align runtime packaging and documentation

Update the OCI image to provide the same ordinary-command expectation as Pi's local environment: Bash plus Git, `rg`, `fd`, and `curl`, available on `PATH` to the dedicated review-tool identity. Create separate service and review-tool identities, keep configuration and SQLite state private to the service identity, and make only disposable review roots writable by the review-tool identity. Keep the single Wormtamer application process and spawn short-lived Git setup, read-helper, and Bash processes only as tool/setup subprocesses.

Update:

- `docs/agents/architecture.md` for the two-tool agent and Git working directories;
- `docs/agents/reliability.md` for the repository-setup deadline, aggregate function-response allowance, bounded image/output behavior, shell lifecycle, Pi truncation behavior, and removal of tool-call/category/archive limits;
- `docs/agents/security.md` for authenticated Git redirect rejection and private staging, the approved Pi-like local-agent trust boundary, and the explicit decision that unrestricted process-permission tools are intentional;
- `AGENTS.md` so future sessions do not reintroduce or request a sandbox for this approved behavior;
- `docs/deployment.md`, `config.example.json`, and panel configuration output after public-source settings are removed, with explicit operator guidance for sizing and monitoring disposable workspace storage separately from persistent SQLite capacity;
- the Dockerfile for required commands.

Keep the accepted trust decision in one authoritative focused-document section and link to it where another document needs context rather than repeating the rationale.

## Verification

The outcome is complete when automated tests and an image-level smoke check demonstrate:

1. A review generation declares exactly `read` and `bash`; no old tool name, Gemini code execution, native URL context, or search grounding is declared.
2. The generated review system instruction contains the exact Pi tool snippets and two file-operation guidelines above, with no command tutorial for Git, fd, or rg.
3. `read` handles relative and absolute text paths, offsets, limits, continuation notices, 2,000-line/50 KiB head truncation, oversized first lines, and correctable filesystem errors; malformed helper JSON or media metadata fails the attempt rather than falling back.
4. Image reads normalize supported formats, convert BMP when needed, preserve aspect ratio, and produce a validated image no larger than 2,000×2,000 with a base64 payload below 4.5 MiB. Oversized images are progressively resized; undecodable or still-oversized images return the explicit omission note with no media part, and resized images report coordinate scaling.
5. `bash` runs in the current repository, exposes stdout and stderr, accepts an optional timeout, reports non-zero exits to the model, and applies 2,000-line/50 KiB tail truncation with a readable bounded full-output file when needed.
6. A command exceeding 16 MiB of combined output or the review's 64 MiB cumulative spool allowance has its active process group terminated and reaped and returns `bash_output_limit_exceeded`; the application's output accumulator memory and spool storage remain within their documented bounds.
7. Explicit tool timeout returns a model-correctable error, while outer deadline, lease-loss, and shutdown cancellation terminate and reap the active process group and propagate the parent context error without another model turn. Tests claim no cleanup guarantee for deliberately detached descendants.
8. Both `read` and `bash` run as the credential-free review-tool identity. They cannot read the deployment configuration, SQLite database/WAL, or service-process environment, receive no PAT/model key, and cannot authenticate to GitLab as the Wormtamer bot; startup rejects permissions that would violate this boundary.
9. An authenticated Git test server returning a redirect receives only the initial request: Git has `http.followRedirects=false`, makes no redirected request, and returns a sanitized setup failure without disclosing the test PAT.
10. While a credentialed Git subprocess is blocked, the review-tool UID and a simulated survivor from an earlier review cannot traverse or modify service-private staging. The workspace becomes tool-owned and visible only after the credentialed process group exits and retained Git configuration passes credential/proxy/helper validation.
11. A shortened repository-setup deadline terminates and reaps Git/helper process groups, removes private staging, exposes no partial workspace, and returns retryable `repository_preparation_timeout`; parent cancellation instead propagates its context error.
12. The cumulative serialized size of JSON outputs and inline `FunctionResponse.Parts` never exceeds 16 MiB. An excess response and later same-batch calls receive only ordered `tool_result_limit_exceeded` bookkeeping responses followed by final-only generation, while more than sixteen small admitted calls can complete normally.
13. The initial current checkout exactly matches the reviewed merge-request SHA, contains `.git`, and exposes branch refs. A Bash command can switch to another fetched branch, and a subsequent `read` observes that branch's file contents.
14. Authorized related repositories are present at the paths supplied to the model with their branches available; unconfigured private repositories are not prepared by trusted setup.
15. The model can use `git`, `rg`, `fd`, Bash, and `curl` by those command names in the production image.
16. Current-project review memory is available through the materialized file, carries provenance, and its exposed versions are audited with a successfully saved review result.
17. Ordinary shell filesystem mutation is visible to later `read` and `bash` calls in the same attempt and disappears when the disposable workspace closes.
18. A review still cannot complete or publish without passing the existing structured result, changed-path, finding-count, forbidden-value, and publication validation, and the model cannot bypass publication with service credentials.
19. Configuration decoding, panel summaries, startup wiring, focused documentation, and deployment examples contain no obsolete public-source or specialized-tool contract.
20. Deployment documentation states that complete Git histories, related repositories, bounded output spools, worktrees, and arbitrary model-created files consume operator-provisioned disposable capacity; no application workspace quota is promised, and disposable exhaustion must not starve persistent SQLite storage.
