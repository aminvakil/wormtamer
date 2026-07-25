# Capture explicit review feedback

Status: proposed

Depends on: [Publish addressable review findings](publish-addressable-findings.md)

## Goal

Durably capture explicit human feedback about an addressable Wormtamer finding so accepted findings, false positives, corrections, and other supported outcomes can become auditable evidence for later learning.

## Scope

- Define a minimal GitLab interaction that references a Wormtamer finding and states an explicit feedback outcome.
- Authenticate, bound, authorize, deduplicate, and persist relevant GitLab feedback webhooks before acknowledging them.
- Preserve the finding, source project, actor identity, feedback category, bounded explanation, delivery identity, and timestamps.
- Retain conflicting feedback as separate evidence rather than overwriting history.

Do not infer feedback from silence, merge status, code changes, discussion resolution, emoji, or unrelated comments. Do not promote feedback directly to trusted memory or let feedback modify jobs and publications.

## Approach

Extend webhook ingress with a separate event parser and transaction for the selected feedback interaction. The interaction may be a structured note command, a reply contract, or another GitLab-native action, but that public contract and the actors allowed to submit each outcome must be decided before this plan is approved.

Webhook authentication proves delivery from GitLab, not that the named actor is trusted. Store contributor feedback as attributed untrusted evidence. Any role lookup or maintainer approval goes through the trusted GitLab broker and fails closed when identity or authorization cannot be established.

Use stable GitLab delivery or object identifiers for idempotency. Keep feedback records separate from workflow jobs and runtime memory even if all use SQLite.

## Risks and Open Questions

- The least confusing GitLab interaction for accepted, rejected, and corrected findings has not been selected.
- GitLab note events may be edited or deleted; the event model must preserve audit history and define how corrections supersede earlier feedback.
- Webhook payload retention currently has no policy and adding comment text increases sensitive stored content.
- Project membership can change after feedback; records need the authorization decision and time without treating future role changes as retroactive proof.

## Verification

- Repeated delivery of the same supported feedback creates one durable event and one feedback record before success is returned.
- Feedback resolves only to an addressable finding in the correct GitLab instance and project.
- Unknown findings, malformed commands, oversized text, unauthorized repositories, and unsupported events cannot alter memory or review state.
- Two humans can provide conflicting feedback without either record being silently discarded.
- Logs contain bounded identifiers and outcomes, not comment bodies or credentials.
