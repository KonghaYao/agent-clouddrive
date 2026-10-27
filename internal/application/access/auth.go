package access

import (
	"context"
	"errors"
	"strings"

	"github.com/agentclouddrive/agent-clouddrive/internal/application/core"
	"github.com/agentclouddrive/agent-clouddrive/internal/domain"
	"github.com/agentclouddrive/agent-clouddrive/internal/ports"
	"github.com/jackc/pgx/v5"
)

type Service struct{ d core.Deps }

func New(d core.Deps) *Service { return &Service{d: d} }

func (s *Service) Authenticate(ctx context.Context, bearer string, agent bool) (core.Actor, error) {
	return s.authenticate(ctx, bearer, agent, false)
}
func (s *Service) AuthenticateReadOnly(ctx context.Context, bearer string, agent bool) (core.Actor, error) {
	return s.authenticate(ctx, bearer, agent, true)
}
func (s *Service) authenticate(ctx context.Context, bearer string, agent, readOnly bool) (core.Actor, error) {
	var a core.Actor
	if s.d.Identity == nil || bearer == "" {
		return a, domain.NewError(domain.Unauthenticated, "valid bearer token required")
	}
	sub, e := s.d.Identity.ValidateToken(ctx, bearer, ports.TokenRequirements{Resource: s.d.Config.MCPURL})
	if e != nil {
		if errors.Is(e, ports.ErrDisabled) {
			if !readOnly {
				s.ObserveDisabled(ctx, sub)
			}
			return a, domain.NewError(domain.AccessDenied, "account disabled")
		}
		if errors.Is(e, ports.ErrUnavailable) || errors.Is(e, ports.ErrUnverified) {
			return a, core.Dependency()
		}
		return a, domain.NewError(domain.Unauthenticated, "invalid token")
	}
	status, e := s.d.Identity.SubjectStatus(ctx, sub.Issuer, sub.Subject)
	if e != nil {
		return a, core.Dependency()
	}
	if !status.Enabled {
		s.disableIdentity(ctx, sub, readOnly)
		return a, domain.NewError(domain.AccessDenied, "account disabled")
	}
	return s.finishAuthentication(ctx, sub, bearer, agent, readOnly)
}

// disableIdentity revokes local access after an authoritative identity source
// reports the account disabled. Read-only feedback never mutates durable state.
func (s *Service) disableIdentity(ctx context.Context, sub ports.Subject, readOnly bool) {
	if readOnly {
		return
	}
	_, _ = s.d.DB.Exec(ctx, `UPDATE principals SET status='disabled',disabled_reason='identity',updated_at=now() WHERE issuer=$1 AND subject=$2`, sub.Issuer, sub.Subject)
	_, _ = s.d.DB.Exec(ctx, `UPDATE share_grants SET state='disabled',disabled_reason='account_disabled' WHERE creator_principal_id IN(SELECT id FROM principals WHERE issuer=$1 AND subject=$2) AND state='active'`, sub.Issuer, sub.Subject)
}

// finishAuthentication runs the identity-independent principal, quota and actor
// projection shared by OIDC and trusted master-key authentication.
func (s *Service) finishAuthentication(ctx context.Context, sub ports.Subject, token string, agent, readOnly bool) (core.Actor, error) {
	var a core.Actor
	var pid, local string
	e := s.d.DB.QueryRow(ctx, `SELECT id::text,status FROM principals WHERE issuer=$1 AND subject=$2`, sub.Issuer, sub.Subject).Scan(&pid, &local)
	if e != nil {
		if !errors.Is(e, pgx.ErrNoRows) || readOnly {
			return a, domain.NewError(domain.Unauthenticated, "initialize your personal space before submitting feedback")
		}
		e = s.d.DB.QueryRow(ctx, `INSERT INTO principals(issuer,subject) VALUES($1,$2) ON CONFLICT(issuer,subject) DO UPDATE SET issuer=excluded.issuer RETURNING id::text,status`, sub.Issuer, sub.Subject).Scan(&pid, &local)
		if e != nil {
			return a, core.NormalizeError(e)
		}
	}
	if !readOnly && local == "disabled" {
		var reenabled string
		e = s.d.DB.QueryRow(ctx, `UPDATE principals SET status='active',disabled_reason=NULL,updated_at=now() WHERE id=$1 AND disabled_reason='identity' RETURNING status`, pid).Scan(&reenabled)
		if e == nil {
			local = reenabled
		}
	}
	if local != "active" {
		return a, domain.NewError(domain.AccessDenied, "account disabled")
	}
	if !readOnly {
		_, e = s.d.DB.Exec(ctx, `INSERT INTO quota_accounts(owner_id,limit_bytes) VALUES($1,$2) ON CONFLICT DO NOTHING`, pid, s.d.Config.DefaultQuota)
		if e != nil {
			return a, e
		}
	}

	typ := domain.ActorUser
	if agent || sub.ClientID != s.d.Config.WebClientID {
		typ = domain.ActorAgent
	}
	a = core.Actor{Principal: domain.Principal{ID: pid, Type: typ, Active: true, StatusVerified: true, Scopes: domain.ParseScopes(strings.Join(sub.Scopes, " "))}, Subject: sub, Token: token}
	return a, nil
}
