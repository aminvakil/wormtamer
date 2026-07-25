# Research constrained public sources

Status: proposed

Depends on: [Inspect the current repository with bounded tools](inspect-current-repository.md)

## Goal

Let reviews consult public documentation and public repositories for versioned API behavior or upstream evidence that is unavailable in authorized internal repositories.

## Scope

- Add read-only tools for explicitly requested public HTTPS resources and public repository revisions.
- Block loopback, link-local, private, metadata-service, internal, and non-HTTPS destinations before and after DNS resolution and redirects.
- Bound requests, redirects, response types, response bytes, retrieval time, and total public research per review.
- Attribute results with source URL, immutable revision when available, and retrieval time.

Do not enable Gemini search grounding, URL retrieval, a general browser, authenticated public services, arbitrary network access, or transmission of private source and full internal diffs in search queries.

## Approach

Add a dedicated public-source broker rather than reusing the credentialed GitLab client. Trusted application code validates structured requests, resolves and revalidates network destinations, fetches bounded content, and returns untrusted attributed evidence to the explicit model tool loop.

Prefer immutable repository objects and canonical documentation URLs. The query contract must restrict what internal context can leave the installation; public content never grants permission or changes tool policy.

## Risks and Open Questions

- DNS rebinding, redirects, alternate IP encodings, and proxy configuration can bypass naive destination checks.
- Search discovery may require a provider or index; no provider should be selected until direct URL and public-repository retrieval proves insufficient.
- Licensing and publication rules may limit how much public content can be reproduced in a review note.

## Verification

- A review can retrieve bounded public documentation or a public file at an attributed revision and use it as evidence.
- Private, local, metadata-service, redirected-to-private, oversized, unsupported, and slow destinations fail without returning content.
- Requests contain no configured secret, private source excerpt, or full internal diff.
- Public instructions cannot invoke undeclared tools, access internal repositories, or publish findings directly.
