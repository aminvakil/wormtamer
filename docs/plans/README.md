# Task Plans

Use a plan for substantial work that changes architecture, security boundaries, persistent state, public contracts, or several components. Routine fixes, contained implementation work, and documentation cleanup do not need permanent plan files.

## Proposed

- [Inspect and retry failed jobs](inspect-and-retry-failed-jobs.md): add bounded local status and safe post-correction retry commands for review and feedback work.
- [Automate dependency maintenance and vulnerability scanning](automate-dependency-maintenance.md): cover Go modules and Docker with Dependabot and run a reproducible `govulncheck` CI gate.

## Deferred

- [Evaluate feedback-driven reviews](evaluate-feedback-driven-reviews.md): revisit after a deployed installation has accumulated enough meaningful structured feedback to define useful measures and sample sizes from evidence.

## KISS

Every roadmap item follows the repository-wide [KISS rules](../../AGENTS.md#kiss). A plan defines direction, not an obligation to handle every conceivable condition. Before approval, narrow it to the smallest safe, complete outcome supported by current requirements and evidence. Risks and open questions do not expand implementation scope unless they block that outcome.

## Format

- Name each plan with a stable short kebab-case outcome; record implementation order only in the roadmap.
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
