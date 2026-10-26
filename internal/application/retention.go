package application

import (
	"context"
	"errors"
	"time"

	"github.com/agentclouddrive/agent-clouddrive/internal/domain"
	"github.com/agentclouddrive/agent-clouddrive/internal/ports"
)

func (s *Service) claimRetentionRoot(ctx context.Context, spaceID string) (Object, error) {
	r, err := one(ctx, s.DB, `UPDATE retention_roots SET lease_owner=$2,lease_token=lease_token+1,lease_until=now()+interval '2 minutes',retry_at=now()+interval '1 minute',attempt=attempt+1,updated_at=now() WHERE space_id=$1 AND state<>'ready' AND retry_at<=now() AND (lease_until IS NULL OR lease_until<=now()) RETURNING to_jsonb(retention_roots)`, spaceID, s.Config.NodeID)
	if domain.ErrorCode(err) == domain.NotFound {
		return nil, domain.NewError(domain.OperationPending, "retention reconciliation is leased or awaiting retry")
	}
	return r, err
}

// recoverRetentionRoot 只采纳受保护命名空间内、固定父身份下的目录。
// 未知 MKCOL 可以按同一名称重试：MKCOL 不覆盖，同名响应必须再读身份。
func (s *Service) recoverRetentionRoot(ctx context.Context, record Object, parent ports.ResourceRef, cred ports.Credential) (ports.ResourceRef, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	root := parent
	root.ResourceID = ""
	name := str(record, "directory_name")
	if name == "" {
		name = s.Config.TrashDir
	}
	if pinned := str(record, "native_parent_id"); pinned != "" && pinned != parent.ResourceID {
		return root, s.deferRetention(ctx, record, "PARENT_IDENTITY_MISMATCH")
	}
	var meta ports.OriginalMetadata
	lookup, canLookup := s.Original.(childLookup)
	err := ports.ErrNotFound
	if canLookup {
		meta, err = lookup.LookupChild(ctx, parent, name, cred)
	} else if str(record, "state") != "prepared" {
		return root, s.deferRetention(ctx, record, "LOOKUP_CAPABILITY_REQUIRED")
	}
	if errors.Is(err, ports.ErrNotFound) {
		tag, persistErr := s.DB.Exec(ctx, `UPDATE retention_roots SET state='executing',directory_name=$3,native_parent_id=$4,updated_at=now() WHERE space_id=$1 AND lease_token=$2 AND lease_until>now()`, str(record, "space_id"), num(record, "lease_token"), name, parent.ResourceID)
		if persistErr != nil {
			return root, persistErr
		}
		if tag.RowsAffected() != 1 {
			return root, domain.NewError(domain.LeaseLost, "retention lease lost")
		}
		meta, err = s.Original.CreateDirectory(ctx, ports.DirectoryRequest{Parent: parent, Name: name, Credential: cred})
		if errors.Is(err, ports.ErrConflict) && canLookup {
			meta, err = lookup.LookupChild(ctx, parent, name, cred)
		}
	}
	if err != nil {
		return root, s.deferRetention(ctx, record, "NATIVE_OUTCOME_UNCONFIRMED")
	}
	if meta.ResourceID == "" || meta.SpaceID != parent.SpaceID || meta.Name != name || !meta.IsDirectory || (str(record, "native_resource_id") != "" && str(record, "native_resource_id") != meta.ResourceID) {
		return root, s.deferRetention(ctx, record, "NATIVE_IDENTITY_MISMATCH")
	}
	persist, done := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer done()
	tag, err := s.DB.Exec(persist, `UPDATE retention_roots SET state='ready',native_resource_id=$3,directory_name=$4,native_parent_id=$5,last_error=NULL,lease_owner=NULL,lease_until=NULL,updated_at=now() WHERE space_id=$1 AND lease_token=$2 AND lease_until>now()`, str(record, "space_id"), num(record, "lease_token"), meta.ResourceID, name, parent.ResourceID)
	if err != nil {
		return root, err
	}
	if tag.RowsAffected() != 1 {
		return root, domain.NewError(domain.LeaseLost, "retention lease lost")
	}
	root.ResourceID = meta.ResourceID
	return root, nil
}

func (s *Service) deferRetention(ctx context.Context, record Object, code string) error {
	persist, done := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer done()
	_, err := s.DB.Exec(persist, `UPDATE retention_roots SET state='outcome_unknown',last_error=$3,retry_at=now()+interval '30 seconds',lease_owner=NULL,lease_until=NULL,updated_at=now() WHERE space_id=$1 AND lease_token=$2 AND lease_until>now()`, str(record, "space_id"), num(record, "lease_token"), code)
	if err != nil {
		return err
	}
	return domain.NewError(domain.OperationPending, "retention reconciliation pending: "+code)
}

// ReconcileRetentionRoots 是重启后的持久恢复入口。每轮有界，失败保留诊断与重试时间。
func (s *Service) ReconcileRetentionRoots(ctx context.Context) error {
	if s.Identity == nil || s.Original == nil {
		return dependency()
	}
	rows, err := many(ctx, s.DB, `SELECT to_jsonb(r) FROM retention_roots r WHERE state<>'ready' AND retry_at<=now() AND (lease_until IS NULL OR lease_until<=now()) ORDER BY retry_at,space_id LIMIT 20`)
	if err != nil {
		return err
	}
	var first error
	for _, row := range rows {
		record, claimErr := s.claimRetentionRoot(ctx, str(row, "space_id"))
		if claimErr != nil {
			continue
		}
		if err = s.reconcileRetentionRecord(ctx, record); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func (s *Service) reconcileRetentionRecord(ctx context.Context, record Object) error {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	sp, err := one(ctx, s.DB, `SELECT to_jsonb(s)||jsonb_build_object('issuer',p.issuer,'subject',p.subject) FROM spaces s JOIN principals p ON p.id=s.owner_id WHERE s.id=$1 AND p.status='active'`, str(record, "space_id"))
	if err != nil {
		return s.deferRetention(ctx, record, "OWNER_UNAVAILABLE")
	}
	subject := ports.Subject{Issuer: str(sp, "issuer"), Subject: str(sp, "subject")}
	status, err := s.Identity.SubjectStatus(ctx, subject.Issuer, subject.Subject)
	if err != nil || !status.Enabled {
		return s.deferRetention(ctx, record, "OWNER_STATUS_UNVERIFIED")
	}
	parent := ports.ResourceRef{Instance: str(sp, "native_instance"), SpaceID: str(sp, "native_space_id"), ResourceID: str(sp, "native_root_resource_id")}
	cred, err := s.Identity.DelegatedCredential(ctx, ports.DelegationRequest{Subject: subject, Audience: s.Config.OCISAudience, Source: ports.RevisionRef{ResourceRef: parent}, JobID: str(record, "operation_id")})
	if err != nil {
		return s.deferRetention(ctx, record, "DELEGATION_UNAVAILABLE")
	}
	_, err = s.recoverRetentionRoot(ctx, record, parent, cred)
	return err
}
