package access

import (
	"context"

	"github.com/agentclouddrive/agent-clouddrive/internal/application/core"
	"github.com/agentclouddrive/agent-clouddrive/internal/domain"
)

func (s *Service) Me(ctx context.Context, a core.Actor) (core.Object, error) {
	q, e := core.One(ctx, s.d.DB, `SELECT to_jsonb(q) FROM quota_accounts q WHERE owner_id=$1`, a.Principal.ID)
	if e != nil {
		return nil, e
	}
	var admin bool
	_ = s.d.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM reviewer_grants WHERE principal_id=$1 AND(can_view_jobs OR can_triage_feedback))`, a.Principal.ID).Scan(&admin)
	return core.Object{"id": a.Principal.ID, "display_name": a.Subject.Subject, "scopes": a.Subject.Scopes, "quota": q, "permissions": core.Object{"admin": admin}, "actor_type": a.Principal.Type}, nil
}
func (s *Service) CreateSession(ctx context.Context, a core.Actor) (core.Object, error) {
	if a.Principal.Type != domain.ActorUser || a.Subject.ClientID != s.d.Config.WebClientID {
		return nil, domain.NewError(domain.AccessDenied, "interactive web client required")
	}
	t, th, e := core.Token()
	if e != nil {
		return nil, e
	}
	csrf, ch, e := core.Token()
	if e != nil {
		return nil, e
	}
	_, e = s.d.DB.Exec(ctx, `INSERT INTO web_sessions(token_hash,csrf_hash,principal_id,expires_at) VALUES($1,$2,$3,now()+interval '30 minutes')`, th, ch, a.Principal.ID)
	return core.Object{"session_token": t, "csrf_token": csrf}, e
}
func (s *Service) VerifySession(ctx context.Context, a core.Actor, t, csrf string) bool {
	if a.Principal.Type != domain.ActorUser || t == "" || csrf == "" {
		return false
	}
	var ok bool
	_ = s.d.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM web_sessions WHERE token_hash=$1 AND csrf_hash=$2 AND principal_id=$3 AND expires_at>now())`, core.Digest(t), core.Digest(csrf), a.Principal.ID).Scan(&ok)
	return ok
}
