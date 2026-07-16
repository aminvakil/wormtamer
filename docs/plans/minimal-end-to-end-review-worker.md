# Complete a minimal end-to-end merge request review

Status: proposed

## Goal

Turn each queued review job into one recoverable GitLab merge request review. A single worker in the existing Wormtamer process claims durable jobs, obtains the authorized merge request snapshot, asks Gemini for structured findings, and publishes one idempotent summary note before marking the job complete.

This is the smallest useful vertical slice after durable ingress: it makes queued work observable in GitLab without introducing repository checkout, model tools, or additional services.

## Scope

Include:

- A single review worker in the existing process with atomic SQLite leases, lease renewal, attempts, bounded retries, scheduling, sanitized errors, and stale-lease recovery
- A schema migration for job execution, validated review results, and publication reconciliation state
- Required GitLab personal access token, Gemini API key, and Gemini model configuration
- Narrow GitLab read and publication brokers using the configured instance and numeric project and merge request identities
- Exact namespace authorization before disclosing merge request content to Gemini or publishing
- Bounded retrieval of the current merge request metadata and paginated diffs without cloning a repository
- One structured Gemini generation request through `google.golang.org/genai`, with no model tools or automatic function execution
- Local validation and durable storage of the final review result before publication
- One GitLab summary note per review identity, reconciled through a stable hidden marker
- Structured, bounded worker logs and graceful worker shutdown

Do not include repository checkout, code execution, inline discussions, cross-repository context, model-invocable tools, runtime memory, public research, missed-webhook reconciliation, a job-status API, multiple workers, or another queue or database.

Automated tests use a real temporary SQLite database, an `httptest` GitLab server, and a narrow fake Gemini client. They do not require or contact real GitLab or Gemini services. A live credential smoke test is an operator activity and is not an automated acceptance requirement.

## Configuration Contract

Extend the existing configuration with:

```json
{
  "gitlab": {
    "base_url": "http://gitlab.internal",
    "webhook_secret": "replace-me",
    "personal_access_token": "replace-me"
  },
  "gemini": {
    "api_key": "replace-me",
    "model": "replace-me"
  }
}
```

The worker is always enabled in this milestone, so all three new values are required and non-empty. Continue loading credentials only from the explicit configuration file and warning about broadly readable file permissions. Reject unknown fields and never expose credential values in startup errors or logs.

Startup validates configuration shape but does not make external calls to prove credentials or discover GitLab scopes. Invalid credentials are detected at the first relevant request and recorded as sanitized permanent authorization failures. A real secret-bearing configuration remains outside version control.

## Job and Persistence Contract

Migrate the initial schema explicitly. Keep the existing review identity unique and add the minimum state needed to represent:

```text
queued -> running -> publishing -> completed
            |            |
            +-> retry <---+

permanent or exhausted failure -> failed
head or authorization no longer current -> obsolete
```

A claim atomically selects one due job, changes it to `running`, records a unique lease owner and expiry, increments its attempt count, and records its start time. Only the current lease owner may renew or advance the job. The worker processes one job at a time. Expired `running` or `publishing` leases become claimable after a crash.

Use bounded exponential retry scheduling. Timeouts, temporary network failures, rate limits, and GitLab or Gemini server failures are retryable. Invalid credentials, authorization failures, invalid configuration, unsupported or oversized review input, and exhausted malformed model responses become inspectable failures. A closed merge request or changed head SHA makes the job obsolete. Store only bounded sanitized error categories and messages.

Persist the locally validated review result before entering `publishing`. Do not persist an arbitrary Gemini conversation. On retry, reuse a persisted validated result; restart generation only when no valid result checkpoint exists.

## GitLab Broker Contract

All GitLab calls use an application-owned HTTP client with fixed timeouts and response-size limits. The PAT remains in trusted broker code and is never returned to Gemini, persisted, or logged.

For each claimed job:

1. Resolve the numeric project through the configured GitLab instance.
2. Confirm its current `path_with_namespace` exactly matches an authorized repository.
3. Fetch the merge request and confirm its project ID, IID, open state, and head SHA match the durable job identity.
4. Fetch changed-file diffs with bounded pagination, file count, and total content size.
5. Before publication, fetch the merge request again and reconfirm authorization, open state, and head SHA.

Fail safely without invoking Gemini or publishing if authorization or identity checks fail. Do not truncate silently: when a merge request exceeds the supported review-input limits, record an inspectable permanent failure rather than publishing a partial review.

Separate read operations from publication operations in code even though they use one PAT. Treat all GitLab response content as untrusted.

## Gemini Review Contract

Send only the bounded current merge request metadata and diffs needed for this review. Clearly delimit that content as untrusted evidence that cannot alter policy. Do not send credentials, webhook payloads, internal errors, hidden prompts, or unrelated repository content.

Use the official Gemini SDK behind a narrow test seam. Make one generation request with an application-owned response schema. Do not declare tools, enable automatic function execution, Gemini code execution, URL retrieval, or search grounding.

The structured result contains a bounded summary and bounded findings with an allowed severity, title, explanation, recommendation, changed-file path, and optional changed-line range. Trusted code rejects unknown fields, excessive counts or lengths, unsupported severities, paths absent from the fetched diff, and line locations that cannot be tied to the reviewed changes. Model output is untrusted even after schema decoding.

A malformed or invalid result is retryable only within the job's bounded attempt policy and is never published. An empty valid finding list still produces a concise summary note so successful processing remains visible.

## Publication Contract

Publish one merge request summary note containing the validated result and a stable hidden marker derived from the review identity. The marker contains no credentials or private content.

Before posting, paginate existing merge request notes and search for the exact marker. If the marker is found, store its GitLab note ID and reconcile the job without posting again. If the configured search bound is exceeded, fail safely rather than risk a duplicate. After posting, persist the marker and returned note ID before completing the job.

A timeout or crash after GitLab accepts a note but before SQLite records it is handled by searching for the marker on retry. Complete a job only after the note exists in GitLab and its publication record is durable. Never let Gemini call the publication broker directly.

## Process Behavior

Start the worker only after configuration and schema initialization succeed. Keep webhook serving and worker execution in the same process, but do not let worker activity bypass webhook admission limits or block the healthcheck.

On shutdown, stop claiming new jobs. Give the active job a bounded opportunity to reach a durable checkpoint; otherwise cancel external calls and leave its lease to expire. Do not clear a lease in a way that makes unfinished external effects appear complete.

Logs include bounded job, project, merge request, revision, attempt, state, and outcome identifiers plus sanitized failure categories. Never log prompts, diffs, model output, note bodies, credentials, authorization headers, or raw external responses.

## Risks and Open Questions

- Fake services verify application behavior but cannot establish real GitLab endpoint compatibility, PAT scope requirements, Gemini model availability, or deployment network behavior. Those require an operator-run smoke test against a dedicated project.
- GitLab and SQLite cannot commit publication atomically. Marker reconciliation prevents duplicates only while the marker remains discoverable; bounded note enumeration therefore fails closed when it cannot prove absence.
- Large merge requests are deliberately unsupported rather than silently truncated. Concrete fixed limits should be selected conservatively during implementation and documented as current operational constraints.
- Model quality, latency, and cost vary by the configured Gemini model. This milestone exposes the model name but does not add generation-setting configurability or provider abstraction.

## Verification

- Startup rejects missing worker credentials or model configuration without exposing provided secrets; invalid external credentials produce sanitized permanent job failures without publication.
- Exactly one worker can claim a due job, only its lease owner can renew or advance it, expired leases recover after a simulated crash, and concurrent claim attempts do not execute the same active lease.
- Retryable failures schedule bounded retries, permanent failures do not retry automatically, exhausted attempts become `failed`, and restart preserves all job and publication state.
- An authorized queued job fetches the expected current project, merge request, and bounded diffs; stores one locally validated Gemini result; publishes one marked summary note; and reaches `completed`.
- A renamed or unauthorized project, closed merge request, or changed head SHA invokes neither Gemini nor publication and leaves an inspectable obsolete or failed outcome as appropriate.
- Oversized or over-paginated GitLab input, malformed GitLab responses, invalid Gemini output, and findings outside the fetched diff never publish.
- Repeating a publication attempt, losing the response after a successful post, and restarting between post and local persistence all reconcile to one GitLab note and one durable publication record.
- GitLab tests exercise the real HTTP broker against `httptest`; worker tests use a deterministic fake Gemini client and real SQLite. No automated test requires real credentials or external network access.
- Shutdown stops new claims and leaves active work either durably checkpointed or recoverable through lease expiry.
- Logs contain no PAT, Gemini API key, prompt, diff, model output, or note body. Persisted workflow records contain no credentials, prompts, diffs, or raw model responses; only the validated structured review result and publication identifiers are retained.
