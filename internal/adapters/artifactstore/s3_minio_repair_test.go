package artifactstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/agentclouddrive/agent-clouddrive/internal/ports"
)

// TestRepairS3MinIORangeIntegration exercises the real S3 REST range semantics
// (206 + Content-Range and 416 at/after EOF) against a real MinIO/S3 endpoint.
// It is skipped unless DRIVE_TEST_S3_ENDPOINT is set, so the normal unit suite
// remains hermetic.
func TestRepairS3MinIORangeIntegration(t *testing.T) {
	endpoint := os.Getenv("DRIVE_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("set DRIVE_TEST_S3_ENDPOINT, DRIVE_TEST_S3_BUCKET, DRIVE_TEST_S3_ACCESS_KEY, DRIVE_TEST_S3_SECRET_KEY to run real S3 range checks")
	}
	region := os.Getenv("DRIVE_TEST_S3_REGION")
	if region == "" {
		region = "us-east-1"
	}
	s, err := NewS3(S3Config{
		Endpoint:  endpoint,
		Region:    region,
		Bucket:    os.Getenv("DRIVE_TEST_S3_BUCKET"),
		AccessKey:      os.Getenv("DRIVE_TEST_S3_ACCESS_KEY"),
		SecretKey:      os.Getenv("DRIVE_TEST_S3_SECRET_KEY"),
		ForcePathStyle: true,
	})
	if err != nil {
		t.Fatalf("new s3: %v", err)
	}
	ctx := context.Background()
	data := "real-minio-range 世界\n"
	sum := sha256.Sum256([]byte(data))
	key := "tmp/repair-minio/attempt/content.md"
	obj, err := s.PutAttempt(ctx, ports.ArtifactWrite{
		Key: key, Body: strings.NewReader(data), Size: int64(len(data)),
		SHA256: hex.EncodeToString(sum[:]), ContentType: "text/markdown",
	})
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if obj.Size != int64(len(data)) {
		t.Fatalf("put size %d", obj.Size)
	}

	c, err := s.Open(ctx, key, &ports.ByteRange{Start: 2, End: 5})
	if err != nil {
		t.Fatalf("range open: %v", err)
	}
	got, readErr := io.ReadAll(c.Body)
	_ = c.Body.Close()
	if readErr != nil {
		t.Fatalf("range read: %v", readErr)
	}
	if string(got) != data[2:6] {
		t.Fatalf("range bytes %q, want %q", got, data[2:6])
	}
	if c.StatusCode != 206 || c.TotalSize != int64(len(data)) {
		t.Fatalf("range metadata status=%d total=%d", c.StatusCode, c.TotalSize)
	}

	// A range starting at or beyond EOF must map to ErrInvalid (416), not a
	// transient dependency failure that would be retried forever.
	_, err = s.Open(ctx, key, &ports.ByteRange{Start: int64(len(data)) + 10, End: -1})
	if !errors.Is(err, ports.ErrInvalid) {
		t.Fatalf("range at EOF: got %v, want ErrInvalid", err)
	}
}
