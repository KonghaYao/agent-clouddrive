package core

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/agentclouddrive/agent-clouddrive/internal/domain"
	"github.com/agentclouddrive/agent-clouddrive/internal/ports"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func ID() string {
	v, e := domain.NewID()
	if e != nil {
		panic(e)
	}
	return v
}
func Token() (string, []byte, error) {
	b := make([]byte, 32)
	if _, e := rand.Read(b); e != nil {
		return "", nil, e
	}
	s := base64.RawURLEncoding.EncodeToString(b)
	h := sha256.Sum256([]byte(s))
	return s, h[:], nil
}
func Digest(s string) []byte { h := sha256.Sum256([]byte(s)); return h[:] }

// ValidIdempotencyKey mirrors the file_operations CHECK length(): PostgreSQL
// length() counts code points, so keys are bounded by runes, not bytes.
func ValidIdempotencyKey(key string) bool {
	return key != "" && utf8.RuneCountInString(key) <= 200
}
func Hash(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func JSONBytes(v any) []byte        { b, _ := json.Marshal(v); return b }
func Str(m Object, k string) string { v, _ := m[k].(string); return v }
func Num(m Object, k string) int64 {
	switch v := m[k].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case int:
		return int64(v)
	case json.Number:
		n, e := v.Int64()
		if e != nil {
			return -1
		}
		return n
	}
	return 0
}
func Null(s string) any {
	if s == "" {
		return nil
	}
	return s
}
func Denied() error { return domain.NewError(domain.NotFound, "resource not found") }
func Dependency() error {
	return domain.NewError(domain.DependencyUnavailable, "required integration is unavailable or has not passed its compatibility gate")
}
func NormalizeError(err error) error {
	if err == nil {
		return nil
	}
	var de *domain.Error
	if errors.As(err, &de) {
		return err
	}
	if errors.Is(err, pgx.ErrNoRows) || errors.Is(err, ports.ErrNotFound) {
		return Denied()
	}
	if errors.Is(err, ports.ErrUnauthorized) {
		return domain.NewError(domain.AccessDenied, "access denied")
	}
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		for _, c := range []domain.Code{domain.VersionConflict, domain.PathConflict, domain.OperationPending, domain.IdempotencyConflict, domain.QuotaExceeded, domain.LeaseLost, domain.AccessDenied, domain.InvalidArgument} {
			if strings.Contains(pe.Message, string(c)) {
				return domain.NewError(c, string(c))
			}
		}
		if pe.Code == "23505" {
			return domain.NewError(domain.PathConflict, "target already exists")
		}
		if pe.Code == "22P02" || pe.Code == "23514" {
			return domain.NewError(domain.InvalidArgument, "invalid request")
		}
	}
	return Dependency()
}

func FileDomain(f Object) domain.File {
	return domain.File{ID: Str(f, "id"), OwnerID: Str(f, "owner_id"), SpaceID: Str(f, "space_id"), ParentID: Str(f, "parent_id"), Name: Str(f, "name"), Kind: domain.Kind(Str(f, "kind")), Lifecycle: domain.Lifecycle(Str(f, "lifecycle")), MetadataRevision: Num(f, "metadata_revision"), CurrentVersionID: Str(f, "current_version_id"), SearchArtifactSetID: Str(f, "search_artifact_set_id"), ActiveOperationID: Str(f, "active_operation_id")}
}

func ReadBounded(r io.Reader, n int64) ([]byte, error) {
	b, e := io.ReadAll(io.LimitReader(r, n+1))
	if int64(len(b)) > n {
		return nil, ports.ErrLimit
	}
	return b, e
}
