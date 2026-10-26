package domain

import (
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"
)

func requireCode(t *testing.T, err error, want Code) {
	t.Helper()
	if err == nil || ErrorCode(err) != want {
		t.Fatalf("error=%v; want %s", err, want)
	}
}
func testFile() File {
	return File{ID: "file-1", OwnerID: "owner", SpaceID: "space", Kind: KindFile, Lifecycle: LifecycleActive, MetadataRevision: 7, CurrentVersionID: "v2"}
}
func testPrincipal() Principal {
	return Principal{ID: "owner", Type: ActorUser, Active: true, StatusVerified: true, Scopes: AllScopes()}
}

var testNow = time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)

func TestPaths(t *testing.T) {
	valid := map[string]string{"/": "/", "/项目/报告.md": "/项目/报告.md", "/cafe\u0301.txt": "/café.txt", "//a///b/": "/a/b", "/A/a": "/A/a", "/..hidden": "/..hidden"}
	for in, want := range valid {
		got, err := NormalizePath(in)
		if err != nil || got != want {
			t.Errorf("NormalizePath(%q)=%q,%v want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"relative", "/a/../b", "/a/./b", "/a\x00b", "/a\nb", "/a\\b", "/.clouddrive-trash/secret", "/reserved/secret", "/\xff"} {
		_, err := NormalizePath(in, "reserved")
		requireCode(t, err, InvalidPath)
	}
	if IsDescendantPath("/ab/c", "/a") || !IsDescendantPath("/a/b", "/a") {
		t.Fatal("path ancestry crossed component boundaries")
	}
}
func TestMoveAndRevision(t *testing.T) {
	f := testFile()
	parent := File{ID: "parent", OwnerID: f.OwnerID, SpaceID: f.SpaceID, Kind: KindDirectory, Lifecycle: LifecycleActive}
	if err := ValidateMove(f, parent, nil); err != nil {
		t.Fatal(err)
	}
	requireCode(t, ValidateMove(f, parent, []string{f.ID}), InvalidPath)
	parent.SpaceID = "other"
	requireCode(t, ValidateMove(f, parent, nil), AccessDenied)
	if err := CheckRevision(f, "v2", 7); err != nil {
		t.Fatal(err)
	}
	requireCode(t, CheckRevision(f, "v1", 7), VersionConflict)
	requireCode(t, CheckRevision(f, "v2", 6), VersionConflict)
	f.ActiveOperationID = "unknown"
	requireCode(t, CheckRevision(f, "v2", 7), OperationPending)
}
func TestImmutableIdentities(t *testing.T) {
	ids := map[string]bool{}
	for i := 0; i < 1000; i++ {
		id, err := NewID()
		if err != nil || len(id) != 36 || id[14] != '4' || ids[id] {
			t.Fatalf("invalid/duplicate ID %q: %v", id, err)
		}
		ids[id] = true
	}
	c := Citation{FileID: "f", VersionID: "v", ArtifactSetID: "a", Location: Location{Page: 12, Block: "p12-b04"}}
	if err := ValidateCitation(c); err != nil {
		t.Fatal(err)
	}
	c.VersionID = ""
	requireCode(t, ValidateCitation(c), InvalidArgument)
}
func TestQuota(t *testing.T) {
	q := QuotaAccount{LimitBytes: 100, UsedBytes: 40}
	reserve, err := ReservationBytes(40, 70)
	if err != nil || reserve != 30 {
		t.Fatal(reserve, err)
	}
	q, err = ReserveQuota(q, reserve)
	if err != nil {
		t.Fatal(err)
	}
	if q.ReservedBytes != 30 {
		t.Fatal(q)
	}
	_, err = ReserveQuota(q, 31)
	requireCode(t, err, QuotaExceeded)
	for _, s := range []OperationState{OperationPrepared, OperationExecuting, OperationOutcomeUnknown, OperationRemoteConfirmed, OperationCommitted} {
		_, err := ReleaseQuota(q, 30, s)
		requireCode(t, err, OperationPending)
	}
	committed, err := CommitQuota(q, 40, 70, 30)
	if err != nil || committed.UsedBytes != 70 || committed.ReservedBytes != 0 {
		t.Fatal(committed, err)
	}
	shrunk, err := CommitQuota(committed, 70, 10, 0)
	if err != nil || shrunk.UsedBytes != 10 {
		t.Fatal(shrunk, err)
	}
	over := QuotaAccount{LimitBytes: 10, UsedBytes: 50}
	if _, err := ReserveQuota(over, 0); err != nil {
		t.Fatal("zero-growth reservation must permit quota recovery", err)
	}
	shrunk, err = CommitQuota(over, 50, 20, 0)
	if err != nil || shrunk.UsedBytes != 20 {
		t.Fatal("must permit shrink after quota reduction", shrunk, err)
	}
	_, err = ReserveQuota(QuotaAccount{LimitBytes: math.MaxInt64, UsedBytes: math.MaxInt64}, 1)
	requireCode(t, err, QuotaExceeded)
	_, err = CommitQuota(q, 0, math.MaxInt64, 30)
	requireCode(t, err, InvalidArgument)
	_, err = ReleaseQuota(q, 31, OperationFailedConfirmed)
	requireCode(t, err, InvalidArgument)
	released, err := ReleaseQuota(q, 30, OperationFailedConfirmed)
	if err != nil || released.UsedBytes != 40 || released.ReservedBytes != 0 {
		t.Fatal(released, err)
	}
}
func TestOperationTransitions(t *testing.T) {
	states := []OperationState{OperationPrepared, OperationExecuting, OperationOutcomeUnknown, OperationRemoteConfirmed, OperationCommitted, OperationFailedConfirmed}
	valid := map[[2]OperationState]bool{{OperationPrepared, OperationExecuting}: true, {OperationPrepared, OperationFailedConfirmed}: true, {OperationExecuting, OperationRemoteConfirmed}: true, {OperationExecuting, OperationOutcomeUnknown}: true, {OperationExecuting, OperationFailedConfirmed}: true, {OperationOutcomeUnknown, OperationRemoteConfirmed}: true, {OperationOutcomeUnknown, OperationFailedConfirmed}: true, {OperationRemoteConfirmed, OperationCommitted}: true}
	for _, from := range states {
		for _, to := range states {
			err := ValidateOperationTransition(from, to)
			if (err == nil) != valid[[2]OperationState{from, to}] {
				t.Errorf("%s -> %s: %v", from, to, err)
			}
		}
	}
	if !BlocksConflictingOperations(OperationOutcomeUnknown) || BlocksConflictingOperations(OperationCommitted) {
		t.Fatal("conflict gate invalid")
	}
}
func TestIdempotency(t *testing.T) {
	a, _ := RequestHash(map[string]any{"a": 1, "b": "x"})
	b, _ := RequestHash(map[string]any{"b": "x", "a": 1})
	if a != b {
		t.Fatal("map key order changed hash")
	}
	if err := CheckIdempotency(a, b); err != nil {
		t.Fatal(err)
	}
	requireCode(t, CheckIdempotency(a, strings.Repeat("0", 64)), IdempotencyConflict)
	requireCode(t, CheckIdempotency("", ""), IdempotencyConflict)
	if !errors.Is(NewError(AccessDenied, "details"), &Error{Code: AccessDenied}) {
		t.Fatal("typed error comparison failed")
	}
}

func TestAuthorization(t *testing.T) {
	base := AccessRequest{Principal: testPrincipal(), File: testFile(), Action: ActionRead, Now: testNow}
	if err := Authorize(base); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		mutate func(*AccessRequest)
		code   Code
	}{
		{"disabled", func(r *AccessRequest) { r.Principal.Active = false }, AccessDenied},
		{"unverified", func(r *AccessRequest) { r.Principal.StatusVerified = false }, DependencyUnavailable},
		{"missing scope", func(r *AccessRequest) { r.Principal.Scopes = ScopeSet{} }, InsufficientScope},
		{"trashed", func(r *AccessRequest) { r.File.Lifecycle = LifecycleTrashed }, NotFound},
		{"ancestor deleted", func(r *AccessRequest) { r.Ancestors = []Lifecycle{LifecycleDeleting} }, NotFound},
		{"agent received share", func(r *AccessRequest) { r.Principal.Type = ActorAgent; r.File.OwnerID = "other" }, AccessDenied},
		{"agent approval", func(r *AccessRequest) { r.Principal.Type = ActorAgent; r.Action = ActionApproveShare }, AccessDenied},
		{"agent audit", func(r *AccessRequest) {
			r.Principal.Type = ActorAgent
			r.Action = ActionAuditRead
			r.ExplicitAudit = true
		}, AccessDenied},
		{"admin alone not audit", func(r *AccessRequest) { r.Action = ActionAuditRead }, AccessDenied},
		{"unknown action", func(r *AccessRequest) { r.Action = "unsupported" }, AccessDenied},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { r := base; tc.mutate(&r); requireCode(t, Authorize(r), tc.code) })
	}
	history := base
	history.File.Lifecycle = LifecycleTrashed
	history.Action = ActionReadHistory
	history.Ancestors = []Lifecycle{LifecycleTrashed}
	if err := Authorize(history); err != nil {
		t.Fatal(err)
	}
	audit := base
	audit.Action = ActionAuditRead
	audit.ExplicitAudit = true
	audit.File.OwnerID = "other"
	audit.File.Lifecycle = LifecycleTrashed
	if err := Authorize(audit); err != nil {
		t.Fatal(err)
	}
}
func TestShareAuthorization(t *testing.T) {
	g := ShareGrant{ID: "g", FileID: "file-1", AudienceType: AudienceUser, AudienceID: "reader", Rights: []ShareRight{RightRead, RightDownload}, VersionPolicy: VersionLatest, State: ShareActive, ApprovedBy: "owner"}
	p := testPrincipal()
	p.ID = "reader"
	base := AccessRequest{Principal: p, File: testFile(), Action: ActionDownload, Share: &g, OwnerActive: true, OwnerStatusVerified: true, Now: testNow}
	if err := Authorize(base); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		mutate func(*AccessRequest, *ShareGrant)
		code   Code
	}{
		{"past version", func(r *AccessRequest, g *ShareGrant) { r.VersionID = "v1" }, NotFound},
		{"stale artifact", func(r *AccessRequest, g *ShareGrant) { r.ArtifactVersionID = "v1" }, NotFound},
		{"owner disabled", func(r *AccessRequest, g *ShareGrant) { r.OwnerActive = false }, NotFound},
		{"owner unavailable", func(r *AccessRequest, g *ShareGrant) { r.OwnerStatusVerified = false }, DependencyUnavailable},
		{"grant disabled", func(r *AccessRequest, g *ShareGrant) { g.State = ShareDisabled }, NotFound},
		{"expiration exact", func(r *AccessRequest, g *ShareGrant) { g.ExpiresAt = testNow }, NotFound},
		{"wrong recipient", func(r *AccessRequest, g *ShareGrant) { g.AudienceID = "someone" }, NotFound},
		{"no group membership", func(r *AccessRequest, g *ShareGrant) { g.AudienceType = AudienceGroup; g.AudienceID = "team" }, NotFound},
		{"shared search", func(r *AccessRequest, g *ShareGrant) { r.Action = ActionSearch }, NotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := base
			copyGrant := g
			r.Share = &copyGrant
			tc.mutate(&r, &copyGrant)
			requireCode(t, Authorize(r), tc.code)
		})
	}
	fixed := g
	fixed.VersionPolicy = VersionFixed
	fixed.VersionID = "v1"
	r := base
	r.Share = &fixed
	r.VersionID = "v1"
	r.Action = ActionReadHistory
	if err := Authorize(r); err != nil {
		t.Fatal(err)
	}
	link := g
	link.AudienceType = AudienceLink
	link.AudienceID = ""
	link.Rights = []ShareRight{RightPreview}
	r = base
	r.Share = &link
	r.Principal = Principal{Type: ActorAnonymous}
	r.Action = ActionPreview
	if err := Authorize(r); err != nil {
		t.Fatal(err)
	}
	r.Action = ActionDownload
	requireCode(t, Authorize(r), NotFound)
}
func TestShareConfirmation(t *testing.T) {
	request := ShareRequest{FileID: "file-1", OwnerID: "owner", AudienceType: AudienceUser, AudienceID: "reader", Rights: []ShareRight{RightRead, RightDownload}, VersionPolicy: VersionLatest}
	agent := testPrincipal()
	agent.Type = ActorAgent
	approval, err := NewShareApproval("approval", agent, request, testFile(), testNow, 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	request.Rights[0] = RightPreview // Mutating original memory must not alter approval.
	grant, err := ConfirmShare(approval, testPrincipal(), approval.RequestHash, testFile(), testNow.Add(time.Minute))
	if err != nil || grant.AudienceID != "reader" || grant.State != ShareActive {
		t.Fatal(grant, err)
	}
	_, err = ConfirmShare(approval, agent, approval.RequestHash, testFile(), testNow)
	requireCode(t, err, AccessDenied)
	_, err = ConfirmShare(approval, testPrincipal(), approval.RequestHash, testFile(), testNow.Add(15*time.Minute))
	requireCode(t, err, ConfirmationExpired)
	modified := approval
	modified.RequestBody.AudienceID = "attacker"
	_, err = ConfirmShare(modified, testPrincipal(), approval.RequestHash, testFile(), testNow)
	requireCode(t, err, IdempotencyConflict)
	approval.State = "approved"
	_, err = ConfirmShare(approval, testPrincipal(), approval.RequestHash, testFile(), testNow)
	requireCode(t, err, ConfirmationRequired)
	token, hash, err := NewShareToken()
	if err != nil || len(token) < 43 || len(hash) != 64 || token == hash || ShareTokenDigest(token) != hash {
		t.Fatal("weak or invalid link token")
	}
}

func TestConversionCompleteness(t *testing.T) {
	results := []PageResult{{Page: 1, Stage: StageText, Attempt: 1, Succeeded: true, ResultRef: "p1"}, {Page: 2, Stage: StageText, Attempt: 1, Succeeded: true, ResultRef: "p2"}, {Page: 2, Stage: StageDescription, Attempt: 1, Succeeded: false, ErrorCode: "provider_not_configured"}}
	c, err := ComputeCompleteness(3, []int{2}, results)
	if err != nil || c.Status != ConversionPartial || !reflect.DeepEqual(c.MissingTextPages, []int{3}) || !reflect.DeepEqual(c.MissingDescriptionPages, []int{2}) {
		t.Fatal(c, err)
	}
	if !reflect.DeepEqual(c.MissingPages(), []int{2, 3}) {
		t.Fatal(c.MissingPages())
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.Status = ConversionReady
	requireCode(t, c.Validate(), InvalidArgument)
	empty, err := ComputeCompleteness(2, nil, nil)
	if err != nil || empty.Status != ConversionFailed {
		t.Fatal(empty, err)
	}
	results = append(results, PageResult{Page: 1, Stage: StageText, Attempt: 2, Succeeded: false, ErrorCode: "timeout"})
	c, err = ComputeCompleteness(3, []int{2}, results)
	if err != nil || !reflect.DeepEqual(c.MissingTextPages, []int{1, 3}) {
		t.Fatal(c, err)
	}
	_, err = ComputeCompleteness(1, nil, []PageResult{{Page: 2, Stage: StageText, Attempt: 1}})
	requireCode(t, err, InvalidArgument)
	_, err = ComputeCompleteness(1, nil, []PageResult{{Page: 1, Stage: StageText, Attempt: 1, Succeeded: true}})
	requireCode(t, err, InvalidArgument)
}
func TestPublicationFence(t *testing.T) {
	state := PublicationState{CurrentVersionSequence: 3, SearchVersionSequence: 1, SearchGeneration: 2, LatestRequestedGeneration: 1, Lease: LeaseFence{Attempt: 2, Token: 8, Until: testNow.Add(time.Minute)}}
	candidate := PublicationCandidate{VersionSequence: 2, Generation: 1, Attempt: 2, LeaseToken: 8, IndexState: "succeeded", Completeness: Completeness{Status: ConversionReady, TotalPages: 1}}
	if err := ValidatePublication(state, candidate, testNow); err != nil {
		t.Fatal("older available version should publish while newest processing", err)
	}
	cases := []struct {
		name   string
		mutate func(*PublicationState, *PublicationCandidate)
		code   Code
	}{
		{"expired", func(s *PublicationState, c *PublicationCandidate) { s.Lease.Until = testNow }, LeaseLost},
		{"old token", func(s *PublicationState, c *PublicationCandidate) { c.LeaseToken = 7 }, LeaseLost},
		{"old attempt", func(s *PublicationState, c *PublicationCandidate) { c.Attempt = 1 }, LeaseLost},
		{"queued index", func(s *PublicationState, c *PublicationCandidate) { c.IndexState = "enqueued" }, OperationPending},
		{"requested replacement", func(s *PublicationState, c *PublicationCandidate) { s.LatestRequestedGeneration = 2 }, PublicationSuperseded},
		{"newer already published", func(s *PublicationState, c *PublicationCandidate) { s.SearchVersionSequence = 3 }, PublicationSuperseded},
		{"same already published", func(s *PublicationState, c *PublicationCandidate) {
			s.SearchVersionSequence = 2
			s.SearchGeneration = 1
		}, PublicationSuperseded},
		{"future source", func(s *PublicationState, c *PublicationCandidate) { c.VersionSequence = 4 }, PublicationSuperseded},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, c := state, candidate
			tc.mutate(&s, &c)
			requireCode(t, ValidatePublication(s, c, testNow), tc.code)
		})
	}
}
func TestBatchIndependence(t *testing.T) {
	items := []BatchItem{{ID: "failed", Status: BatchFailed}, {ID: "child", Status: BatchPending, DependsOn: []string{"failed"}}, {ID: "grandchild", Status: BatchPending, DependsOn: []string{"child"}}, {ID: "independent", Status: BatchPending}, {ID: "done", Status: BatchSucceeded}, {ID: "ready", Status: BatchPending, DependsOn: []string{"done"}}, {ID: "waiting", Status: BatchPending, DependsOn: []string{"independent"}}}
	ready, blocked, err := ScheduleBatch(items)
	if err != nil || !reflect.DeepEqual(ready, []string{"independent", "ready"}) || !reflect.DeepEqual(blocked, []string{"child", "grandchild"}) {
		t.Fatal(ready, blocked, err)
	}
	_, _, err = ScheduleBatch([]BatchItem{{ID: "a", Status: BatchPending, DependsOn: []string{"b"}}, {ID: "b", Status: BatchPending, DependsOn: []string{"a"}}})
	requireCode(t, err, InvalidArgument)
	_, _, err = ScheduleBatch([]BatchItem{{ID: "a", Status: BatchPending, DependsOn: []string{"unknown"}}})
	requireCode(t, err, InvalidArgument)
}

func FuzzNormalizePath(f *testing.F) {
	for _, s := range []string{"/a", "/cafe\u0301", "/a/../b", "/\xff", "/.clouddrive-trash"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		p, err := NormalizePath(s)
		if err != nil {
			return
		}
		again, err := NormalizePath(p)
		if err != nil || again != p || !strings.HasPrefix(p, "/") {
			t.Fatalf("normalization not idempotent: %q -> %q -> %q (%v)", s, p, again, err)
		}
	})
}
