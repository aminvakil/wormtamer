# Task Plans

Use a plan for substantial work that changes architecture, security boundaries, persistent state, public contracts, or several components. Routine fixes, contained implementation work, and documentation cleanup do not need permanent plan files.

## Format

- Name the file with a short kebab-case outcome.
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
