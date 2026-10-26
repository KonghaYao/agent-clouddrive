-- 未决操作占用以身份建图；终态保留记录供审计，但不再阻塞。
CREATE TABLE IF NOT EXISTS file_operation_holds (
    operation_id uuid NOT NULL REFERENCES file_operations(id) ON DELETE RESTRICT,
    file_id uuid NOT NULL REFERENCES files(id) ON DELETE RESTRICT,
    hold_kind text NOT NULL CHECK (hold_kind IN ('source','target_parent','ancestor')),
    PRIMARY KEY (operation_id, file_id, hold_kind)
);
CREATE INDEX IF NOT EXISTS file_operation_holds_file ON file_operation_holds(file_id, operation_id);

CREATE OR REPLACE FUNCTION guard_lifecycle_holds() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE v_space uuid; v_busy boolean;
BEGIN
    SELECT space_id INTO STRICT v_space FROM files WHERE id=NEW.file_id;
    PERFORM pg_advisory_xact_lock(hashtextextended(v_space::text,0));
    WITH RECURSIVE subtree AS (
        SELECT id FROM files WHERE id=NEW.file_id
        UNION ALL SELECT f.id FROM files f JOIN subtree t ON f.parent_id=t.id
    ) SELECT EXISTS (
        SELECT 1 FROM subtree t JOIN file_operation_holds h ON h.file_id=t.id
        JOIN file_operations o ON o.id=h.operation_id
        WHERE o.state NOT IN ('committed','failed_confirmed')
    ) INTO v_busy;
    IF v_busy THEN RAISE EXCEPTION 'OPERATION_PENDING: subtree has unresolved holds' USING ERRCODE='55000'; END IF;
    RETURN NEW;
END $$;
DROP TRIGGER IF EXISTS lifecycle_holds_guard ON file_operations;
CREATE TRIGGER lifecycle_holds_guard BEFORE INSERT ON file_operations FOR EACH ROW EXECUTE FUNCTION guard_lifecycle_holds();

CREATE OR REPLACE FUNCTION record_lifecycle_holds() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO file_operation_holds VALUES(NEW.id,NEW.file_id,'source') ON CONFLICT DO NOTHING;
    IF NEW.kind IN ('move','rename','restore','mkdir') THEN
        WITH RECURSIVE ancestry AS (
            SELECT id,parent_id,0 AS depth FROM files WHERE id=NULLIF(NEW.intent->>'target_parent_id','')::uuid
            UNION ALL SELECT f.id,f.parent_id,a.depth+1 FROM files f JOIN ancestry a ON f.id=a.parent_id
        ) INSERT INTO file_operation_holds(operation_id,file_id,hold_kind)
        SELECT NEW.id,id,CASE WHEN depth=0 THEN 'target_parent' ELSE 'ancestor' END FROM ancestry
        ON CONFLICT DO NOTHING;
    END IF;
    RETURN NEW;
END $$;
DROP TRIGGER IF EXISTS lifecycle_holds_record ON file_operations;
CREATE TRIGGER lifecycle_holds_record AFTER INSERT ON file_operations FOR EACH ROW EXECUTE FUNCTION record_lifecycle_holds();

INSERT INTO file_operation_holds(operation_id,file_id,hold_kind)
SELECT id,file_id,'source' FROM file_operations WHERE state NOT IN ('committed','failed_confirmed') ON CONFLICT DO NOTHING;
WITH RECURSIVE ancestry AS (
    SELECT o.id AS operation_id,f.id,f.parent_id,0 AS depth FROM file_operations o
    JOIN files f ON f.id=NULLIF(o.intent->>'target_parent_id','')::uuid
    WHERE o.kind IN ('move','rename','restore','mkdir') AND o.state NOT IN ('committed','failed_confirmed')
    UNION ALL SELECT a.operation_id,f.id,f.parent_id,a.depth+1 FROM files f JOIN ancestry a ON f.id=a.parent_id
) INSERT INTO file_operation_holds(operation_id,file_id,hold_kind)
SELECT operation_id,id,CASE WHEN depth=0 THEN 'target_parent' ELSE 'ancestor' END FROM ancestry ON CONFLICT DO NOTHING;

ALTER TABLE batch_operations ADD COLUMN IF NOT EXISTS lease_owner text;
ALTER TABLE batch_operations ADD COLUMN IF NOT EXISTS lease_until timestamptz;
ALTER TABLE batch_operations ADD COLUMN IF NOT EXISTS lease_token bigint NOT NULL DEFAULT 0;

ALTER TABLE retention_roots ADD COLUMN IF NOT EXISTS lease_owner text;
ALTER TABLE retention_roots ADD COLUMN IF NOT EXISTS lease_until timestamptz;
ALTER TABLE retention_roots ADD COLUMN IF NOT EXISTS lease_token bigint NOT NULL DEFAULT 0;
ALTER TABLE retention_roots ADD COLUMN IF NOT EXISTS retry_at timestamptz NOT NULL DEFAULT now();
ALTER TABLE retention_roots ADD COLUMN IF NOT EXISTS attempt integer NOT NULL DEFAULT 0;
ALTER TABLE retention_roots ADD COLUMN IF NOT EXISTS last_error text;
ALTER TABLE retention_roots ADD COLUMN IF NOT EXISTS directory_name text;
ALTER TABLE retention_roots ADD COLUMN IF NOT EXISTS native_parent_id text;
CREATE INDEX IF NOT EXISTS retention_roots_claim ON retention_roots(retry_at,lease_until) WHERE state <> 'ready';
