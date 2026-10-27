package application

import (
	"context"

	"github.com/agentclouddrive/agent-clouddrive/internal/application/access"
)

type MasterKeyAuth = access.MasterKeyAuth

// ErrInvalidOnBehalfOf is a malformed trusted-caller assertion (HTTP 400), not
// an identity failure.
var ErrInvalidOnBehalfOf = access.ErrInvalidOnBehalfOf

func (s *Service) AuthenticateMasterKey(ctx context.Context, in MasterKeyAuth) (Actor, error) {
	return access.New(s.deps()).AuthenticateMasterKey(ctx, in)
}

func (s *Service) IsMasterKey(raw string) bool {
	return access.New(s.deps()).IsMasterKey(raw)
}
