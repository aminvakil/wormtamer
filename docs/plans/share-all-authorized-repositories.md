# Share all authorized repositories

Status: proposed

## Goal

Let an operator explicitly declare that every authorized repository has a shared review audience, making every other authorized repository available as related context without writing an all-to-all directional `repository_sharing` map. Preserve deny-by-default cross-repository access when that declaration is absent.

## Scope

Add a top-level `share_all_authorized_repositories` boolean configuration field with a default of `false`.

- When omitted or `false`, existing directional `repository_sharing` behavior remains unchanged.
- When `true`, each authorized repository is related to every other authorized repository, excluding itself.
- Reject `true` together with a non-empty `repository_sharing` map rather than defining precedence or exception semantics.
- Keep GitLab authorization checks, immutable revision pinning, repository and tool-call limits, secret checks, and repository-scoped memory unchanged.
- Continue fetching related repositories lazily only after Gemini requests a repository tool.

This flag does not authorize unlisted repositories, broaden public-source access, share runtime memory across repositories, eagerly search repositories, or guarantee that Gemini inspects every available repository.

## Approach

Decode and validate the explicit boolean in deployment configuration. After rejecting an ambiguous combination with user-supplied directional rules, derive the effective directional sharing map from `authorized_repositories`: for each review target, include every other configured path. A single authorized repository therefore produces no related repositories and remains valid.

Pass the derived map through the existing GitLab snapshot and repository broker path. The review input should list all other authorized paths as `related_repositories` in deterministic order, while trusted broker checks continue to enforce the same effective relation independently of model input. No new model tool or eager archive operation is needed.

Document the flag as an operator assertion about disclosure, not a convenience search setting: every person able to view merge requests in any authorized repository may receive information derived from every other authorized repository. Keep the directional map as the recommended option when repository audiences differ.

## Risks and Open Questions

A single-team deployment does not necessarily imply identical GitLab readership. Enabling this flag can disclose facts derived from a more restricted repository in findings published to a less restricted repository. The explicit false default, mutually exclusive configuration, and deployment warning are required safeguards.

Large authorized lists increase the repository names disclosed in each review and may present more choices than Gemini can inspect under the existing resource limit. The flag grants bounded availability, not exhaustive cross-repository analysis; do not raise limits as part of this work.

## Verification

- Omitting the flag or setting it to `false` preserves existing directional sharing and denies unconfigured cross-repository tool calls.
- Setting the flag to `true` exposes every other authorized repository, but not the current repository, in each review snapshot and permits the corresponding broker-authorized tool calls.
- Setting the flag to `true` with a non-empty `repository_sharing` map fails configuration validation with a clear error.
- A one-repository configuration with the flag enabled starts successfully and exposes no related repositories.
- Unlisted repositories remain unavailable regardless of the flag.
- Related archives are not resolved or downloaded unless Gemini requests repository context.
- Existing per-review repository, tool-call, output, and secret limits still apply to all-to-all sharing.
