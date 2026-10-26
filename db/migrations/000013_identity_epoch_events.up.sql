-- 停用历史不能由当前 enabled 状态代替；历史授权与事件均保留。
ALTER TABLE principals ADD COLUMN IF NOT EXISTS identity_epoch bigint NOT NULL DEFAULT 0 CHECK(identity_epoch >= 0);
ALTER TABLE share_grants ADD COLUMN IF NOT EXISTS identity_epoch bigint NOT NULL DEFAULT 0;
ALTER TABLE audit_grants ADD COLUMN IF NOT EXISTS id uuid NOT NULL DEFAULT gen_random_uuid();
CREATE UNIQUE INDEX IF NOT EXISTS audit_grants_id ON audit_grants(id);
ALTER TABLE audit_grants ADD COLUMN IF NOT EXISTS identity_epoch bigint NOT NULL DEFAULT 0;
ALTER TABLE audit_grants ADD COLUMN IF NOT EXISTS auditor_epoch bigint NOT NULL DEFAULT 0;

CREATE TABLE IF NOT EXISTS identity_event_cursors (
 source text PRIMARY KEY,
 anchor text NOT NULL,
 updated_at timestamptz NOT NULL DEFAULT now(),
 gap_count bigint NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS identity_events (
 source text NOT NULL,
 event_id text NOT NULL,
 subject text NOT NULL,
 disabled boolean NOT NULL,
 occurred_at timestamptz NOT NULL,
 processed_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(source,event_id)
);

CREATE OR REPLACE FUNCTION guard_identity_epoch() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.identity_epoch < OLD.identity_epoch THEN
  RAISE EXCEPTION 'IDENTITY_EPOCH_REGRESSION' USING ERRCODE='23514';
 END IF;
 IF NEW.status='disabled' AND OLD.status<>'disabled' THEN
  NEW.identity_epoch := GREATEST(NEW.identity_epoch,OLD.identity_epoch+1);
 END IF;
 RETURN NEW;
END $$;
DROP TRIGGER IF EXISTS principals_epoch_guard ON principals;
CREATE TRIGGER principals_epoch_guard BEFORE UPDATE ON principals FOR EACH ROW EXECUTE FUNCTION guard_identity_epoch();

CREATE OR REPLACE FUNCTION revoke_principal_shares() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.identity_epoch <> OLD.identity_epoch OR (NEW.status='disabled' AND OLD.status<>'disabled') THEN
  UPDATE share_grants SET state=CASE WHEN NEW.status='disabled' THEN 'disabled' ELSE 'revoked' END,
   disabled_reason=CASE WHEN NEW.status='disabled' THEN 'account_disabled' ELSE 'identity_epoch_changed' END,revoked_at=now()
   WHERE state='active' AND (creator_principal_id=NEW.id OR file_id IN(SELECT id FROM files WHERE owner_id=NEW.id));
  UPDATE share_approvals SET state='rejected',decided_at=now()
   WHERE state='pending' AND (requester_principal=NEW.id OR file_id IN(SELECT id FROM files WHERE owner_id=NEW.id));
 END IF;
 RETURN NEW;
END $$;
-- 既有触发器只监听 status；epoch 变化也必须触发撤销。
DROP TRIGGER IF EXISTS principals_disable_shares ON principals;
DROP TRIGGER IF EXISTS principals_epoch_shares ON principals;
CREATE TRIGGER principals_epoch_shares AFTER UPDATE OF status,identity_epoch ON principals FOR EACH ROW EXECUTE FUNCTION revoke_principal_shares();

CREATE OR REPLACE FUNCTION bind_share_epoch() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 SELECT identity_epoch INTO STRICT NEW.identity_epoch FROM principals WHERE id=NEW.creator_principal_id FOR SHARE;
 RETURN NEW;
END $$;
DROP TRIGGER IF EXISTS grants_bind_epoch ON share_grants;
CREATE TRIGGER grants_bind_epoch BEFORE INSERT ON share_grants FOR EACH ROW EXECUTE FUNCTION bind_share_epoch();

CREATE OR REPLACE FUNCTION bind_audit_epoch() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 SELECT p.identity_epoch INTO STRICT NEW.identity_epoch FROM principals p JOIN files f ON f.owner_id=p.id WHERE f.id=NEW.file_id FOR SHARE OF p;
 SELECT identity_epoch INTO STRICT NEW.auditor_epoch FROM principals WHERE id=NEW.principal_id FOR SHARE;
 RETURN NEW;
END $$;
DROP TRIGGER IF EXISTS audit_bind_epoch ON audit_grants;
CREATE TRIGGER audit_bind_epoch BEFORE INSERT ON audit_grants FOR EACH ROW EXECUTE FUNCTION bind_audit_epoch();

-- 分享解析统一校验创建者/owner 状态与代次，普通文件读取不受此函数影响。
CREATE OR REPLACE FUNCTION share_identity_valid(p_grant uuid) RETURNS boolean LANGUAGE sql STABLE AS $$
 SELECT COALESCE(bool_and(g.identity_epoch=creator.identity_epoch AND g.identity_epoch=owner.identity_epoch AND creator.status='active' AND owner.status='active'),false)
 FROM share_grants g JOIN files f ON f.id=g.file_id JOIN principals owner ON owner.id=f.owner_id JOIN principals creator ON creator.id=g.creator_principal_id WHERE g.id=p_grant;
$$;
-- 旧库无法证明迁移前事件连续性。首次同步缺失游标时提升 epoch 并永久撤销旧分享，必须人工新确认。
