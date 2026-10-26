package application

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"github.com/agentclouddrive/agent-clouddrive/internal/application/access"
	"github.com/agentclouddrive/agent-clouddrive/internal/domain"
)

func (s *Service) list(ctx context.Context, a Actor, in Object) (any, error) {
	if e := access.New(s.deps()).Scope(a, "drive.read"); e != nil {
		return nil, e
	}
	if pid := str(in, "parent_id"); pid != "" {
		if _, e := access.New(s.deps()).Owned(ctx, a, pid, domain.ActionList); e != nil {
			return nil, e
		}
	}
	limit := int(num(in, "limit"))
	if limit <= 0 {
		limit = 100
	}
	if limit > 100 {
		limit = 100
	}
	var cursor [2]string
	if raw := str(in, "cursor"); raw != "" {
		b, e := base64.RawURLEncoding.DecodeString(raw)
		if e != nil || json.Unmarshal(b, &cursor) != nil {
			return nil, domain.NewError(domain.InvalidArgument, "invalid cursor")
		}
	}
	items, e := many(ctx, s.DB, `SELECT to_jsonb(f)-'native_instance'-'native_resource_id'-'active_operation_id'||jsonb_build_object('size_bytes',f.size,'conversion',(SELECT jsonb_build_object('status',j.state,'error_code',j.error_code) FROM conversion_jobs j WHERE j.source_version_id=f.current_version_id ORDER BY generation DESC LIMIT 1)) FROM files f WHERE f.owner_id=$1 AND f.parent_id IS NOT DISTINCT FROM $2::uuid AND f.lifecycle='active' AND f.native_resource_id IS NOT NULL AND (f.kind='directory' OR f.current_version_id IS NOT NULL) AND ($3='' OR (f.name,f.id::text)>($3,$4)) ORDER BY f.name,f.id LIMIT $5`, a.Principal.ID, null(str(in, "parent_id")), cursor[0], cursor[1], limit+1)
	if e != nil {
		return nil, e
	}
	valid := []Object{}
	more := len(items) > limit
	if more {
		items = items[:limit]
	}
	for _, f := range items {
		if _, e = access.New(s.deps()).Owned(ctx, a, str(f, "id"), domain.ActionList); e == nil {
			valid = append(valid, f)
		}
	}
	var next any
	if more && len(items) > 0 {
		last := items[len(items)-1]
		next = base64.RawURLEncoding.EncodeToString(js([2]string{str(last, "name"), str(last, "id")}))
	}
	return Object{"items": valid, "next_cursor": next}, nil
}
