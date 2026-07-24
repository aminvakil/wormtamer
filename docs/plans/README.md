# Task Plans

Use a plan for substantial work that changes architecture, security boundaries, persistent state, public contracts, or several components. Routine fixes, contained implementation work, and documentation cleanup do not need permanent plan files.

## Proposed Roadmap

Implement plans in numeric order unless a plan's status or dependencies are explicitly changed:

1. [Inspect the current repository with bounded tools](01-inspect-current-repository.md)
2. [Inspect authorized related repositories](02-inspect-authorized-repositories.md)
3. [Publish addressable review findings](03-publish-addressable-findings.md)
4. [Capture explicit review feedback](04-capture-explicit-review-feedback.md)
5. [Curate runtime review memory](05-curate-runtime-review-memory.md)
6. [Retrieve approved review memory](06-retrieve-review-memory.md)
7. [Evaluate feedback-driven reviews](07-evaluate-feedback-driven-reviews.md)
8. [Research constrained public sources](08-research-public-sources.md)
9. [Update the project README for completed capabilities](09-update-project-readme.md)

All roadmap plans are proposed. Resolve each plan's open policy and interaction questions before approving implementation.

## KISS

Every roadmap item follows the repository-wide [KISS rules](../../AGENTS.md#kiss). A plan defines direction, not an obligation to handle every conceivable condition. Before approval, narrow it to the smallest safe, complete outcome supported by current requirements and evidence. Risks and open questions do not expand implementation scope unless they block that outcome.

## Format

- Name roadmap plans with a two-digit implementation-order prefix and short kebab-case outcome, such as `01-inspect-current-repository.md`.
- Describe one coherent outcome per file.
- Use [`_template.md`](_template.md) as optional guidance.
- Include only sections that help resolve or verify the work.

## Lifecycle

Use `proposed`, `approved`, or `in-progress` as the status. Keep the plan accurate when material scope or decisions change.

After implementation:

1. Move durable decisions into the relevant document under `docs/agents/`.
2. Delete the completed plan; Git retains its history.
3. Add a lesson only for a non-obvious finding supported by evidence.

Plans describe intent and observable acceptance criteria. They are not work logs: do not record routine command output, successful test runs, link checks, or `git diff` inspection.
