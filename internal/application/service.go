package application

import (
	"context"
	"fmt"
	"io"

	"github.com/agentclouddrive/agent-clouddrive/internal/application/access"
	"github.com/agentclouddrive/agent-clouddrive/internal/application/batch"
	"github.com/agentclouddrive/agent-clouddrive/internal/application/core"
	"github.com/agentclouddrive/agent-clouddrive/internal/application/delegation"
	"github.com/agentclouddrive/agent-clouddrive/internal/config"
	"github.com/agentclouddrive/agent-clouddrive/internal/domain"
	"github.com/agentclouddrive/agent-clouddrive/internal/ports"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Actor = core.Actor
type Object = core.Object
type DelegationConfig = core.DelegationConfig

type Service struct {
	DB              *pgxpool.Pool
	Config          config.Config
	Original        ports.OriginalStore
	Artifacts       ports.ArtifactStore
	Identity        ports.IdentityAdapter
	Search          ports.SearchIndex
	Converter       ports.Converter
	Preview         ports.Converter
	Delegation      *DelegationConfig
	MasterKeyStatus *core.MasterKeyStatusCache
	feedbackRate    anonymousRate
}

// Forwarding shims keep the not-yet-migrated facade files and tests compiling
// while the canonical implementations live in core. They are removed as each
// module moves out of the facade.
func id() string                                       { return core.ID() }
func token() (string, []byte, error)                   { return core.Token() }
func digest(s string) []byte                           { return core.Digest(s) }
func validIdempotencyKey(key string) bool              { return core.ValidIdempotencyKey(key) }
func hash(v any) string                                { return core.Hash(v) }
func js(v any) []byte                                  { return core.JSONBytes(v) }
func str(m Object, k string) string                    { return core.Str(m, k) }
func num(m Object, k string) int64                     { return core.Num(m, k) }
func null(s string) any                                { return core.Null(s) }
func denied() error                                    { return core.Denied() }
func dependency() error                                { return core.Dependency() }
func normalizeError(err error) error                   { return core.NormalizeError(err) }
func fileDomain(f Object) domain.File                  { return core.FileDomain(f) }
func readBounded(r io.Reader, n int64) ([]byte, error) { return core.ReadBounded(r, n) }

func Error(err error) error { return core.NormalizeError(err) }

func one(ctx context.Context, q core.Querier, sql string, args ...any) (Object, error) {
	return core.One(ctx, q, sql, args...)
}
func many(ctx context.Context, q core.Querier, sql string, args ...any) ([]Object, error) {
	return core.Many(ctx, q, sql, args...)
}

func (s *Service) Authenticate(ctx context.Context, bearer string, agent bool) (Actor, error) {
	return access.New(s.deps()).Authenticate(ctx, bearer, agent)
}
func (s *Service) AuthenticateReadOnly(ctx context.Context, bearer string, agent bool) (Actor, error) {
	return access.New(s.deps()).AuthenticateReadOnly(ctx, bearer, agent)
}
func (s *Service) Me(ctx context.Context, a Actor) (Object, error) {
	return access.New(s.deps()).Me(ctx, a)
}
func (s *Service) CreateSession(ctx context.Context, a Actor) (Object, error) {
	return access.New(s.deps()).CreateSession(ctx, a)
}
func (s *Service) VerifySession(ctx context.Context, a Actor, t, csrf string) bool {
	return access.New(s.deps()).VerifySession(ctx, a, t, csrf)
}
func (s *Service) ReadContent(ctx context.Context, a Actor, fid, vid string, rng *ports.ByteRange) (ports.Content, error) {
	action := domain.ActionDownload
	if vid != "" {
		action = domain.ActionReadHistory
	}
	f, e := access.New(s.deps()).Owned(ctx, a, fid, action)
	owner := a.Subject
	subjectToken := a.Token
	if e != nil {
		f, owner, e = s.SharedFile(ctx, a, fid, vid, domain.ActionDownload)
		subjectToken = ""
	}
	if e != nil {
		return ports.Content{}, e
	}
	if vid == "" {
		vid = str(f, "preview_version_id")
		if vid == "" {
			vid = str(f, "current_version_id")
		}
	}
	return access.New(s.deps()).OriginalVersion(ctx, owner, subjectToken, fid, vid, rng)
}

func (s *Service) deps() core.Deps {
	return core.Deps{
		DB: s.DB, Config: s.Config, Original: s.Original,
		Artifacts: s.Artifacts, Identity: s.Identity, Search: s.Search,
		Converter: s.Converter, Preview: s.Preview, Delegation: s.Delegation,
		MasterKeyStatus: s.MasterKeyStatus,
	}
}

func (s *Service) DelegationStatus(ctx context.Context, a Actor) (any, error) {
	return delegation.New(s.deps()).Status(ctx, a)
}
func (s *Service) BeginDelegation(ctx context.Context, a Actor) (Object, error) {
	return delegation.New(s.deps()).Begin(ctx, a)
}
func (s *Service) CompleteDelegation(ctx context.Context, state, code string) error {
	return delegation.New(s.deps()).Complete(ctx, state, code)
}
func (s *Service) Batch(ctx context.Context, a Actor, in Object) (any, error) {
	return batch.New(s.deps()).Batch(ctx, a, in, s.Execute)
}

var _ = fmt.Sprintf
