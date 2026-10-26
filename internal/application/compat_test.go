package application

import (
	"context"

	"github.com/agentclouddrive/agent-clouddrive/internal/application/access"
	"github.com/agentclouddrive/agent-clouddrive/internal/domain"
)

// Test-only shim: the authorization primitive lives in access after P2, but the
// facade's PostgreSQL integration tests still exercise it directly.
func (s *Service) owned(ctx context.Context, a Actor, fileID string, action domain.Action) (Object, error) {
	return access.New(s.deps()).Owned(ctx, a, fileID, action)
}
