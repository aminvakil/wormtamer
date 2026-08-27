# Use all-or-nothing repository sharing

Status: approved

## Goal

Remove directional repository-sharing rules while retaining the simpler installation-wide choice to expose either no related private repositories or every other authorized repository to a review.

## Scope

Remove the `repository_sharing` configuration field and keep `share_all_authorized_repositories` as the only cross-repository sharing control:

- `false` prepares only the repository under review.
- `true` prepares every other repository in `authorized_repositories`.

Enabling sharing remains an operator assertion that every authorized repository has the same review audience. Exact authorization by configured repository path remains unchanged. This plan does not alter unrestricted model-directed network access, runtime review memory, repository preparation safety, or the review-tool credential boundary.

The read-only panel remains and reports the effective mode without reconstructing directional rules.

## Approach

- Remove directional sharing from configuration types, validation, examples, panel configuration, and focused documentation. Strict JSON decoding will reject the removed field.
- Pass the all-repositories choice directly to the GitLab integration instead of deriving or storing a directional map.
- For an authorized repository under review, derive related repositories from the authorized set, exclude the current repository, and sort the result for deterministic preparation and prompts.
- Keep authorization checks independent from sharing derivation: an unlisted repository is never eligible for trusted preparation.
- Describe only the two effective modes in the architecture, security, deployment, and user-facing configuration documentation.

## Verification

- Configuration containing `repository_sharing` is rejected as unknown.
- With sharing disabled, a review exposes no related private repository.
- With sharing enabled, a review exposes every and only other authorized repository, once each and in deterministic order.
- The current repository is never also presented as related, including installations with only one authorized repository.
- Project rename and exact-path authorization behavior remain unchanged.
- The panel displays the effective current-only or all-authorized mode without exposing credentials or filesystem paths.
