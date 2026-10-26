package access

import (
	"context"
	"encoding/json"

	"github.com/agentclouddrive/agent-clouddrive/internal/application/core"
	"github.com/agentclouddrive/agent-clouddrive/internal/ports"
)

func (s *Service) OriginalVersion(ctx context.Context, owner ports.Subject, subjectToken, fid, vid string, rng *ports.ByteRange) (ports.Content, error) {
	var ref ports.RevisionRef
	var b []byte
	e := s.d.DB.QueryRow(ctx, `SELECT native_revision_ref FROM file_versions WHERE id=$1 AND file_id=$2`, vid, fid).Scan(&b)
	if e != nil {
		return ports.Content{}, core.NormalizeError(e)
	}
	if e = json.Unmarshal(b, &ref); e != nil {
		return ports.Content{}, e
	}
	cred, e := s.d.Identity.DelegatedCredential(ctx, ports.DelegationRequest{Subject: owner, SubjectToken: subjectToken, Audience: s.d.Config.OCISAudience, Source: ref})
	if e != nil {
		return ports.Content{}, e
	}
	return s.d.Original.OpenRevision(ctx, ref, rng, cred)
}
