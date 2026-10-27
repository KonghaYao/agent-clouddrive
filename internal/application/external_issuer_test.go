package application

import (
	"context"
	"testing"

	"github.com/agentclouddrive/agent-clouddrive/internal/ports"
)

type externalIssuerIdentity struct{ mutationIdentity }

func (externalIssuerIdentity) IdentityEvents(_ context.Context, issuer, anchor string) (ports.IdentityEventBatch, error) {
	if issuer == "external-idp" {
		return ports.IdentityEventBatch{}, ports.ErrUnsupportedIssuer
	}
	return ports.IdentityEventBatch{Anchor: "fixture-empty", Continuous: anchor == "fixture-empty"}, nil
}

func TestIdentitySyncSkipsUnsupportedExternalIssuer(t *testing.T) {
	s, a, _ := shareLifecycleFixture(t)
	ctx := context.Background()
	if _, e := s.DB.Exec(ctx, `INSERT INTO principals(issuer,subject) VALUES('external-idp','ext-user')`); e != nil {
		t.Fatal(e)
	}
	var before int64
	if e := s.DB.QueryRow(ctx, `SELECT identity_epoch FROM principals WHERE issuer='external-idp' AND subject='ext-user'`).Scan(&before); e != nil {
		t.Fatal(e)
	}
	s.Identity = externalIssuerIdentity{}
	if e := s.syncIdentityEvents(ctx); e != nil {
		t.Fatalf("unsupported external issuer must not fail the whole sync: %v", e)
	}
	var after int64
	if e := s.DB.QueryRow(ctx, `SELECT identity_epoch FROM principals WHERE issuer='external-idp' AND subject='ext-user'`).Scan(&after); e != nil || after != before {
		t.Fatalf("external issuer epoch must not move: %d -> %d (%v)", before, after, e)
	}
	var cursors int
	if e := s.DB.QueryRow(ctx, `SELECT count(*) FROM identity_event_cursors WHERE source='external-idp'`).Scan(&cursors); e != nil || cursors != 0 {
		t.Fatalf("no cursor may be fabricated for an external issuer: %d %v", cursors, e)
	}
	if e := s.DB.QueryRow(ctx, `SELECT count(*) FROM identity_event_cursors WHERE source=$1`, a.Subject.Issuer).Scan(&cursors); e != nil || cursors != 1 {
		t.Fatalf("the OIDC issuer must still sync: %d %v", cursors, e)
	}
}
