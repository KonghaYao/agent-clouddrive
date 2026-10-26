package domain

import (
	"crypto/rand"
	"encoding/hex"
	"time"
)

type ActorType string

const (
	ActorUser      ActorType = "user"
	ActorAgent     ActorType = "agent"
	ActorAnonymous ActorType = "anonymous"
	ActorWorker    ActorType = "worker"
)

type Kind string

const (
	KindFile      Kind = "file"
	KindDirectory Kind = "directory"
)

type Lifecycle string

const (
	LifecycleActive    Lifecycle = "active"
	LifecycleDeleting  Lifecycle = "deleting"
	LifecycleTrashed   Lifecycle = "trashed"
	LifecycleRestoring Lifecycle = "restoring"
)

type File struct {
	ID                  string    `json:"id"`
	OwnerID             string    `json:"owner_id"`
	SpaceID             string    `json:"space_id"`
	ParentID            string    `json:"parent_id,omitempty"`
	Name                string    `json:"name"`
	Kind                Kind      `json:"kind"`
	Lifecycle           Lifecycle `json:"lifecycle"`
	MetadataRevision    int64     `json:"metadata_revision"`
	CurrentVersionID    string    `json:"current_version_id,omitempty"`
	SearchArtifactSetID string    `json:"search_artifact_set_id,omitempty"`
	ActiveOperationID   string    `json:"active_operation_id,omitempty"`
}

type Version struct {
	ID                string    `json:"id"`
	FileID            string    `json:"file_id"`
	Sequence          int64     `json:"version_seq"`
	CommittedAt       time.Time `json:"committed_at"`
	Size              int64     `json:"size"`
	SHA256            string    `json:"sha256"`
	NativeRevisionRef string    `json:"native_revision_ref"`
}

type Citation struct {
	FileID        string   `json:"file_id"`
	VersionID     string   `json:"version_id"`
	ArtifactSetID string   `json:"artifact_set_id"`
	Location      Location `json:"location"`
}
type Location struct {
	Page     int      `json:"page,omitempty"`
	Block    string   `json:"block,omitempty"`
	AssetIDs []string `json:"asset_ids,omitempty"`
}

// NewID returns an unpredictable RFC 4122 UUID. Database timestamps and sequence
// numbers supply ordering; IDs must never be derived from mutable file paths.
func NewID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	var out [36]byte
	hex.Encode(out[0:8], b[0:4])
	out[8] = '-'
	hex.Encode(out[9:13], b[4:6])
	out[13] = '-'
	hex.Encode(out[14:18], b[6:8])
	out[18] = '-'
	hex.Encode(out[19:23], b[8:10])
	out[23] = '-'
	hex.Encode(out[24:36], b[10:16])
	return string(out[:]), nil
}

// CheckRevision must be repeated under a database row lock at final commit.
// Passing preflight alone is not authorization to overwrite an intervening edit.
func CheckRevision(file File, expectedVersionID string, expectedMetadataRevision int64) error {
	if file.ActiveOperationID != "" {
		return NewError(OperationPending, "a prior operation must be reconciled first")
	}
	if expectedMetadataRevision < 1 || file.MetadataRevision != expectedMetadataRevision || file.CurrentVersionID != expectedVersionID {
		return &Error{Code: VersionConflict, Message: "read the current version before retrying", Details: map[string]any{"current_version_id": file.CurrentVersionID, "metadata_revision": file.MetadataRevision}}
	}
	return nil
}

func ValidateCitation(c Citation) error {
	if c.FileID == "" || c.VersionID == "" || c.ArtifactSetID == "" || c.Location.Page < 0 {
		return NewError(InvalidArgument, "a citation must pin file, source version, and artifact set")
	}
	return nil
}
