# Task Plans

Use a plan for substantial work that changes architecture, security boundaries, persistent state, public contracts, or several components. Routine fixes, contained implementation work, and documentation cleanup do not need permanent plan files.

## KISS

Every roadmap item follows the repository-wide [KISS rules](../../AGENTS.md#kiss). A plan defines direction, not an obligation to handle every conceivable condition. Before approval, narrow it to the smallest safe, complete outcome supported by current requirements and evidence. Risks and open questions do not expand implementation scope unless they block that outcome.

## Roadmap

Implement approved plans in this order:

1. [Use all-or-nothing repository sharing](all-or-nothing-repository-sharing.md)
2. [Remove process-local diagnostic buffers](remove-process-diagnostic-buffers.md)
3. [Remove model usage and pricing persistence](remove-model-usage-and-pricing.md)
4. [Recover jobs without leases](recover-jobs-without-leases.md)

The final two plans both change SQLite state. Complete them in one unreleased sequence with one final schema rebaseline rather than adding an intermediate migration.

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
