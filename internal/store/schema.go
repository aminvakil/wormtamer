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
ON review_jobs (state, next_attempt_at, created_at, id);

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
    attempt_count INTEGER NOT NULL DEFAULT 0,
    next_attempt_at TEXT NOT NULL,
    last_error_category TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE (gitlab_instance, project_id, merge_request_iid)
);

CREATE INDEX feedback_jobs_due_idx
ON feedback_jobs (state, next_attempt_at, id);

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

`
