package application

import (
	"context"
	"github.com/agentclouddrive/agent-clouddrive/internal/ports"
	"time"
)

// Used only on non-feedback paths. A verified disabled identity permanently
// disables existing shares through DB triggers; later reactivation never revives them.
func (s *Service) observeDisabled(ctx context.Context, subject ports.Subject) {
	if subject.Issuer == "" || subject.Subject == "" {
		return
	}
	bounded, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_, _ = s.DB.Exec(bounded, `UPDATE principals SET status='disabled',disabled_reason=CASE WHEN status='active' OR disabled_reason='identity' THEN 'identity' ELSE disabled_reason END,updated_at=now() WHERE issuer=$1 AND subject=$2 AND(status='active' OR disabled_reason='identity')`, subject.Issuer, subject.Subject)
}
