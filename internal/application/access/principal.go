package access

import (
	"context"
	"time"

	"github.com/agentclouddrive/agent-clouddrive/internal/application/core"
	"github.com/agentclouddrive/agent-clouddrive/internal/domain"
	"github.com/agentclouddrive/agent-clouddrive/internal/ports"
)

func (s *Service) Credential(ctx context.Context, a core.Actor, source ports.RevisionRef) (ports.Credential, error) {
	if s.d.Identity == nil {
		return ports.Credential{}, core.Dependency()
	}
	return s.d.Identity.DelegatedCredential(ctx, ports.DelegationRequest{Subject: a.Subject, SubjectToken: a.Token, Audience: s.d.Config.OCISAudience, Source: source})
}
func (s *Service) Scope(a core.Actor, scope string) error {
	if !a.Principal.Active || !a.Principal.StatusVerified {
		return domain.NewError(domain.AccessDenied, "account disabled")
	}
	if !a.Principal.Scopes[scope] {
		return domain.NewError(domain.InsufficientScope, "requires "+scope)
	}
	return nil
}

// Used only on non-feedback paths. A verified disabled identity permanently
// disables existing shares through DB triggers; later reactivation never revives them.
func (s *Service) ObserveDisabled(ctx context.Context, subject ports.Subject) {
	if subject.Issuer == "" || subject.Subject == "" {
		return
	}
	bounded, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_, _ = s.d.DB.Exec(bounded, `UPDATE principals SET status='disabled',disabled_reason=CASE WHEN status='active' OR disabled_reason='identity' THEN 'identity' ELSE disabled_reason END,updated_at=now() WHERE issuer=$1 AND subject=$2 AND(status='active' OR disabled_reason='identity')`, subject.Issuer, subject.Subject)
}
