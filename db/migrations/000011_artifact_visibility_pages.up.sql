ALTER TABLE conversion_jobs ADD COLUMN IF NOT EXISTS page_count integer NOT NULL DEFAULT 0;
ALTER TABLE conversion_jobs ADD COLUMN IF NOT EXISTS pages_ready integer NOT NULL DEFAULT 0;
ALTER TABLE conversion_jobs ADD COLUMN IF NOT EXISTS pages_failed integer NOT NULL DEFAULT 0;
ALTER TABLE conversion_jobs ADD COLUMN IF NOT EXISTS completeness jsonb NOT NULL DEFAULT '{}'::jsonb;
ALTER TABLE conversion_jobs ADD COLUMN IF NOT EXISTS actual_engine jsonb NOT NULL DEFAULT '{}'::jsonb;
ALTER TABLE conversion_page_results ADD COLUMN IF NOT EXISTS recorded_at timestamptz NOT NULL DEFAULT clock_timestamp();
ALTER TABLE artifact_sets ADD COLUMN IF NOT EXISTS attempt integer NOT NULL DEFAULT 1;

CREATE OR REPLACE FUNCTION guard_page_result() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    PERFORM 1 FROM conversion_jobs WHERE id=NEW.job_id AND state='processing'
      AND lease_token=NEW.lease_token AND attempt=NEW.attempt AND lease_until>clock_timestamp() FOR UPDATE;
    IF NOT FOUND THEN RAISE EXCEPTION 'STALE_WORKER' USING ERRCODE='55000'; END IF;
    RETURN NEW;
END $$;
ALTER TABLE artifact_sets DROP CONSTRAINT IF EXISTS artifact_sets_source_version_id_generation_key;
CREATE UNIQUE INDEX IF NOT EXISTS artifact_sets_generation_attempt ON artifact_sets(source_version_id,generation,attempt);
CREATE OR REPLACE FUNCTION guard_artifact_attempt() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.attempt IS DISTINCT FROM OLD.attempt THEN RAISE EXCEPTION 'IMMUTABLE_ARTIFACT_ATTEMPT' USING ERRCODE='23514'; END IF;
    RETURN NEW;
END $$;
DROP TRIGGER IF EXISTS artifact_attempt_guard ON artifact_sets;
CREATE TRIGGER artifact_attempt_guard BEFORE UPDATE ON artifact_sets FOR EACH ROW EXECUTE FUNCTION guard_artifact_attempt();

CREATE TABLE IF NOT EXISTS artifact_activation_tasks (
    file_id uuid NOT NULL REFERENCES files(id),
    artifact_set_id uuid NOT NULL REFERENCES artifact_sets(id),
    state text NOT NULL DEFAULT 'queued' CHECK (state IN ('queued','leased','done')),
    attempt integer NOT NULL DEFAULT 0,
    lease_owner text,
    lease_token bigint NOT NULL DEFAULT 0,
    lease_until timestamptz,
    retry_at timestamptz NOT NULL DEFAULT now(),
    last_error text,
    PRIMARY KEY(file_id,artifact_set_id)
);
CREATE TABLE IF NOT EXISTS artifact_cleanup_tasks (
    artifact_set_id uuid PRIMARY KEY REFERENCES artifact_sets(id),
    state text NOT NULL DEFAULT 'queued' CHECK (state IN ('queued','leased','done')),
    attempt integer NOT NULL DEFAULT 0,
    lease_owner text,
    lease_token bigint NOT NULL DEFAULT 0,
    lease_until timestamptz,
    retry_at timestamptz NOT NULL DEFAULT now(),
    last_error text
);
CREATE INDEX IF NOT EXISTS artifact_activation_claim ON artifact_activation_tasks(state,retry_at,lease_until);
CREATE INDEX IF NOT EXISTS artifact_cleanup_claim ON artifact_cleanup_tasks(state,retry_at,lease_until);

-- 可见指针只在索引确认后更新；源版本、祖先生命周期和代次同时参与 CAS。
CREATE OR REPLACE FUNCTION activate_artifact(p_file_id uuid,p_artifact_set_id uuid) RETURNS boolean LANGUAGE plpgsql AS $$
DECLARE a artifact_sets; f files; old artifact_sets;
BEGIN
    SELECT * INTO STRICT f FROM files WHERE id=p_file_id;
    PERFORM pg_advisory_xact_lock(hashtextextended(f.space_id::text,0));
    SELECT * INTO STRICT f FROM files WHERE id=p_file_id FOR UPDATE;
    SELECT * INTO STRICT a FROM artifact_sets WHERE id=p_artifact_set_id;
    SELECT * INTO old FROM artifact_sets WHERE id=f.search_artifact_set_id;
    IF a.published_at IS NULL OR a.index_state<>'succeeded' OR a.indexed_at IS NULL
       OR NOT file_is_active(f.id) OR f.current_version_id IS DISTINCT FROM a.source_version_id
       OR NOT EXISTS(SELECT 1 FROM file_versions WHERE id=a.source_version_id AND file_id=f.id)
       OR EXISTS(SELECT 1 FROM principals WHERE id=f.owner_id AND status<>'active') THEN RETURN false; END IF;
    IF old.id IS NOT NULL AND old.source_version_id=a.source_version_id AND (old.generation,old.attempt)>(a.generation,a.attempt) THEN RETURN false; END IF;
    IF f.search_artifact_set_id IS DISTINCT FROM a.id THEN
        UPDATE files SET search_artifact_set_id=a.id,updated_at=now() WHERE id=f.id;
        IF old.id IS NOT NULL THEN
            INSERT INTO artifact_cleanup_tasks(artifact_set_id) VALUES(old.id)
            ON CONFLICT(artifact_set_id) DO UPDATE SET state='queued',retry_at=now();
        END IF;
        INSERT INTO outbox(event_type,aggregate_id,payload,deduplication_key)
        VALUES('search.published',f.id,jsonb_build_object('file_id',f.id,'artifact_set_id',a.id,'previous_artifact_set_id',old.id),
          'search.activated:'||a.id::text||':'||f.metadata_revision::text) ON CONFLICT DO NOTHING;
    END IF;
    RETURN true;
END $$;

CREATE OR REPLACE FUNCTION publish_artifact(p_job_id uuid,p_lease_token bigint,p_artifact_set_id uuid) RETURNS boolean LANGUAGE plpgsql AS $$
DECLARE j conversion_jobs; a artifact_sets; v file_versions; activated boolean;
BEGIN
    SELECT * INTO STRICT j FROM conversion_jobs WHERE id=p_job_id FOR UPDATE;
    SELECT * INTO STRICT a FROM artifact_sets WHERE id=p_artifact_set_id FOR UPDATE;
    SELECT * INTO STRICT v FROM file_versions WHERE id=j.source_version_id;
    IF j.state IN ('ready','partial') AND j.artifact_set_id=a.id AND j.lease_token=p_lease_token THEN
        RETURN EXISTS(SELECT 1 FROM files WHERE id=v.file_id AND search_artifact_set_id=a.id);
    END IF;
    IF j.state<>'processing' OR j.lease_token<>p_lease_token OR j.lease_until<=clock_timestamp() THEN
        RAISE EXCEPTION 'STALE_WORKER' USING ERRCODE='55000';
    END IF;
    IF a.attempt>j.attempt OR (a.source_version_id,a.generation,a.recipe_fingerprint,a.recipe_snapshot)
       IS DISTINCT FROM (j.source_version_id,j.generation,j.recipe_fingerprint,j.recipe_snapshot) THEN
        RAISE EXCEPTION 'ARTIFACT_JOB_MISMATCH' USING ERRCODE='23514';
    END IF;
    IF a.index_state<>'succeeded' OR a.indexed_at IS NULL THEN RAISE EXCEPTION 'INDEX_NOT_READY' USING ERRCODE='55000'; END IF;
    IF a.published_at IS NULL THEN UPDATE artifact_sets SET published_at=now() WHERE id=a.id; END IF;
    activated := activate_artifact(v.file_id,a.id);
    INSERT INTO artifact_activation_tasks(file_id,artifact_set_id,state) VALUES(v.file_id,a.id,CASE WHEN activated THEN 'done' ELSE 'queued' END)
    ON CONFLICT(file_id,artifact_set_id) DO NOTHING;
    INSERT INTO artifact_cleanup_tasks(artifact_set_id) VALUES(a.id) ON CONFLICT DO NOTHING;
    UPDATE conversion_jobs SET state=a.completeness->>'status',artifact_set_id=a.id,completeness=a.completeness,
        retry_at=now()+interval '5 seconds',lease_owner=NULL,lease_until=NULL,updated_at=now() WHERE id=j.id;
    RETURN activated;
END $$;

-- 恢复事务中创建任务，不依赖消息送达；目录恢复逐文件触发，涵盖整个子树。
CREATE OR REPLACE FUNCTION enqueue_restored_artifact() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.lifecycle='active' AND OLD.lifecycle IS DISTINCT FROM 'active' THEN
        WITH RECURSIVE tree AS (
            SELECT id,current_version_id FROM files WHERE id=NEW.id
            UNION ALL SELECT f.id,f.current_version_id FROM files f JOIN tree t ON f.parent_id=t.id
        )
        INSERT INTO artifact_activation_tasks(file_id,artifact_set_id)
        SELECT tree.id,a.id FROM tree JOIN LATERAL (
            SELECT id FROM artifact_sets WHERE source_version_id=tree.current_version_id
              AND published_at IS NOT NULL AND index_state='succeeded'
            ORDER BY generation DESC,attempt DESC LIMIT 1
        ) a ON true
        ON CONFLICT(file_id,artifact_set_id) DO UPDATE SET state='queued',retry_at=now();
    END IF;
    RETURN NEW;
END $$;
DROP TRIGGER IF EXISTS restored_artifact_activation ON files;
CREATE TRIGGER restored_artifact_activation AFTER UPDATE OF lifecycle ON files FOR EACH ROW EXECUTE FUNCTION enqueue_restored_artifact();

-- 安全回填：只重建已确认索引的不可变产物；不猜测未完成转换结果。
INSERT INTO artifact_activation_tasks(file_id,artifact_set_id)
SELECT f.id,a.id FROM files f JOIN LATERAL (
    SELECT id FROM artifact_sets WHERE source_version_id=f.current_version_id AND published_at IS NOT NULL AND index_state='succeeded'
    ORDER BY generation DESC,attempt DESC LIMIT 1
) a ON true ON CONFLICT DO NOTHING;
INSERT INTO artifact_cleanup_tasks(artifact_set_id)
SELECT id FROM artifact_sets WHERE index_state<>'pending' ON CONFLICT DO NOTHING;
