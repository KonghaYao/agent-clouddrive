package domain

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"sort"
	"time"
)

type AudienceType string

const (
	AudienceUser  AudienceType = "user"
	AudienceGroup AudienceType = "group"
	AudienceLink  AudienceType = "link"
)

type ShareRight string

const (
	RightRead     ShareRight = "read"
	RightDownload ShareRight = "download"
	RightPreview  ShareRight = "preview"
)

type VersionPolicy string

const (
	VersionLatest VersionPolicy = "latest"
	VersionFixed  VersionPolicy = "fixed"
)

type ShareState string

const (
	ShareActive   ShareState = "active"
	ShareRevoked  ShareState = "revoked"
	ShareDisabled ShareState = "disabled"
)

type ShareRequest struct {
	FileID        string        `json:"file_id"`
	OwnerID       string        `json:"owner_id"`
	AudienceType  AudienceType  `json:"audience_type"`
	AudienceID    string        `json:"audience_id,omitempty"`
	Rights        []ShareRight  `json:"rights"`
	VersionPolicy VersionPolicy `json:"version_policy"`
	VersionID     string        `json:"version_id,omitempty"`
	ExpiresAt     time.Time     `json:"expires_at,omitempty"`
}
type ShareApproval struct {
	ID                 string       `json:"id"`
	RequesterPrincipal string       `json:"requester_principal"`
	RequestHash        string       `json:"request_hash"`
	RequestBody        ShareRequest `json:"request_body"`
	ExpiresAt          time.Time    `json:"expires_at"`
	State              string       `json:"state"`
}
type ShareGrant struct {
	ID            string        `json:"id"`
	FileID        string        `json:"file_id"`
	AudienceType  AudienceType  `json:"audience_type"`
	AudienceID    string        `json:"audience_id,omitempty"`
	Rights        []ShareRight  `json:"rights"`
	VersionPolicy VersionPolicy `json:"version_policy"`
	VersionID     string        `json:"version_id,omitempty"`
	State         ShareState    `json:"state"`
	ApprovedBy    string        `json:"approved_by"`
	ExpiresAt     time.Time     `json:"expires_at,omitempty"`
}

func hasRight(rights []ShareRight, right ShareRight) bool {
	for _, r := range rights {
		if r == right {
			return true
		}
	}
	return false
}

func ValidateShareRequest(r ShareRequest, file File, now time.Time) error {
	if file.Kind != KindFile || file.Lifecycle != LifecycleActive || r.FileID != file.ID || r.OwnerID != file.OwnerID {
		return NewError(AccessDenied, "only active owned files can be shared")
	}
	if now.IsZero() || (!r.ExpiresAt.IsZero() && !r.ExpiresAt.After(now)) {
		return NewError(InvalidArgument, "share expiration must be in the future")
	}
	switch r.AudienceType {
	case AudienceLink:
		if r.AudienceID != "" || len(r.Rights) != 1 || r.Rights[0] != RightPreview {
			return NewError(InvalidArgument, "anonymous links grant original preview only")
		}
	case AudienceUser, AudienceGroup:
		if r.AudienceID == "" || len(r.Rights) == 0 {
			return NewError(InvalidArgument, "internal share audience and rights are required")
		}
		for _, right := range r.Rights {
			if right != RightRead && right != RightDownload {
				return NewError(InvalidArgument, "internal shares permit read and download only")
			}
		}
	default:
		return NewError(InvalidArgument, "unknown share audience")
	}
	seen := map[ShareRight]bool{}
	for _, right := range r.Rights {
		if seen[right] {
			return NewError(InvalidArgument, "duplicate share right")
		}
		seen[right] = true
	}
	if r.VersionPolicy == VersionLatest {
		if r.VersionID != "" {
			return NewError(InvalidArgument, "latest policy cannot pin a historical version")
		}
	} else if r.VersionPolicy == VersionFixed {
		if r.VersionID == "" {
			return NewError(InvalidArgument, "historical shares must pin an explicit source version")
		}
	} else {
		return NewError(InvalidArgument, "invalid version policy")
	}
	return nil
}

// NewShareApproval makes a copy of rights so a caller cannot mutate the approved
// audience/permissions by retaining a slice. Persist the body and digest together.
func NewShareApproval(id string, requester Principal, request ShareRequest, file File, now time.Time, ttl time.Duration) (ShareApproval, error) {
	if requester.ID != file.OwnerID || !requester.Active || !requester.StatusVerified || !requester.Scopes.Has(ScopeShareRequest) || (requester.Type != ActorAgent && requester.Type != ActorUser) {
		return ShareApproval{}, NewError(AccessDenied, "share request is not authorized")
	}
	if err := ValidateShareRequest(request, file, now); err != nil {
		return ShareApproval{}, err
	}
	if id == "" || ttl <= 0 {
		return ShareApproval{}, NewError(InvalidArgument, "approval ID and positive lifetime are required")
	}
	request.Rights = append([]ShareRight(nil), request.Rights...)
	sort.Slice(request.Rights, func(i, j int) bool { return request.Rights[i] < request.Rights[j] })
	hash, err := RequestHash(request)
	if err != nil {
		return ShareApproval{}, err
	}
	return ShareApproval{ID: id, RequesterPrincipal: requester.ID, RequestHash: hash, RequestBody: request, ExpiresAt: now.Add(ttl), State: "pending"}, nil
}

// ConfirmShare rejects agents even with every OAuth scope. requestedHash must be
// the digest displayed by the CSRF-protected interactive confirmation screen.
// Perform state transition pending->approved and grant insertion atomically.
func ConfirmShare(approval ShareApproval, actor Principal, requestedHash string, file File, now time.Time) (ShareGrant, error) {
	if actor.Type != ActorUser || actor.ID != approval.RequestBody.OwnerID || actor.ID != file.OwnerID || !actor.Active || !actor.StatusVerified {
		return ShareGrant{}, NewError(AccessDenied, "only the owning interactive user can approve")
	}
	if approval.State != "pending" {
		return ShareGrant{}, NewError(ConfirmationRequired, "a new confirmation is required")
	}
	if now.IsZero() || !now.Before(approval.ExpiresAt) {
		return ShareGrant{}, NewError(ConfirmationExpired, "share confirmation expired")
	}
	hash, err := RequestHash(approval.RequestBody)
	if err != nil {
		return ShareGrant{}, err
	}
	if subtle.ConstantTimeCompare([]byte(hash), []byte(approval.RequestHash)) != 1 || subtle.ConstantTimeCompare([]byte(hash), []byte(requestedHash)) != 1 {
		return ShareGrant{}, NewError(IdempotencyConflict, "confirmed share request was modified")
	}
	if err := ValidateShareRequest(approval.RequestBody, file, now); err != nil {
		return ShareGrant{}, err
	}
	r := approval.RequestBody
	return ShareGrant{FileID: r.FileID, AudienceType: r.AudienceType, AudienceID: r.AudienceID, Rights: append([]ShareRight(nil), r.Rights...), VersionPolicy: r.VersionPolicy, VersionID: r.VersionID, State: ShareActive, ApprovedBy: actor.ID, ExpiresAt: r.ExpiresAt}, nil
}

// NewShareToken returns a 256-bit bearer token and its digest. Store the digest
// only; neither the plaintext token nor resource names belong in access logs.
func NewShareToken() (token, digest string, err error) {
	b := make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		return "", "", err
	}
	token = base64.RawURLEncoding.EncodeToString(b)
	return token, ShareTokenDigest(token), nil
}
func ShareTokenDigest(token string) string {
	s := sha256.Sum256([]byte(token))
	return hex.EncodeToString(s[:])
}
