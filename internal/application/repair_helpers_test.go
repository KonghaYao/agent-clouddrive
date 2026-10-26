package application

import (
	"testing"

	"github.com/agentclouddrive/agent-clouddrive/internal/domain"
)

func repairRequireCode(t *testing.T, e error, want domain.Code) {
	t.Helper()
	if e == nil {
		t.Fatalf("expected %s, got success", want)
	}
	if got := domain.ErrorCode(e); got != want {
		t.Fatalf("expected %s, got %s: %v", want, got, e)
	}
}
