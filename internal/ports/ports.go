// Package ports defines infrastructure contracts. Implementations never decide
// business authorization: callers must enforce ownership, lifecycle and fencing.
package ports

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

var (
	ErrNotFound     = errors.New("not found")
	ErrConflict     = errors.New("conflict")
	ErrUnauthorized = errors.New("unauthorized")
	// ErrDisabled is returned only with a cryptographically verified Subject.
	ErrDisabled       = errors.New("authoritative identity is disabled")
	ErrUnavailable    = errors.New("dependency unavailable")
	ErrUnverified     = errors.New("integration capability has not been verified")
	ErrInvalid        = errors.New("invalid input")
	ErrIntegrity      = errors.New("integrity check failed")
	ErrLimit          = errors.New("resource limit exceeded")
	ErrOutcomeUnknown = errors.New("remote mutation outcome unknown; reconcile before retry")
	// ErrUnsupportedIssuer means the identity source has no event feed for a
	// trusted external issuer. Callers must not treat this as a sync failure.
	ErrUnsupportedIssuer = errors.New("identity source does not provide events for this issuer")
)

type DependencyError struct {
	Dependency, Operation, Code string
	Status                      int
	Cause                       error
}

func (e *DependencyError) Error() string {
	return fmt.Sprintf("%s %s: %s (status %d)", e.Dependency, e.Operation, e.Code, e.Status)
}
func (e *DependencyError) Unwrap() error { return e.Cause }

type Credential struct {
	AccessToken string    `json:"-"`
	ExpiresAt   time.Time `json:"-"`
}
type ResourceRef struct {
	Instance   string `json:"instance"`
	SpaceID    string `json:"space_id"`
	ResourceID string `json:"resource_id"`
}
type RevisionRef struct {
	ResourceRef
	RevisionID string `json:"revision_id"`
	ETag       string `json:"etag,omitempty"`
}

// ByteRange is a single inclusive range. End=-1 means to EOF; nil means full content.
type ByteRange struct {
	Start int64
	End   int64
}
type Content struct {
	Body                            io.ReadCloser
	Size                            int64
	TotalSize                       int64
	ContentType, ETag, ContentRange string
	StatusCode                      int
}
type OriginalMetadata struct {
	ResourceRef
	RelativePath, Name, ETag string
	Size                     int64
	IsDirectory              bool
	ModifiedAt               time.Time
}
type NativeRevision struct {
	ID, ETag   string
	Size       int64
	ModifiedAt time.Time
}
type UploadRequest struct {
	OperationID, SpaceID, ParentResourceID, Name string
	Size                                         int64
	ExpectedETag                                 string
	ExistingResourceID                           string
	Credential                                   Credential
}

// NativeURL is secret and must remain server-side; expose a controlled business URL.
type UploadInfo struct {
	NativeURL   string    `json:"-"`
	OperationID string    `json:"operation_id"`
	Length      int64     `json:"length"`
	ExpiresAt   time.Time `json:"expires_at"`
}
type UploadStatus struct {
	ResourceID     string
	ETag           string
	Offset, Length int64
	Complete       bool
	ExpiresAt      time.Time
}
type MoveRequest struct {
	Source             ResourceRef
	DestinationParent  ResourceRef
	Name, ExpectedETag string
	Credential         Credential
}
type DirectoryRequest struct {
	Parent     ResourceRef
	Name       string
	Credential Credential
}
type ReconcileRequest struct {
	// TransferCompleted may be set only from persisted server-observed PATCH completion, never client input.
	TransferCompleted              bool
	TargetParent                   ResourceRef
	TargetName                     string
	Upload                         UploadInfo
	Target                         ResourceRef
	ExpectedSize                   int64
	ExpectedETag, NativeRevisionID string
	Credential                     Credential
}
type ReconcileResult struct {
	State    string
	Metadata OriginalMetadata
	Revision RevisionRef
	Reason   string
}
type OriginalStore interface {
	CreateUpload(context.Context, UploadRequest) (UploadInfo, error)
	StatUpload(context.Context, UploadInfo, Credential) (UploadStatus, error)
	Stat(context.Context, ResourceRef, Credential) (OriginalMetadata, error)
	OpenRevision(context.Context, RevisionRef, *ByteRange, Credential) (Content, error)
	Revisions(context.Context, ResourceRef, Credential) ([]NativeRevision, error)
	Move(context.Context, MoveRequest) (OriginalMetadata, error)
	CreateDirectory(context.Context, DirectoryRequest) (OriginalMetadata, error)
	Reconcile(context.Context, ReconcileRequest) (ReconcileResult, error)
}

type Object struct {
	Key         string `json:"key"`
	Size        int64  `json:"size"`
	SHA256      string `json:"sha256"`
	ContentType string `json:"content_type"`
}
type ArtifactWrite struct {
	Key                 string
	Body                io.Reader
	Size                int64
	SHA256, ContentType string
}
type ArtifactStore interface {
	PutAttempt(context.Context, ArtifactWrite) (Object, error)
	Publish(context.Context, string, string, string) (Object, error)
	Open(context.Context, string, *ByteRange) (Content, error)
	Verify(context.Context, string, string) (Object, error)
}

type Subject struct {
	Issuer    string    `json:"issuer"`
	Subject   string    `json:"subject"`
	Scopes    []string  `json:"scopes"`
	ExpiresAt time.Time `json:"expires_at"`
	ClientID  string    `json:"client_id,omitempty"`
}
type SubjectStatus struct {
	Enabled      bool
	NativeUserID string
}
type TokenRequirements struct {
	Scopes   []string
	Resource string
}

// AuditCapability 只能由显式审核入口构造，vault 必须重新验证持久授权。
type AuditCapability struct {
	AuditorPrincipalID string
	GrantID            string
	FileID             string
	VersionID          string
	Purpose            string
	ExpiresAt          time.Time
}

// IdentityEventSource 提供可信、可重放的管理员事件，按最新到最旧排序。
// Anchor 缺失或事件配置不可靠时不得声称历史连续。
type IdentityEvent struct {
	ID      string
	Subject string
	Disable bool
	Time    time.Time
}
type IdentityEventBatch struct {
	Events     []IdentityEvent
	Anchor     string
	Continuous bool
}
type IdentityEventSource interface {
	IdentityEvents(context.Context, string, string) (IdentityEventBatch, error)
}

type DelegationRequest struct {
	Subject      Subject
	SubjectToken string
	Audience     string
	Scopes       []string
	Source       RevisionRef
	JobID        string
	Audit        *AuditCapability
}

// JobCredentialRequest asks for a narrowly scoped credential for a background
// job. Callers must authorize the subject/source first; providers never decide
// business access. A job credential is never accepted as an inbound token.
type JobCredentialRequest struct {
	Subject  Subject
	Audience string
	Source   RevisionRef
	JobID    string
	Scopes   []string
}
type JobCredentialProvider interface {
	JobCredential(context.Context, JobCredentialRequest) (Credential, error)
}
type IdentityAdapter interface {
	ValidateToken(context.Context, string, TokenRequirements) (Subject, error)
	SubjectStatus(context.Context, string, string) (SubjectStatus, error)
	Groups(context.Context, string, string) ([]string, error)
	DelegatedCredential(context.Context, DelegationRequest) (Credential, error)
}

type EngineInfo struct {
	Name     string `json:"name"`
	Version  string `json:"version"`
	Model    string `json:"model,omitempty"`
	Provider string `json:"provider,omitempty"`
}
type Capabilities struct {
	SchemaVersion  int        `json:"schema_version"`
	Engine         EngineInfo `json:"engine"`
	Capabilities   []string   `json:"capabilities"`
	Formats        []string   `json:"formats"`
	MaxInputBytes  int64      `json:"max_input_bytes"`
	MaxOutputBytes int64      `json:"max_output_bytes"`
	MaxPages       int        `json:"max_pages"`
}
type ConversionInput struct {
	URL             string `json:"url,omitempty"`
	Handle          string `json:"handle,omitempty"`
	MIMEType        string `json:"media_type"`
	Name            string `json:"filename,omitempty"`
	ContentBase64   string `json:"content_base64,omitempty"`
	FileID          string `json:"file_id"`
	SourceVersionID string `json:"source_version_id"`
	SHA256          string `json:"sha256"`
	Size            int64  `json:"size"`
	PageStart       int    `json:"page_start,omitempty"`
	PageEnd         int    `json:"page_end,omitempty"`
}
type Recipe struct {
	PageReport      bool           `json:"page_report,omitempty"`
	ResumePages     []int          `json:"resume_pages,omitempty"`
	Fingerprint     string         `json:"fingerprint"`
	Engine          string         `json:"engine,omitempty"`
	EngineVersion   string         `json:"engine_version,omitempty"`
	DescribeImages  bool           `json:"describe_images,omitempty"`
	Page            int            `json:"page,omitempty"`
	Config          map[string]any `json:"config,omitempty"`
	Model           string         `json:"model,omitempty"`
	PromptVersion   string         `json:"prompt_version,omitempty"`
	MarkdownVersion string         `json:"markdown_version,omitempty"`
	ChunkVersion    string         `json:"chunk_version,omitempty"`
	TimeoutSeconds  int            `json:"timeout_seconds"`
	MaxPages        int            `json:"max_pages"`
	MaxOutputBytes  int64          `json:"max_output_bytes"`
}
type ConversionRequest struct {
	PageReport  bool            `json:"page_report,omitempty"`
	ResumePages []int           `json:"resume_pages,omitempty"`
	JobID       string          `json:"job_id"`
	AttemptID   string          `json:"attempt_id"`
	Capability  string          `json:"capability"`
	Input       ConversionInput `json:"input"`
	Recipe      Recipe          `json:"recipe"`
	OutputScope string          `json:"output_scope"`
}
type Completeness struct {
	Status                  string   `json:"status"`
	TotalPages              int      `json:"total_pages"`
	MissingTextPages        []int    `json:"missing_text_pages"`
	MissingDescriptionPages []int    `json:"missing_description_pages"`
	Warnings                []string `json:"warnings"`
}
type ConversionArtifact struct {
	Object
	Kind         string `json:"kind"`
	Page         int    `json:"page,omitempty"`
	AssetID      string `json:"asset_id,omitempty"`
	InlineBase64 string `json:"inline_base64,omitempty"`
}
type ConversionError struct {
	Code      string `json:"code"`
	Stage     string `json:"stage,omitempty"`
	Page      int    `json:"page,omitempty"`
	Retryable bool   `json:"retryable"`
	Message   string `json:"message,omitempty"`
}
type Location struct {
	Block    string   `json:"block"`
	Page     int      `json:"page,omitempty"`
	AssetIDs []string `json:"asset_ids,omitempty"`
}
type ConversionAsset struct {
	ID            string `json:"id"`
	MediaType     string `json:"media_type"`
	ContentBase64 string `json:"content_base64"`
	SHA256        string `json:"sha256"`
	Page          int    `json:"page"`
}
type ConversionPage struct {
	Page              int    `json:"page"`
	TextStatus        string `json:"text_status"`
	DescriptionStatus string `json:"description_status"`
	Block             string `json:"block"`
}
type ConversionResult struct {
	Model        json.RawMessage   `json:"model,omitempty"`
	Status       string            `json:"status"`
	Engine       EngineInfo        `json:"engine"`
	Markdown     string            `json:"markdown"`
	Assets       []ConversionAsset `json:"assets"`
	Pages        []ConversionPage  `json:"pages"`
	Completeness Completeness      `json:"completeness"`
	Locations    []Location        `json:"locations,omitempty"`
	Errors       []ConversionError `json:"errors"`
}

// PageEvent 是 v2 JSONL 页/阶段增量；ResultRef 使用内联、校验和受控结果，不接受外部对象地址。
type PageEvent struct {
	Type      string           `json:"type"`
	Page      int              `json:"page"`
	Stage     string           `json:"stage"`
	State     string           `json:"state"`
	ResultRef json.RawMessage  `json:"result_ref"`
	Error     *ConversionError `json:"error,omitempty"`
}

type PageConverter interface {
	ExecutePages(context.Context, ConversionRequest, func(PageEvent) error) (ConversionResult, error)
}

type Converter interface {
	Capabilities(context.Context) (Capabilities, error)
	Execute(context.Context, ConversionRequest) (ConversionResult, error)
}

type SearchDocument struct {
	ID               string    `json:"id"`
	OwnerID          string    `json:"owner_id"`
	SpaceID          string    `json:"space_id"`
	FileID           string    `json:"file_id"`
	VersionID        string    `json:"version_id"`
	ArtifactSetID    string    `json:"artifact_set_id"`
	SourcePath       string    `json:"source_path"`
	VersionCreatedAt time.Time `json:"version_created_at"`
	Title            string    `json:"title"`
	Headings         []string  `json:"headings"`
	Content          string    `json:"content"`
	Page             int       `json:"page,omitempty"`
	BlockID          string    `json:"block_id"`
	AssetIDs         []string  `json:"asset_ids,omitempty"`
	Completeness     string    `json:"completeness"`
	MissingPages     []int     `json:"missing_pages"`
}
type SearchTask struct {
	UID int64 `json:"task_uid"`
}
type SearchQuery struct {
	OwnerID, Query string
	Offset, Limit  int
	FileIDs        []string
}

// Candidates are untrusted until the application reauthorizes against PostgreSQL.
// No raw total/facet count is exposed by this contract.
type SearchCandidates struct {
	Hits        []SearchDocument
	NextOffset  int
	MayHaveMore bool
}
type SearchIndex interface {
	Configure(context.Context) error
	Upsert(context.Context, []SearchDocument) (SearchTask, error)
	Wait(context.Context, SearchTask) error
	Query(context.Context, SearchQuery) (SearchCandidates, error)
	UpdatePaths(context.Context, string, map[string]string) (SearchTask, error)
	DeleteArtifactSet(context.Context, string, string) (SearchTask, error)
}

type PersonalSpace struct {
	SpaceID        string
	RootResourceID string
	Name           string
}
type PersonalSpaceResolver interface {
	ResolvePersonalSpace(context.Context, Credential) (PersonalSpace, error)
}

// SpaceProvisioner creates a native project space for an externally asserted
// identity. It is a high-privilege administrative capability and must never be
// used for file reads or writes.
type ProvisionSpaceRequest struct {
	OwnerIssuer  string
	OwnerSubject string
	// Name must be stable across retries for the same owner.
	Name string
}
type ProvisionedSpace struct {
	NativeInstance       string
	NativeSpaceID        string
	NativeRootResourceID string
}
type SpaceProvisioner interface {
	ProvisionSpace(context.Context, ProvisionSpaceRequest) (ProvisionedSpace, error)
}

// UploadStreamer is optional. The application supplies the persisted upload only.
type UploadStreamer interface {
	StreamUpload(context.Context, UploadInfo, int64, io.Reader, int64, Credential) (UploadStatus, error)
}
