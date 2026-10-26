-- PostgreSQL 16+. Historical data is intentionally never ON DELETE CASCADE.
-- Apply each migration in a transaction; see db/README.md.
CREATE TABLE principals (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    issuer text NOT NULL CHECK (issuer <> ''),
    subject text NOT NULL CHECK (subject <> ''),
    display_name text NOT NULL DEFAULT '',
    status text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (issuer, subject)
);
CREATE TABLE spaces (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_id uuid NOT NULL REFERENCES principals(id) ON DELETE RESTRICT,
    native_instance text NOT NULL CHECK (native_instance <> ''),
    native_space_id text NOT NULL CHECK (native_space_id <> ''),
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (owner_id), UNIQUE (native_instance, native_space_id), UNIQUE (id, owner_id)
);
CREATE TABLE quota_accounts (
    owner_id uuid PRIMARY KEY REFERENCES principals(id) ON DELETE RESTRICT,
    limit_bytes bigint NOT NULL CHECK (limit_bytes >= 0),
    used_bytes bigint NOT NULL DEFAULT 0 CHECK (used_bytes >= 0),
    reserved_bytes bigint NOT NULL DEFAULT 0 CHECK (reserved_bytes >= 0),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE files (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_id uuid NOT NULL REFERENCES principals(id) ON DELETE RESTRICT,
    space_id uuid NOT NULL,
    parent_id uuid,
    name text NOT NULL CHECK (name <> '' AND name NOT IN ('.', '..') AND position('/' IN name) = 0 AND position(chr(92) IN name) = 0 AND name = normalize(name, NFC)),
    path text NOT NULL CHECK (left(path, 1) = '/' AND path !~ '//|/\.\.?(/|$)' AND path = normalize(path, NFC)),
    kind text NOT NULL CHECK (kind IN ('file', 'directory')),
    lifecycle text NOT NULL DEFAULT 'active' CHECK (lifecycle IN ('active', 'deleting', 'trashed', 'restoring')),
    metadata_revision bigint NOT NULL DEFAULT 1 CHECK (metadata_revision > 0),
    size bigint NOT NULL DEFAULT 0 CHECK (size >= 0),
    content_type text NOT NULL DEFAULT 'application/octet-stream',
    current_version_id uuid,
    search_artifact_set_id uuid,
    active_operation_id uuid,
    native_instance text NOT NULL,
    native_resource_id text,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (id, owner_id, space_id),
    UNIQUE (native_instance, space_id, native_resource_id),
    FOREIGN KEY (space_id, owner_id) REFERENCES spaces(id, owner_id) ON DELETE RESTRICT,
    FOREIGN KEY (parent_id, owner_id, space_id) REFERENCES files(id, owner_id, space_id) ON DELETE RESTRICT,
    CHECK (parent_id IS DISTINCT FROM id),
    CHECK (kind = 'file' OR (current_version_id IS NULL AND search_artifact_set_id IS NULL AND size = 0))
);
CREATE UNIQUE INDEX files_live_name ON files (space_id, parent_id, name) NULLS NOT DISTINCT WHERE lifecycle IN ('active', 'deleting', 'restoring');
CREATE UNIQUE INDEX files_live_path ON files (space_id, path) WHERE lifecycle IN ('active', 'deleting', 'restoring');
CREATE INDEX files_owner_parent ON files(owner_id, parent_id, lifecycle, name);
CREATE TABLE file_versions (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    file_id uuid NOT NULL REFERENCES files(id) ON DELETE RESTRICT,
    version_seq bigint NOT NULL CHECK (version_seq > 0),
    committed_at timestamptz NOT NULL DEFAULT now(),
    size bigint NOT NULL CHECK (size >= 0),
    sha256 text NOT NULL CHECK (sha256 ~ '^[a-f0-9]{64}$'),
    content_type text NOT NULL DEFAULT 'application/octet-stream',
    native_etag text NOT NULL CHECK (native_etag <> ''),
    native_revision_ref jsonb NOT NULL CHECK (jsonb_typeof(native_revision_ref) = 'object' AND native_revision_ref <> '{}'::jsonb),
    UNIQUE (file_id, version_seq), UNIQUE (file_id, id)
);
ALTER TABLE files ADD CONSTRAINT files_current_version_fk FOREIGN KEY (id, current_version_id) REFERENCES file_versions(file_id, id) ON DELETE RESTRICT DEFERRABLE INITIALLY IMMEDIATE;
CREATE TABLE artifact_sets (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    source_version_id uuid NOT NULL REFERENCES file_versions(id) ON DELETE RESTRICT,
    generation bigint NOT NULL CHECK (generation > 0),
    recipe_fingerprint text NOT NULL CHECK (recipe_fingerprint <> ''),
    recipe_snapshot jsonb NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(recipe_snapshot) = 'object'),
    manifest_key text NOT NULL CHECK (manifest_key <> ''),
    markdown_key text NOT NULL CHECK (markdown_key <> ''),
    manifest_sha256 text NOT NULL CHECK (manifest_sha256 ~ '^[a-f0-9]{64}$'),
    markdown_sha256 text NOT NULL CHECK (markdown_sha256 ~ '^[a-f0-9]{64}$'),
    completeness jsonb NOT NULL CHECK (jsonb_typeof(completeness) = 'object' AND completeness ? 'status' AND completeness->>'status' IN ('ready', 'partial')),
    index_task_id bigint,
    index_state text NOT NULL DEFAULT 'pending' CHECK (index_state IN ('pending', 'enqueued', 'succeeded', 'failed')),
    indexed_at timestamptz,
    published_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (source_version_id, generation), UNIQUE (source_version_id, id),
    CHECK (published_at IS NULL OR (index_state = 'succeeded' AND indexed_at IS NOT NULL))
);
ALTER TABLE files ADD CONSTRAINT files_search_artifact_fk FOREIGN KEY (search_artifact_set_id) REFERENCES artifact_sets(id) ON DELETE RESTRICT;
CREATE TABLE conversion_heads (
    source_version_id uuid PRIMARY KEY REFERENCES file_versions(id) ON DELETE RESTRICT,
    latest_generation bigint NOT NULL DEFAULT 0 CHECK (latest_generation >= 0)
);
CREATE TABLE conversion_jobs (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    source_version_id uuid NOT NULL REFERENCES file_versions(id) ON DELETE RESTRICT,
    generation bigint NOT NULL CHECK (generation > 0),
    recipe_fingerprint text NOT NULL,
    recipe_snapshot jsonb NOT NULL CHECK (jsonb_typeof(recipe_snapshot) = 'object'),
    state text NOT NULL DEFAULT 'queued' CHECK (state IN ('queued', 'processing', 'ready', 'partial', 'not_converted', 'blocked', 'failed')),
    attempt integer NOT NULL DEFAULT 0 CHECK (attempt >= 0),
    max_attempts integer NOT NULL DEFAULT 5 CHECK (max_attempts > 0),
    lease_token bigint NOT NULL DEFAULT 0 CHECK (lease_token >= 0),
    lease_owner text,
    lease_until timestamptz,
    retry_at timestamptz NOT NULL DEFAULT now(),
    error_code text,
    error_detail jsonb,
    artifact_set_id uuid,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (source_version_id, generation),
    FOREIGN KEY (source_version_id, artifact_set_id) REFERENCES artifact_sets(source_version_id, id) ON DELETE RESTRICT,
    CHECK ((state = 'processing') = (lease_until IS NOT NULL AND lease_owner IS NOT NULL))
);
CREATE INDEX conversion_jobs_claim ON conversion_jobs(state, retry_at, lease_until);
CREATE TABLE conversion_page_results (
    job_id uuid NOT NULL REFERENCES conversion_jobs(id) ON DELETE RESTRICT,
    page_no integer NOT NULL CHECK (page_no > 0),
    stage text NOT NULL CHECK (stage IN ('text', 'ocr', 'description', 'render')),
    attempt integer NOT NULL CHECK (attempt > 0),
    lease_token bigint NOT NULL CHECK (lease_token > 0),
    state text NOT NULL CHECK (state IN ('ready', 'failed', 'not_required')),
    result_ref jsonb,
    error_code text,
    PRIMARY KEY (job_id, page_no, stage, attempt),
    CHECK (state <> 'failed' OR error_code IS NOT NULL)
);
CREATE TABLE file_operations (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    file_id uuid NOT NULL REFERENCES files(id) ON DELETE RESTRICT,
    actor_principal_id uuid NOT NULL REFERENCES principals(id) ON DELETE RESTRICT,
    actor_type text NOT NULL CHECK (actor_type IN ('user', 'agent', 'service')),
    kind text NOT NULL CHECK (kind IN ('create', 'overwrite', 'mkdir', 'move', 'rename', 'delete', 'restore')),
    expected_revision bigint NOT NULL CHECK (expected_revision > 0),
    expected_version_id uuid,
    idempotency_key text NOT NULL CHECK (length(idempotency_key) BETWEEN 1 AND 200),
    request_hash text NOT NULL CHECK (request_hash ~ '^[a-f0-9]{64}$'),
    intent jsonb NOT NULL CHECK (jsonb_typeof(intent) = 'object'),
    state text NOT NULL DEFAULT 'prepared' CHECK (state IN ('prepared', 'executing', 'outcome_unknown', 'remote_confirmed', 'committed', 'failed_confirmed')),
    remote_result jsonb,
    result_version_id uuid,
    retry_at timestamptz NOT NULL DEFAULT now(),
    lease_owner text,
    lease_token bigint NOT NULL DEFAULT 0,
    lease_until timestamptz,
    error_code text,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (actor_principal_id, kind, idempotency_key), UNIQUE (file_id, id),
    FOREIGN KEY (file_id, expected_version_id) REFERENCES file_versions(file_id, id) ON DELETE RESTRICT,
    FOREIGN KEY (file_id, result_version_id) REFERENCES file_versions(file_id, id) ON DELETE RESTRICT,
    CHECK (state NOT IN ('remote_confirmed', 'committed') OR remote_result IS NOT NULL)
);
CREATE UNIQUE INDEX file_operations_one_unresolved ON file_operations(file_id) WHERE state NOT IN ('committed', 'failed_confirmed');
CREATE INDEX file_operations_reconcile ON file_operations(state, retry_at, lease_until) WHERE state IN ('executing', 'outcome_unknown', 'remote_confirmed');
ALTER TABLE files ADD CONSTRAINT files_active_operation_fk FOREIGN KEY (id, active_operation_id) REFERENCES file_operations(file_id, id) ON DELETE RESTRICT;
CREATE TABLE upload_sessions (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    operation_id uuid NOT NULL UNIQUE REFERENCES file_operations(id) ON DELETE RESTRICT,
    expected_size bigint NOT NULL CHECK (expected_size >= 0),
    expected_version_id uuid REFERENCES file_versions(id) ON DELETE RESTRICT,
    native_upload_ref text,
    node_id text NOT NULL CHECK (node_id <> ''),
    state text NOT NULL DEFAULT 'created' CHECK (state IN ('created', 'uploading', 'executing', 'outcome_unknown', 'committed', 'failed_confirmed')),
    received_bytes bigint NOT NULL DEFAULT 0 CHECK (received_bytes >= 0 AND received_bytes <= expected_size),
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE quota_reservations (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    upload_id uuid NOT NULL UNIQUE REFERENCES upload_sessions(id) ON DELETE RESTRICT,
    owner_id uuid NOT NULL REFERENCES quota_accounts(owner_id) ON DELETE RESTRICT,
    bytes bigint NOT NULL CHECK (bytes >= 0),
    old_size bigint NOT NULL CHECK (old_size >= 0),
    state text NOT NULL DEFAULT 'held' CHECK (state IN ('held', 'committed', 'released')),
    created_at timestamptz NOT NULL DEFAULT now(),
    resolved_at timestamptz,
    CHECK ((state = 'held') = (resolved_at IS NULL))
);
CREATE INDEX quota_reservations_owner ON quota_reservations(owner_id) WHERE state = 'held';
CREATE TABLE trash_entries (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    root_file_id uuid NOT NULL REFERENCES files(id) ON DELETE RESTRICT,
    operation_id uuid NOT NULL REFERENCES file_operations(id) ON DELETE RESTRICT,
    original_parent_id uuid REFERENCES files(id) ON DELETE RESTRICT,
    original_name text NOT NULL,
    original_path text NOT NULL,
    reserved_location text NOT NULL UNIQUE,
    deleted_at timestamptz NOT NULL DEFAULT now(),
    state text NOT NULL DEFAULT 'deleting' CHECK (state IN ('deleting', 'trashed', 'restoring', 'restored', 'failed_confirmed')),
    restored_at timestamptz
);
CREATE UNIQUE INDEX trash_entries_live ON trash_entries(root_file_id) WHERE state NOT IN ('restored','failed_confirmed');
CREATE TABLE share_approvals (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    requester_principal uuid NOT NULL REFERENCES principals(id) ON DELETE RESTRICT,
    file_id uuid NOT NULL REFERENCES files(id) ON DELETE RESTRICT,
    request_hash text NOT NULL CHECK (request_hash ~ '^[a-f0-9]{64}$'),
    request_body jsonb NOT NULL CHECK (jsonb_typeof(request_body) = 'object'),
    expires_at timestamptz NOT NULL,
    state text NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'approved', 'rejected', 'expired')),
    approved_by uuid REFERENCES principals(id) ON DELETE RESTRICT,
    decided_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    CHECK ((state = 'approved') = (approved_by IS NOT NULL))
);
CREATE TABLE share_grants (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    approval_id uuid NOT NULL UNIQUE REFERENCES share_approvals(id) ON DELETE RESTRICT,
    file_id uuid NOT NULL REFERENCES files(id) ON DELETE RESTRICT,
    creator_principal_id uuid NOT NULL REFERENCES principals(id) ON DELETE RESTRICT,
    audience_type text NOT NULL CHECK (audience_type IN ('user', 'group', 'link')),
    audience_id text,
    token_hash bytea UNIQUE,
    rights text[] NOT NULL,
    version_policy text NOT NULL DEFAULT 'latest' CHECK (version_policy IN ('latest', 'fixed')),
    version_id uuid,
    state text NOT NULL DEFAULT 'active' CHECK (state IN ('active', 'revoked', 'disabled')),
    disabled_reason text,
    approved_by uuid NOT NULL REFERENCES principals(id) ON DELETE RESTRICT,
    expires_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    revoked_at timestamptz,
    FOREIGN KEY (file_id, version_id) REFERENCES file_versions(file_id, id) ON DELETE RESTRICT,
    CHECK ((version_policy = 'fixed') = (version_id IS NOT NULL)),
    CHECK ((audience_type = 'link' AND audience_id IS NULL AND token_hash IS NOT NULL AND octet_length(token_hash) = 32 AND rights = ARRAY['preview']::text[]) OR
           (audience_type IN ('user', 'group') AND audience_id IS NOT NULL AND token_hash IS NULL AND cardinality(rights) > 0 AND rights <@ ARRAY['read', 'download']::text[]))
);
CREATE INDEX share_grants_audience ON share_grants(audience_type, audience_id, state);
CREATE INDEX share_grants_file ON share_grants(file_id, state);
-- Privacy boundary: no identity, timestamp, session, request, IP, trace, or operation.
-- Do not add feedback audit/outbox events or retain HTTP/MCP argument logs.
CREATE TABLE feedback (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    file_id uuid NOT NULL,
    version_id uuid NOT NULL,
    artifact_set_id uuid,
    category text NOT NULL CHECK (category IN ('text', 'description', 'missing_page', 'format', 'other')),
    body text NOT NULL CHECK (length(body) BETWEEN 1 AND 10000),
    received_day date NOT NULL DEFAULT (now() AT TIME ZONE 'UTC')::date,
    triage_state text NOT NULL DEFAULT 'new' CHECK (triage_state IN ('new', 'reviewed', 'resolved')),
    FOREIGN KEY (file_id, version_id) REFERENCES file_versions(file_id, id) ON DELETE RESTRICT,
    FOREIGN KEY (version_id, artifact_set_id) REFERENCES artifact_sets(source_version_id, id) ON DELETE RESTRICT
);
CREATE TABLE outbox (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    event_type text NOT NULL CHECK (event_type <> '' AND event_type !~* 'feedback'),
    aggregate_id uuid NOT NULL,
    payload jsonb NOT NULL CHECK (jsonb_typeof(payload) = 'object'),
    deduplication_key text NOT NULL UNIQUE,
    state text NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'publishing', 'published')),
    attempt integer NOT NULL DEFAULT 0,
    lease_token bigint NOT NULL DEFAULT 0,
    lease_owner text,
    lease_until timestamptz,
    retry_at timestamptz NOT NULL DEFAULT now(),
    published_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    CHECK ((state = 'published') = (published_at IS NOT NULL))
);
CREATE INDEX outbox_claim ON outbox(state, retry_at, lease_until) WHERE state <> 'published';
CREATE TABLE batch_operations (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    actor_principal_id uuid NOT NULL REFERENCES principals(id) ON DELETE RESTRICT,
    idempotency_key text NOT NULL,
    request_hash text NOT NULL CHECK (request_hash ~ '^[a-f0-9]{64}$'),
    state text NOT NULL DEFAULT 'running' CHECK (state IN ('running', 'completed')),
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (actor_principal_id, idempotency_key)
);
CREATE TABLE batch_items (
    batch_id uuid NOT NULL REFERENCES batch_operations(id) ON DELETE RESTRICT,
    item_id text NOT NULL,
    depends_on text[] NOT NULL DEFAULT '{}',
    command text NOT NULL,
    input jsonb NOT NULL CHECK (jsonb_typeof(input) = 'object'),
    idempotency_key text NOT NULL,
    operation_id uuid REFERENCES file_operations(id) ON DELETE RESTRICT,
    status text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'running', 'succeeded', 'failed', 'blocked_dependency')),
    result jsonb,
    PRIMARY KEY (batch_id, item_id),
    UNIQUE (batch_id, idempotency_key),
    CHECK (NOT item_id = ANY(depends_on))
);
CREATE TABLE web_sessions (
    token_hash bytea PRIMARY KEY CHECK (octet_length(token_hash) = 32),
    csrf_hash bytea NOT NULL CHECK (octet_length(csrf_hash) = 32),
    principal_id uuid NOT NULL REFERENCES principals(id) ON DELETE RESTRICT,
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX web_sessions_expiry ON web_sessions(expires_at);
CREATE TABLE access_tickets (
    token_hash bytea PRIMARY KEY CHECK (octet_length(token_hash) = 32),
    principal_id uuid REFERENCES principals(id) ON DELETE RESTRICT,
    share_grant_id uuid REFERENCES share_grants(id) ON DELETE RESTRICT,
    file_id uuid NOT NULL,
    version_id uuid NOT NULL,
    artifact_set_id uuid,
    scope text NOT NULL CHECK (scope IN ('original.read', 'artifact.read', 'preview.read', 'worker.read')),
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (file_id, version_id) REFERENCES file_versions(file_id, id) ON DELETE RESTRICT,
    FOREIGN KEY (version_id, artifact_set_id) REFERENCES artifact_sets(source_version_id, id) ON DELETE RESTRICT,
    CHECK (principal_id IS NOT NULL OR share_grant_id IS NOT NULL)
);
CREATE INDEX access_tickets_expiry ON access_tickets(expires_at);

-- Persist the actual oCIS retained-directory resource. Never blindly repeat an unknown MKCOL.
CREATE TABLE retention_roots (
    space_id uuid PRIMARY KEY REFERENCES spaces(id) ON DELETE RESTRICT,
    native_resource_id text,
    state text NOT NULL DEFAULT 'prepared' CHECK (state IN ('prepared','executing','ready','outcome_unknown')),
    operation_id uuid NOT NULL UNIQUE DEFAULT gen_random_uuid(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK (state <> 'ready' OR native_resource_id IS NOT NULL)
);
