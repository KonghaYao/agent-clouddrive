package access

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"strings"

	"github.com/agentclouddrive/agent-clouddrive/internal/application/core"
	"github.com/agentclouddrive/agent-clouddrive/internal/domain"
	"github.com/agentclouddrive/agent-clouddrive/internal/ports"
)

func (s *Service) spaceByOwner(ctx context.Context, ownerID string) (core.Object, error) {
	return core.One(ctx, s.d.DB, `SELECT to_jsonb(s) FROM spaces s WHERE owner_id=$1`, ownerID)
}

// insertSpace is idempotent per owner and never overwrites an existing mapping.
// A native-space unique conflict is reconciled only when this owner already has
// a durable row; otherwise it is reported as a conflict.
func (s *Service) insertSpace(ctx context.Context, ownerID, instance, spaceID, rootID string) (core.Object, error) {
	if instance == "" || spaceID == "" || rootID == "" {
		return nil, core.Dependency()
	}
	sp, e := core.One(ctx, s.d.DB, `INSERT INTO spaces(owner_id,native_instance,native_space_id,native_root_resource_id) VALUES($1,$2,$3,$4) ON CONFLICT (owner_id) DO NOTHING RETURNING to_jsonb(spaces)`, ownerID, instance, spaceID, rootID)
	if e == nil {
		return sp, nil
	}
	code := domain.ErrorCode(e)
	if code != domain.NotFound && code != domain.PathConflict {
		return nil, e
	}
	existing, qe := s.spaceByOwner(ctx, ownerID)
	if qe == nil {
		return existing, nil
	}
	if code == domain.PathConflict {
		return nil, domain.NewError(domain.PathConflict, "native space is already mapped to another owner")
	}
	return nil, qe
}

// externalSpaceIssuer decides whether an identity must use the provisioner
// instead of the interactive /me/drives path. The primary OIDC issuer always
// keeps the existing path.
func (s *Service) externalSpaceIssuer(issuer string) bool {
	if issuer == "" {
		return false
	}
	if s.d.Config.OIDCIssuer != "" {
		// The only interactive issuer in this deployment is the configured OIDC
		// issuer. Every other trust domain must use the provisioner; never let
		// an unknown external issuer fall back to the service account's /me/drives.
		return strings.TrimRight(issuer, "/") != strings.TrimRight(s.d.Config.OIDCIssuer, "/")
	}
	if s.d.Config.MasterKeyIssuer != "" && issuer == s.d.Config.MasterKeyIssuer {
		return true
	}
	for _, allowed := range s.d.Config.ExternalSpaceIssuers {
		if allowed == issuer {
			return true
		}
	}
	return false
}

func (s *Service) provisionExternalSpace(ctx context.Context, a core.Actor) (core.Object, error) {
	if a.Subject.Issuer == "" || a.Subject.Subject == "" {
		return nil, domain.NewError(domain.Unauthenticated, "external identity is incomplete")
	}
	name := externalSpaceName(a.Subject.Issuer, a.Subject.Subject)
	if s.d.SpaceProvisioner == nil || !s.d.Config.SpaceProvisionerEnabled {
		s.auditSpaceProvisionFailure(a.Subject.Issuer, a.Subject.Subject, name, domain.SpaceProvisionerUnavailable)
		return nil, domain.NewError(domain.SpaceProvisionerUnavailable, "space provisioner is not enabled for external identities")
	}
	if verifier, ok := s.d.SpaceProvisioner.(interface{ Verified() bool }); ok && !verifier.Verified() {
		s.auditSpaceProvisionFailure(a.Subject.Issuer, a.Subject.Subject, name, domain.SpaceProvisionerUnavailable)
		return nil, domain.NewError(domain.SpaceProvisionerUnavailable, "space provisioner is not verified")
	}
	native, e := s.d.SpaceProvisioner.ProvisionSpace(ctx, ports.ProvisionSpaceRequest{OwnerIssuer: a.Subject.Issuer, OwnerSubject: a.Subject.Subject, Name: name})
	if e != nil {
		if errors.Is(e, ports.ErrConflict) {
			existing, qe := s.spaceByOwner(ctx, a.Principal.ID)
			if qe == nil {
				s.auditSpaceProvision(a.Subject.Issuer, a.Subject.Subject, core.Str(existing, "native_space_id"), name, "reused")
				return existing, nil
			}
			s.auditSpaceProvisionFailure(a.Subject.Issuer, a.Subject.Subject, name, domain.PathConflict)
			return nil, domain.NewError(domain.PathConflict, "native space already exists but is not mapped")
		}
		if errors.Is(e, ports.ErrUnverified) || errors.Is(e, ports.ErrUnauthorized) || errors.Is(e, ports.ErrUnavailable) ||
			errors.Is(e, ports.ErrOutcomeUnknown) || errors.Is(e, ports.ErrIntegrity) || errors.Is(e, ports.ErrInvalid) || errors.Is(e, ports.ErrNotFound) {
			s.auditSpaceProvisionFailure(a.Subject.Issuer, a.Subject.Subject, name, domain.SpaceProvisionerUnavailable)
			return nil, domain.NewError(domain.SpaceProvisionerUnavailable, "space provisioner is unavailable")
		}
		normalized := core.NormalizeError(e)
		s.auditSpaceProvisionFailure(a.Subject.Issuer, a.Subject.Subject, name, domain.ErrorCode(normalized))
		return nil, normalized
	}
	if native.NativeInstance == "" || native.NativeSpaceID == "" || native.NativeRootResourceID == "" {
		s.auditSpaceProvisionFailure(a.Subject.Issuer, a.Subject.Subject, name, domain.SpaceProvisionerUnavailable)
		return nil, domain.NewError(domain.SpaceProvisionerUnavailable, "space provisioner returned an incomplete space")
	}
	sp, e := s.insertSpace(ctx, a.Principal.ID, native.NativeInstance, native.NativeSpaceID, native.NativeRootResourceID)
	if e != nil {
		slog.Warn("native space provision could not be mapped", "operation", "space_provision", "issuer", auditText(a.Subject.Issuer), "subject", auditText(a.Subject.Subject), "drive_id", auditText(native.NativeSpaceID), "name", auditText(name), "status", "failed", "code", string(domain.ErrorCode(e)))
		s.auditSpaceProvisionFailure(a.Subject.Issuer, a.Subject.Subject, name, domain.ErrorCode(e))
		return nil, e
	}
	mappedID := core.Str(sp, "native_space_id")
	if mappedID != native.NativeSpaceID {
		slog.Warn("provisioned native space was superseded by an existing mapping", "operation", "space_provision", "issuer", auditText(a.Subject.Issuer), "subject", auditText(a.Subject.Subject), "provisioned_drive_id", auditText(native.NativeSpaceID), "mapped_drive_id", auditText(mappedID), "name", auditText(name), "status", "conflict_reused")
		s.auditSpaceProvision(a.Subject.Issuer, a.Subject.Subject, mappedID, name, "conflict_reused")
	} else {
		s.auditSpaceProvision(a.Subject.Issuer, a.Subject.Subject, native.NativeSpaceID, name, "created")
	}
	return sp, nil
}

func (s *Service) auditSpaceProvision(issuer, subject, driveID, name, status string) {
	slog.Info("native space provision", "operation", "space_provision", "issuer", auditText(issuer), "subject", auditText(subject), "drive_id", auditText(driveID), "name", auditText(name), "status", status)
}

func (s *Service) auditSpaceProvisionFailure(issuer, subject, name string, code domain.Code) {
	slog.Warn("native space provision failed", "operation", "space_provision", "issuer", auditText(issuer), "subject", auditText(subject), "name", auditText(name), "status", "failed", "code", string(code))
}

// externalSpaceName is deterministic for a trust-domain subject and contains no
// random component, so a retry targets the same drive name.
func externalSpaceName(issuer, subject string) string {
	sum := sha256.Sum256([]byte(issuer + "\x00" + subject))
	suffix := hex.EncodeToString(sum[:4])
	base := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			return r
		default:
			return '-'
		}
	}, subject)
	base = strings.Trim(base, "-._")
	if base == "" {
		base = "user"
	}
	if len(base) > 48 {
		base = base[:48]
	}
	return "drive-" + base + "-" + suffix
}
