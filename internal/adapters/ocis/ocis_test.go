package ocis

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"github.com/agentclouddrive/agent-clouddrive/internal/ports"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestUnverifiedCapabilitiesFailClosed(t *testing.T) {
	s, _ := New(Config{URL: "http://ocis"})
	ctx := context.Background()
	if _, e := s.OpenRevision(ctx, ports.RevisionRef{}, nil, ports.Credential{}); !errors.Is(e, ports.ErrUnverified) {
		t.Fatal(e)
	}
	if _, e := s.Move(ctx, ports.MoveRequest{}); !errors.Is(e, ports.ErrUnverified) {
		t.Fatal(e)
	}
	if _, e := s.CreateUpload(ctx, ports.UploadRequest{OperationID: "op", SpaceID: "s", ParentResourceID: "p", Name: "n", ExistingResourceID: "f", ExpectedETag: "etag"}); !errors.Is(e, ports.ErrUnverified) {
		t.Fatal(e)
	}
}
func TestReadIgnoresStaleServerPreconditionSafely(t *testing.T) {
	ctx := context.Background()
	requests := []string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		if r.Method == "GET" && r.URL.Path == "/dav/spaces/space!file" {
			w.Header().Set("ETag", `"new"`)
			fmt.Fprint(w, "NEW BYTES MUST NEVER ESCAPE")
			return
		}
		if r.Method == "PROPFIND" {
			w.WriteHeader(207)
			fmt.Fprint(w, `<d:multistatus xmlns:d="DAV:"><d:response><d:href>/dav/meta/space%21file/v/revision-old</d:href><d:propstat><d:prop><d:getetag>"old"</d:getetag><d:getcontentlength>3</d:getcontentlength></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response></d:multistatus>`)
			return
		}
		if r.URL.Path == "/dav/meta/space!file/v/revision-old" {
			w.Header().Set("ETag", `"old"`)
			fmt.Fprint(w, "OLD")
			return
		}
		w.WriteHeader(404)
	}))
	defer srv.Close()
	s, _ := New(Config{URL: srv.URL, VerifiedRevisionRead: true})
	c, e := s.OpenRevision(ctx, ports.RevisionRef{ResourceRef: ports.ResourceRef{SpaceID: "space", ResourceID: "space!file"}, RevisionID: `etag:"old"`, ETag: `"old"`}, nil, ports.Credential{AccessToken: "token"})
	if e != nil {
		t.Fatal(e)
	}
	b, _ := io.ReadAll(c.Body)
	c.Body.Close()
	if string(b) != "OLD" || len(requests) != 3 {
		t.Fatalf("unsafe fixed read %q %+v", b, requests)
	}
}
func TestUploadRejectsForeignLocation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") != "*" {
			t.Fatal("new upload must not replace")
		}
		w.Header().Set("Location", "http://evil.example/data/token")
		w.WriteHeader(201)
	}))
	defer srv.Close()
	s, _ := New(Config{URL: srv.URL})
	_, e := s.CreateUpload(context.Background(), ports.UploadRequest{OperationID: "op", SpaceID: "s", ParentResourceID: "p", Name: "name", Credential: ports.Credential{AccessToken: "secret"}})
	if !errors.Is(e, ports.ErrOutcomeUnknown) {
		t.Fatal(e)
	}
}

// ownUserBasic is exclusively a native integration fixture, not application
// identity delegation or administrator impersonation.
type ownUserBasic struct {
	base           http.RoundTripper
	user, password string
}

func (t ownUserBasic) RoundTrip(r *http.Request) (*http.Response, error) {
	q := r.Clone(r.Context())
	q.Header = r.Header.Clone()
	q.SetBasicAuth(t.user, t.password)
	return t.base.RoundTrip(q)
}
func TestOCISIntegration(t *testing.T) {
	endpoint := os.Getenv("DRIVE_TEST_OCIS_URL")
	if endpoint == "" {
		t.Skip("set DRIVE_TEST_OCIS_URL for native oCIS integration")
	}
	pool, e := x509.SystemCertPool()
	if e != nil {
		t.Fatal(e)
	}
	if filename := os.Getenv("DRIVE_TEST_OCIS_CA_FILE"); filename != "" {
		pem, e := os.ReadFile(filename)
		if e != nil || !pool.AppendCertsFromPEM(pem) {
			t.Fatal("invalid test CA")
		}
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}, ResponseHeaderTimeout: 15 * time.Second}
	client := &http.Client{Transport: ownUserBasic{transport, os.Getenv("DRIVE_TEST_OCIS_USERNAME"), os.Getenv("DRIVE_TEST_OCIS_PASSWORD")}, Timeout: 30 * time.Second}
	s, e := New(Config{URL: endpoint, Client: client, VerifiedRevisionRead: true, VerifiedMoveHistory: true, VerifiedConditionalOverwrite: true})
	if e != nil {
		t.Fatal(e)
	}
	ctx := context.Background()
	cred := ports.Credential{AccessToken: "test-only-own-user-transport"}
	var personal ports.PersonalSpace
	for tries := 0; tries < 40; tries++ {
		personal, e = s.ResolvePersonalSpace(ctx, cred)
		if e == nil {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if e != nil {
		t.Fatal("personal space", e)
	}
	// Native Graph readiness precedes oCIS frontend registry readiness.
	time.Sleep(5 * time.Second)
	root := ports.ResourceRef{Instance: s.cfg.Instance, SpaceID: personal.SpaceID, ResourceID: personal.RootResourceID}
	name := "adapter-" + strconv.FormatInt(time.Now().UnixNano(), 10) + ".txt"
	first := "first immutable content"
	upload, e := s.CreateUpload(ctx, ports.UploadRequest{OperationID: "first", SpaceID: root.SpaceID, ParentResourceID: root.ResourceID, Name: name, Size: int64(len(first)), Credential: cred})
	if e != nil {
		t.Fatal("create tus", e)
	}
	if _, e = s.StreamUpload(ctx, upload, 0, strings.NewReader(first[:5]), 5, cred); e != nil {
		t.Fatal("first chunk", e)
	}
	state, e := s.StatUpload(ctx, upload, cred)
	if e != nil || state.Offset != 5 {
		t.Fatal("resume HEAD", state, e)
	}
	state, e = s.StreamUpload(ctx, upload, 5, strings.NewReader(first[5:]), int64(len(first)-5), cred)
	if e != nil || !state.Complete || state.ResourceID == "" {
		t.Fatal("final chunk", state, e)
	}
	ref := ports.ResourceRef{Instance: s.cfg.Instance, SpaceID: root.SpaceID, ResourceID: state.ResourceID}
	meta, e := s.Stat(ctx, ref, cred)
	if e != nil {
		t.Fatal("stat stable ID", e)
	}
	pin := ports.RevisionRef{ResourceRef: ref, RevisionID: "etag:" + meta.ETag, ETag: meta.ETag}
	read := func(expected string, br *ports.ByteRange) {
		t.Helper()
		c, e := s.OpenRevision(ctx, pin, br, cred)
		if e != nil {
			t.Fatal("fixed version", e)
		}
		b, e := io.ReadAll(c.Body)
		c.Body.Close()
		if e != nil || string(b) != expected {
			t.Fatalf("fixed bytes got %q want %q: %v", b, expected, e)
		}
	}
	read(first, nil)
	second := "second current content"
	upload2, e := s.CreateUpload(ctx, ports.UploadRequest{OperationID: "second-serialized", SpaceID: root.SpaceID, ParentResourceID: root.ResourceID, Name: name, Size: int64(len(second)), ExistingResourceID: ref.ResourceID, ExpectedETag: meta.ETag, Credential: cred})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.StreamUpload(ctx, upload2, 0, strings.NewReader(second), int64(len(second)), cred); e != nil {
		t.Fatal(e)
	}
	read(first, nil)
	read(first[:3], &ports.ByteRange{Start: 0, End: 2})
	folder, e := s.CreateDirectory(ctx, ports.DirectoryRequest{Parent: root, Name: "trash-" + name, Credential: cred})
	if e != nil {
		t.Fatal("create trash folder", e)
	}
	meta, e = s.Stat(ctx, ref, cred)
	if e != nil {
		t.Fatal(e)
	}
	moved, e := s.Move(ctx, ports.MoveRequest{Source: ref, DestinationParent: folder.ResourceRef, Name: name, ExpectedETag: meta.ETag, Credential: cred})
	if e != nil || moved.ResourceID != ref.ResourceID {
		t.Fatal("MOVE", moved, e)
	}
	read(first, nil)
	restored, e := s.Move(ctx, ports.MoveRequest{Source: ref, DestinationParent: root, Name: name, ExpectedETag: moved.ETag, Credential: cred})
	if e != nil || restored.ResourceID != ref.ResourceID {
		t.Fatal("restore", restored, e)
	}
	read(first, nil)
	t.Log("real oCIS: personal Graph mapping, tus chunk resume, stable ID, response-ETag-gated historical reads, Range, MOVE/restore with history passed; no final native tus CAS claimed")
}

func TestReconcileRequiresServerTransportProof(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(207)
		fmt.Fprint(w, `<d:multistatus xmlns:d="DAV:" xmlns:oc="http://owncloud.org/ns"><d:response><d:href>/dav/spaces/space!file</d:href><d:propstat><d:prop><oc:fileid>space!file</oc:fileid><d:getetag>"etag"</d:getetag><d:getcontentlength>3</d:getcontentlength></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response></d:multistatus>`)
	}))
	defer srv.Close()
	s, _ := New(Config{URL: srv.URL, VerifiedRevisionRead: true})
	in := ports.ReconcileRequest{Target: ports.ResourceRef{SpaceID: "space", ResourceID: "space!file"}, ExpectedSize: 3, Credential: ports.Credential{AccessToken: "token"}}
	out, e := s.Reconcile(context.Background(), in)
	if e != nil || out.State != "outcome_unknown" || calls != 0 {
		t.Fatal("path alone accepted", out, e, calls)
	}
	in.TransferCompleted = true
	out, e = s.Reconcile(context.Background(), in)
	if e != nil || out.State != "remote_confirmed" || out.Revision.RevisionID != `etag:"etag"` {
		t.Fatal(out, e)
	}
}
func TestMutationServerErrorIsUnknown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(502) }))
	defer srv.Close()
	s, _ := New(Config{URL: srv.URL})
	_, e := s.CreateUpload(context.Background(), ports.UploadRequest{OperationID: "op", SpaceID: "s", ParentResourceID: "p", Name: "name", Credential: ports.Credential{AccessToken: "token"}})
	if !errors.Is(e, ports.ErrOutcomeUnknown) {
		t.Fatal("unsafe definitive failure", e)
	}
}
