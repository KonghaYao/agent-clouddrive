// Package ocis wraps official WebDAV, Graph and tus endpoints. Feature gates
// distinguish implemented HTTP contracts from verified deployed capabilities.
package ocis

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"github.com/agentclouddrive/agent-clouddrive/internal/ports"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	URL, Instance                                                           string
	InstanceAliases                                                         []string
	Client                                                                  *http.Client
	MaxMetadataBytes, MaxUploadChunkBytes                                   int64
	VerifiedRevisionRead, VerifiedMoveHistory, VerifiedConditionalOverwrite bool
}
type Store struct {
	cfg    Config
	base   *url.URL
	client *http.Client
}

func New(cfg Config) (*Store, error) {
	u, e := url.Parse(cfg.URL)
	if e != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, ports.ErrInvalid
	}
	if cfg.Instance == "" {
		cfg.Instance = u.Host
	}
	aliases := make([]string, 0, len(cfg.InstanceAliases))
	for _, alias := range cfg.InstanceAliases {
		if alias == "" || alias == cfg.Instance || strings.ContainsAny(alias, "\x00\r\n") {
			continue
		}
		duplicate := false
		for _, existing := range aliases {
			if existing == alias {
				duplicate = true
				break
			}
		}
		if !duplicate {
			aliases = append(aliases, alias)
		}
	}
	cfg.InstanceAliases = aliases
	if cfg.MaxMetadataBytes <= 0 {
		cfg.MaxMetadataBytes = 4 << 20
	}
	if cfg.MaxUploadChunkBytes <= 0 {
		cfg.MaxUploadChunkBytes = 100 << 20
	}
	cl := &http.Client{}
	if cfg.Client != nil {
		*cl = *cfg.Client
	}
	// Headers are bounded in time, but a 10 GiB streamed body is governed by
	// the request context rather than an inappropriate short whole-file timeout.
	if cl.Transport == nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.ResponseHeaderTimeout = 30 * time.Second
		cl.Transport = transport
	}
	cl.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Store{cfg: cfg, base: u, client: cl}, nil
}
func safeSegment(s string) bool {
	return s != "" && s != "." && s != ".." && !strings.ContainsAny(s, "/\\\x00\r\n")
}
func (s *Store) resource(r ports.ResourceRef) error {
	if r.Instance != "" && r.Instance != s.cfg.Instance && !s.instanceAlias(r.Instance) {
		return ports.ErrInvalid
	}
	if !safeSegment(r.ResourceID) || !safeSegment(r.SpaceID) {
		return ports.ErrInvalid
	}
	return nil
}
func (s *Store) instanceAlias(instance string) bool {
	for _, alias := range s.cfg.InstanceAliases {
		if alias == instance {
			return true
		}
	}
	return false
}
func (s *Store) endpoint(p string) string {
	u := *s.base
	u.Path = strings.TrimRight(s.base.Path, "/") + p
	u.RawPath = ""
	return u.String()
}
func (s *Store) dav(r ports.ResourceRef) string { return s.endpoint("/dav/spaces/" + r.ResourceID) }
func (s *Store) do(ctx context.Context, method, endpoint string, body io.Reader, length int64, cred ports.Credential, headers http.Header) (*http.Response, error) {
	if cred.AccessToken == "" || !cred.ExpiresAt.IsZero() && !time.Now().Before(cred.ExpiresAt) {
		return nil, ports.ErrUnauthorized
	}
	u, e := url.Parse(endpoint)
	if e != nil || u.Scheme != s.base.Scheme || u.Host != s.base.Host || u.User != nil {
		return nil, ports.ErrInvalid
	}
	req, e := http.NewRequestWithContext(ctx, method, endpoint, body)
	if e != nil {
		return nil, ports.ErrInvalid
	}
	if headers != nil {
		req.Header = headers.Clone()
	}
	req.Header.Set("Authorization", "Bearer "+cred.AccessToken)
	req.ContentLength = length
	resp, e := s.client.Do(req)
	if e != nil {
		cause := ports.ErrUnavailable
		if method == "POST" || method == "PATCH" || method == "MOVE" || method == "MKCOL" {
			cause = ports.ErrOutcomeUnknown
		}
		return nil, &ports.DependencyError{Dependency: "ocis", Operation: method, Code: "transport_error", Cause: cause}
	}
	return resp, nil
}
func failure(resp *http.Response, method string) error {
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	cause := ports.ErrUnavailable
	if resp.StatusCode >= 500 && (strings.HasPrefix(method, "POST") || strings.HasPrefix(method, "PATCH") || method == "MOVE" || method == "MKCOL") {
		cause = ports.ErrOutcomeUnknown
	}
	switch resp.StatusCode {
	case 401, 403:
		cause = ports.ErrUnauthorized
	case 404:
		cause = ports.ErrNotFound
	case 409, 412:
		cause = ports.ErrConflict
	case 501:
		cause = ports.ErrUnverified
	}
	return &ports.DependencyError{Dependency: "ocis", Operation: method, Code: "http_error", Status: resp.StatusCode, Cause: cause}
}
func (s *Store) read(resp *http.Response, out any, xmlBody bool) error {
	defer resp.Body.Close()
	b, e := io.ReadAll(io.LimitReader(resp.Body, s.cfg.MaxMetadataBytes+1))
	if e != nil {
		return ports.ErrUnavailable
	}
	if int64(len(b)) > s.cfg.MaxMetadataBytes {
		return ports.ErrLimit
	}
	if xmlBody {
		e = xml.Unmarshal(b, out)
	} else {
		e = json.Unmarshal(b, out)
	}
	if e != nil {
		return ports.ErrIntegrity
	}
	return nil
}

const properties = `<?xml version="1.0"?><d:propfind xmlns:d="DAV:" xmlns:oc="http://owncloud.org/ns"><d:prop><oc:fileid/><d:getetag/><d:getcontentlength/><d:getlastmodified/><d:displayname/><d:resourcetype/></d:prop></d:propfind>`

type property struct {
	FileID       string `xml:"http://owncloud.org/ns fileid"`
	ETag         string `xml:"DAV: getetag"`
	Size         int64  `xml:"DAV: getcontentlength"`
	Modified     string `xml:"DAV: getlastmodified"`
	Name         string `xml:"DAV: displayname"`
	ResourceType struct {
		Collection *struct{} `xml:"DAV: collection"`
	} `xml:"DAV: resourcetype"`
}
type multistatus struct {
	Responses []struct {
		Href  string `xml:"DAV: href"`
		Props []struct {
			Status string   `xml:"DAV: status"`
			Value  property `xml:"DAV: prop"`
		} `xml:"DAV: propstat"`
	} `xml:"DAV: response"`
}

func (s *Store) propfind(ctx context.Context, endpoint string, depth string, cred ports.Credential) (multistatus, error) {
	var out multistatus
	r, e := s.do(ctx, "PROPFIND", endpoint, strings.NewReader(properties), int64(len(properties)), cred, http.Header{"Depth": []string{depth}, "Content-Type": []string{"application/xml"}})
	if e != nil {
		return out, e
	}
	if r.StatusCode != 207 {
		return out, failure(r, "PROPFIND")
	}
	return out, s.read(r, &out, true)
}
func metadata(ms multistatus, ref ports.ResourceRef) (ports.OriginalMetadata, error) {
	if len(ms.Responses) != 1 {
		return ports.OriginalMetadata{}, ports.ErrIntegrity
	}
	m := ports.OriginalMetadata{ResourceRef: ref}
	for _, ps := range ms.Responses[0].Props {
		if !strings.Contains(ps.Status, " 200 ") {
			continue
		}
		p := ps.Value
		if p.FileID != "" {
			m.ResourceID = p.FileID
		}
		if p.ETag != "" {
			m.ETag = p.ETag
		}
		if p.Name != "" {
			m.Name = p.Name
		}
		if p.Size >= 0 {
			m.Size = p.Size
		}
		if p.ResourceType.Collection != nil {
			m.IsDirectory = true
		}
		if p.Modified != "" {
			m.ModifiedAt, _ = http.ParseTime(p.Modified)
		}
	}
	u, e := url.Parse(ms.Responses[0].Href)
	if e != nil {
		return m, ports.ErrIntegrity
	}
	m.RelativePath = u.Path
	if m.Name == "" {
		m.Name = path.Base(strings.TrimRight(u.Path, "/"))
	}
	if m.ResourceID == "" || m.Size < 0 {
		return m, ports.ErrIntegrity
	}
	return m, nil
}
func (s *Store) Stat(ctx context.Context, ref ports.ResourceRef, cred ports.Credential) (ports.OriginalMetadata, error) {
	if e := s.resource(ref); e != nil {
		return ports.OriginalMetadata{}, e
	}
	ms, e := s.propfind(ctx, s.dav(ref), "0", cred)
	if e != nil {
		return ports.OriginalMetadata{}, e
	}
	m, e := metadata(ms, ref)
	if e == nil && m.ResourceID != ref.ResourceID {
		return ports.OriginalMetadata{}, ports.ErrIntegrity
	}
	return m, e
}
func (s *Store) statChild(ctx context.Context, parent ports.ResourceRef, name string, cred ports.Credential) (ports.OriginalMetadata, error) {
	if e := s.resource(parent); e != nil {
		return ports.OriginalMetadata{}, e
	}
	if !safeSegment(name) {
		return ports.OriginalMetadata{}, ports.ErrInvalid
	}
	ms, e := s.propfind(ctx, s.dav(parent)+"/"+url.PathEscape(name), "0", cred)
	if e != nil {
		return ports.OriginalMetadata{}, e
	}
	ref := parent
	ref.ResourceID = ""
	return metadata(ms, ref)
}

// LookupChild resolves a name relative to a stable native parent. It does not
// establish that an earlier unknown mutation created that child.
func (s *Store) LookupChild(ctx context.Context, parent ports.ResourceRef, name string, cred ports.Credential) (ports.OriginalMetadata, error) {
	return s.statChild(ctx, parent, name, cred)
}
func (s *Store) ResolvePersonalSpace(ctx context.Context, cred ports.Credential) (ports.PersonalSpace, error) {
	var out struct {
		Value []struct {
			ID, Name  string
			DriveType string `json:"driveType"`
			Root      struct {
				ID string `json:"id"`
			} `json:"root"`
		} `json:"value"`
	}
	resp, e := s.do(ctx, "GET", s.endpoint("/graph/v1.0/me/drives"), nil, 0, cred, nil)
	if e != nil {
		return ports.PersonalSpace{}, e
	}
	if resp.StatusCode != 200 {
		return ports.PersonalSpace{}, failure(resp, "GET drives")
	}
	if e = s.read(resp, &out, false); e != nil {
		return ports.PersonalSpace{}, e
	}
	found := []ports.PersonalSpace{}
	for _, d := range out.Value {
		if d.DriveType == "personal" && safeSegment(d.ID) && safeSegment(d.Root.ID) {
			found = append(found, ports.PersonalSpace{SpaceID: d.ID, RootResourceID: d.Root.ID, Name: d.Name})
		}
	}
	if len(found) != 1 {
		return ports.PersonalSpace{}, ports.ErrUnverified
	}
	return found[0], nil
}
func (s *Store) Revisions(ctx context.Context, ref ports.ResourceRef, cred ports.Credential) ([]ports.NativeRevision, error) {
	if !s.cfg.VerifiedRevisionRead {
		return nil, ports.ErrUnverified
	}
	if e := s.resource(ref); e != nil {
		return nil, e
	}
	endpoint := s.endpoint("/dav/meta/" + ref.ResourceID + "/v")
	ms, e := s.propfind(ctx, endpoint, "1", cred)
	if e != nil {
		return nil, e
	}
	out := []ports.NativeRevision{}
	prefix := "/dav/meta/" + ref.ResourceID + "/v/"
	for _, response := range ms.Responses {
		u, e := url.Parse(response.Href)
		if e != nil {
			return nil, ports.ErrIntegrity
		}
		decoded := u.Path
		idx := strings.Index(decoded, prefix)
		if idx < 0 {
			continue
		}
		id := strings.TrimPrefix(decoded[idx:], prefix)
		if !safeSegment(id) {
			continue
		}
		v := ports.NativeRevision{ID: id}
		for _, ps := range response.Props {
			if strings.Contains(ps.Status, " 200 ") {
				v.ETag = ps.Value.ETag
				v.Size = ps.Value.Size
				v.ModifiedAt, _ = http.ParseTime(ps.Value.Modified)
			}
		}
		out = append(out, v)
	}
	return out, nil
}

// OpenRevision accepts an immutable native history ID, or an etag: pin for a
// version that was current at commit time. oCIS 8.2 ignores GET If-Match, so the
// response ETag MUST be compared before a single body byte reaches a caller.
func (s *Store) OpenRevision(ctx context.Context, ref ports.RevisionRef, br *ports.ByteRange, cred ports.Credential) (ports.Content, error) {
	var out ports.Content
	if !s.cfg.VerifiedRevisionRead {
		return out, ports.ErrUnverified
	}
	if e := s.resource(ref.ResourceRef); e != nil {
		return out, e
	}
	expected := ref.ETag
	currentPin := strings.HasPrefix(ref.RevisionID, "etag:")
	if currentPin {
		pinned := strings.TrimPrefix(ref.RevisionID, "etag:")
		if expected != "" && expected != pinned {
			return out, ports.ErrInvalid
		}
		expected = pinned
	}
	if expected == "" || (!currentPin && !safeSegment(ref.RevisionID)) {
		return out, ports.ErrInvalid
	}
	endpoint := s.endpoint("/dav/meta/" + ref.ResourceID + "/v/" + ref.RevisionID)
	if currentPin {
		endpoint = s.dav(ref.ResourceRef)
	}
	headers := http.Header{"If-Match": []string{expected}}
	if br != nil {
		if br.Start < 0 || br.End < -1 || br.End >= 0 && br.End < br.Start {
			return out, ports.ErrInvalid
		}
		r := fmt.Sprintf("bytes=%d-", br.Start)
		if br.End >= 0 {
			r += strconv.FormatInt(br.End, 10)
		}
		headers.Set("Range", r)
	}
	resp, e := s.do(ctx, "GET", endpoint, nil, 0, cred, headers)
	if e != nil {
		return out, e
	}
	if currentPin && resp.StatusCode == http.StatusPreconditionFailed {
		resp.Body.Close()
		revisions, e := s.Revisions(ctx, ref.ResourceRef, cred)
		if e != nil {
			return out, e
		}
		for _, r := range revisions {
			if r.ETag == expected {
				return s.OpenRevision(ctx, ports.RevisionRef{ResourceRef: ref.ResourceRef, RevisionID: r.ID, ETag: expected}, br, cred)
			}
		}
		return out, ports.ErrNotFound
	}
	if resp.StatusCode != 200 && resp.StatusCode != 206 {
		return out, failure(resp, "GET revision")
	}
	if resp.Header.Get("ETag") != expected {
		resp.Body.Close()
		if !currentPin {
			return out, ports.ErrIntegrity
		}
		revisions, e := s.Revisions(ctx, ref.ResourceRef, cred)
		if e != nil {
			return out, e
		}
		for _, r := range revisions {
			if r.ETag == expected {
				return s.OpenRevision(ctx, ports.RevisionRef{ResourceRef: ref.ResourceRef, RevisionID: r.ID, ETag: expected}, br, cred)
			}
		}
		return out, ports.ErrNotFound
	}
	if br != nil && resp.StatusCode != 206 {
		resp.Body.Close()
		return out, ports.ErrIntegrity
	}
	out = ports.Content{Body: resp.Body, Size: resp.ContentLength, TotalSize: resp.ContentLength, ETag: resp.Header.Get("ETag"), ContentType: resp.Header.Get("Content-Type"), ContentRange: resp.Header.Get("Content-Range"), StatusCode: resp.StatusCode}
	if br != nil {
		parts := strings.Split(out.ContentRange, "/")
		if len(parts) != 2 {
			resp.Body.Close()
			return ports.Content{}, ports.ErrIntegrity
		}
		out.TotalSize, e = strconv.ParseInt(parts[1], 10, 64)
		var start, end, total int64
		count, rangeErr := fmt.Sscanf(out.ContentRange, "bytes %d-%d/%d", &start, &end, &total)
		expectedEnd := br.End
		if expectedEnd < 0 || expectedEnd >= total {
			expectedEnd = total - 1
		}
		if e != nil || count != 3 || rangeErr != nil || start != br.Start || end != expectedEnd || total <= end || end < start || resp.ContentLength != end-start+1 {
			resp.Body.Close()
			return ports.Content{}, ports.ErrIntegrity
		}
	}
	return out, nil
}
func (s *Store) CreateUpload(ctx context.Context, in ports.UploadRequest) (ports.UploadInfo, error) {
	var out ports.UploadInfo
	if in.OperationID == "" || !safeSegment(in.Name) || in.Size < 0 || !safeSegment(in.SpaceID) || !safeSegment(in.ParentResourceID) {
		return out, ports.ErrInvalid
	}
	if in.ExistingResourceID != "" && (!s.cfg.VerifiedConditionalOverwrite || in.ExpectedETag == "") {
		return out, ports.ErrUnverified
	}
	headers := http.Header{"Tus-Resumable": []string{"1.0.0"}, "Upload-Length": []string{strconv.FormatInt(in.Size, 10)}, "Upload-Metadata": []string{"filename " + base64.StdEncoding.EncodeToString([]byte(in.Name))}}
	if in.ExistingResourceID != "" {
		headers.Set("If-Match", in.ExpectedETag)
	} else {
		headers.Set("If-None-Match", "*")
	}
	ref := ports.ResourceRef{Instance: s.cfg.Instance, SpaceID: in.SpaceID, ResourceID: in.ParentResourceID}
	resp, e := s.do(ctx, "POST", s.dav(ref), nil, 0, in.Credential, headers)
	if e != nil {
		return out, e
	}
	if resp.StatusCode != 201 {
		return out, failure(resp, "POST tus")
	}
	defer resp.Body.Close()
	u, e := url.Parse(resp.Header.Get("Location"))
	if e != nil || resp.Header.Get("Location") == "" {
		return out, ports.ErrOutcomeUnknown
	}
	u = s.base.ResolveReference(u)
	if !s.uploadURL(u.String()) {
		return out, ports.ErrOutcomeUnknown
	}
	out = ports.UploadInfo{NativeURL: u.String(), OperationID: in.OperationID, Length: in.Size}
	out.ExpiresAt, _ = http.ParseTime(resp.Header.Get("Upload-Expires"))
	return out, nil
}
func (s *Store) uploadURL(raw string) bool {
	u, e := url.Parse(raw)
	return e == nil && u.Scheme == s.base.Scheme && u.Host == s.base.Host && u.User == nil && u.Fragment == "" && strings.HasPrefix(u.Path, strings.TrimRight(s.base.Path, "/")+"/data/")
}
func status(resp *http.Response, expectedLength int64) (ports.UploadStatus, error) {
	offset, e := strconv.ParseInt(resp.Header.Get("Upload-Offset"), 10, 64)
	if e != nil || offset < 0 {
		return ports.UploadStatus{}, ports.ErrIntegrity
	}
	length := expectedLength
	if v := resp.Header.Get("Upload-Length"); v != "" {
		length, e = strconv.ParseInt(v, 10, 64)
		if e != nil || length != expectedLength {
			return ports.UploadStatus{}, ports.ErrIntegrity
		}
	}
	if offset > length {
		return ports.UploadStatus{}, ports.ErrIntegrity
	}
	expiry, _ := http.ParseTime(resp.Header.Get("Upload-Expires"))
	return ports.UploadStatus{Offset: offset, Length: length, Complete: offset == length, ExpiresAt: expiry, ResourceID: resp.Header.Get("Oc-Fileid"), ETag: resp.Header.Get("ETag")}, nil
}
func (s *Store) StatUpload(ctx context.Context, in ports.UploadInfo, cred ports.Credential) (ports.UploadStatus, error) {
	if !s.uploadURL(in.NativeURL) || in.Length < 0 {
		return ports.UploadStatus{}, ports.ErrInvalid
	}
	resp, e := s.do(ctx, "HEAD", in.NativeURL, nil, 0, cred, http.Header{"Tus-Resumable": []string{"1.0.0"}})
	if e != nil {
		return ports.UploadStatus{}, e
	}
	if resp.StatusCode != 200 && resp.StatusCode != 204 {
		return ports.UploadStatus{}, failure(resp, "HEAD tus")
	}
	defer resp.Body.Close()
	return status(resp, in.Length)
}
func (s *Store) StreamUpload(ctx context.Context, in ports.UploadInfo, offset int64, body io.Reader, length int64, cred ports.Credential) (ports.UploadStatus, error) {
	if !s.uploadURL(in.NativeURL) || length < 0 || length > s.cfg.MaxUploadChunkBytes || offset < 0 || offset > in.Length || length > in.Length-offset || body == nil {
		return ports.UploadStatus{}, ports.ErrInvalid
	}
	resp, e := s.do(ctx, "PATCH", in.NativeURL, io.LimitReader(body, length), length, cred, http.Header{"Tus-Resumable": []string{"1.0.0"}, "Upload-Offset": []string{strconv.FormatInt(offset, 10)}, "Content-Type": []string{"application/offset+octet-stream"}})
	if e != nil {
		return ports.UploadStatus{}, e
	}
	if resp.StatusCode != 204 {
		return ports.UploadStatus{}, failure(resp, "PATCH tus")
	}
	defer resp.Body.Close()
	out, e := status(resp, in.Length)
	if e == nil && out.Offset != offset+length {
		return out, ports.ErrOutcomeUnknown
	}
	return out, e
}
func (s *Store) CreateDirectory(ctx context.Context, in ports.DirectoryRequest) (ports.OriginalMetadata, error) {
	if e := s.resource(in.Parent); e != nil {
		return ports.OriginalMetadata{}, e
	}
	if !safeSegment(in.Name) {
		return ports.OriginalMetadata{}, ports.ErrInvalid
	}
	resp, e := s.do(ctx, "MKCOL", s.dav(in.Parent)+"/"+url.PathEscape(in.Name), nil, 0, in.Credential, http.Header{"If-None-Match": []string{"*"}})
	if e != nil {
		return ports.OriginalMetadata{}, e
	}
	if resp.StatusCode != 201 {
		return ports.OriginalMetadata{}, failure(resp, "MKCOL")
	}
	resp.Body.Close()
	m, e := s.statChild(ctx, in.Parent, in.Name, in.Credential)
	if e != nil {
		return m, ports.ErrOutcomeUnknown
	}
	return m, nil
}
func (s *Store) Move(ctx context.Context, in ports.MoveRequest) (ports.OriginalMetadata, error) {
	if !s.cfg.VerifiedMoveHistory {
		return ports.OriginalMetadata{}, ports.ErrUnverified
	}
	if e := s.resource(in.Source); e != nil {
		return ports.OriginalMetadata{}, e
	}
	if e := s.resource(in.DestinationParent); e != nil {
		return ports.OriginalMetadata{}, e
	}
	if in.Source.SpaceID != in.DestinationParent.SpaceID || !safeSegment(in.Name) || in.ExpectedETag == "" {
		return ports.OriginalMetadata{}, ports.ErrInvalid
	}
	source, e := s.Stat(ctx, in.Source, in.Credential)
	if e != nil {
		return ports.OriginalMetadata{}, e
	}
	var before []ports.NativeRevision
	if !source.IsDirectory {
		before, e = s.Revisions(ctx, in.Source, in.Credential)
		if e != nil {
			return ports.OriginalMetadata{}, e
		}
	}
	headers := http.Header{"Destination": []string{s.dav(in.DestinationParent) + "/" + url.PathEscape(in.Name)}, "Overwrite": []string{"F"}, "If-Match": []string{in.ExpectedETag}}
	resp, e := s.do(ctx, "MOVE", s.dav(in.Source), nil, 0, in.Credential, headers)
	if e != nil {
		return ports.OriginalMetadata{}, e
	}
	if resp.StatusCode != 201 && resp.StatusCode != 204 {
		return ports.OriginalMetadata{}, failure(resp, "MOVE")
	}
	resp.Body.Close()
	after, e := s.statChild(ctx, in.DestinationParent, in.Name, in.Credential)
	if e != nil || after.ResourceID != in.Source.ResourceID {
		return after, ports.ErrOutcomeUnknown
	}
	if source.IsDirectory {
		if !after.IsDirectory {
			return after, ports.ErrOutcomeUnknown
		}
		return after, nil
	}
	revs, e := s.Revisions(ctx, in.Source, in.Credential)
	if e != nil {
		return after, ports.ErrOutcomeUnknown
	}
	seen := map[string]bool{}
	for _, r := range revs {
		seen[r.ID] = true
	}
	for _, r := range before {
		if !seen[r.ID] {
			return after, ports.ErrOutcomeUnknown
		}
	}
	return after, nil
}
func (s *Store) Reconcile(ctx context.Context, in ports.ReconcileRequest) (ports.ReconcileResult, error) {
	out := ports.ReconcileResult{State: "outcome_unknown"}
	if !in.TransferCompleted {
		if in.Upload.NativeURL == "" {
			out.Reason = "transfer_completion_unconfirmed"
			return out, nil
		}
		status, e := s.StatUpload(ctx, in.Upload, in.Credential)
		if e != nil {
			out.Reason = "transfer_status_unavailable"
			return out, e
		}
		if !status.Complete {
			out.Reason = "transfer_incomplete"
			return out, nil
		}
		if status.ResourceID != "" && in.Target.ResourceID != "" && status.ResourceID != in.Target.ResourceID {
			return out, ports.ErrIntegrity
		}
	}
	var m ports.OriginalMetadata
	var e error
	if in.Target.ResourceID != "" {
		m, e = s.Stat(ctx, in.Target, in.Credential)
	} else {
		m, e = s.statChild(ctx, in.TargetParent, in.TargetName, in.Credential)
	}
	if e != nil {
		return out, e
	}
	out.Metadata = m
	if m.Size != in.ExpectedSize {
		out.Reason = "size_mismatch"
		return out, nil
	}
	if in.ExpectedETag != "" && m.ETag != in.ExpectedETag {
		out.Reason = "etag_mismatch"
		return out, nil
	}
	if in.NativeRevisionID == "" || strings.HasPrefix(in.NativeRevisionID, "etag:") {
		if !s.cfg.VerifiedRevisionRead {
			return out, ports.ErrUnverified
		}
		if m.ETag == "" {
			out.Reason = "native_etag_missing"
			return out, nil
		}
		if in.NativeRevisionID != "" && in.NativeRevisionID != "etag:"+m.ETag {
			out.Reason = "native_etag_mismatch"
			return out, nil
		}
		out.State = "remote_confirmed"
		out.Revision = ports.RevisionRef{ResourceRef: m.ResourceRef, RevisionID: "etag:" + m.ETag, ETag: m.ETag}
		return out, nil
	}
	revisions, e := s.Revisions(ctx, m.ResourceRef, in.Credential)
	if e != nil {
		return out, e
	}
	for _, r := range revisions {
		if r.ID == in.NativeRevisionID {
			out.State = "remote_confirmed"
			out.Revision = ports.RevisionRef{ResourceRef: m.ResourceRef, RevisionID: r.ID, ETag: r.ETag}
			return out, nil
		}
	}
	out.Reason = "native_revision_missing"
	return out, nil
}

var _ ports.OriginalStore = (*Store)(nil)
var _ ports.PersonalSpaceResolver = (*Store)(nil)
var _ ports.UploadStreamer = (*Store)(nil)
