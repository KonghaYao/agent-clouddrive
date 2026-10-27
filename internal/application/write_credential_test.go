package application

import (
	"context"
	"testing"
	"time"

	"github.com/agentclouddrive/agent-clouddrive/internal/ports"
)

type writeRecordingIdentity struct {
	mutationIdentity
	requests []ports.DelegationRequest
}

func (i *writeRecordingIdentity) DelegatedCredential(_ context.Context, in ports.DelegationRequest) (ports.Credential, error) {
	i.requests = append(i.requests, in)
	return ports.Credential{AccessToken: "write-credential", ExpiresAt: time.Now().Add(time.Hour)}, nil
}

func hasWriteScopes(scopes []string) bool {
	return len(scopes) == 2 && scopes[0] == "drive.read" && scopes[1] == "drive.write"
}

func TestWritePathsUseWriteServiceCredential(t *testing.T) {
	s, a, _ := uploadFixture(t)
	ctx := context.Background()
	s.Config.ExternalWriteEnabled = true
	s.Config.ExternalWriteIssuers = []string{"fenix"}
	s.Config.OCISAudience = "ocis"
	record := &writeRecordingIdentity{}
	s.Identity = record
	a.Token = ""
	a.Subject.Issuer = "fenix"

	begin, e := s.BeginUpload(ctx, a, Object{"name": "write.txt", "size_bytes": int64(4), "idempotency_key": "write-begin"})
	if e != nil {
		t.Fatal(e)
	}
	if len(record.requests) != 1 || !hasWriteScopes(record.requests[0].Scopes) || record.requests[0].SubjectToken != "" {
		t.Fatalf("begin upload did not request a write credential: %+v", record.requests)
	}
	up := begin.(map[string]any)
	if _, _, e = s.UploadNative(ctx, a, str(up, "id")); e != nil {
		t.Fatal(e)
	}
	if len(record.requests) != 2 || !hasWriteScopes(record.requests[1].Scopes) {
		t.Fatalf("upload native did not request a write credential: %+v", record.requests)
	}
	if _, e = s.FinishUpload(ctx, a, str(up, "id")); e != nil {
		t.Fatal(e)
	}
	if len(record.requests) != 3 || !hasWriteScopes(record.requests[2].Scopes) {
		t.Fatalf("finish upload did not request a write credential: %+v", record.requests)
	}
	if _, e = s.Mutate(ctx, a, "mkdir", Object{"name": "write-dir", "idempotency_key": "write-mkdir"}); e != nil {
		t.Fatal(e)
	}
	if len(record.requests) != 4 || !hasWriteScopes(record.requests[3].Scopes) {
		t.Fatalf("mutation did not request a write credential: %+v", record.requests)
	}

	// Interactive OIDC callers keep using their user token even when external
	// writes are disabled.
	a.Token = "user-token"
	s.Config.ExternalWriteEnabled = false
	if _, e = s.Mutate(ctx, a, "mkdir", Object{"name": "user-dir", "idempotency_key": "user-mkdir"}); e != nil {
		t.Fatal(e)
	}
	last := record.requests[len(record.requests)-1]
	if last.SubjectToken != "user-token" || len(last.Scopes) != 0 {
		t.Fatalf("OIDC write did not use the user token: %+v", last)
	}
}
