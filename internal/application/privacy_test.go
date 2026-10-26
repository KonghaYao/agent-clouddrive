package application

import (
	"context"
	"github.com/agentclouddrive/agent-clouddrive/internal/ports"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type privacyIdentity struct {
	mutationIdentity
	disabled bool
}

func (p privacyIdentity) ValidateToken(context.Context, string, ports.TokenRequirements) (ports.Subject, error) {
	return ports.Subject{Issuer: "test", Subject: "owner", ClientID: "web", Scopes: []string{"drive.read", "drive.feedback"}, ExpiresAt: time.Now().Add(time.Hour)}, nil
}
func (p privacyIdentity) SubjectStatus(context.Context, string, string) (ports.SubjectStatus, error) {
	return ports.SubjectStatus{Enabled: !p.disabled}, nil
}
func TestFeedbackLeavesNoIdentityActivityRecord(t *testing.T) {
	s, a, _ := uploadFixture(t)
	f := seedUpload(t, s, a, "feedback.txt", 3)
	ctx := context.Background()
	s.Identity = privacyIdentity{}
	s.Config.WebClientID = "web"
	var before, after string
	var eventsBefore, eventsAfter int
	_ = s.DB.QueryRow(ctx, `SELECT to_jsonb(p)::text FROM principals p WHERE id=$1`, a.Principal.ID).Scan(&before)
	_ = s.DB.QueryRow(ctx, `SELECT count(*) FROM outbox`).Scan(&eventsBefore)
	authenticated, e := s.AuthenticateReadOnly(ctx, "test-credential", true)
	if e != nil {
		t.Fatal(e)
	}
	_, e = s.feedback(ctx, authenticated, Object{"file_id": str(f, "id"), "version_id": str(f, "current_version_id"), "category": "text", "body": "Missing paragraph"})
	if e != nil {
		t.Fatal(e)
	}
	_ = s.DB.QueryRow(ctx, `SELECT to_jsonb(p)::text FROM principals p WHERE id=$1`, a.Principal.ID).Scan(&after)
	_ = s.DB.QueryRow(ctx, `SELECT count(*) FROM outbox`).Scan(&eventsAfter)
	if before != after || eventsBefore != eventsAfter {
		t.Fatal("feedback changed identity activity/outbox")
	}
	var body string
	if e = s.DB.QueryRow(ctx, `SELECT body FROM feedback`).Scan(&body); e != nil || body != "Missing paragraph" {
		t.Fatal(e, body)
	}
	s.Identity = privacyIdentity{disabled: true}
	if _, e = s.AuthenticateReadOnly(ctx, "test-credential", true); e == nil {
		t.Fatal("disabled feedback authenticated")
	}
	_ = s.DB.QueryRow(ctx, `SELECT to_jsonb(p)::text FROM principals p WHERE id=$1`, a.Principal.ID).Scan(&after)
	if before != after {
		t.Fatal("denied anonymous request mutated identity state")
	}
}
func TestOfflineVerifierEncryptionBoundToPrincipal(t *testing.T) {
	key := make([]byte, 32)
	cipher, e := sealSecret(key, []byte("verifier"), []byte("owner"))
	if e != nil {
		t.Fatal(e)
	}
	plain, e := openSecret(key, cipher, []byte("owner"))
	if e != nil || string(plain) != "verifier" {
		t.Fatal(e)
	}
	if _, e = openSecret(key, cipher, []byte("other")); e == nil {
		t.Fatal("wrong principal decrypted")
	}
	cipher[len(cipher)-1] ^= 1
	if _, e = openSecret(key, cipher, []byte("owner")); e == nil {
		t.Fatal("tampered ciphertext accepted")
	}
}

func TestAnonymousRateIsGlobalBoundedAndEphemeral(t *testing.T) {
	var limiter anonymousRate
	now := time.Unix(1000, 0)
	var admitted atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if limiter.allow(now) {
				admitted.Add(1)
			}
		}()
	}
	wg.Wait()
	if admitted.Load() != 60 {
		t.Fatalf("admitted %d", admitted.Load())
	}
	if !limiter.allow(now.Add(time.Minute)) {
		t.Fatal("window did not expire")
	}
}
