# Inspect authorized related repositories

Status: proposed

Depends on: [Inspect the current repository with bounded tools](inspect-current-repository.md)

## Goal

Allow a review to inspect relevant code and contracts in other explicitly authorized GitLab repositories when cross-repository context is needed for a correct finding.

## Scope

- Extend the read-only repository broker to repositories named in the installation's exact `authorized_repositories` allowlist.
- Resolve and attribute an immutable revision for every inspected repository.
- Fetch related repositories lazily and bound repositories, revisions, files, bytes, tool calls, and retained cache data per review.
- Enforce both GitLab credential access and application authorization for every request.

Do not discover repositories from the token's visibility, eagerly clone or index every authorized repository, write to repositories, execute repository content, or disclose one repository merely because the model requested it.

## Approach

Build on the current-repository tool contract rather than introducing a second access path. A model request identifies an exact configured repository and a bounded read or search operation. Trusted code checks authorization before resolving a revision or fetching content, and tool results identify the repository and revision from which every result came.

Cross-repository disclosure must fail closed unless the installation's sharing rule permits the merge request audience to receive that information. The minimum sharing policy and how requester visibility is established must be resolved before this plan is approved; an allowlist entry alone grants application access but does not prove equal human audiences.

Caches remain installation-local, disposable, size-bounded, and non-authoritative. They must never mix credentials, memory, or workspace state between reviews.

## Risks and Open Questions

- Private repositories may have different audiences even within one team; publishing excerpts or derived facts can leak information.
- Default-branch movement makes unattributed context irreproducible, so revision selection and attribution must be deterministic.
- Repository rename behavior must continue to fail closed until configuration is updated.
- Nested dependencies can cause unbounded repository fan-out unless the broker enforces a small application-owned limit.

## Verification

- A review can inspect a configured related repository and receives results attributed to its immutable revision.
- Requests for visible-but-unconfigured, renamed, inaccessible, or sharing-ineligible repositories reveal no repository content.
- Repository and aggregate resource limits stop fan-out without bypassing durable retry and failure semantics.
- Content from one repository cannot change authorization, request another credential, or write to GitLab.
