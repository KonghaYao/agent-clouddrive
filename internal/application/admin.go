package application

import (
	"context"
	"github.com/agentclouddrive/agent-clouddrive/internal/domain"
)

func (s *Service) Admin(ctx context.Context, a Actor, kind string) (any, error) {
	if a.Principal.Type != domain.ActorUser {
		return nil, domain.NewError(domain.AccessDenied, "Agent has no reviewer access")
	}
	var jobs, feedback bool
	e := s.DB.QueryRow(ctx, `SELECT can_view_jobs,can_triage_feedback FROM reviewer_grants WHERE principal_id=$1`, a.Principal.ID).Scan(&jobs, &feedback)
	if e != nil || kind == "jobs" && !jobs || kind == "feedback" && !feedback {
		return nil, domain.NewError(domain.AccessDenied, "explicit review grant required")
	}
	var rows []Object
	if kind == "jobs" {
		rows, e = many(ctx, s.DB, `SELECT to_jsonb(j)-'recipe_snapshot'-'error_detail' FROM conversion_jobs j ORDER BY updated_at DESC LIMIT 100`)
	} else {
		rows, e = many(ctx, s.DB, `SELECT to_jsonb(f) FROM feedback f ORDER BY received_day DESC,id LIMIT 100`)
	}
	return Object{"items": rows}, normalizeError(e)
}
