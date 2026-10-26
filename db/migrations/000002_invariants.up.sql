-- Business invariants are enforced inside PostgreSQL, not only in HTTP handlers.
CREATE FUNCTION prevent_historical_delete() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'HISTORY_RETAINED: % cannot be deleted', TG_TABLE_NAME USING ERRCODE = '23514';
END $$;
CREATE FUNCTION prevent_version_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'IMMUTABLE_VERSION: source versions cannot be changed' USING ERRCODE = '23514';
END $$;
CREATE TRIGGER file_versions_immutable BEFORE UPDATE OR DELETE ON file_versions FOR EACH ROW EXECUTE FUNCTION prevent_version_mutation();
CREATE TRIGGER files_retained BEFORE DELETE ON files FOR EACH ROW EXECUTE FUNCTION prevent_historical_delete();
CREATE TRIGGER artifacts_retained BEFORE DELETE ON artifact_sets FOR EACH ROW EXECUTE FUNCTION prevent_historical_delete();
CREATE TRIGGER feedback_retained BEFORE DELETE ON feedback FOR EACH ROW EXECUTE FUNCTION prevent_historical_delete();
CREATE TRIGGER operations_retained BEFORE DELETE ON file_operations FOR EACH ROW EXECUTE FUNCTION prevent_historical_delete();
CREATE TRIGGER trash_retained BEFORE DELETE ON trash_entries FOR EACH ROW EXECUTE FUNCTION prevent_historical_delete();

CREATE FUNCTION guard_feedback_update() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF (to_jsonb(NEW) - 'triage_state') IS DISTINCT FROM (to_jsonb(OLD) - 'triage_state') THEN
        RAISE EXCEPTION 'IMMUTABLE_FEEDBACK: only triage state can change' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER feedback_update_guard BEFORE UPDATE ON feedback FOR EACH ROW EXECUTE FUNCTION guard_feedback_update();

CREATE FUNCTION file_is_active(p_file_id uuid) RETURNS boolean LANGUAGE sql STABLE AS $$
    WITH RECURSIVE ancestry AS (
        SELECT f.id, f.parent_id, f.lifecycle, f.owner_id, f.native_resource_id, ARRAY[f.id] AS visited FROM files f WHERE f.id = p_file_id
        UNION ALL
        SELECT f.id, f.parent_id, f.lifecycle, f.owner_id, f.native_resource_id, a.visited || f.id
        FROM files f JOIN ancestry a ON f.id = a.parent_id WHERE NOT f.id = ANY(a.visited)
    )
    SELECT COALESCE(bool_and(a.lifecycle = 'active' AND p.status = 'active' AND a.native_resource_id IS NOT NULL), false)
    FROM ancestry a JOIN principals p ON p.id = a.owner_id;
$$;

CREATE FUNCTION guard_file_projection() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE v_parent files; v_operation file_operations; v_cycle boolean; v_version file_versions; v_artifact_file uuid;
BEGIN
    -- All tree mutations serialize briefly per space, never for the entire upload.
    PERFORM pg_advisory_xact_lock(hashtextextended(NEW.space_id::text, 0));
    IF TG_OP = 'UPDATE' THEN
        IF (NEW.id, NEW.owner_id, NEW.space_id, NEW.native_instance, NEW.kind) IS DISTINCT FROM
           (OLD.id, OLD.owner_id, OLD.space_id, OLD.native_instance, OLD.kind) THEN
            RAISE EXCEPTION 'IMMUTABLE_IDENTITY' USING ERRCODE = '23514';
        END IF;
        IF OLD.native_resource_id IS NOT NULL AND NEW.native_resource_id IS DISTINCT FROM OLD.native_resource_id THEN
            RAISE EXCEPTION 'IMMUTABLE_NATIVE_RESOURCE' USING ERRCODE = '23514';
        END IF;
        IF NEW.metadata_revision < OLD.metadata_revision THEN
            RAISE EXCEPTION 'REVISION_REGRESSION' USING ERRCODE = '23514';
        END IF;
        IF OLD.active_operation_id IS NOT NULL AND NEW.active_operation_id IS DISTINCT FROM OLD.active_operation_id THEN
            SELECT * INTO v_operation FROM file_operations WHERE id = OLD.active_operation_id;
            IF v_operation.state NOT IN ('committed', 'failed_confirmed') THEN
                RAISE EXCEPTION 'OPERATION_PENDING: unresolved operation cannot be cleared' USING ERRCODE = '55000';
            END IF;
        END IF;
    END IF;
    IF TG_OP = 'INSERT' OR NEW.parent_id IS DISTINCT FROM OLD.parent_id THEN
        IF NEW.parent_id IS NOT NULL THEN
            SELECT * INTO STRICT v_parent FROM files WHERE id = NEW.parent_id;
            IF v_parent.kind <> 'directory' OR NOT file_is_active(v_parent.id) THEN
                RAISE EXCEPTION 'INVALID_PARENT' USING ERRCODE = '23514';
            END IF;
            IF v_parent.active_operation_id IS NOT NULL THEN
                RAISE EXCEPTION 'OPERATION_PENDING: parent is locked' USING ERRCODE = '55000';
            END IF;
            WITH RECURSIVE ancestry AS (
                SELECT id, parent_id FROM files WHERE id = NEW.parent_id
                UNION ALL SELECT f.id, f.parent_id FROM files f JOIN ancestry a ON f.id = a.parent_id
            ) SELECT EXISTS(SELECT 1 FROM ancestry WHERE id = NEW.id) INTO v_cycle;
            IF v_cycle THEN RAISE EXCEPTION 'INVALID_PATH: directory cycle' USING ERRCODE = '23514'; END IF;
        END IF;
    END IF;
    IF NEW.current_version_id IS NOT NULL THEN
        SELECT * INTO STRICT v_version FROM file_versions WHERE id = NEW.current_version_id AND file_id = NEW.id;
        IF v_version.size <> NEW.size THEN RAISE EXCEPTION 'SIZE_PROJECTION_MISMATCH' USING ERRCODE = '23514'; END IF;
        IF TG_OP = 'UPDATE' AND OLD.current_version_id IS DISTINCT FROM NEW.current_version_id AND OLD.current_version_id IS NOT NULL THEN
            IF v_version.version_seq <= (SELECT version_seq FROM file_versions WHERE id = OLD.current_version_id) THEN
                RAISE EXCEPTION 'VERSION_REGRESSION' USING ERRCODE = '23514';
            END IF;
        END IF;
    END IF;
    IF NEW.search_artifact_set_id IS NOT NULL THEN
        SELECT v.file_id INTO v_artifact_file FROM artifact_sets a JOIN file_versions v ON v.id = a.source_version_id
        WHERE a.id = NEW.search_artifact_set_id AND a.published_at IS NOT NULL;
        IF v_artifact_file IS DISTINCT FROM NEW.id THEN RAISE EXCEPTION 'INVALID_SEARCH_ARTIFACT' USING ERRCODE = '23514'; END IF;
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER files_projection_guard BEFORE INSERT OR UPDATE ON files FOR EACH ROW EXECUTE FUNCTION guard_file_projection();

CREATE FUNCTION guard_operation_transition() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.state <> 'prepared' THEN RAISE EXCEPTION 'OPERATION_MUST_START_PREPARED' USING ERRCODE = '23514'; END IF;
        RETURN NEW;
    END IF;
    IF (NEW.id, NEW.file_id, NEW.actor_principal_id, NEW.actor_type, NEW.kind, NEW.expected_revision, NEW.expected_version_id, NEW.idempotency_key, NEW.request_hash, NEW.intent)
        IS DISTINCT FROM
       (OLD.id, OLD.file_id, OLD.actor_principal_id, OLD.actor_type, OLD.kind, OLD.expected_revision, OLD.expected_version_id, OLD.idempotency_key, OLD.request_hash, OLD.intent) THEN
        RAISE EXCEPTION 'IMMUTABLE_OPERATION_INTENT' USING ERRCODE = '23514';
    END IF;
    IF NEW.state <> OLD.state AND NOT (
        (OLD.state = 'prepared' AND NEW.state IN ('executing', 'failed_confirmed')) OR
        (OLD.state = 'executing' AND NEW.state IN ('remote_confirmed', 'outcome_unknown', 'failed_confirmed')) OR
        (OLD.state = 'outcome_unknown' AND NEW.state IN ('remote_confirmed', 'failed_confirmed')) OR
        (OLD.state = 'remote_confirmed' AND NEW.state = 'committed')
    ) THEN RAISE EXCEPTION 'INVALID_OPERATION_TRANSITION: % -> %', OLD.state, NEW.state USING ERRCODE = '23514'; END IF;
    IF OLD.state = 'outcome_unknown' AND NEW.state <> OLD.state AND (NEW.remote_result IS NULL OR NEW.remote_result = '{}'::jsonb) THEN
        RAISE EXCEPTION 'RECONCILIATION_EVIDENCE_REQUIRED' USING ERRCODE = '23514';
    END IF;
    IF OLD.state IN ('committed', 'failed_confirmed') AND NEW IS DISTINCT FROM OLD THEN
        RAISE EXCEPTION 'IMMUTABLE_TERMINAL_OPERATION' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER operations_state_guard BEFORE INSERT OR UPDATE ON file_operations FOR EACH ROW EXECUTE FUNCTION guard_operation_transition();

CREATE FUNCTION begin_file_operation(p_operation_id uuid, p_file_id uuid, p_actor_id uuid, p_actor_type text, p_kind text,
    p_expected_revision bigint, p_expected_version_id uuid, p_idempotency_key text, p_request_hash text, p_intent jsonb)
RETURNS file_operations LANGUAGE plpgsql AS $$
DECLARE v_file files; v_op file_operations; v_busy boolean;
BEGIN
    SELECT * INTO v_op FROM file_operations WHERE actor_principal_id = p_actor_id AND kind = p_kind AND idempotency_key = p_idempotency_key;
    IF FOUND THEN
        IF v_op.request_hash <> p_request_hash OR v_op.file_id <> p_file_id THEN RAISE EXCEPTION 'IDEMPOTENCY_CONFLICT' USING ERRCODE = '23505'; END IF;
        RETURN v_op;
    END IF;
    SELECT * INTO STRICT v_file FROM files WHERE id = p_file_id;
    PERFORM pg_advisory_xact_lock(hashtextextended(v_file.space_id::text, 0));
    -- Repeat after serialization: another request might have committed the same key.
    SELECT * INTO v_op FROM file_operations WHERE actor_principal_id = p_actor_id AND kind = p_kind AND idempotency_key = p_idempotency_key;
    IF FOUND THEN
        IF v_op.request_hash <> p_request_hash OR v_op.file_id <> p_file_id THEN RAISE EXCEPTION 'IDEMPOTENCY_CONFLICT' USING ERRCODE = '23505'; END IF;
        RETURN v_op;
    END IF;
    SELECT * INTO STRICT v_file FROM files WHERE id = p_file_id FOR UPDATE;
    IF v_file.owner_id <> p_actor_id OR NOT EXISTS(SELECT 1 FROM principals WHERE id = p_actor_id AND status = 'active') THEN
        RAISE EXCEPTION 'NOT_FOUND' USING ERRCODE = '42501';
    END IF;
    IF v_file.metadata_revision <> p_expected_revision OR v_file.current_version_id IS DISTINCT FROM p_expected_version_id THEN
        RAISE EXCEPTION 'VERSION_CONFLICT' USING ERRCODE = '40001';
    END IF;
    IF p_kind = 'restore' THEN
        IF v_file.lifecycle <> 'trashed' THEN RAISE EXCEPTION 'INVALID_LIFECYCLE' USING ERRCODE = '23514'; END IF;
    ELSIF p_kind IN ('create','mkdir') AND v_file.current_version_id IS NULL AND v_file.native_resource_id IS NULL AND v_file.lifecycle = 'active' THEN
        IF v_file.parent_id IS NOT NULL AND NOT file_is_active(v_file.parent_id) THEN RAISE EXCEPTION 'NOT_FOUND' USING ERRCODE = '42501'; END IF;
    ELSIF NOT file_is_active(p_file_id) THEN RAISE EXCEPTION 'NOT_FOUND' USING ERRCODE = '42501'; END IF;
    WITH RECURSIVE relatives AS (
        SELECT id, parent_id, active_operation_id FROM files WHERE id = p_file_id
        UNION ALL SELECT f.id, f.parent_id, f.active_operation_id FROM files f JOIN relatives a ON f.id = a.parent_id
    ), descendants AS (
        SELECT id, active_operation_id FROM files WHERE id = p_file_id
        UNION ALL SELECT f.id, f.active_operation_id FROM files f JOIN descendants d ON f.parent_id = d.id
    ) SELECT EXISTS(SELECT 1 FROM relatives WHERE active_operation_id IS NOT NULL UNION ALL SELECT 1 FROM descendants WHERE active_operation_id IS NOT NULL) INTO v_busy;
    IF v_busy THEN RAISE EXCEPTION 'OPERATION_PENDING' USING ERRCODE = '55000'; END IF;
    INSERT INTO file_operations(id,file_id,actor_principal_id,actor_type,kind,expected_revision,expected_version_id,idempotency_key,request_hash,intent)
    VALUES(p_operation_id,p_file_id,p_actor_id,p_actor_type,p_kind,p_expected_revision,p_expected_version_id,p_idempotency_key,p_request_hash,p_intent) RETURNING * INTO v_op;
    UPDATE files SET active_operation_id = v_op.id, updated_at = now() WHERE id = p_file_id;
    RETURN v_op;
END $$;

CREATE FUNCTION guard_reservation_transition() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE v_state text;
BEGIN
    IF (NEW.id, NEW.upload_id, NEW.owner_id, NEW.bytes, NEW.old_size) IS DISTINCT FROM (OLD.id, OLD.upload_id, OLD.owner_id, OLD.bytes, OLD.old_size) THEN
        RAISE EXCEPTION 'IMMUTABLE_RESERVATION' USING ERRCODE = '23514';
    END IF;
    IF NEW.state <> OLD.state THEN
        IF OLD.state <> 'held' THEN RAISE EXCEPTION 'RESERVATION_ALREADY_RESOLVED' USING ERRCODE = '23514'; END IF;
        SELECT o.state INTO v_state FROM upload_sessions u JOIN file_operations o ON o.id = u.operation_id WHERE u.id = NEW.upload_id;
        IF (NEW.state = 'released' AND v_state <> 'failed_confirmed') OR (NEW.state = 'committed' AND v_state <> 'committed') THEN
            RAISE EXCEPTION 'UNRESOLVED_REMOTE_OUTCOME: quota cannot be released' USING ERRCODE = '55000';
        END IF;
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER reservations_state_guard BEFORE UPDATE ON quota_reservations FOR EACH ROW EXECUTE FUNCTION guard_reservation_transition();
CREATE TRIGGER reservations_retained BEFORE DELETE ON quota_reservations FOR EACH ROW EXECUTE FUNCTION prevent_historical_delete();

CREATE FUNCTION reserve_upload_quota(p_upload_id uuid) RETURNS quota_reservations LANGUAGE plpgsql AS $$
DECLARE v_upload upload_sessions; v_op file_operations; v_file files; v_account quota_accounts; v_res quota_reservations; v_bytes bigint;
BEGIN
    SELECT * INTO STRICT v_upload FROM upload_sessions WHERE id = p_upload_id;
    SELECT * INTO STRICT v_op FROM file_operations WHERE id = v_upload.operation_id;
    SELECT * INTO STRICT v_file FROM files WHERE id = v_op.file_id;
    PERFORM pg_advisory_xact_lock(hashtextextended(v_file.space_id::text, 0));
    SELECT * INTO STRICT v_file FROM files WHERE id = v_op.file_id FOR UPDATE;
    SELECT * INTO v_res FROM quota_reservations WHERE upload_id = p_upload_id;
    IF FOUND THEN RETURN v_res; END IF;
    SELECT * INTO STRICT v_op FROM file_operations WHERE id = v_upload.operation_id FOR UPDATE;
    IF v_op.state <> 'prepared' OR v_op.kind NOT IN ('create','overwrite') OR v_file.kind <> 'file' OR v_file.active_operation_id IS DISTINCT FROM v_op.id THEN
        RAISE EXCEPTION 'INVALID_UPLOAD_OPERATION' USING ERRCODE = '23514';
    END IF;
    IF v_file.metadata_revision <> v_op.expected_revision OR v_file.current_version_id IS DISTINCT FROM v_upload.expected_version_id OR v_upload.expected_version_id IS DISTINCT FROM v_op.expected_version_id THEN
        RAISE EXCEPTION 'VERSION_CONFLICT' USING ERRCODE = '40001';
    END IF;
    SELECT * INTO STRICT v_account FROM quota_accounts WHERE owner_id = v_file.owner_id FOR UPDATE;
    v_bytes := greatest(v_upload.expected_size - v_file.size, 0);
    -- Subtraction avoids overflow for attacker-supplied very large byte counts.
    IF v_bytes > 0 AND (v_account.used_bytes > v_account.limit_bytes OR v_account.reserved_bytes > v_account.limit_bytes - v_account.used_bytes OR v_bytes > v_account.limit_bytes - v_account.used_bytes - v_account.reserved_bytes) THEN
        RAISE EXCEPTION 'QUOTA_EXCEEDED' USING ERRCODE = '53100';
    END IF;
    UPDATE quota_accounts SET reserved_bytes = reserved_bytes + v_bytes, updated_at = now() WHERE owner_id = v_file.owner_id;
    INSERT INTO quota_reservations(upload_id,owner_id,bytes,old_size) VALUES(p_upload_id,v_file.owner_id,v_bytes,v_file.size) RETURNING * INTO v_res;
    RETURN v_res;
END $$;

CREATE FUNCTION request_conversion(p_source_version_id uuid, p_recipe_fingerprint text, p_recipe_snapshot jsonb)
RETURNS conversion_jobs LANGUAGE plpgsql AS $$
DECLARE v_generation bigint; v_job conversion_jobs;
BEGIN
    -- Immutable recipe snapshot ensures changing deployment config cannot mutate a queued job.
    INSERT INTO conversion_heads(source_version_id,latest_generation) VALUES(p_source_version_id,1)
        ON CONFLICT(source_version_id) DO UPDATE SET latest_generation = conversion_heads.latest_generation + 1
        RETURNING latest_generation INTO v_generation;
    INSERT INTO conversion_jobs(source_version_id,generation,recipe_fingerprint,recipe_snapshot)
        VALUES(p_source_version_id,v_generation,p_recipe_fingerprint,p_recipe_snapshot) RETURNING * INTO v_job;
    INSERT INTO outbox(event_type,aggregate_id,payload,deduplication_key)
        VALUES('conversion.requested',v_job.id,jsonb_build_object('job_id',v_job.id,'source_version_id',p_source_version_id),'conversion.requested:' || v_job.id::text);
    RETURN v_job;
END $$;

CREATE FUNCTION commit_content_operation(p_operation_id uuid, p_version_id uuid, p_size bigint, p_sha256 text, p_native_etag text,
    p_native_revision_ref jsonb, p_native_resource_id text, p_content_type text, p_recipe_fingerprint text, p_recipe_snapshot jsonb)
RETURNS file_versions LANGUAGE plpgsql AS $$
DECLARE v_op file_operations; v_file files; v_upload upload_sessions; v_res quota_reservations; v_version file_versions; v_seq bigint;
BEGIN
    SELECT * INTO STRICT v_op FROM file_operations WHERE id = p_operation_id;
    SELECT * INTO STRICT v_file FROM files WHERE id = v_op.file_id;
    PERFORM pg_advisory_xact_lock(hashtextextended(v_file.space_id::text, 0));
    SELECT * INTO STRICT v_file FROM files WHERE id = v_op.file_id FOR UPDATE;
    SELECT * INTO STRICT v_op FROM file_operations WHERE id = p_operation_id FOR UPDATE;
    IF v_op.state = 'committed' THEN SELECT * INTO STRICT v_version FROM file_versions WHERE id = v_op.result_version_id; RETURN v_version; END IF;
    IF v_op.state <> 'remote_confirmed' OR v_op.kind NOT IN ('create','overwrite') OR v_file.active_operation_id IS DISTINCT FROM v_op.id THEN
        RAISE EXCEPTION 'REMOTE_COMMIT_NOT_CONFIRMED' USING ERRCODE = '55000';
    END IF;
    IF v_file.metadata_revision <> v_op.expected_revision OR v_file.current_version_id IS DISTINCT FROM v_op.expected_version_id THEN
        RAISE EXCEPTION 'VERSION_CONFLICT' USING ERRCODE = '40001';
    END IF;
    SELECT * INTO STRICT v_upload FROM upload_sessions WHERE operation_id = p_operation_id FOR UPDATE;
    SELECT * INTO STRICT v_res FROM quota_reservations WHERE upload_id = v_upload.id FOR UPDATE;
    IF v_upload.expected_size <> p_size OR v_res.old_size <> v_file.size OR v_res.state <> 'held' OR p_native_resource_id IS NULL OR p_native_resource_id = '' THEN
        RAISE EXCEPTION 'UPLOAD_RESULT_MISMATCH' USING ERRCODE = '23514';
    END IF;
    PERFORM 1 FROM quota_accounts WHERE owner_id = v_file.owner_id FOR UPDATE;
    SELECT COALESCE(max(version_seq),0) + 1 INTO v_seq FROM file_versions WHERE file_id = v_file.id;
    INSERT INTO file_versions(id,file_id,version_seq,size,sha256,native_etag,native_revision_ref,content_type)
        VALUES(p_version_id,v_file.id,v_seq,p_size,p_sha256,p_native_etag,p_native_revision_ref,p_content_type) RETURNING * INTO v_version;
    UPDATE file_operations SET state = 'committed', result_version_id = v_version.id, updated_at = now() WHERE id = p_operation_id;
    UPDATE files SET current_version_id = v_version.id, metadata_revision = metadata_revision + 1, size = p_size,
        native_resource_id = p_native_resource_id, content_type = p_content_type, active_operation_id = NULL, updated_at = now() WHERE id = v_file.id;
    UPDATE quota_accounts SET used_bytes = used_bytes - v_res.old_size + p_size, reserved_bytes = reserved_bytes - v_res.bytes, updated_at = now() WHERE owner_id = v_file.owner_id;
    UPDATE quota_reservations SET state = 'committed', resolved_at = now() WHERE id = v_res.id;
    UPDATE upload_sessions SET state = 'committed', received_bytes = p_size, updated_at = now() WHERE id = v_upload.id;
    PERFORM request_conversion(v_version.id,p_recipe_fingerprint,p_recipe_snapshot);
    INSERT INTO outbox(event_type,aggregate_id,payload,deduplication_key)
        VALUES('file.version_committed',v_file.id,jsonb_build_object('file_id',v_file.id,'version_id',v_version.id),'file.version_committed:' || v_version.id::text);
    RETURN v_version;
END $$;

CREATE FUNCTION release_upload_quota(p_upload_id uuid) RETURNS boolean LANGUAGE plpgsql AS $$
DECLARE v_op file_operations; v_file files; v_res quota_reservations;
BEGIN
    SELECT o.* INTO STRICT v_op FROM upload_sessions u JOIN file_operations o ON o.id = u.operation_id WHERE u.id = p_upload_id;
    SELECT * INTO STRICT v_file FROM files WHERE id = v_op.file_id;
    PERFORM pg_advisory_xact_lock(hashtextextended(v_file.space_id::text, 0));
    PERFORM 1 FROM files WHERE id = v_op.file_id FOR UPDATE;
    SELECT * INTO STRICT v_op FROM file_operations WHERE id = v_op.id FOR UPDATE;
    SELECT * INTO STRICT v_res FROM quota_reservations WHERE upload_id = p_upload_id FOR UPDATE;
    IF v_res.state = 'released' THEN RETURN false; END IF;
    IF v_res.state <> 'held' OR v_op.state <> 'failed_confirmed' THEN RAISE EXCEPTION 'UNRESOLVED_REMOTE_OUTCOME' USING ERRCODE = '55000'; END IF;
    UPDATE quota_accounts SET reserved_bytes = reserved_bytes - v_res.bytes, updated_at = now() WHERE owner_id = v_res.owner_id;
    UPDATE quota_reservations SET state = 'released', resolved_at = now() WHERE id = v_res.id;
    UPDATE upload_sessions SET state = 'failed_confirmed', updated_at = now() WHERE id = p_upload_id;
    UPDATE files SET active_operation_id = NULL, updated_at = now() WHERE id = v_file.id AND active_operation_id = v_op.id;
    RETURN true;
END $$;

CREATE FUNCTION guard_artifact_update() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.published_at IS NOT NULL AND NEW IS DISTINCT FROM OLD THEN
        RAISE EXCEPTION 'IMMUTABLE_PUBLISHED_ARTIFACT' USING ERRCODE = '23514';
    END IF;
    IF (NEW.id, NEW.source_version_id, NEW.generation, NEW.recipe_fingerprint, NEW.recipe_snapshot, NEW.manifest_key, NEW.markdown_key, NEW.manifest_sha256, NEW.markdown_sha256, NEW.completeness)
        IS DISTINCT FROM
       (OLD.id, OLD.source_version_id, OLD.generation, OLD.recipe_fingerprint, OLD.recipe_snapshot, OLD.manifest_key, OLD.markdown_key, OLD.manifest_sha256, OLD.markdown_sha256, OLD.completeness) THEN
        RAISE EXCEPTION 'IMMUTABLE_FINAL_ARTIFACT' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER artifact_update_guard BEFORE UPDATE ON artifact_sets FOR EACH ROW EXECUTE FUNCTION guard_artifact_update();

CREATE FUNCTION guard_job_identity() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF (NEW.id, NEW.source_version_id, NEW.generation, NEW.recipe_fingerprint, NEW.recipe_snapshot)
       IS DISTINCT FROM (OLD.id, OLD.source_version_id, OLD.generation, OLD.recipe_fingerprint, OLD.recipe_snapshot) THEN
        RAISE EXCEPTION 'IMMUTABLE_JOB_RECIPE' USING ERRCODE = '23514';
    END IF;
    IF NEW.lease_token < OLD.lease_token OR NEW.attempt < OLD.attempt THEN RAISE EXCEPTION 'FENCE_REGRESSION' USING ERRCODE = '23514'; END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER conversion_jobs_identity BEFORE UPDATE ON conversion_jobs FOR EACH ROW EXECUTE FUNCTION guard_job_identity();
CREATE FUNCTION guard_page_result() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NOT EXISTS(SELECT 1 FROM conversion_jobs WHERE id = NEW.job_id AND state = 'processing' AND lease_token = NEW.lease_token AND attempt = NEW.attempt AND lease_until > clock_timestamp()) THEN
        RAISE EXCEPTION 'STALE_WORKER' USING ERRCODE = '55000';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER page_result_fence BEFORE INSERT OR UPDATE ON conversion_page_results FOR EACH ROW EXECUTE FUNCTION guard_page_result();

CREATE FUNCTION publish_artifact(p_job_id uuid, p_lease_token bigint, p_artifact_set_id uuid) RETURNS boolean LANGUAGE plpgsql AS $$
DECLARE v_job conversion_jobs; v_artifact artifact_sets; v_version file_versions; v_file files; v_current_seq bigint; v_current_generation bigint; v_latest bigint; v_publish boolean;
BEGIN
    SELECT * INTO STRICT v_job FROM conversion_jobs WHERE id = p_job_id;
    SELECT * INTO STRICT v_version FROM file_versions WHERE id = v_job.source_version_id;
    SELECT * INTO STRICT v_file FROM files WHERE id = v_version.file_id;
    PERFORM pg_advisory_xact_lock(hashtextextended(v_file.space_id::text, 0));
    SELECT * INTO STRICT v_file FROM files WHERE id = v_version.file_id FOR UPDATE;
    SELECT * INTO STRICT v_job FROM conversion_jobs WHERE id = p_job_id FOR UPDATE;
    -- Safe message redelivery after a successful transaction.
    IF v_job.state IN ('ready','partial') AND v_job.artifact_set_id = p_artifact_set_id AND v_job.lease_token = p_lease_token THEN RETURN v_file.search_artifact_set_id = p_artifact_set_id; END IF;
    IF v_job.state <> 'processing' OR v_job.lease_token <> p_lease_token OR v_job.lease_until <= clock_timestamp() THEN
        RAISE EXCEPTION 'STALE_WORKER' USING ERRCODE = '55000';
    END IF;
    SELECT * INTO STRICT v_artifact FROM artifact_sets WHERE id = p_artifact_set_id FOR UPDATE;
    IF v_artifact.source_version_id <> v_job.source_version_id OR v_artifact.generation <> v_job.generation OR v_artifact.recipe_fingerprint <> v_job.recipe_fingerprint OR v_artifact.recipe_snapshot <> v_job.recipe_snapshot THEN
        RAISE EXCEPTION 'ARTIFACT_JOB_MISMATCH' USING ERRCODE = '23514';
    END IF;
    IF v_artifact.index_state <> 'succeeded' OR v_artifact.indexed_at IS NULL THEN RAISE EXCEPTION 'INDEX_NOT_READY' USING ERRCODE = '55000'; END IF;
    SELECT latest_generation INTO STRICT v_latest FROM conversion_heads WHERE source_version_id = v_job.source_version_id FOR UPDATE;
    SELECT v.version_seq, a.generation INTO v_current_seq,v_current_generation FROM artifact_sets a JOIN file_versions v ON v.id = a.source_version_id WHERE a.id = v_file.search_artifact_set_id;
    v_publish := v_latest = v_job.generation AND file_is_active(v_file.id) AND
        (v_file.search_artifact_set_id IS NULL OR v_version.version_seq > v_current_seq OR (v_version.version_seq = v_current_seq AND v_job.generation > v_current_generation));
    -- Retain every successful final artifact even if a newer version already won.
    IF v_artifact.published_at IS NULL THEN UPDATE artifact_sets SET published_at = now() WHERE id = p_artifact_set_id; END IF;
    IF v_publish THEN UPDATE files SET search_artifact_set_id = p_artifact_set_id, updated_at = now() WHERE id = v_file.id; END IF;
    UPDATE conversion_jobs SET state = v_artifact.completeness->>'status', artifact_set_id = p_artifact_set_id, lease_owner = NULL, lease_until = NULL, updated_at = now() WHERE id = p_job_id;
    IF v_publish THEN
        INSERT INTO outbox(event_type,aggregate_id,payload,deduplication_key)
        VALUES('search.published',v_file.id,jsonb_build_object('file_id',v_file.id,'artifact_set_id',p_artifact_set_id,'previous_artifact_set_id',v_file.search_artifact_set_id),'search.published:' || p_artifact_set_id::text);
    END IF;
    RETURN v_publish;
END $$;

CREATE FUNCTION revoke_principal_shares() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.status = 'disabled' AND OLD.status <> 'disabled' THEN
        UPDATE share_grants SET state = 'disabled', disabled_reason = 'account_disabled', revoked_at = now() WHERE creator_principal_id = NEW.id AND state = 'active';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER principals_disable_shares AFTER UPDATE OF status ON principals FOR EACH ROW EXECUTE FUNCTION revoke_principal_shares();
CREATE FUNCTION revoke_subtree_shares() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.lifecycle <> 'active' AND OLD.lifecycle = 'active' THEN
        WITH RECURSIVE subtree AS (
            SELECT NEW.id AS id UNION ALL SELECT f.id FROM files f JOIN subtree s ON f.parent_id = s.id
        ) UPDATE share_grants SET state = 'disabled', disabled_reason = 'file_deleted', revoked_at = now() WHERE file_id IN (SELECT id FROM subtree) AND state = 'active';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER files_disable_shares AFTER UPDATE OF lifecycle ON files FOR EACH ROW EXECUTE FUNCTION revoke_subtree_shares();

CREATE FUNCTION guard_share_approval() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF (NEW.id,NEW.requester_principal,NEW.file_id,NEW.request_hash,NEW.request_body,NEW.expires_at) IS DISTINCT FROM
       (OLD.id,OLD.requester_principal,OLD.file_id,OLD.request_hash,OLD.request_body,OLD.expires_at) THEN
        RAISE EXCEPTION 'IMMUTABLE_SHARE_REQUEST' USING ERRCODE = '23514';
    END IF;
    IF OLD.state <> 'pending' AND NEW IS DISTINCT FROM OLD THEN RAISE EXCEPTION 'APPROVAL_ALREADY_DECIDED' USING ERRCODE = '23514'; END IF;
    IF NEW.state = 'approved' AND (OLD.expires_at <= clock_timestamp() OR NEW.approved_by IS DISTINCT FROM (SELECT owner_id FROM files WHERE id = NEW.file_id)) THEN
        RAISE EXCEPTION 'INVALID_APPROVAL' USING ERRCODE = '42501';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER approvals_guard BEFORE UPDATE ON share_approvals FOR EACH ROW EXECUTE FUNCTION guard_share_approval();
CREATE FUNCTION guard_share_grant() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE v_approval share_approvals; v_file files;
BEGIN
    IF TG_OP = 'UPDATE' THEN
        IF (to_jsonb(NEW) - ARRAY['state','disabled_reason','revoked_at']) IS DISTINCT FROM (to_jsonb(OLD) - ARRAY['state','disabled_reason','revoked_at']) THEN
            RAISE EXCEPTION 'IMMUTABLE_SHARE_SCOPE: new approval required' USING ERRCODE = '23514';
        END IF;
        IF OLD.state <> 'active' AND NEW.state = 'active' THEN RAISE EXCEPTION 'SHARE_RECONFIRMATION_REQUIRED' USING ERRCODE = '42501'; END IF;
        RETURN NEW;
    END IF;
    SELECT * INTO STRICT v_file FROM files WHERE id = NEW.file_id;
    SELECT * INTO STRICT v_approval FROM share_approvals WHERE id = NEW.approval_id;
    IF v_file.kind <> 'file' OR NOT file_is_active(v_file.id) OR v_file.owner_id <> NEW.creator_principal_id OR
       v_approval.file_id <> NEW.file_id OR v_approval.state <> 'approved' OR v_approval.approved_by <> NEW.approved_by OR NEW.approved_by <> v_file.owner_id THEN
        RAISE EXCEPTION 'SHARE_APPROVAL_REQUIRED' USING ERRCODE = '42501';
    END IF;
    -- Request body uses these canonical fields; approval cannot be swapped to a different recipient.
    IF v_approval.request_body->>'audience_type' IS DISTINCT FROM NEW.audience_type OR
       v_approval.request_body->>'audience_id' IS DISTINCT FROM NEW.audience_id OR
       v_approval.request_body->>'version_policy' IS DISTINCT FROM NEW.version_policy OR
       v_approval.request_body->>'version_id' IS DISTINCT FROM NEW.version_id::text OR
       v_approval.request_body->'rights' IS DISTINCT FROM to_jsonb(NEW.rights) THEN
        RAISE EXCEPTION 'APPROVAL_SCOPE_MISMATCH' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER grants_guard BEFORE INSERT OR UPDATE ON share_grants FOR EACH ROW EXECUTE FUNCTION guard_share_grant();
