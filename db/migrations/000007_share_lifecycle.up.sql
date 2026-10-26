-- Closing a file/account invalidates pending confirmations, not only issued grants.
-- A restore/re-enable always requires a fresh, explicit request and confirmation.
CREATE OR REPLACE FUNCTION revoke_principal_shares() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.status = 'disabled' AND OLD.status <> 'disabled' THEN
        UPDATE share_grants SET state = 'disabled', disabled_reason = 'account_disabled', revoked_at = now()
            WHERE creator_principal_id = NEW.id AND state = 'active';
        UPDATE share_approvals SET state = 'rejected', decided_at = now()
            WHERE state = 'pending' AND (requester_principal = NEW.id OR file_id IN (SELECT id FROM files WHERE owner_id = NEW.id));
    END IF;
    RETURN NEW;
END $$;
CREATE OR REPLACE FUNCTION revoke_subtree_shares() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.lifecycle <> 'active' AND OLD.lifecycle = 'active' THEN
        WITH RECURSIVE subtree AS (
            SELECT NEW.id AS id UNION ALL SELECT f.id FROM files f JOIN subtree s ON f.parent_id = s.id
        ) UPDATE share_grants SET state = 'disabled', disabled_reason = 'file_deleted', revoked_at = now()
            WHERE file_id IN (SELECT id FROM subtree) AND state = 'active';
        WITH RECURSIVE subtree AS (
            SELECT NEW.id AS id UNION ALL SELECT f.id FROM files f JOIN subtree s ON f.parent_id = s.id
        ) UPDATE share_approvals SET state = 'rejected', decided_at = now()
            WHERE file_id IN (SELECT id FROM subtree) AND state = 'pending';
    END IF;
    RETURN NEW;
END $$;

CREATE FUNCTION guard_new_share_request() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE v_file files; v_principal principals;
BEGIN
    SELECT * INTO STRICT v_file FROM files WHERE id = NEW.file_id;
    PERFORM pg_advisory_xact_lock(hashtextextended(v_file.space_id::text,0));
    SELECT * INTO STRICT v_file FROM files WHERE id = NEW.file_id FOR SHARE;
    SELECT * INTO STRICT v_principal FROM principals WHERE id = v_file.owner_id FOR SHARE;
    IF v_file.kind <> 'file' OR NOT file_is_active(v_file.id) OR v_principal.status <> 'active' OR
       NEW.requester_principal <> v_file.owner_id OR NEW.state <> 'pending' OR NEW.approved_by IS NOT NULL THEN
        RAISE EXCEPTION 'SHARE_FILE_INACTIVE' USING ERRCODE = '42501';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER approvals_insert_lifecycle BEFORE INSERT ON share_approvals FOR EACH ROW EXECUTE FUNCTION guard_new_share_request();

CREATE FUNCTION guard_grant_lifecycle() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE v_file files; v_principal principals; v_approval share_approvals; v_expiry timestamptz;
BEGIN
    SELECT * INTO STRICT v_file FROM files WHERE id = NEW.file_id;
    PERFORM pg_advisory_xact_lock(hashtextextended(v_file.space_id::text,0));
    SELECT * INTO STRICT v_file FROM files WHERE id = NEW.file_id FOR SHARE;
    -- FOR SHARE serializes against account disable, even without an advisory lock
    -- in the identity-event transaction. The disable trigger then sees this grant.
    SELECT * INTO STRICT v_principal FROM principals WHERE id = v_file.owner_id FOR SHARE;
    SELECT * INTO STRICT v_approval FROM share_approvals WHERE id = NEW.approval_id FOR SHARE;
    IF NOT file_is_active(v_file.id) OR v_principal.status <> 'active' OR v_approval.state <> 'approved' OR v_approval.expires_at <= clock_timestamp() THEN
        RAISE EXCEPTION 'SHARE_FILE_INACTIVE' USING ERRCODE = '42501';
    END IF;
    v_expiry := NULLIF(NULLIF(v_approval.request_body->>'expires_at','0001-01-01T00:00:00Z'),'')::timestamptz;
    IF v_expiry IS DISTINCT FROM NEW.expires_at THEN
        RAISE EXCEPTION 'APPROVAL_EXPIRY_MISMATCH' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER grants_insert_lifecycle BEFORE INSERT ON share_grants FOR EACH ROW EXECUTE FUNCTION guard_grant_lifecycle();
