package access

import (
	"context"
	"time"

	"github.com/agentclouddrive/agent-clouddrive/internal/application/core"
	"github.com/agentclouddrive/agent-clouddrive/internal/domain"
	"github.com/agentclouddrive/agent-clouddrive/internal/ports"
)

func (s *Service) Owned(ctx context.Context, a core.Actor, fid string, action domain.Action) (core.Object, error) {
	f, e := core.One(ctx, s.d.DB, `SELECT to_jsonb(f) FROM files f WHERE id=$1 AND owner_id=$2`, fid, a.Principal.ID)
	if e != nil {
		return nil, e
	}
	rows, e := s.d.DB.Query(ctx, `WITH RECURSIVE parents AS(SELECT p.id,p.parent_id,p.lifecycle FROM files p WHERE p.id=$1 UNION ALL SELECT p.id,p.parent_id,p.lifecycle FROM files p JOIN parents c ON p.id=c.parent_id) SELECT lifecycle FROM parents`, core.Null(core.Str(f, "parent_id")))
	if e != nil {
		return nil, e
	}
	anc := []domain.Lifecycle{}
	for rows.Next() {
		var l string
		_ = rows.Scan(&l)
		anc = append(anc, domain.Lifecycle(l))
	}
	rows.Close()
	e = domain.Authorize(domain.AccessRequest{Principal: a.Principal, File: core.FileDomain(f), Action: action, Ancestors: anc, OwnerActive: true, OwnerStatusVerified: true, Now: time.Now()})
	if e != nil {
		return nil, e
	}
	return f, nil
}
func (s *Service) Space(ctx context.Context, a core.Actor) (core.Object, error) {
	sp, e := core.One(ctx, s.d.DB, `SELECT to_jsonb(s) FROM spaces s WHERE owner_id=$1`, a.Principal.ID)
	if e == nil {
		return sp, nil
	}
	resolver, ok := s.d.Original.(ports.PersonalSpaceResolver)
	if !ok {
		return nil, core.Dependency()
	}
	cred, e := s.Credential(ctx, a, ports.RevisionRef{})
	if e != nil {
		return nil, e
	}
	native, e := resolver.ResolvePersonalSpace(ctx, cred)
	if e != nil {
		return nil, e
	}
	return core.One(ctx, s.d.DB, `INSERT INTO spaces(owner_id,native_instance,native_space_id,native_root_resource_id) VALUES($1,$2,$3,$4) ON CONFLICT(owner_id) DO UPDATE SET owner_id=excluded.owner_id RETURNING to_jsonb(spaces)`, a.Principal.ID, s.d.Config.OCISURL, native.SpaceID, native.RootResourceID)
}
func (s *Service) Ref(ctx context.Context, f core.Object) (ports.ResourceRef, error) {
	var sid, instance string
	e := s.d.DB.QueryRow(ctx, `SELECT native_space_id,native_instance FROM spaces WHERE id=$1`, core.Str(f, "space_id")).Scan(&sid, &instance)
	return ports.ResourceRef{Instance: instance, SpaceID: sid, ResourceID: core.Str(f, "native_resource_id")}, e
}
func (s *Service) Parent(ctx context.Context, a core.Actor, pid string) (core.Object, ports.ResourceRef, error) {
	return s.mutationParent(ctx, a, pid, false)
}

// RestoreParent 仅授予恢复目标校验，不改变调用方 scopes。
func (s *Service) RestoreParent(ctx context.Context, a core.Actor, pid string) (core.Object, ports.ResourceRef, error) {
	return s.mutationParent(ctx, a, pid, true)
}

func (s *Service) mutationParent(ctx context.Context, a core.Actor, pid string, restore bool) (core.Object, ports.ResourceRef, error) {
	if restore {
		if e := s.Scope(a, domain.ScopeRestore); e != nil {
			return nil, ports.ResourceRef{}, e
		}
	}
	sp, e := s.Space(ctx, a)
	if e != nil {
		return nil, ports.ResourceRef{}, e
	}
	if pid == "" {
		ref := ports.ResourceRef{Instance: core.Str(sp, "native_instance"), SpaceID: core.Str(sp, "native_space_id"), ResourceID: core.Str(sp, "native_root_resource_id")}
		return core.Object{"id": "", "path": "/", "space_id": core.Str(sp, "id")}, ref, nil
	}
	var p core.Object
	if restore {
		p, e = core.One(ctx, s.d.DB, `SELECT to_jsonb(f) FROM files f WHERE id=$1 AND owner_id=$2 AND space_id=$3 AND file_is_active(id)`, pid, a.Principal.ID, core.Str(sp, "id"))
	} else {
		p, e = s.Owned(ctx, a, pid, domain.ActionWrite)
	}
	if e != nil {
		return nil, ports.ResourceRef{}, e
	}
	if core.Str(p, "native_resource_id") == "" {
		return nil, ports.ResourceRef{}, domain.NewError(domain.OperationPending, "directory creation is not confirmed")
	}
	if core.Str(p, "kind") != "directory" {
		return nil, ports.ResourceRef{}, domain.NewError(domain.InvalidPath, "parent is not a directory")
	}
	r, e := s.Ref(ctx, p)
	return p, r, e
}
