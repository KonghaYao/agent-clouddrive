package application

import (
	"context"
	"encoding/json"
	"errors"
	"path"
	"time"

	"github.com/agentclouddrive/agent-clouddrive/internal/domain"
	"github.com/agentclouddrive/agent-clouddrive/internal/ports"
)

// convergeRetainedTarget 修复迁移前已成功移入失效父目录的 MOVE。
// 补偿计划在远端调用前持久化；重启先读稳定父身份，绝不盲目重复 MOVE。
func (s *Service) convergeRetainedTarget(ctx context.Context, op, f Object, fence int64) (any, error) {
	if fence == 0 {
		return nil, domain.NewError(domain.OperationPending, "retained target requires leased reconciliation")
	}
	if str(op, "kind") != "move" && str(op, "kind") != "rename" && str(op, "kind") != "restore" {
		return nil, domain.NewError(domain.OperationPending, "operation requires controlled recovery")
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	intent, _ := op["intent"].(map[string]any)
	parentID := str(intent, "old_parent_id")
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, str(f, "space_id")); err != nil {
		return nil, err
	}
	sp, err := one(ctx, tx, `SELECT to_jsonb(s) FROM spaces s WHERE id=$1`, str(f, "space_id"))
	if err != nil {
		return nil, err
	}
	parent := ports.ResourceRef{Instance: str(sp, "native_instance"), SpaceID: str(sp, "native_space_id"), ResourceID: str(sp, "native_root_resource_id")}
	parentPath := "/"
	if parentID != "" {
		p, e := one(ctx, tx, `SELECT to_jsonb(f) FROM files f WHERE id=$1 AND owner_id=$2 AND space_id=$3 AND file_is_active(id) AND active_operation_id IS NULL`, parentID, str(f, "owner_id"), str(f, "space_id"))
		if e != nil {
			return nil, domain.NewError(domain.OperationPending, "original parent unavailable for compensation")
		}
		parent.ResourceID, parentPath = str(p, "native_resource_id"), str(p, "path")
		_, err = tx.Exec(ctx, `WITH RECURSIVE a AS(SELECT id,parent_id FROM files WHERE id=$2 UNION ALL SELECT f.id,f.parent_id FROM files f JOIN a ON f.id=a.parent_id) INSERT INTO file_operation_holds(operation_id,file_id,hold_kind) SELECT $1,id,'ancestor' FROM a ON CONFLICT DO NOTHING`, str(op, "id"), parentID)
		if err != nil {
			return nil, err
		}
	}
	projection := Object{"parent_id": parentID, "name": str(intent, "old_name"), "path": path.Join(parentPath, str(intent, "old_name"))}
	if str(op, "kind") == "restore" {
		r, e := one(ctx, tx, `SELECT to_jsonb(r) FROM retention_roots r WHERE space_id=$1 AND state='ready'`, str(f, "space_id"))
		if e != nil {
			return nil, e
		}
		parent.ResourceID = str(r, "native_resource_id")
		projection["parent_id"], projection["path"], projection["lifecycle"] = "", str(intent, "old_path"), "trashed"
	}
	var collision bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM files WHERE space_id=$1 AND path=$2 AND id<>$3 AND lifecycle IN('active','deleting','restoring') UNION ALL SELECT 1 FROM file_operations o JOIN files f ON f.id=o.file_id WHERE f.space_id=$1 AND f.id<>$3 AND o.state NOT IN('committed','failed_confirmed') AND (o.intent->>'target_path'=$2 OR o.remote_result->'compensation_plan'->'projection'->>'path'=$2))`, str(f, "space_id"), str(projection, "path"), str(f, "id")).Scan(&collision); err != nil {
		return nil, err
	}
	if collision {
		return nil, domain.NewError(domain.PathConflict, "compensation path occupied")
	}
	tag, err := tx.Exec(ctx, `UPDATE file_operations SET remote_result=remote_result||jsonb_build_object('compensation_plan',$3::jsonb) WHERE id=$1 AND lease_token=$2 AND lease_until>now() AND state='remote_confirmed'`, str(op, "id"), fence, js(Object{"parent": parent, "projection": projection}))
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() != 1 {
		return nil, domain.NewError(domain.LeaseLost, "compensation lease lost")
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return s.executeCompensation(ctx, op, f, fence, parent, projection)
}

func (s *Service) executeCompensation(ctx context.Context, op, f Object, fence int64, parent ports.ResourceRef, projection Object) (any, error) {
	lookup, ok := s.Original.(childLookup)
	if !ok || s.Identity == nil {
		return nil, dependency()
	}
	principal, err := one(ctx, s.DB, `SELECT to_jsonb(p) FROM principals p WHERE id=$1 AND status='active'`, str(f, "owner_id"))
	if err != nil {
		return nil, err
	}
	subject := ports.Subject{Issuer: str(principal, "issuer"), Subject: str(principal, "subject")}
	status, err := s.Identity.SubjectStatus(ctx, subject.Issuer, subject.Subject)
	if err != nil || !status.Enabled {
		return nil, dependency()
	}
	intent, _ := op["intent"].(map[string]any)
	var source ports.ResourceRef
	if json.Unmarshal(js(intent["source"]), &source) != nil {
		return nil, dependency()
	}
	cred, err := s.Identity.DelegatedCredential(ctx, ports.DelegationRequest{Subject: subject, Audience: s.Config.OCISAudience, Source: ports.RevisionRef{ResourceRef: source}, JobID: str(op, "id")})
	if err != nil {
		return nil, err
	}
	meta, lookupErr := lookup.LookupChild(ctx, parent, str(projection, "name"), cred)
	if lookupErr == nil && meta.ResourceID != source.ResourceID {
		return nil, domain.NewError(domain.PathConflict, "compensation destination occupied")
	}
	if lookupErr != nil {
		if !errors.Is(lookupErr, ports.ErrNotFound) {
			return nil, lookupErr
		}
		current, e := s.Original.Stat(ctx, source, cred)
		if e != nil {
			return nil, e
		}
		meta, err = s.Original.Move(ctx, ports.MoveRequest{Source: source, DestinationParent: parent, Name: str(projection, "name"), ExpectedETag: current.ETag, Credential: cred})
		if err != nil {
			return nil, err
		}
	}
	if meta.ResourceID != source.ResourceID || meta.SpaceID != source.SpaceID || meta.Name != str(projection, "name") {
		return nil, ports.ErrIntegrity
	}
	if err = s.verifyRetainedRevisions(ctx, str(f, "id"), cred); err != nil {
		return nil, err
	}
	tag, err := s.DB.Exec(ctx, `UPDATE file_operations SET remote_result=remote_result||jsonb_build_object('metadata',$3::jsonb,'converged_projection',$4::jsonb,'compensated',true) WHERE id=$1 AND lease_token=$2 AND lease_until>now() AND state='remote_confirmed'`, str(op, "id"), fence, js(meta), js(projection))
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() != 1 {
		return nil, domain.NewError(domain.LeaseLost, "compensation lease lost")
	}
	return s.commitMetadataMutationFenced(ctx, str(op, "id"), fence)
}
