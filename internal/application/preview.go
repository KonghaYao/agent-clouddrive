package application

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/agentclouddrive/agent-clouddrive/internal/application/access"
	"github.com/agentclouddrive/agent-clouddrive/internal/domain"
	"github.com/agentclouddrive/agent-clouddrive/internal/ports"
	"io"
	"strings"
	"time"
)

type PreviewManifest struct {
	VersionID    string             `json:"version_id"`
	Pages        []PreviewPage      `json:"pages"`
	Completeness ports.Completeness `json:"completeness"`
}
type PreviewPage struct {
	Page int    `json:"page"`
	Key  string `json:"key,omitempty"`
	URL  string `json:"url,omitempty"`
}

func (s *Service) linkFile(ctx context.Context, raw, vid string) (Object, ports.Subject, error) {
	row, e := one(ctx, s.DB, `SELECT to_jsonb(g)-'token_hash'||jsonb_build_object('file',to_jsonb(f),'issuer',p.issuer,'subject',p.subject) FROM share_grants g JOIN files f ON f.id=g.file_id JOIN principals p ON p.id=f.owner_id WHERE g.token_hash=$1 AND g.state='active' AND g.audience_type='link' AND p.status='active' AND file_is_active(f.id) AND(g.expires_at IS NULL OR g.expires_at>now())`, digest(raw))
	if e != nil {
		return nil, ports.Subject{}, denied()
	}
	f, _ := row["file"].(map[string]any)
	sub := ports.Subject{Issuer: str(row, "issuer"), Subject: str(row, "subject")}
	status, e := s.Identity.SubjectStatus(ctx, sub.Issuer, sub.Subject)
	if e != nil {
		return nil, sub, dependency()
	}
	if !status.Enabled {
		access.New(s.deps()).ObserveDisabled(ctx, sub)
	}
	var g domain.ShareGrant
	_ = json.Unmarshal(js(row), &g)
	requestedVersion := vid
	if requestedVersion == "" && g.VersionPolicy == domain.VersionFixed {
		requestedVersion = g.VersionID
	}
	e = domain.Authorize(domain.AccessRequest{Principal: domain.Principal{Type: domain.ActorAnonymous}, File: fileDomain(f), Action: domain.ActionPreview, VersionID: requestedVersion, Share: &g, OwnerActive: status.Enabled, OwnerStatusVerified: true, Now: time.Now()})
	if e != nil {
		return nil, sub, denied()
	}
	if g.VersionPolicy == domain.VersionFixed {
		f["preview_version_id"] = g.VersionID
	} else {
		f["preview_version_id"] = str(f, "current_version_id")
	}
	return f, sub, nil
}
func (s *Service) previewFile(ctx context.Context, a Actor, fid, vid string) (Object, ports.Subject, string, error) {
	if vid == "" {
		f, e := access.New(s.deps()).Owned(ctx, a, fid, domain.ActionPreview)
		if e == nil {
			return f, a.Subject, a.Token, nil
		}
		f, sub, e := s.SharedFile(ctx, a, fid, "", domain.ActionPreview)
		return f, sub, "", e
	}
	// An explicit historical preview is an explicit history read, never a
	// downgrade of the current-version grant. The version must still belong to
	// this file before its raster assets can be selected.
	f, e := access.New(s.deps()).Owned(ctx, a, fid, domain.ActionReadHistory)
	if e == nil {
		var belongs bool
		if e = s.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM file_versions WHERE id=$1 AND file_id=$2)`, vid, fid).Scan(&belongs); e != nil {
			return nil, ports.Subject{}, "", normalizeError(e)
		}
		if !belongs {
			return nil, ports.Subject{}, "", denied()
		}
		f["preview_version_id"] = vid
		return f, a.Subject, a.Token, nil
	}
	f, sub, e := s.SharedFile(ctx, a, fid, vid, domain.ActionPreview)
	return f, sub, "", e
}
func (s *Service) preparePreview(ctx context.Context, f Object, owner ports.Subject, subjectToken, vid string) (PreviewManifest, error) {
	var pm PreviewManifest
	if vid == "" {
		vid = str(f, "current_version_id")
	}
	key := s.previewPrefix(vid) + "/manifest.json"
	if existing, e := s.Artifacts.Open(ctx, key, nil); e == nil {
		defer existing.Body.Close()
		b, e := readBounded(existing.Body, 1<<20)
		if e == nil {
			e = json.Unmarshal(b, &pm)
		}
		return pm, e
	}
	if s.Preview == nil {
		return pm, dependency()
	}
	version, e := one(ctx, s.DB, `SELECT to_jsonb(v)-'native_revision_ref'-'native_etag' FROM file_versions v WHERE id=$1 AND file_id=$2`, vid, str(f, "id"))
	if e != nil {
		return pm, e
	}
	if num(version, "size") > s.Config.ConvertMax {
		return pm, domain.NewError(domain.UploadTooLarge, "preview exceeds configured rendering limit")
	}
	c, e := access.New(s.deps()).OriginalVersion(ctx, owner, subjectToken, str(f, "id"), vid, nil)
	if e != nil {
		return pm, e
	}
	b, e := readBounded(c.Body, s.Config.ConvertMax)
	c.Body.Close()
	if e != nil {
		return pm, e
	}
	job, attempt := id(), id()
	out, e := s.Preview.Execute(ctx, ports.ConversionRequest{JobID: job, AttemptID: attempt, Capability: "preview.render", Input: ports.ConversionInput{ContentBase64: base64.StdEncoding.EncodeToString(b), MIMEType: str(version, "content_type"), Name: str(f, "name"), FileID: str(f, "id"), SourceVersionID: vid, Size: int64(len(b))}, Recipe: ports.Recipe{TimeoutSeconds: 600, MaxPages: s.Config.ConvertPages, MaxOutputBytes: 64 << 20}, OutputScope: "tmp/" + job + "/" + attempt + "/"})
	if e != nil {
		return pm, e
	}
	if len(out.Assets) == 0 {
		return pm, dependency()
	}
	pm = PreviewManifest{VersionID: vid, Pages: []PreviewPage{}, Completeness: out.Completeness}
	for _, asset := range out.Assets {
		if asset.MediaType != "image/png" {
			return pm, domain.NewError(domain.AccessDenied, "unsafe preview output rejected")
		}
		data, e := base64.StdEncoding.DecodeString(asset.ContentBase64)
		if e != nil {
			return pm, e
		}
		dst := fmt.Sprintf("%s/page-%d.png", s.previewPrefix(vid), asset.Page)
		tmp := fmt.Sprintf("tmp/%s/%s/page-%d.png", job, attempt, asset.Page)
		obj, e := s.Artifacts.PutAttempt(ctx, ports.ArtifactWrite{Key: tmp, Body: bytes.NewReader(data), Size: int64(len(data)), SHA256: asset.SHA256, ContentType: "image/png"})
		if e != nil {
			return pm, e
		}
		_, e = s.Artifacts.Publish(ctx, tmp, dst, obj.SHA256)
		if e != nil {
			return pm, e
		}
		pm.Pages = append(pm.Pages, PreviewPage{Page: asset.Page, Key: dst})
	}
	manifest := js(pm)
	mh := sha256.Sum256(manifest)
	tmp := "tmp/" + job + "/" + attempt + "/manifest.json"
	obj, e := s.Artifacts.PutAttempt(ctx, ports.ArtifactWrite{Key: tmp, Body: bytes.NewReader(manifest), Size: int64(len(manifest)), SHA256: hex.EncodeToString(mh[:]), ContentType: "application/json"})
	if e != nil {
		return pm, e
	}
	_, e = s.Artifacts.Publish(ctx, tmp, key, obj.SHA256)
	return pm, e
}
func (s *Service) PreviewInfo(ctx context.Context, a Actor, fid, raw, vid string) (any, error) {
	var f Object
	var owner ports.Subject
	var tok string
	var e error
	if raw != "" {
		f, owner, e = s.linkFile(ctx, raw, vid)
	} else {
		f, owner, tok, e = s.previewFile(ctx, a, fid, vid)
	}
	if e != nil {
		return nil, e
	}
	vid = str(f, "preview_version_id")
	pm, e := s.preparePreview(ctx, f, owner, tok, vid)
	if e != nil {
		return nil, normalizeError(e)
	}
	for i := range pm.Pages {
		pm.Pages[i].Key = ""
		if raw != "" {
			pm.Pages[i].URL = fmt.Sprintf("/s/%s/pages/%d?version_id=%s", raw, pm.Pages[i].Page, pm.VersionID)
		} else {
			pm.Pages[i].URL = fmt.Sprintf("/v1/preview/%s/pages/%d?version_id=%s", str(f, "id"), pm.Pages[i].Page, pm.VersionID)
		}
	}
	return pm, nil
}
func (s *Service) PreviewPage(ctx context.Context, a Actor, fid, raw, vid string, page int) (ports.Content, error) {
	var f Object
	var e error
	if raw != "" {
		f, _, e = s.linkFile(ctx, raw, vid)
	} else {
		f, _, _, e = s.previewFile(ctx, a, fid, vid)
	}
	if e != nil {
		return ports.Content{}, e
	}
	expected := str(f, "preview_version_id")
	if expected != "" && vid != expected {
		return ports.Content{}, denied()
	}
	if vid == "" || page < 1 {
		return ports.Content{}, denied()
	}
	return s.Artifacts.Open(ctx, fmt.Sprintf("%s/page-%d.png", s.previewPrefix(vid), page), nil)
}
func (s *Service) ReadAsset(ctx context.Context, a Actor, aid, asset string) (ports.Content, error) {
	r, e := one(ctx, s.DB, `SELECT to_jsonb(a)||jsonb_build_object('file_id',v.file_id) FROM artifact_sets a JOIN file_versions v ON v.id=a.source_version_id WHERE a.id=$1 AND a.published_at IS NOT NULL`, aid)
	if e != nil {
		return ports.Content{}, e
	}
	if _, e = access.New(s.deps()).Owned(ctx, a, str(r, "file_id"), domain.ActionReadHistory); e != nil {
		return ports.Content{}, e
	}
	if asset == "" || strings.ContainsAny(asset, "/\\") {
		return ports.Content{}, domain.NewError(domain.InvalidArgument, "invalid asset ID")
	}
	c, e := s.Artifacts.Open(ctx, str(r, "manifest_key"), nil)
	if e != nil {
		return ports.Content{}, e
	}
	b, e := readBounded(c.Body, 4<<20)
	c.Body.Close()
	if e != nil {
		return ports.Content{}, e
	}
	var m struct{ Assets []struct{ ID, Key string } }
	if e = json.Unmarshal(b, &m); e != nil {
		return ports.Content{}, e
	}
	for _, item := range m.Assets {
		if item.ID == asset {
			return s.Artifacts.Open(ctx, item.Key, nil)
		}
	}
	return ports.Content{}, denied()
}
func (s *Service) ReadTicket(ctx context.Context, raw string, rng *ports.ByteRange) (ports.Content, error) {
	r, e := one(ctx, s.DB, `SELECT to_jsonb(t)-'token_hash'||jsonb_build_object('issuer',p.issuer,'subject',p.subject,'status',p.status) FROM access_tickets t JOIN principals p ON p.id=t.principal_id WHERE t.token_hash=$1 AND t.scope='original.read' AND t.expires_at>now()`, digest(raw))
	if e != nil || str(r, "status") != "active" {
		return ports.Content{}, denied()
	}
	sub := ports.Subject{Issuer: str(r, "issuer"), Subject: str(r, "subject")}
	status, e := s.Identity.SubjectStatus(ctx, sub.Issuer, sub.Subject)
	if e != nil {
		return ports.Content{}, dependency()
	}
	if !status.Enabled {
		access.New(s.deps()).ObserveDisabled(ctx, sub)
		return ports.Content{}, denied()
	}
	a := Actor{Principal: domain.Principal{ID: str(r, "principal_id"), Type: domain.ActorUser, Active: true, StatusVerified: true, Scopes: domain.AllScopes()}, Subject: sub}
	if _, e = access.New(s.deps()).Owned(ctx, a, str(r, "file_id"), domain.ActionDownload); e != nil {
		return ports.Content{}, e
	}
	return access.New(s.deps()).OriginalVersion(ctx, sub, "", str(r, "file_id"), str(r, "version_id"), rng)
}

var _ io.Reader

func (s *Service) AssetLink(ctx context.Context, a Actor, in Object) (any, error) {
	aid := str(in, "artifact_set_id")
	asset := str(in, "asset_id")
	c, e := s.ReadAsset(ctx, a, aid, asset)
	if e != nil {
		return nil, e
	}
	c.Body.Close()
	return Object{"url": strings.TrimRight(s.Config.PublicURL, "/") + "/v1/artifacts/" + aid + "/assets/" + asset, "authentication": "Bearer required", "artifact_set_id": aid, "asset_id": asset}, nil
}

func (s *Service) previewPrefix(vid string) string {
	return "previews/" + vid + "/" + hash(Object{"pipeline": s.Config.PipelineVersion, "max_pages": s.Config.ConvertPages, "format": "raster-png-v1"})[:16]
}
