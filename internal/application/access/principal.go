package access

import (
	"context"
	"log/slog"
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

// CredentialForWrite selects a user token for interactive OIDC callers, or a
// separate write-capable service credential for an allowed external trust
// domain. It never falls back to a grant or the read-only service credential.
func (s *Service) CredentialForWrite(ctx context.Context, a core.Actor, source ports.RevisionRef) (ports.Credential, error) {
	if a.Token != "" {
		// Interactive OIDC callers keep their user token. The action-specific
		// scope was already enforced by the write use case.
		return s.Credential(ctx, a, source)
	}
	if e := s.Scope(a, "drive.write"); e != nil {
		return ports.Credential{}, e
	}
	if !s.d.Config.ExternalWriteEnabled {
		return ports.Credential{}, domain.NewError(domain.AccessDenied, "external writes disabled")
	}
	if !externalWriteIssuerAllowed(s.d.Config.ExternalWriteIssuers, a.Subject.Issuer) {
		return ports.Credential{}, domain.NewError(domain.AccessDenied, "external writes are not allowed for this issuer")
	}
	if s.d.Identity == nil {
		return ports.Credential{}, core.Dependency()
	}
	cred, e := s.d.Identity.DelegatedCredential(ctx, ports.DelegationRequest{
		Subject:  a.Subject,
		Audience: s.d.Config.OCISAudience,
		Source:   source,
		Scopes:   []string{"drive.read", "drive.write"},
	})
	if e != nil {
		return ports.Credential{}, e
	}
	slog.Info("write service credential issued", "credential_kind", "write_service", "subject", auditText(a.Subject.Subject), "issuer", auditText(a.Subject.Issuer), "source", auditText(source.ResourceID), "operation", auditText(writeOperation(ctx)))
	return cred, nil
}

func externalWriteIssuerAllowed(issuers []string, issuer string) bool {
	if issuer == "" {
		return false
	}
	for _, allowed := range issuers {
		if allowed == issuer {
			return true
		}
	}
	return false
}

type writeOperationKey struct{}

// WithWriteOperation annotates a write credential request for audit only. It
// never grants authority.
func WithWriteOperation(ctx context.Context, operation string) context.Context {
	if operation == "" {
		operation = "write"
	}
	return context.WithValue(ctx, writeOperationKey{}, operation)
}

func writeOperation(ctx context.Context) string {
	if operation, ok := ctx.Value(writeOperationKey{}).(string); ok && operation != "" {
		return operation
	}
	return "write"
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
