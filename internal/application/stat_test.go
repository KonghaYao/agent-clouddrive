package application

import (
	"context"
	"testing"

	"github.com/agentclouddrive/agent-clouddrive/internal/domain"
	"github.com/agentclouddrive/agent-clouddrive/internal/ports"
)

func TestAgentStatOfAnotherOwnersFileIsNotFound(t *testing.T) {
	s, owner, _ := uploadFixture(t)
	ctx := context.Background()
	f := seedUpload(t, s, owner, "owner-only.txt", 3)
	other := Actor{
		Principal: domain.Principal{ID: id(), Type: domain.ActorAgent, Active: true, StatusVerified: true, Scopes: domain.AllScopes()},
		Subject:   ports.Subject{Issuer: "test", Subject: "agent-other"},
	}
	if _, e := s.DB.Exec(ctx, `INSERT INTO principals(id,issuer,subject) VALUES($1,'test','agent-other')`, other.Principal.ID); e != nil {
		t.Fatal(e)
	}
	if _, e := s.Execute(ctx, other, "stat", Object{"file_id": str(f, "id")}); domain.ErrorCode(e) != domain.NotFound {
		t.Fatalf("agent stat of another owner's file: %v", e)
	}
}
