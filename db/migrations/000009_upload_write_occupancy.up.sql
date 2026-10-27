-- PATCH 准入证据必须先于远端派发；未知结果不能按时间释放占用。
ALTER TABLE upload_sessions ADD COLUMN IF NOT EXISTS patch_epoch bigint NOT NULL DEFAULT 0 CHECK (patch_epoch >= 0);
ALTER TABLE upload_sessions ADD COLUMN IF NOT EXISTS in_flight_patches integer NOT NULL DEFAULT 0 CHECK (in_flight_patches BETWEEN 0 AND 1);
ALTER TABLE upload_sessions ADD COLUMN IF NOT EXISTS abandon_requested_at timestamptz;
ALTER TABLE upload_sessions ADD COLUMN IF NOT EXISTS native_termination_confirmed_at timestamptz;
-- 升级时旧 executing 没有派发证据，不能把 offset=0 当成从未发送。
ALTER TABLE upload_sessions ADD COLUMN IF NOT EXISTS legacy_dispatch_unknown boolean NOT NULL DEFAULT true;
ALTER TABLE upload_sessions ALTER COLUMN legacy_dispatch_unknown SET DEFAULT false;
UPDATE upload_sessions u SET legacy_dispatch_unknown=false FROM file_operations o
WHERE o.id=u.operation_id AND o.state IN ('prepared','committed','failed_confirmed') AND u.legacy_dispatch_unknown;
CREATE TABLE IF NOT EXISTS upload_patch_attempts (
    upload_id uuid NOT NULL REFERENCES upload_sessions(id) ON DELETE RESTRICT,
    epoch bigint NOT NULL CHECK (epoch > 0),
    state text NOT NULL CHECK (state IN ('dispatched','outcome_unknown','succeeded','reconciled')),
    admitted_at timestamptz NOT NULL DEFAULT now(),
    ended_at timestamptz,
    offset_start bigint NOT NULL CHECK (offset_start >= 0),
    offset_end bigint NOT NULL CHECK (offset_end >= offset_start),
    error_code text,
    PRIMARY KEY (upload_id,epoch),
    CHECK ((state IN ('succeeded','reconciled')) = (ended_at IS NOT NULL))
);
CREATE UNIQUE INDEX IF NOT EXISTS upload_patch_one_unresolved ON upload_patch_attempts(upload_id)
WHERE state IN ('dispatched','outcome_unknown');
CREATE INDEX IF NOT EXISTS uploads_completion_recovery ON upload_sessions(transfer_completed,operation_id);

CREATE OR REPLACE FUNCTION guard_upload_failure_evidence() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.state IN ('remote_confirmed','committed') AND NEW.state IS DISTINCT FROM OLD.state AND EXISTS (
        SELECT 1 FROM upload_sessions u WHERE u.operation_id=NEW.id AND
        (u.in_flight_patches>0 OR EXISTS (SELECT 1 FROM upload_patch_attempts a WHERE a.upload_id=u.id AND a.state IN ('dispatched','outcome_unknown')))
    ) THEN
        RAISE EXCEPTION 'OPERATION_PENDING: PATCH is unresolved' USING ERRCODE='55000';
    END IF;
    IF NEW.state='failed_confirmed' AND NEW.state IS DISTINCT FROM OLD.state AND EXISTS (
        SELECT 1 FROM upload_sessions u WHERE u.operation_id=NEW.id AND
        (u.in_flight_patches>0 OR EXISTS (SELECT 1 FROM upload_patch_attempts a WHERE a.upload_id=u.id AND a.state IN ('dispatched','outcome_unknown')) OR
        ((u.patch_epoch>0 OR u.legacy_dispatch_unknown) AND u.native_termination_confirmed_at IS NULL) OR u.transfer_completed)
    ) THEN
        RAISE EXCEPTION 'OPERATION_PENDING: upload termination is unconfirmed' USING ERRCODE='55000';
    END IF;
    RETURN NEW;
END $$;
DROP TRIGGER IF EXISTS upload_failure_evidence_guard ON file_operations;
CREATE TRIGGER upload_failure_evidence_guard BEFORE UPDATE OF state ON file_operations
FOR EACH ROW EXECUTE FUNCTION guard_upload_failure_evidence();
