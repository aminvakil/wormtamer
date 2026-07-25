# Retrieve approved review memory

Status: proposed

Depends on:

- [Inspect the current repository with bounded tools](inspect-current-repository.md)
- [Curate runtime review memory](curate-runtime-review-memory.md)

## Goal

Allow Gemini to retrieve relevant, approved review lessons during a review while preserving memory scope, provenance, and application-controlled trust boundaries.

## Scope

- Add a read-only memory tool to the explicit Gemini function-calling loop.
- Return only approved, current records whose installation and repository scope permits use in the active review.
- Bound queries, candidates, result bytes, call count, and time.
- Attribute every result with its memory identity, scope, evidence reference, confidence, approval, and update time.

Do not let Gemini create, approve, edit, or broaden memory; include unapproved or superseded records; expose raw feedback unnecessarily; or persist arbitrary model conversation.

## Approach

A trusted memory broker converts validated structured queries into deterministic scoped retrieval. Authorization and approval filters are applied before relevance ranking so model wording cannot widen access. The tool presents memory as untrusted guidance subordinate to current code and team policy. A successful query with no applicable records returns an empty result. Broker or SQLite errors propagate into the existing job retry or failure workflow; the review must not continue after requested context could not be retrieved.

Start with the smallest retrieval mechanism that works for measured memory volume. Do not add eager embeddings, a vector database, or external search service without evidence that bounded SQLite retrieval is insufficient. Record only the bounded tool audit needed to explain which memory influenced a review.

## Risks and Open Questions

- Poor ranking can make correct memory ineffective or flood the context with irrelevant guidance.
- Feedback-derived text can contain prompt injection even after human approval; approval establishes usefulness, not executable authority.
- The relationship between cross-repository memory and cross-repository source visibility must remain explicit.
- Retrieval audits need a retention policy and must not become prompt or source archives.

## Verification

- A review can retrieve an applicable approved lesson and receives its scope and evidence attribution.
- Unapproved, rejected, superseded, out-of-scope, and other-installation records are never returned.
- Model requests cannot alter approval filters, increase bounds, or write memory.
- A successful query with no applicable memory returns an empty result and the review may continue.
- A broker or SQLite retrieval error stops the review and enters the existing retry or failure workflow without publishing a degraded result.
- Tests demonstrate that current repository evidence overrides conflicting memory in the final validation path.
