package application

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/agentclouddrive/agent-clouddrive/internal/domain"
	"github.com/agentclouddrive/agent-clouddrive/internal/ports"
)

func repairPublishPreview(t *testing.T, s *Service, vid, pageBody string) {
	t.Helper()
	ctx := context.Background()
	key := s.previewPrefix(vid) + "/page-1.png"
	if _, e := s.publishBytes(ctx, "tmp/repair-preview/"+vid+"/page-1.png", key, "image/png", []byte(pageBody)); e != nil {
		t.Fatal(e)
	}
	manifest := js(PreviewManifest{VersionID: vid, Pages: []PreviewPage{{Page: 1, Key: key}}, Completeness: ports.Completeness{Status: "ready", TotalPages: 1, MissingTextPages: []int{}, MissingDescriptionPages: []int{}}})
	if _, e := s.publishBytes(ctx, "tmp/repair-preview/"+vid+"/manifest.json", s.previewPrefix(vid)+"/manifest.json", "application/json", manifest); e != nil {
		t.Fatal(e)
	}
}

func repairPreviewPageBody(t *testing.T, s *Service, a Actor, fid, vid string) string {
	t.Helper()
	c, e := s.PreviewPage(context.Background(), a, fid, "", vid, 1)
	if e != nil {
		t.Fatalf("preview page %s: %v", vid, e)
	}
	defer c.Body.Close()
	b, e := io.ReadAll(c.Body)
	if e != nil {
		t.Fatal(e)
	}
	return string(b)
}

// B15: both historically authorized versions can be previewed explicitly; the
// default stays on the version selected by the grant policy; unauthorized or
// revoked versions are rejected and a fixed link cannot switch versions.
func TestRepairPreviewSelectsAuthorizedHistoricalVersion(t *testing.T) {
	s, owner, fid, old, newer := reviewHistoricalSource(t)
	ctx := context.Background()
	receiver := owner
	receiver.Principal.ID = id()
	receiver.Principal.Type = domain.ActorUser
	receiver.Subject = ports.Subject{Issuer: "test", Subject: "repair-preview-recipient"}
	if _, e := s.DB.Exec(ctx, `INSERT INTO principals(id,issuer,subject) VALUES($1,'test','repair-preview-recipient')`, receiver.Principal.ID); e != nil {
		t.Fatal(e)
	}
	grantIDs := map[string]string{}
	for _, vid := range []string{old, newer} {
		request, e := s.RequestShare(ctx, owner, Object{"file_id": fid, "audience_type": "user", "audience_id": receiver.Principal.ID, "version_policy": "fixed", "version_id": vid, "rights": []string{"read"}})
		if e != nil {
			t.Fatal(e)
		}
		approved, e := s.ApproveShare(ctx, owner, str(request.(Object), "id"))
		if e != nil {
			t.Fatal(e)
		}
		grantIDs[vid] = str(approved.(Object), "id")
		repairPublishPreview(t, s, vid, "raster-"+vid)
	}

	defaultInfo, e := s.PreviewInfo(ctx, receiver, fid, "", "")
	if e != nil {
		t.Fatalf("default preview failed: %v", e)
	}
	defaultManifest := defaultInfo.(PreviewManifest)
	if defaultManifest.VersionID != old && defaultManifest.VersionID != newer {
		t.Fatalf("default preview chose %q", defaultManifest.VersionID)
	}
	if len(defaultManifest.Pages) != 1 || !strings.Contains(defaultManifest.Pages[0].URL, "version_id="+defaultManifest.VersionID) {
		t.Fatalf("default manifest pages are not version-bound: %+v", defaultManifest.Pages)
	}

	for _, vid := range []string{old, newer} {
		info, e := s.PreviewInfo(ctx, receiver, fid, "", vid)
		if e != nil {
			t.Fatalf("preview info for authorized version %s: %v", vid, e)
		}
		manifest := info.(PreviewManifest)
		if manifest.VersionID != vid {
			t.Fatalf("requested %s, manifest selected %s", vid, manifest.VersionID)
		}
		if body := repairPreviewPageBody(t, s, receiver, fid, vid); body != "raster-"+vid {
			t.Fatalf("version %s returned page from another version: %q", vid, body)
		}
	}

	if body := repairPreviewPageBody(t, s, owner, fid, old); body != "raster-"+old {
		t.Fatalf("owner historical preview returned %q", body)
	}

	// A version that belongs to a different file must not be selectable even by
	// its owner, and no asset from that relationship may be opened.
	otherFile, otherVersion := id(), id()
	if _, e = s.DB.Exec(ctx, `INSERT INTO files(id,owner_id,space_id,name,path,kind,native_instance,native_resource_id) SELECT $1,owner_id,space_id,'other.png','/other.png','file',native_instance,'other-native' FROM files WHERE id=$2`, otherFile, fid); e != nil {
		t.Fatal(e)
	}
	if _, e = s.DB.Exec(ctx, `INSERT INTO file_versions(id,file_id,version_seq,size,sha256,content_type,native_etag,native_revision_ref) VALUES($1,$2,1,10,$3,'image/png','other-etag','{"revision_id":"etag:other-etag"}')`, otherVersion, otherFile, strings.Repeat("c", 64)); e != nil {
		t.Fatal(e)
	}
	_, e = s.PreviewInfo(ctx, owner, fid, "", otherVersion)
	repairRequireCode(t, e, domain.NotFound)
	if _, e = s.PreviewPage(ctx, owner, fid, "", otherVersion, 1); e == nil {
		t.Fatal("preview page accepted a version from another file")
	}

	// A third retained version with no grant must not be selectable.
	third := id()
	if _, e = s.DB.Exec(ctx, `INSERT INTO file_versions(id,file_id,version_seq,size,sha256,content_type,native_etag,native_revision_ref) SELECT $2,file_id,3,10,sha256,content_type,'third-etag',native_revision_ref FROM file_versions WHERE id=$1`, old, third); e != nil {
		t.Fatal(e)
	}
	_, e = s.PreviewInfo(ctx, receiver, fid, "", third)
	repairRequireCode(t, e, domain.NotFound)

	// Revoking one fixed grant only removes that version.
	if _, e = s.RevokeShare(ctx, owner, grantIDs[old]); e != nil {
		t.Fatal(e)
	}
	_, e = s.PreviewInfo(ctx, receiver, fid, "", old)
	repairRequireCode(t, e, domain.NotFound)
	if _, e = s.PreviewInfo(ctx, receiver, fid, "", newer); e != nil {
		t.Fatalf("unrelated grant was affected by revocation: %v", e)
	}

	// A fixed anonymous link is bound to its version and cannot be widened by
	// passing another source version in the URL.
	linkRequest, e := s.RequestShare(ctx, owner, Object{"file_id": fid, "audience_type": "link", "version_policy": "fixed", "version_id": newer, "rights": []string{"preview"}})
	if e != nil {
		t.Fatal(e)
	}
	approvedLink, e := s.ApproveShare(ctx, owner, str(linkRequest.(Object), "id"))
	if e != nil {
		t.Fatal(e)
	}
	token := strings.TrimPrefix(str(approvedLink.(Object), "url"), strings.TrimRight(s.Config.PublicURL, "/")+"/s/")
	linkInfo, e := s.PreviewInfo(ctx, Actor{}, "", token, newer)
	if e != nil {
		t.Fatalf("fixed link cannot preview its bound version: %v", e)
	}
	if linkInfo.(PreviewManifest).VersionID != newer {
		t.Fatal("fixed link switched version")
	}
	if _, e = s.PreviewInfo(ctx, Actor{}, "", token, old); e == nil {
		t.Fatal("fixed link admitted an unbound historical version")
	}
}

// B15: a latest-version grant still refuses an explicitly requested old
// version, while the default resolves to the current source version.
func TestRepairPreviewLatestGrantRejectsHistoricalOverride(t *testing.T) {
	s, owner, fid, old, newer := reviewHistoricalSource(t)
	ctx := context.Background()
	receiver := owner
	receiver.Principal.ID = id()
	receiver.Principal.Type = domain.ActorUser
	receiver.Subject = ports.Subject{Issuer: "test", Subject: "repair-preview-latest"}
	if _, e := s.DB.Exec(ctx, `INSERT INTO principals(id,issuer,subject) VALUES($1,'test','repair-preview-latest')`, receiver.Principal.ID); e != nil {
		t.Fatal(e)
	}
	request, e := s.RequestShare(ctx, owner, Object{"file_id": fid, "audience_type": "user", "audience_id": receiver.Principal.ID, "version_policy": "latest", "rights": []string{"read"}})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.ApproveShare(ctx, owner, str(request.(Object), "id")); e != nil {
		t.Fatal(e)
	}
	for _, vid := range []string{old, newer} {
		repairPublishPreview(t, s, vid, "raster-"+vid)
	}
	info, e := s.PreviewInfo(ctx, receiver, fid, "", "")
	if e != nil {
		t.Fatal(e)
	}
	if info.(PreviewManifest).VersionID != newer {
		t.Fatalf("latest grant default selected %s", info.(PreviewManifest).VersionID)
	}
	_, e = s.PreviewInfo(ctx, receiver, fid, "", old)
	repairRequireCode(t, e, domain.NotFound)
}

func TestRepairPreviewManifestVersionMatchesAssetVersion(t *testing.T) {
	s, owner, fid, old, newer := reviewHistoricalSource(t)
	ctx := context.Background()
	repairPublishPreview(t, s, old, "raster-"+old)
	repairPublishPreview(t, s, newer, "raster-"+newer)
	for _, vid := range []string{old, newer} {
		info, e := s.PreviewInfo(ctx, owner, fid, "", vid)
		if e != nil {
			t.Fatal(e)
		}
		manifest := info.(PreviewManifest)
		if manifest.VersionID != vid {
			t.Fatalf("manifest version %s, requested %s", manifest.VersionID, vid)
		}
		page := repairPreviewPageBody(t, s, owner, fid, manifest.VersionID)
		if page != "raster-"+vid {
			t.Fatalf("manifest and asset versions diverged: %q", page)
		}
	}
}
