# Evaluate review generation settings on current merge requests

Status: proposed

Depends on:

- [Improve model-facing instructions](improve-model-facing-instructions.md)

## Goal

Determine whether high thinking improves actionable-finding recall on operator-selected current merge requests and whether the existing 8,192-token per-generation ceiling constrains high-thinking reviews. Use the normal Wormtamer worker, repository tools, validation, persistence, and GitLab publication path so the result reflects deployed behavior rather than an isolated prompt example.

## Deliberately Redacted Evaluation Cases

Repository paths, GitLab instance details, MR numbers, commit SHAs, titles, diffs, expected findings, prohibited findings, and resulting comments are intentionally absent from this repository. They may identify private infrastructure and must not be added to this plan, another tracked file, a commit message, or default test output.

Before execution, stop and ask the operator to provide the private evaluation manifest in the current trusted session. Do not infer or recover it from tracked documentation. Request, for every case:

- Exact GitLab repository path, MR IID, and expected head SHA.
- Whether the case is an expected-finding case, an expected-clean case, or a false-positive control.
- For an expected finding, a nuanced outcome describing the changed behavior, triggering conditions, concrete affected caller or configuration semantics, acceptable changed-file anchor paths, the actionable correction, and claims that would be inaccurate or overstated.
- For an expected-clean case, why the change is acceptable and which tempting generic or speculative findings must not be reported.
- Any acceptable variation in title, severity, explanation, or recommendation that should not affect classification.
- Whether bounded repository tool use is expected, including the specific reference search or contextual read whose absence would explain a miss.

Also ask the operator to confirm the exact model endpoint, trial count, configuration path, database path, set of authorized projects, expected open non-draft MR set, bot identity, and permission for each destructive or external action: stopping Wormtamer, deleting the database and sidecars, deleting exact Wormtamer-authored review notes, publishing replacement notes, and leaving the final accepted notes in GitLab.

Keep this manifest transient. If a temporary local file is required, create it outside the repository with restrictive permissions, never print credentials, exclude prompt and repository content from default output, and remove the file after classification. Persist or commit only the eventual application setting and general rationale, not private case identities or outputs.

## Scope

- Pin the evaluation to the exact stable Flash-Lite endpoint selected for deployment.
- Compare default thinking with high thinking after the model-facing instruction work is complete.
- Evaluate a larger output-token ceiling only when finish reasons or available usage evidence show that 8,192 tokens constrained a generation turn.
- Run the selected variants repeatedly against the exact operator-supplied MR revisions.
- Remove Wormtamer's SQLite database, WAL, and shared-memory files between trials so each trial starts without jobs, results, publication records, or runtime memory.
- Remove only the existing Wormtamer-authored review notes for the selected MR revisions before a repeated trial so the deterministic publication marker cannot reconcile an earlier note instead of posting that trial's result.
- Let the final accepted trial's GitLab comments remain when authorized.
- Collect bounded non-content generation metadata and classify the published findings against the transient manifest.

Do not add the private manifest to version control, add a second reviewer or verification pass, use multiple candidates, enable automatic tool execution, execute repository-controlled code, add prompt optimization, add a permanent experiment identifier to publication markers, or expose thinking and token settings as deployment configuration. Do not delete human-authored notes, modify MR branches, trigger empty commits, or treat more findings as an improvement without checking their actionability.

## Suitable Outcomes

A suitable expected finding must identify a concrete changed behavior, the conditions that trigger it, the affected caller or operational semantics, and an actionable correction supported by the diff or bounded repository context. It must anchor to a supplied changed path and must avoid stronger claims than the evidence supports.

An expected-clean result must contain no actionable findings. Generic maintainability, portability, hardcoded-value, cleanup, naming, or "verify this" advice does not become suitable merely because it is plausible.

A false-positive control must explicitly name the tempting but unsupported interpretation in the transient manifest. A differently worded finding with the same unsupported substance still fails the control.

Classification must use the complete finding explanation and recommendation rather than exact title matching. Reasonable wording and severity variation is acceptable when the concrete behavior and correction match the operator-supplied outcome.

## Approach

Use a temporary evaluation configuration derived without printing credentials. Restrict it to the operator-approved projects, bind webhook ingress to loopback, preserve restrictive configuration permissions, and abort if reconciliation would include an unexpected non-draft MR. Keep debug logging disabled unless a specific tool-selection failure requires a protected short-lived diagnostic run.

Build application-owned generation variants from the same post-instruction source:

1. Default thinking with 8,192 maximum output tokens.
2. High thinking with 8,192 maximum output tokens.
3. High thinking with 16,384 maximum output tokens only if variant 2 has `MAX_TOKENS`, invalid completion attributable to truncation, or available output-plus-thinking usage consistently approaches the ceiling.
4. High thinking with 32,768 maximum output tokens only if the same evidence remains at 16,384 and the model's documented limit permits it.

Run each executed variant for the operator-approved repetition count, defaulting to three. Before every trial, stop Wormtamer, verify the supplied MR heads and expected open-MR set, delete only bot-authored notes containing the exact Wormtamer marker for the supplied identities, and remove the configured SQLite database together with its `-wal` and `-shm` files. Database deletion alone is insufficient because GitLab notes survive and marker reconciliation would bind a newly generated result to an old note. Start one Wormtamer process, let reconciliation finish the selected jobs, capture bounded outcomes and comments transiently, then stop it before the next reset.

For every Gemini generation turn, record the configured endpoint, resolved model version when returned, candidate finish reason, candidate token count, available output and thinking token counts, elapsed time, tool-call count, and final validation outcome. Do not record thought text. Treat finish reason as the primary truncation evidence; total token count includes prompt input and must not be compared directly with the output ceiling.

Classify each published review against the transient expected outcome. Record only transiently whether each required issue was found, whether its explanation matches the concrete behavior, whether expected repository context was requested, and whether an unsupported finding appeared. Do not use summary approval language as evidence of correctness.

Stop at high thinking with 8,192 tokens when all turns finish normally with adequate available headroom and a larger ceiling has no evidenced purpose. A missed expected issue with a normal finish and substantial headroom is a reasoning or instruction failure, not evidence for raising the ceiling.

## Acceptance Constraints

- High thinking must detect every operator-designated expected issue in at least two of three trials, or the equivalent operator-approved repetition threshold.
- Findings must match the nuanced expected behavior and avoid explicitly prohibited overclaims.
- No trial accepted for deployment may produce a finding prohibited by a false-positive control.
- High thinking must not improve recall merely by adding unsupported findings to expected-clean cases.
- Every accepted finding must pass the existing changed-path and structured-result validation without weakening either rule.
- No selected generation turn may complete under `MAX_TOKENS` or another incomplete finish reason.
- A larger output ceiling is accepted only when the immediately smaller ceiling shows truncation or measured pressure and the larger ceiling resolves it without violating the finding-quality constraints.
- The selected setting must complete within the existing two-minute review-loop deadline and remain operable under observed free-tier rate limits.

If high thinking at 8,192 tokens satisfies these constraints, keep the current ceiling and do not run larger-ceiling variants. If high thinking does not satisfy recall while all turns finish normally with headroom, do not raise the ceiling; revise model-facing behavior or reconsider model capability in a separate task.

## Risks and Open Questions

This procedure intentionally deletes Wormtamer-authored GitLab notes and local review state for operator-supplied test identities. Exact author and marker checks and explicit execution-time authorization are mandatory so human notes and unrelated bot output cannot be deleted. The process must be stopped before database removal, and one replica must remain the invariant.

Deleting the database also removes runtime memory and prior workflow evidence, so each trial measures review behavior without accumulated memory. That is desirable for comparing generation settings but does not evaluate feedback-driven improvement.

The selected cases and repetitions expose regressions and obvious instability but do not establish a general accuracy rate. They are evidence for operator-specified observed behavior, not a benchmark for all repositories.

## Verification

- The committed repository contains no private evaluation manifest, repository identity, MR IID, revision, expected private behavior, or captured review output.
- Execution does not begin until the operator supplies the transient manifest and explicitly confirms every destructive and publishing action.
- Every trial uses the pinned endpoint, exact supplied heads, same post-instruction prompt and tool contracts, and one Wormtamer process.
- Unexpected open non-draft MRs, changed heads, failure to identify the exact bot author, or failure to establish exact marker ownership aborts the trial before destructive actions or publication.
- Only exact Wormtamer-authored review notes for supplied identities are removed; human and unrelated notes remain untouched.
- SQLite, WAL, and shared-memory state are absent before each process starts and are never removed while Wormtamer is running.
- Published comments, locally validated results, finish reasons, available usage, latency, and tool selection can be associated transiently with a specific variant and repetition without retaining private prompts, repository content, or thought text.
- The acceptance constraints determine whether high thinking and any larger ceiling are adopted; a subjective increase in comment volume does not.
- The final accepted trial's comments remain on GitLab when authorized, and superseded trial comments do not remain as ambiguous reviews.
