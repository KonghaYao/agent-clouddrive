\set ON_ERROR_STOP on
BEGIN;
CREATE FUNCTION pg_temp.assert_true(p_check boolean, p_message text) RETURNS void LANGUAGE plpgsql AS $$
BEGIN IF p_check IS DISTINCT FROM true THEN RAISE EXCEPTION 'TEST FAILED: %',p_message; END IF; END $$;
CREATE FUNCTION pg_temp.assert_raises(p_sql text, p_expected text) RETURNS void LANGUAGE plpgsql AS $$
DECLARE v_raised boolean := false;
BEGIN
    BEGIN EXECUTE p_sql;
    EXCEPTION WHEN OTHERS THEN
        IF position(p_expected IN SQLERRM) = 0 THEN RAISE EXCEPTION 'TEST FAILED: expected %, got %',p_expected,SQLERRM; END IF;
        v_raised := true;
    END;
    IF NOT v_raised THEN RAISE EXCEPTION 'TEST FAILED: expected error %',p_expected; END IF;
END $$;
CREATE FUNCTION pg_temp.commit_fixture(p_file uuid,p_owner uuid,p_size bigint) RETURNS file_versions LANGUAGE plpgsql AS $$
DECLARE f files; o file_operations; u upload_sessions; v file_versions;
BEGIN
    SELECT * INTO STRICT f FROM files WHERE id = p_file;
    SELECT * INTO o FROM begin_file_operation(gen_random_uuid(),p_file,p_owner,'agent',CASE WHEN f.current_version_id IS NULL THEN 'create' ELSE 'overwrite' END,
        f.metadata_revision,f.current_version_id,gen_random_uuid()::text,repeat('a',64),'{}');
    INSERT INTO upload_sessions(operation_id,expected_size,expected_version_id,node_id,expires_at) VALUES(o.id,p_size,f.current_version_id,'test-node',now()+interval '1 hour') RETURNING * INTO u;
    PERFORM reserve_upload_quota(u.id);
    UPDATE file_operations SET state='executing' WHERE id=o.id;
    UPDATE file_operations SET state='remote_confirmed',remote_result='{"verified":true}' WHERE id=o.id;
    SELECT * INTO v FROM commit_content_operation(o.id,gen_random_uuid(),p_size,repeat('b',64),'etag-' || o.id::text,jsonb_build_object('revision',o.id),
        COALESCE(f.native_resource_id,'native-' || f.id::text),'text/plain','test-recipe','{}');
    RETURN v;
END $$;
CREATE FUNCTION pg_temp.artifact_fixture(p_job uuid) RETURNS artifact_sets LANGUAGE plpgsql AS $$
DECLARE j conversion_jobs; a artifact_sets;
BEGIN
    UPDATE conversion_jobs SET state='processing',attempt=attempt+1,lease_token=lease_token+1,lease_owner='worker-test',lease_until=now()+interval '1 hour' WHERE id=p_job RETURNING * INTO j;
    INSERT INTO artifact_sets(source_version_id,generation,recipe_fingerprint,recipe_snapshot,manifest_key,markdown_key,manifest_sha256,markdown_sha256,completeness,index_state,indexed_at)
    VALUES(j.source_version_id,j.generation,j.recipe_fingerprint,j.recipe_snapshot,'manifests/' || j.id,'markdown/' || j.id,repeat('a',64),repeat('b',64),'{"status":"ready","missing_text_pages":[],"missing_description_pages":[]}', 'succeeded',now()) RETURNING * INTO a;
    RETURN a;
END $$;
DO $$
DECLARE
    owner uuid := gen_random_uuid(); other_owner uuid := gen_random_uuid(); space uuid := gen_random_uuid();
    f uuid := gen_random_uuid(); other_file uuid := gen_random_uuid(); parent uuid := gen_random_uuid(); child uuid := gen_random_uuid();
    v1 file_versions; v2 file_versions; v3 file_versions; op file_operations; u upload_sessions; q quota_accounts; r quota_reservations;
    j1 conversion_jobs; j2 conversion_jobs; j3 conversion_jobs; a1 artifact_sets; a2 artifact_sets; a3 artifact_sets;
    approval uuid := gen_random_uuid(); grant_id uuid := gen_random_uuid(); feedback_id uuid := gen_random_uuid(); fid uuid; result boolean;
BEGIN
    INSERT INTO principals(id,issuer,subject) VALUES(owner,'https://id.example','alice'),(other_owner,'https://id.example','bob');
    INSERT INTO spaces(id,owner_id,native_instance,native_space_id) VALUES(space,owner,'ocis','test-space');
    INSERT INTO quota_accounts(owner_id,limit_bytes) VALUES(owner,1000);
    INSERT INTO files(id,owner_id,space_id,name,path,kind,native_instance) VALUES(f,owner,space,'a.txt','/a.txt','file','ocis'),(other_file,owner,space,'b.txt','/b.txt','file','ocis');
    PERFORM pg_temp.assert_true(NOT file_is_active(f),'provisional files are hidden');
    SELECT * INTO v1 FROM pg_temp.commit_fixture(f,owner,300);
    PERFORM pg_temp.assert_true(file_is_active(f),'committed original becomes visible');
    SELECT * INTO q FROM quota_accounts WHERE owner_id=owner;
    PERFORM pg_temp.assert_true(q.used_bytes=300 AND q.reserved_bytes=0,'first commit charges quota exactly once');
    SELECT * INTO op FROM file_operations WHERE result_version_id=v1.id;
    SELECT * INTO v2 FROM commit_content_operation(op.id,gen_random_uuid(),300,repeat('b',64),'ignored','{"revision":"ignored"}','ignored','text/plain','test-recipe','{}');
    PERFORM pg_temp.assert_true(v2.id=v1.id,'idempotent committed operation returns original version');
    PERFORM pg_temp.assert_true((SELECT count(*)=1 FROM file_versions WHERE file_id=f),'idempotent retry does not create history');
    PERFORM pg_temp.assert_raises(format('SELECT pg_temp.commit_fixture(%L,%L,701)',other_file,owner),'QUOTA_EXCEEDED');
    PERFORM pg_temp.assert_true((SELECT active_operation_id IS NULL FROM files WHERE id=other_file),'failed atomic admission rolls back operation lock');
    SELECT * INTO v2 FROM pg_temp.commit_fixture(f,owner,450);
    SELECT * INTO v3 FROM pg_temp.commit_fixture(f,owner,100);
    SELECT * INTO q FROM quota_accounts WHERE owner_id=owner;
    PERFORM pg_temp.assert_true(q.used_bytes=100 AND q.reserved_bytes=0,'overwrite growth and shrink count latest version only');
    PERFORM pg_temp.assert_true(v3.version_seq=3 AND (SELECT count(*)=3 FROM file_versions WHERE file_id=f),'every non-idempotent overwrite retains immutable history');
    PERFORM pg_temp.assert_raises(format('UPDATE file_versions SET size=1 WHERE id=%L',v1.id),'IMMUTABLE_VERSION');
    PERFORM pg_temp.assert_raises(format('DELETE FROM file_versions WHERE id=%L',v1.id),'IMMUTABLE_VERSION');
    PERFORM pg_temp.assert_raises(format('DELETE FROM files WHERE id=%L',f),'HISTORY_RETAINED');
    PERFORM pg_temp.assert_raises(format('SELECT begin_file_operation(gen_random_uuid(),%L,%L,''agent'',''overwrite'',1,NULL,''stale'',%L,''{}'')',f,owner,repeat('a',64)),'VERSION_CONFLICT');
    PERFORM pg_temp.assert_raises(format('SELECT begin_file_operation(gen_random_uuid(),%L,%L,''agent'',''overwrite'',4,%L,''not-owner'',%L,''{}'')',f,other_owner,v3.id,repeat('a',64)),'NOT_FOUND');
    SELECT * INTO op FROM begin_file_operation(gen_random_uuid(),f,owner,'agent','overwrite',4,v3.id,'unknown-operation',repeat('c',64),'{}');
    INSERT INTO upload_sessions(operation_id,expected_size,expected_version_id,node_id,expires_at) VALUES(op.id,500,v3.id,'test',now()+interval '1 hour') RETURNING * INTO u;
    SELECT * INTO r FROM reserve_upload_quota(u.id);
    PERFORM pg_temp.assert_true(r.bytes=400,'replacement reserves growth only');
    UPDATE file_operations SET state='executing' WHERE id=op.id;
    UPDATE file_operations SET state='outcome_unknown' WHERE id=op.id;
    PERFORM pg_temp.assert_raises(format('SELECT release_upload_quota(%L)',u.id),'UNRESOLVED_REMOTE_OUTCOME');
    PERFORM pg_temp.assert_raises(format('UPDATE files SET active_operation_id=NULL WHERE id=%L',f),'OPERATION_PENDING');
    PERFORM pg_temp.assert_raises(format('SELECT begin_file_operation(gen_random_uuid(),%L,%L,''agent'',''move'',4,%L,''blocked-move'',%L,''{}'')',f,owner,v3.id,repeat('a',64)),'OPERATION_PENDING');
    PERFORM pg_temp.assert_raises(format('UPDATE file_operations SET state=''failed_confirmed'' WHERE id=%L',op.id),'RECONCILIATION_EVIDENCE_REQUIRED');
    UPDATE file_operations SET state='failed_confirmed',remote_result='{"reconciliation":"not committed"}' WHERE id=op.id;
    PERFORM pg_temp.assert_true(release_upload_quota(u.id),'reconciliation-confirmed failure releases reservation');
    PERFORM pg_temp.assert_true(NOT release_upload_quota(u.id),'repeated release is idempotent');
    PERFORM pg_temp.assert_true((SELECT used_bytes=100 AND reserved_bytes=0 FROM quota_accounts WHERE owner_id=owner),'unknown operation never prematurely frees quota');
    PERFORM pg_temp.assert_raises(format('SELECT begin_file_operation(gen_random_uuid(),%L,%L,''agent'',''overwrite'',4,%L,''unknown-operation'',%L,''{}'')',f,owner,v3.id,repeat('d',64)),'IDEMPOTENCY_CONFLICT');
    SELECT * INTO j1 FROM conversion_jobs WHERE source_version_id=v1.id;
    SELECT * INTO j2 FROM conversion_jobs WHERE source_version_id=v3.id;
    SELECT * INTO a1 FROM pg_temp.artifact_fixture(j1.id);
    SELECT * INTO a2 FROM pg_temp.artifact_fixture(j2.id);
    PERFORM pg_temp.assert_true(publish_artifact(j2.id,1,a2.id),'latest version publishes');
    PERFORM pg_temp.assert_true(NOT publish_artifact(j1.id,1,a1.id),'late old version cannot regress search pointer');
    PERFORM pg_temp.assert_true((SELECT search_artifact_set_id=a2.id FROM files WHERE id=f),'search points at higher source sequence');
    PERFORM pg_temp.assert_raises(format('UPDATE artifact_sets SET markdown_key=''changed'' WHERE id=%L',a2.id),'IMMUTABLE_PUBLISHED_ARTIFACT');
    PERFORM pg_temp.assert_raises(format('DELETE FROM artifact_sets WHERE id=%L',a2.id),'HISTORY_RETAINED');
    SELECT * INTO j3 FROM request_conversion(v3.id,'recipe-v2','{"engine":"2"}');
    SELECT * INTO a3 FROM pg_temp.artifact_fixture(j3.id);
    UPDATE conversion_jobs SET lease_until=now()-interval '1 second' WHERE id=j3.id;
    PERFORM pg_temp.assert_raises(format('SELECT publish_artifact(%L,1,%L)',j3.id,a3.id),'STALE_WORKER');
    UPDATE conversion_jobs SET attempt=attempt+1,lease_token=lease_token+1,lease_until=now()+interval '1 hour' WHERE id=j3.id;
    PERFORM pg_temp.assert_raises(format('SELECT publish_artifact(%L,1,%L)',j3.id,a3.id),'STALE_WORKER');
    PERFORM pg_temp.assert_true(publish_artifact(j3.id,2,a3.id),'renewed fenced attempt publishes newer generation');
    PERFORM pg_temp.assert_true((SELECT search_artifact_set_id=a3.id FROM files WHERE id=f),'new generation selected');
    PERFORM pg_temp.assert_true((SELECT published_at IS NOT NULL FROM artifact_sets WHERE id=a2.id),'old cited generation preserved');
    INSERT INTO share_approvals(id,requester_principal,file_id,request_hash,request_body,expires_at)
      VALUES(approval,owner,f,repeat('a',64),jsonb_build_object('audience_type','user','audience_id',other_owner::text,'version_policy','latest','rights',jsonb_build_array('read','download')),now()+interval '15 minutes');
    PERFORM pg_temp.assert_raises(format('UPDATE share_approvals SET request_body=''{}'' WHERE id=%L',approval),'IMMUTABLE_SHARE_REQUEST');
    UPDATE share_approvals SET state='approved',approved_by=owner,decided_at=now() WHERE id=approval;
    INSERT INTO share_grants(id,approval_id,file_id,creator_principal_id,audience_type,audience_id,rights,version_policy,approved_by)
      VALUES(grant_id,approval,f,owner,'user',other_owner::text,ARRAY['read','download'],'latest',owner);
    UPDATE principals SET status='disabled' WHERE id=owner;
    PERFORM pg_temp.assert_true(NOT file_is_active(f),'disabled account closes file access');
    PERFORM pg_temp.assert_true((SELECT state='disabled' FROM share_grants WHERE id=grant_id),'disabled account revokes shares');
    UPDATE principals SET status='active' WHERE id=owner;
    PERFORM pg_temp.assert_true((SELECT state='disabled' FROM share_grants WHERE id=grant_id),'account re-enable does not revive shares');
    PERFORM pg_temp.assert_raises(format('UPDATE share_grants SET state=''active'' WHERE id=%L',grant_id),'SHARE_RECONFIRMATION_REQUIRED');
    INSERT INTO feedback(id,file_id,version_id,artifact_set_id,category,body) VALUES(feedback_id,f,v3.id,a3.id,'text','Incorrect text on page 1');
    PERFORM pg_temp.assert_true((SELECT count(*)=8 FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='feedback'),'feedback has only eight approved fields');
    PERFORM pg_temp.assert_true(NOT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='feedback' AND column_name ~* 'actor|author|session|trace|request|operation|created_at|ip_address'),'feedback stores no source linkage');
    PERFORM pg_temp.assert_raises(format('UPDATE feedback SET body=''different'' WHERE id=%L',feedback_id),'IMMUTABLE_FEEDBACK');
    UPDATE feedback SET triage_state='reviewed' WHERE id=feedback_id;
    PERFORM pg_temp.assert_raises(format('DELETE FROM feedback WHERE id=%L',feedback_id),'HISTORY_RETAINED');
    INSERT INTO files(id,owner_id,space_id,name,path,kind,native_instance,native_resource_id) VALUES(parent,owner,space,'parent','/parent','directory','ocis','native-parent');
    INSERT INTO files(id,owner_id,space_id,parent_id,name,path,kind,native_instance,native_resource_id) VALUES(child,owner,space,parent,'child','/parent/child','directory','ocis','native-child');
    PERFORM pg_temp.assert_raises(format('UPDATE files SET parent_id=%L WHERE id=%L',child,parent),'INVALID_PATH');
    UPDATE files SET lifecycle='deleting' WHERE id=parent;
    PERFORM pg_temp.assert_true(NOT file_is_active(child),'ancestor deletion immediately gates descendants');
    PERFORM pg_temp.assert_true((SELECT used_bytes=100 FROM quota_accounts WHERE owner_id=owner),'delete never frees latest-version quota');
    RAISE NOTICE 'PASS: quota, unknown-outcome safeguards, idempotency, immutability, version and generation priority, stale workers, share approval, disable/re-enable, anonymous feedback, ancestor gates';
END $$;
ROLLBACK;
