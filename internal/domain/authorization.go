package domain

import (
	"strings"
	"time"
)

type ScopeSet map[string]bool

const (
	ScopeRead         = "drive.read"
	ScopeWrite        = "drive.write"
	ScopeDelete       = "drive.delete"
	ScopeRestore      = "drive.restore"
	ScopeConvert      = "drive.convert"
	ScopeShareRequest = "drive.share.request"
	ScopeFeedback     = "drive.feedback"
)

func ParseScopes(value string) ScopeSet {
	s := ScopeSet{}
	for _, v := range strings.Fields(value) {
		s[v] = true
	}
	return s
}
func AllScopes() ScopeSet {
	return ParseScopes("drive.read drive.write drive.delete drive.restore drive.convert drive.share.request drive.feedback")
}
func (s ScopeSet) Has(scope string) bool { return s[scope] }

type Principal struct {
	ID             string
	Type           ActorType
	Active         bool
	StatusVerified bool
	Scopes         ScopeSet
	Groups         []string
}

type Action string

const (
	ActionList         Action = "list"
	ActionSearch       Action = "search"
	ActionStat         Action = "stat"
	ActionRead         Action = "read"
	ActionDownload     Action = "download"
	ActionReadMarkdown Action = "read_markdown"
	ActionReadAsset    Action = "read_asset"
	ActionReadHistory  Action = "read_history"
	ActionWrite        Action = "write"
	ActionDelete       Action = "delete"
	ActionRestore      Action = "restore"
	ActionConvert      Action = "convert"
	ActionRequestShare Action = "request_share"
	ActionApproveShare Action = "approve_share"
	ActionPreview      Action = "preview"
	ActionAuditRead    Action = "audit_read"
	ActionListTrash    Action = "list_trash"
	ActionFeedback     Action = "feedback"
)

func RequiredScope(action Action) string {
	switch action {
	case ActionList, ActionSearch, ActionStat, ActionRead, ActionDownload, ActionReadMarkdown, ActionReadAsset, ActionReadHistory, ActionPreview, ActionListTrash:
		return ScopeRead
	case ActionWrite:
		return ScopeWrite
	case ActionDelete:
		return ScopeDelete
	case ActionRestore:
		return ScopeRestore
	case ActionConvert:
		return ScopeConvert
	case ActionRequestShare:
		return ScopeShareRequest
	case ActionFeedback:
		return ScopeFeedback
	default:
		return ""
	}
}

type AccessRequest struct {
	Principal           Principal
	File                File
	Action              Action
	VersionID           string
	ArtifactVersionID   string
	Ancestors           []Lifecycle
	Share               *ShareGrant
	OwnerActive         bool
	OwnerStatusVerified bool
	ExplicitAudit       bool
	Now                 time.Time
}

// Authorize is intentionally also called AFTER search hits, and on EACH content,
// preview resource, and asset request. Search-engine filters are not authority.
func Authorize(r AccessRequest) error {
	p := r.Principal
	if p.Type != ActorAnonymous {
		if p.ID == "" {
			return NewError(Unauthenticated, "authentication is required")
		}
		if !p.StatusVerified {
			return NewError(DependencyUnavailable, "account status could not be verified")
		}
		if !p.Active {
			return NewError(AccessDenied, "account is disabled")
		}
	}
	if p.Type != ActorUser && p.Type != ActorAgent && p.Type != ActorAnonymous {
		return NewError(AccessDenied, "principal is not authorized for this interface")
	}
	if p.Type == ActorAgent && (p.ID != r.File.OwnerID || r.Action == ActionApproveShare || r.Action == ActionAuditRead) {
		return NewError(AccessDenied, "agents may access only their own personal space and cannot approve shares or audit")
	}
	// Audit is a separate grant, never implied by administrative role or scopes.
	if r.Action == ActionAuditRead {
		if p.Type == ActorUser && r.ExplicitAudit {
			return nil
		}
		return NewError(AccessDenied, "explicit audit authorization is required")
	}
	scope := RequiredScope(r.Action)
	if scope == "" && r.Action != ActionApproveShare {
		return NewError(AccessDenied, "unsupported action")
	}
	if p.Type != ActorAnonymous && scope != "" && !p.Scopes.Has(scope) {
		return NewError(InsufficientScope, "required scope: "+scope)
	}
	if p.ID != "" && p.ID == r.File.OwnerID {
		if r.Action == ActionApproveShare && p.Type != ActorUser {
			return NewError(AccessDenied, "interactive user confirmation is required")
		}
		if r.Action == ActionRestore {
			if r.File.Lifecycle != LifecycleTrashed {
				return NewError(AccessDenied, "only trashed resources can be restored")
			}
			return nil
		}
		if r.Action == ActionReadHistory || r.Action == ActionListTrash {
			return nil
		}
		if r.File.Lifecycle != LifecycleActive {
			return NewError(NotFound, "resource not found")
		}
		for _, state := range r.Ancestors {
			if state != LifecycleActive {
				return NewError(NotFound, "resource not found")
			}
		}
		return nil
	}
	if p.Type == ActorAgent {
		return NewError(AccessDenied, "agents cannot use received shares")
	}
	if !r.OwnerStatusVerified {
		return NewError(DependencyUnavailable, "owner status could not be verified")
	}
	if !r.OwnerActive || r.File.Lifecycle != LifecycleActive {
		return NewError(NotFound, "resource not found")
	}
	for _, state := range r.Ancestors {
		if state != LifecycleActive {
			return NewError(NotFound, "resource not found")
		}
	}
	return authorizeShare(r)
}

func authorizeShare(r AccessRequest) error {
	g := r.Share
	deny := func() error { return NewError(NotFound, "resource not found") }
	if g == nil || g.State != ShareActive || g.FileID != r.File.ID || r.File.Kind != KindFile {
		return deny()
	}
	if r.Now.IsZero() {
		return NewError(InvalidArgument, "authorization requires the current time")
	}
	if (!g.ExpiresAt.IsZero() && !r.Now.Before(g.ExpiresAt)) || g.ApprovedBy == "" {
		return deny()
	}
	switch g.AudienceType {
	case AudienceLink:
		// Link proof must be validated by digest lookup before constructing the grant.
		if r.Action != ActionPreview || !hasRight(g.Rights, RightPreview) {
			return deny()
		}
	case AudienceUser:
		if r.Principal.Type != ActorUser || r.Principal.ID != g.AudienceID {
			return deny()
		}
	case AudienceGroup:
		if r.Principal.Type != ActorUser || !contains(r.Principal.Groups, g.AudienceID) {
			return deny()
		}
	default:
		return deny()
	}
	allowed := false
	switch r.Action {
	case ActionPreview:
		allowed = hasRight(g.Rights, RightPreview) || hasRight(g.Rights, RightRead)
	case ActionStat, ActionRead, ActionReadHistory:
		allowed = hasRight(g.Rights, RightRead)
	case ActionDownload:
		allowed = hasRight(g.Rights, RightDownload)
		// Shared search/Markdown/assets are not delivered by the v1 received-share UI.
	}
	if !allowed {
		return deny()
	}
	version := r.VersionID
	if version == "" {
		version = r.File.CurrentVersionID
	}
	switch g.VersionPolicy {
	case VersionLatest:
		if version != r.File.CurrentVersionID || (r.ArtifactVersionID != "" && r.ArtifactVersionID != r.File.CurrentVersionID) {
			return deny()
		}
	case VersionFixed:
		if g.VersionID == "" || version != g.VersionID || (r.ArtifactVersionID != "" && r.ArtifactVersionID != g.VersionID) {
			return deny()
		}
	default:
		return deny()
	}
	return nil
}
func contains(list []string, value string) bool {
	for _, s := range list {
		if s == value {
			return true
		}
	}
	return false
}
