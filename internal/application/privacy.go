package application

import (
	"context"
	"github.com/agentclouddrive/agent-clouddrive/internal/dbgen"
	"github.com/agentclouddrive/agent-clouddrive/internal/domain"
	"github.com/jackc/pgx/v5/pgtype"
	"sync"
	"time"
)

func pgID(value string) pgtype.UUID { var v pgtype.UUID; _ = v.Scan(value); return v }

// submitFeedback is deliberately not wrapped in audit, outbox or trace calls.
// sqlc's generated signature contains only document references and feedback.
func (s *Service) submitFeedback(ctx context.Context, in Object) error {
	if !s.feedbackRate.allow(time.Now()) {
		return domain.NewError(domain.RateLimited, "feedback temporarily rate limited")
	}
	_, e := dbgen.New(s.DB).SubmitAnonymousFeedback(ctx, dbgen.SubmitAnonymousFeedbackParams{ID: pgID(id()), FileID: pgID(str(in, "file_id")), VersionID: pgID(str(in, "version_id")), ArtifactSetID: pgID(str(in, "artifact_set_id")), Category: str(in, "category"), Body: str(in, "body")})
	return e
}

// Global-per-process only: no principal/IP/session key or persistent timestamp.
// This protects the single-node reference without introducing a source log.
type anonymousRate struct {
	mu      sync.Mutex
	started time.Time
	count   int
}

func (r *anonymousRate) allow(now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.started.IsZero() || now.Sub(r.started) >= time.Minute || now.Before(r.started) {
		r.started = now
		r.count = 0
	}
	if r.count >= 60 {
		return false
	}
	r.count++
	return true
}
