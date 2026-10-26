package application

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/agentclouddrive/agent-clouddrive/internal/domain"
)

// PostgreSQL bounds file_operations.idempotency_key with length() BETWEEN 1 AND
// 200, which counts code points. These cases pin the application boundary to
// the same semantics without requiring a database.
func TestValidIdempotencyKey(t *testing.T) {
	cases := []struct {
		name string
		key  string
		want bool
	}{
		{"empty", "", false},
		{"200 ASCII characters", strings.Repeat("k", 200), true},
		{"201 ASCII characters", strings.Repeat("k", 201), false},
		{"200 multibyte characters", strings.Repeat("键", 200), true},
		{"201 multibyte characters", strings.Repeat("键", 201), false},
		{"browser upload key", "upload:" + strings.Repeat("a", 36), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := validIdempotencyKey(c.key); got != c.want {
				t.Fatalf("validIdempotencyKey(%d bytes, %d runes) = %v, want %v", len(c.key), utf8.RuneCountInString(c.key), got, c.want)
			}
		})
	}
	if key := strings.Repeat("键", 200); len(key) <= 200 {
		t.Fatal("multibyte fixture must exceed the byte limit to prove rune semantics")
	}
}

func TestBeginUploadRejectsInvalidKeyBeforeUsingDatabase(t *testing.T) {
	s := &Service{}
	s.Config.UploadMax = 1 << 20
	a := Actor{Principal: domain.Principal{Active: true, StatusVerified: true, Scopes: domain.ScopeSet{"drive.write": true}}}
	for name, key := range map[string]string{"empty": "", "overlong ASCII": strings.Repeat("k", 201), "overlong multibyte": strings.Repeat("键", 201)} {
		t.Run(name, func(t *testing.T) {
			_, err := s.BeginUpload(context.Background(), a, Object{"name": "test_智能办公助手.docx", "size_bytes": int64(1), "idempotency_key": key})
			if domain.ErrorCode(err) != domain.InvalidArgument {
				t.Fatalf("BeginUpload error = %v, want %s", err, domain.InvalidArgument)
			}
		})
	}
}
