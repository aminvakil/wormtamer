# Inspect the current repository with bounded tools

Status: proposed

## Goal

Let Gemini inspect relevant files outside the merge request diff at the reviewed commit so findings can account for existing definitions, callers, tests, and configuration without executing repository-controlled code.

## Scope

- Add a trusted repository broker that obtains an immutable snapshot of the merge request's current repository and keeps GitLab credentials outside the workspace and model context.
- Add narrowly scoped model tools for listing, reading, and searching text files within that snapshot.
- Replace the single Gemini request with a small explicit function-calling loop whose arguments, results, call count, bytes, and duration are bounded by application code.
- Preserve the existing validated structured result and idempotent publication flow.

Do not inspect other repositories, access public sources, execute builds or repository code, expose a shell, persist model conversations, or make repository caches authoritative.

## Approach

The GitLab broker resolves the exact reviewed revision and materializes a disposable workspace through trusted application code. The repository broker validates every requested path, rejects traversal and unsupported file types, and returns bounded attributed content. Workspaces contain no credentials, SQLite state, memory, or host sockets and are cleaned between reviews.

Gemini may request only declared read-only operations. The application explicitly dispatches each request after policy validation and requires the existing final result schema when the loop ends. Exhausted limits fail visibly rather than silently omitting requested context.

Whether the immutable snapshot is obtained through GitLab archive APIs, repository APIs, or a hook-disabled checkout must be decided before this plan is approved; the selected mechanism must keep credentials out of command arguments, process output, and workspace metadata.

## Risks and Open Questions

- Symlinks, submodules, unusual paths, binary files, and archive extraction can cross workspace boundaries unless handled fail-closed.
- Repository size limits and cleanup after crashes need explicit bounds.
- Tool output is untrusted prompt input and must pass the same configured-secret checks as merge request diffs.
- The Gemini SDK's exact manual function-calling behavior must be covered by a narrow test seam rather than automatic tool execution.

## Verification

- A review can request a definition or caller outside the changed diff at the exact reviewed revision and use it in a valid finding.
- Requests for traversal paths, unsupported content, excessive output, undeclared tools, or revisions other than the reviewed snapshot are rejected without disclosure.
- Prompt injection in repository content cannot add tools, increase limits, publish directly, or access credentials.
- Cancellation, restart, or tool failure leaves the durable job recoverable and the workspace disposable.
- Reviews that need no additional context still complete through the same validated publication path.
