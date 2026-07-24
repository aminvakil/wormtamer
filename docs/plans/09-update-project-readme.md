# Update the project README for completed capabilities

Status: proposed

Depends on: plans 01 through 08 in the [proposed roadmap](README.md)

## Goal

Update the root `README.md` after the implementation roadmap is complete so its product description, capabilities, limitations, configuration guidance, and documentation links accurately describe the delivered repository inspection and feedback-driven learning behavior.

## Scope

- Replace statements that describe the current diff-only reviewer with verified descriptions of repository tools, authorized cross-repository inspection, approved runtime memory, feedback handling, evaluation, and constrained public research.
- Update user-facing setup and operation guidance for configuration or GitLab webhook requirements introduced by the completed plans.
- Keep the quick start concise and link to authoritative architecture, reliability, security, and deployment documentation for details.
- Retain explicit limitations, especially single-tenant and one-replica operation, application-enforced authorization, bounded read-only tools, and the prohibition on repository-controlled code execution.

Do not document planned behavior as implemented, copy detailed design rules into the README, or change application behavior as part of this documentation task.

## Approach

Complete this plan only after plans 01 through 08 have been implemented and their durable decisions have moved into the focused documents under `docs/agents/`. Audit every root README claim against the resulting code and configuration, remove obsolete diff-only descriptions, and present the smallest end-to-end explanation needed by a new operator or evaluator.

Use links instead of duplicating detailed security and reliability contracts. If a roadmap capability was changed or deferred, document the actual final behavior rather than the original plan.

## Risks and Open Questions

- Completing plans at different scopes than proposed can make a mechanical roadmap summary inaccurate; the README must be based on shipped behavior.
- New feedback webhooks, permissions, or configuration may also require deployment-document updates during their own implementation and should not be deferred solely to this final README pass.

## Verification

- A new reader can determine what repositories and sources Wormtamer can inspect, how explicit feedback becomes approved memory, and which limitations still apply.
- Setup examples match the final configuration schema and required GitLab events and permissions.
- The README contains no claim for an unimplemented or deferred roadmap capability and no obsolete statement that reviews use only merge request diffs.
- Detailed behavior has one authoritative home under `docs/agents/` or `docs/deployment.md`, with the README linking to it rather than repeating it.
