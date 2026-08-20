package store

const currentSchema = `
CREATE TABLE webhook_events (
    id INTEGER PRIMARY KEY,
    delivery_id TEXT NOT NULL UNIQUE,
    gitlab_instance TEXT NOT NULL,
    project_id INTEGER NOT NULL,
    project_path TEXT NOT NULL,
    merge_request_iid INTEGER NOT NULL,
    head_sha TEXT NOT NULL,
    action TEXT NOT NULL,
    outcome TEXT NOT NULL,
    payload_json BLOB NOT NULL CHECK(length(payload_json) <= 1048576),
    received_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

CREATE TABLE review_jobs (
    id INTEGER PRIMARY KEY,
    source_event_id INTEGER REFERENCES webhook_events(id),
    gitlab_instance TEXT NOT NULL,
    project_id INTEGER NOT NULL,
    merge_request_iid INTEGER NOT NULL,
    head_sha TEXT NOT NULL,
    state TEXT NOT NULL DEFAULT 'queued',
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    lease_owner TEXT,
    lease_expires_at TEXT,
    attempt_count INTEGER NOT NULL DEFAULT 0,
    started_at TEXT,
    next_attempt_at TEXT,
    last_error_category TEXT,
    last_error_message TEXT,
    updated_at TEXT,
    patch_id_sha TEXT
        CHECK(
            patch_id_sha IS NULL OR (
                length(patch_id_sha) IN (40, 64) AND
                patch_id_sha = lower(patch_id_sha) AND
                patch_id_sha NOT GLOB '*[^0-9a-f]*'
            )
        ),
    patch_id_status TEXT NOT NULL DEFAULT 'unknown'
        CHECK(patch_id_status IN ('unknown', 'pending', 'available', 'unavailable'))
        CHECK((patch_id_status = 'available') = (patch_id_sha IS NOT NULL)),
    equivalent_to_job_id INTEGER REFERENCES review_jobs(id)
        CHECK(equivalent_to_job_id IS NULL OR equivalent_to_job_id != id)
        CHECK(equivalent_to_job_id IS NULL OR patch_id_status = 'available'),
    UNIQUE (gitlab_instance, project_id, merge_request_iid, head_sha)
);

CREATE INDEX review_jobs_due_idx
ON review_jobs (state, next_attempt_at, lease_expires_at);

CREATE INDEX review_jobs_patch_id_idx
ON review_jobs (
    gitlab_instance, project_id, merge_request_iid,
    patch_id_status, patch_id_sha, id DESC
);

CREATE TABLE review_results (
    job_id INTEGER PRIMARY KEY REFERENCES review_jobs(id) ON DELETE CASCADE,
    result_json BLOB NOT NULL CHECK(length(result_json) <= 65536),
    created_at TEXT NOT NULL
);

CREATE TABLE publications (
    job_id INTEGER PRIMARY KEY REFERENCES review_jobs(id) ON DELETE CASCADE,
    marker TEXT NOT NULL UNIQUE CHECK(length(marker) <= 256),
    gitlab_note_id INTEGER NOT NULL CHECK(gitlab_note_id > 0),
    created_at TEXT NOT NULL
);

CREATE TABLE review_findings (
    finding_id TEXT PRIMARY KEY
        CHECK(length(finding_id) = 31)
        CHECK(substr(finding_id, 1, 5) = 'WT-F-')
        CHECK(substr(finding_id, 6) NOT GLOB '*[^A-Z2-7]*'),
    job_id INTEGER NOT NULL REFERENCES review_results(job_id) ON DELETE CASCADE,
    finding_index INTEGER NOT NULL CHECK(finding_index >= 0 AND finding_index < 20),
    UNIQUE (job_id, finding_index)
);

CREATE TABLE review_memory_retrievals (
    job_id INTEGER NOT NULL REFERENCES review_results(job_id) ON DELETE CASCADE,
    memory_id TEXT NOT NULL
        CHECK(length(memory_id) = 31)
        CHECK(substr(memory_id, 1, 5) = 'WT-M-')
        CHECK(substr(memory_id, 6) NOT GLOB '*[^A-Z2-7]*'),
    memory_updated_at TEXT NOT NULL,
    retrieved_at TEXT NOT NULL,
    PRIMARY KEY (job_id, memory_id, memory_updated_at)
);

CREATE TABLE feedback_jobs (
    id INTEGER PRIMARY KEY,
    source_event_id INTEGER NOT NULL UNIQUE REFERENCES webhook_events(id),
    review_job_id INTEGER NOT NULL REFERENCES review_results(job_id),
    gitlab_instance TEXT NOT NULL,
    project_id INTEGER NOT NULL CHECK(project_id > 0),
    project_path TEXT NOT NULL,
    merge_request_iid INTEGER NOT NULL CHECK(merge_request_iid > 0),
    head_sha TEXT NOT NULL,
    terminal_state TEXT NOT NULL CHECK(terminal_state IN ('closed', 'merged')),
    state TEXT NOT NULL DEFAULT 'queued',
    lease_owner TEXT,
    lease_expires_at TEXT,
    attempt_count INTEGER NOT NULL DEFAULT 0,
    next_attempt_at TEXT NOT NULL,
    last_error_category TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE (gitlab_instance, project_id, merge_request_iid)
);

CREATE INDEX feedback_jobs_due_idx
ON feedback_jobs (state, next_attempt_at, lease_expires_at);

CREATE TABLE review_memories (
    memory_id TEXT PRIMARY KEY
        CHECK(length(memory_id) = 31)
        CHECK(substr(memory_id, 1, 5) = 'WT-M-')
        CHECK(substr(memory_id, 6) NOT GLOB '*[^A-Z2-7]*'),
    feedback_job_id INTEGER NOT NULL UNIQUE REFERENCES feedback_jobs(id) ON DELETE CASCADE,
    lesson TEXT NOT NULL CHECK(length(lesson) > 0 AND length(lesson) <= 4096),
    source_url TEXT NOT NULL CHECK(length(source_url) <= 2048),
    created_at TEXT NOT NULL
);

CREATE TABLE model_generations (
    id INTEGER PRIMARY KEY,
    request_kind TEXT NOT NULL CHECK(request_kind IN ('review', 'feedback')),
    review_job_id INTEGER REFERENCES review_jobs(id),
    feedback_job_id INTEGER REFERENCES feedback_jobs(id),
    workflow_attempt INTEGER NOT NULL CHECK(workflow_attempt > 0),
    review_turn INTEGER CHECK(review_turn IS NULL OR (review_turn >= 0 AND review_turn <= 1000)),
    configured_model TEXT NOT NULL CHECK(length(configured_model) > 0 AND length(configured_model) <= 256),
    resolved_model TEXT CHECK(resolved_model IS NULL OR length(resolved_model) <= 256),
    request_started_at TEXT NOT NULL,
    completed_at TEXT,
    completion_state TEXT NOT NULL CHECK(completion_state IN ('started', 'response', 'failed', 'unknown')),
    latency_ms INTEGER CHECK(latency_ms IS NULL OR (latency_ms >= 0 AND latency_ms <= 86400000)),
    finish_reason TEXT CHECK(finish_reason IS NULL OR length(finish_reason) <= 128),
    structured_validation TEXT CHECK(structured_validation IS NULL OR length(structured_validation) <= 128),
    tool_calls_available INTEGER NOT NULL DEFAULT 0 CHECK(tool_calls_available IN (0, 1)),
    tool_call_count INTEGER CHECK(tool_call_count IS NULL OR (tool_call_count >= 0 AND tool_call_count <= 32768)),
    tool_names_json BLOB CHECK(tool_names_json IS NULL OR length(tool_names_json) <= 65536),
    final_only INTEGER NOT NULL CHECK(final_only IN (0, 1)),
    usage_metadata_available INTEGER NOT NULL DEFAULT 0 CHECK(usage_metadata_available IN (0, 1)),
    usage_metadata_valid INTEGER NOT NULL DEFAULT 0 CHECK(usage_metadata_valid IN (0, 1) AND usage_metadata_valid <= usage_metadata_available),
    prompt_tokens INTEGER CHECK(prompt_tokens IS NULL OR (prompt_tokens >= 0 AND prompt_tokens <= 2147483647)),
    cached_tokens INTEGER CHECK(cached_tokens IS NULL OR (cached_tokens >= 0 AND cached_tokens <= 2147483647)),
    tool_use_prompt_tokens INTEGER CHECK(tool_use_prompt_tokens IS NULL OR (tool_use_prompt_tokens >= 0 AND tool_use_prompt_tokens <= 2147483647)),
    candidate_tokens INTEGER CHECK(candidate_tokens IS NULL OR (candidate_tokens >= 0 AND candidate_tokens <= 2147483647)),
    thought_tokens INTEGER CHECK(thought_tokens IS NULL OR (thought_tokens >= 0 AND thought_tokens <= 2147483647)),
    total_tokens INTEGER CHECK(total_tokens IS NULL OR (total_tokens >= 0 AND total_tokens <= 2147483647)),
    cost_source TEXT CHECK(cost_source IS NULL OR cost_source IN ('litellm_catalog', 'endpoint_response')),
    estimated_cost_picos INTEGER CHECK(estimated_cost_picos IS NULL OR estimated_cost_picos >= 0),
    CHECK(
        (request_kind = 'review' AND review_job_id IS NOT NULL AND feedback_job_id IS NULL AND review_turn IS NOT NULL) OR
        (request_kind = 'feedback' AND review_job_id IS NULL AND feedback_job_id IS NOT NULL AND review_turn IS NULL)
    ),
    CHECK(completion_state != 'started' OR completed_at IS NULL),
    CHECK(usage_metadata_valid = 0 OR (
        prompt_tokens IS NOT NULL AND prompt_tokens > 0 AND cached_tokens IS NOT NULL AND tool_use_prompt_tokens IS NOT NULL AND
        candidate_tokens IS NOT NULL AND thought_tokens IS NOT NULL AND total_tokens IS NOT NULL AND total_tokens > 0 AND
        cached_tokens <= prompt_tokens AND
        total_tokens = prompt_tokens + tool_use_prompt_tokens + candidate_tokens + thought_tokens
    )),
    CHECK(estimated_cost_picos IS NULL OR cost_source IS NOT NULL),
    CHECK(estimated_cost_picos IS NULL OR cost_source != 'litellm_catalog' OR usage_metadata_valid = 1)
);

CREATE INDEX model_generations_time_idx ON model_generations (request_started_at DESC, id DESC);
CREATE INDEX model_generations_review_idx ON model_generations (review_job_id, id DESC);
CREATE INDEX model_generations_feedback_idx ON model_generations (feedback_job_id, id DESC);
`
