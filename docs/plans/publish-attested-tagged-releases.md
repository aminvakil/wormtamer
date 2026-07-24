# Publish attested tagged releases

Status: in-progress

## Goal

Publish a versioned Wormtamer image and verifiable build provenance for each signed `v*` Git tag after the repository and GHCR package are public.

## Scope

- Keep pull requests and pushes to `main` as build-and-test workflows without image publication.
- After build and runtime checks pass for a tag such as `v1.2.3`, publish the same image as `ghcr.io/aminvakil/wormtamer:1.2.3` and `ghcr.io/aminvakil/wormtamer:latest`.
- Attest the exact digest produced by that build and push the provenance to GHCR.

Do not publish `v1.2.3`, `1.2`, or `1` image aliases. Do not create or edit GitHub releases in the workflow. Do not add another platform, signing service, key-management system, SBOM, or release artifact.

## Approach

Add `v*` tag pushes to the existing workflow trigger and run the deploy job only for those tags. Strip the leading `v` to produce the exact image version, then perform one build that pushes both the version and `latest` tags.

After the repository and GHCR package are public, grant the deploy job `attestations: write` and `id-token: write`. Run the supported `actions/attest-build-provenance` action after image publication, using `ghcr.io/aminvakil/wormtamer` as the subject name, the build step's digest as the subject digest, and registry publication enabled.

## Risks and Open Questions

- GitHub does not provide artifact attestations for user-owned private repositories; repository visibility is a prerequisite.
- GitHub release creation remains independent of the workflow, so a release may appear before image publication finishes or may remain if publication fails.
- The attestation must use the digest output from the same build step that pushed both image tags, not a mutable tag resolved later.
- `latest` intentionally moves only from tagged builds; untagged `main` commits are not published.

## Verification

- Pull requests and pushes to `main` build and runtime-test Wormtamer without publishing an image.
- Pushing `v1.2.3` runs the existing checks and publishes only `1.2.3` and `latest`; both tags resolve to the same digest.
- GitHub and GHCR expose provenance whose subject is that exact published digest.
- GitHub CLI verification succeeds for both `ghcr.io/aminvakil/wormtamer:1.2.3` and `ghcr.io/aminvakil/wormtamer:latest` against the `aminvakil/wormtamer` repository.
- A publication or attestation failure fails the workflow; because attestation follows publication, an attestation failure may leave both image tags published.
