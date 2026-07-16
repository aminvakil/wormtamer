# Task Plans

Use this directory for implementation plans. Each task has one plan file so its motivation, boundaries, decisions, and verification remain reviewable without being mixed with unrelated work.

Use [`_template.md`](_template.md) when creating a plan. The template is guidance, not a requirement to fill irrelevant sections with boilerplate.

## One Task per File

Name plans with a short kebab-case description:

```text
docs/plans/durable-webhook-ingestion.md
docs/plans/gitlab-comment-publication.md
docs/plans/runtime-memory-retrieval.md
```

Do not put several independent tasks into one roadmap file. Do not create separate plan files for every tiny code edit involved in one coherent outcome.

A plan file should remain after completion as an explanation of the intended change and its verification. Update its status rather than deleting it.

## Task Size

A well-sized task has:

- One primary outcome
- A clear boundary and explicit non-goals
- A change that can be reviewed as one coherent unit
- Verification that demonstrates the outcome independently
- Enough value to justify implementation and review on its own

### Too small

A task is probably too small when it only describes:

- Renaming one symbol as part of a larger change
- Adding a helper used solely by the next planned task
- Adding a schema field without the behavior that consumes it
- Adding tests separately from the behavior they verify
- Mechanical edits that cannot provide value independently

Combine these with the task that makes them meaningful.

### Too large

A task is probably too large when it:

- Has multiple independent user-visible outcomes
- Crosses unrelated subsystem boundaries
- Requires separate architectural decisions that can be delivered independently
- Cannot be explained with one primary goal
- Has verification covering several unrelated behaviors
- Would leave reviewers unable to assess or revert it as one unit

Split it into ordered task files and state dependencies explicitly. Every split task should still leave the repository in a valid state.

File count and line count are warning signals, not sizing rules. A cross-cutting reliability change may legitimately touch several files; a one-file rewrite may still be too large.

## Plan Lifecycle

Use one of these statuses:

- `proposed`: written but not yet agreed
- `approved`: agreed and ready to implement
- `in-progress`: currently being implemented
- `completed`: implemented and verified
- `superseded`: replaced by another named plan or decision

Before implementation:

1. Inspect the current code and relevant agent documentation.
2. State assumptions and unresolved questions.
3. Describe the outcome, approach, affected areas, risks, and verification.
4. Identify dependencies on other plan files.
5. Obtain approval when the task changes architecture, public contracts, security boundaries, persistent state, or several components.

During implementation:

- Keep the plan accurate when material scope or decisions change.
- Do not silently add unrelated work.
- Split newly discovered independent work into another plan.
- Record meaningful deviations and their reasons.

After implementation:

- Record the verification actually performed.
- Mark the plan `completed` only when its success criteria pass.
- Move durable architectural or security decisions into `docs/agents/`; a completed plan is history, not the only source of current policy.
- Add genuinely non-obvious findings to `docs/agents/lessons-learned.md`.

## Content Guidelines

A plan should normally contain:

- Summary and status
- Context and current behavior
- Goal and non-goals
- Proposed approach
- Expected changes by component or file when known
- Reliability and security implications when relevant
- Trade-offs and rejected alternatives
- Verification and success criteria
- Dependencies and open questions

Plans should describe intent and boundaries, not prescribe speculative low-level code before the repository has enough evidence. Reference exact files and symbols only after confirming they exist.
