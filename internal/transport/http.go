// Package transport maps Hertz REST and the official MCP SDK to identical use cases.
// It intentionally has no general request/access logger, especially on anonymous feedback.
// Upload diagnostics are opt-in via DRIVE_TUS_DIAGNOSTIC=1 and contain only upload id,
// method, status and code — never headers, paths, tokens or bodies.
package transport

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/agentclouddrive/agent-clouddrive/internal/application"
	"github.com/agentclouddrive/agent-clouddrive/internal/domain"
	"github.com/agentclouddrive/agent-clouddrive/internal/ports"
	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/protocol"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type API struct{ Service *application.Service }

func (a *API) origin(origin string) bool {
	for _, v := range a.Service.Config.AllowedOrigins {
		if v == origin {
			return true
		}
	}
	return false
}
func fail(c *app.RequestContext, e error) {
	if errors.Is(e, application.ErrInvalidOnBehalfOf) {
		c.JSON(http.StatusBadRequest, map[string]any{"error": domain.NewError(domain.InvalidArgument, "X-On-Behalf-Of is required and must be a safe identifier")})
		return
	}
	e = application.Error(e)
	var de *domain.Error
	if !errors.As(e, &de) {
		de = domain.NewError(domain.DependencyUnavailable, "dependency unavailable")
	}
	c.JSON(domain.HTTPStatus(e), map[string]any{"error": de})
}
func remoteIP(addr string) string {
	if host, _, e := net.SplitHostPort(addr); e == nil {
		return host
	}
	return addr
}
func authStatus(e error) int {
	if errors.Is(e, application.ErrInvalidOnBehalfOf) {
		return http.StatusBadRequest
	}
	return domain.HTTPStatus(application.Error(e))
}
func (a *API) masterKeyRequest(bearer, keyID, onBehalfOf, sourceIP, route string) (application.MasterKeyAuth, bool) {
	if onBehalfOf == "" && !a.Service.IsMasterKey(bearer) {
		return application.MasterKeyAuth{}, false
	}
	return application.MasterKeyAuth{Bearer: bearer, KeyID: keyID, OnBehalfOf: onBehalfOf, RemoteIP: sourceIP, Route: route}, true
}
func (a *API) actor(ctx context.Context, c *app.RequestContext) (application.Actor, error) {
	h := string(c.GetHeader("Authorization"))
	if !strings.HasPrefix(h, "Bearer ") {
		return application.Actor{}, domain.NewError(domain.Unauthenticated, "bearer token required")
	}
	bearer := strings.TrimPrefix(h, "Bearer ")
	route := string(c.Path())
	source := a.sourceIP(c.RemoteAddr().String(), func(k string) string { return string(c.GetHeader(k)) })
	if in, ok := a.masterKeyRequest(bearer, string(c.GetHeader("X-Key-Id")), string(c.GetHeader("X-On-Behalf-Of")), source, route); ok {
		in.ReadOnly = route == "/v1/feedback"
		return a.Service.AuthenticateMasterKey(ctx, in)
	}
	if route == "/v1/feedback" {
		return a.Service.AuthenticateReadOnly(ctx, bearer, false)
	}
	return a.Service.Authenticate(ctx, bearer, false)
}
func readInput(c *app.RequestContext) (application.Object, error) {
	in := application.Object{}
	r := c.RequestBodyStream()
	if r == nil {
		r = strings.NewReader(string(c.Request.Body()))
	}
	b, e := io.ReadAll(io.LimitReader(r, (1<<20)+1))
	if e != nil {
		return nil, e
	}
	if len(b) > 1<<20 {
		return nil, domain.NewError(domain.UploadTooLarge, "JSON request is limited to 1 MiB")
	}
	if len(b) > 0 {
		d := json.NewDecoder(strings.NewReader(string(b)))
		d.UseNumber()
		if e = d.Decode(&in); e != nil {
			return nil, domain.NewError(domain.InvalidArgument, "invalid JSON object")
		}
	}
	for _, k := range []string{"parent_id", "cursor", "version_id", "artifact_set_id", "query"} {
		if v := c.Query(k); v != "" {
			in[k] = v
		}
	}
	for _, k := range []string{"limit", "offset"} {
		if v := c.Query(k); v != "" {
			n, _ := strconv.ParseInt(v, 10, 64)
			in[k] = n
		}
	}
	return in, nil
}
func (a *API) route(name, param, key string) app.HandlerFunc {
	return func(ctx context.Context, c *app.RequestContext) {
		actor, e := a.actor(ctx, c)
		if e != nil {
			fail(c, e)
			return
		}
		in, e := readInput(c)
		if e != nil {
			fail(c, e)
			return
		}
		if key != "" {
			in[key] = c.Param(param)
		}
		if name == "approve_share" {
			if !a.origin(string(c.GetHeader("Origin"))) || !a.Service.VerifySession(ctx, actor, string(c.Cookie("drive_session")), string(c.GetHeader("X-CSRF-Token"))) {
				fail(c, domain.NewError(domain.ConfirmationRequired, "interactive session and CSRF confirmation required"))
				return
			}
			actor.Interactive = true
		}
		out, e := a.Service.Execute(ctx, actor, name, in)
		if e != nil {
			fail(c, e)
			return
		}
		c.Header("Cache-Control", "no-store")
		c.JSON(200, out)
	}
}
func (a *API) Server() *server.Hertz {
	h := server.New(server.WithHostPorts(a.Service.Config.Listen), server.WithStreamBody(true), server.WithDisablePreParseMultipartForm(true), server.WithMaxRequestBodySize(64<<20))
	h.Use(func(ctx context.Context, c *app.RequestContext) {
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("Referrer-Policy", "no-referrer")
		c.Header("Cache-Control", "no-store")
		origin := string(c.GetHeader("Origin"))
		if origin != "" {
			if !a.origin(origin) {
				c.AbortWithStatus(403)
				return
			}
			c.Header("Access-Control-Allow-Origin", origin)
			c.Header("Vary", "Origin")
			c.Header("Access-Control-Allow-Credentials", "true")
			c.Header("Access-Control-Expose-Headers", "Upload-Offset,Upload-Length,Tus-Resumable,Location,Content-Range,Accept-Ranges")
			if string(c.Method()) == "OPTIONS" {
				c.Header("Access-Control-Allow-Methods", "GET,POST,PATCH,DELETE,HEAD,OPTIONS")
				c.Header("Access-Control-Allow-Headers", "Authorization,Content-Type,X-CSRF-Token,Tus-Resumable,Upload-Offset,Upload-Length,Upload-Metadata,Range,If-Match")
				c.AbortWithStatus(204)
				return
			}
		}
		c.Next(ctx)
	})
	h.GET("/healthz", func(ctx context.Context, c *app.RequestContext) {
		if e := a.Service.DB.Ping(ctx); e != nil {
			c.JSON(503, map[string]any{"status": "unready"})
			return
		}
		c.JSON(200, map[string]any{"status": "ok", "service": "agent-clouddrive"})
	})
	h.GET("/.well-known/oauth-protected-resource", func(_ context.Context, c *app.RequestContext) { c.JSON(200, a.Metadata()) })
	h.GET("/.well-known/oauth-protected-resource/mcp", func(_ context.Context, c *app.RequestContext) { c.JSON(200, a.Metadata()) })
	h.POST("/v1/session", func(ctx context.Context, c *app.RequestContext) {
		if !a.origin(string(c.GetHeader("Origin"))) {
			fail(c, domain.NewError(domain.AccessDenied, "trusted web origin required"))
			return
		}
		actor, e := a.actor(ctx, c)
		if e != nil {
			fail(c, e)
			return
		}
		out, e := a.Service.CreateSession(ctx, actor)
		if e != nil {
			fail(c, e)
			return
		}
		session, _ := out["session_token"].(string)
		delete(out, "session_token")
		c.SetCookie("drive_session", session, 1800, "/", "", protocol.CookieSameSiteStrictMode, strings.HasPrefix(a.Service.Config.PublicURL, "https:"), true)
		c.JSON(200, out)
	})
	for _, r := range []struct{ method, path, name, param, key string }{
		{"GET", "/v1/admin/jobs", "admin_jobs", "", ""}, {"GET", "/v1/admin/feedback", "admin_feedback", "", ""}, {"GET", "/v1/delegation/status", "delegation_status", "", ""}, {"GET", "/v1/me", "me", "", ""}, {"GET", "/v1/files", "list", "", ""}, {"GET", "/v1/files/:id", "stat", "id", "file_id"}, {"POST", "/v1/directories", "mkdir", "", ""}, {"PATCH", "/v1/files/:id/location", "move", "id", "file_id"}, {"POST", "/v1/uploads", "begin_upload", "", ""}, {"POST", "/v1/uploads/:id/finalize", "finish_upload", "id", "upload_id"}, {"POST", "/v1/uploads/:id/abandon", "abandon_upload", "id", "upload_id"}, {"GET", "/v1/operations/:id", "operation", "id", "operation_id"}, {"GET", "/v1/files/:id/versions", "versions", "id", "file_id"}, {"GET", "/v1/files/:id/markdown", "markdown", "id", "file_id"}, {"POST", "/v1/files/:id/conversions", "reconvert", "id", "file_id"}, {"POST", "/v1/search", "search", "", ""}, {"DELETE", "/v1/files/:id", "delete", "id", "file_id"}, {"GET", "/v1/trash", "trash", "", ""}, {"POST", "/v1/trash/:id/restore", "restore", "id", "trash_id"}, {"POST", "/v1/batches", "batch", "", ""}, {"POST", "/v1/share-requests", "request_share", "", ""}, {"GET", "/v1/share-requests", "share_requests", "", ""}, {"GET", "/v1/share-requests/:id", "share_request", "id", "request_id"}, {"POST", "/v1/share-requests/:id/approve", "approve_share", "id", "request_id"}, {"GET", "/v1/shares", "shares", "", ""}, {"POST", "/v1/shares/:id/revoke", "revoke_share", "id", "share_id"}, {"GET", "/v1/shared-with-me", "shared", "", ""}, {"POST", "/v1/feedback", "feedback", "", ""}} {
		h.Handle(r.method, r.path, a.route(r.name, r.param, r.key))
	}
	h.POST("/v1/delegation/start", func(ctx context.Context, c *app.RequestContext) {
		if !a.origin(string(c.GetHeader("Origin"))) {
			fail(c, domain.NewError(domain.AccessDenied, "trusted origin required"))
			return
		}
		actor, e := a.actor(ctx, c)
		if e != nil {
			fail(c, e)
			return
		}
		out, e := a.Service.BeginDelegation(ctx, actor)
		if e != nil {
			fail(c, e)
			return
		}
		state, _ := out["state_cookie"].(string)
		delete(out, "state_cookie")
		c.SetCookie("drive_delegation", state, 600, "/v1/delegation", "", protocol.CookieSameSiteLaxMode, strings.HasPrefix(a.Service.Config.PublicURL, "https:"), true)
		c.JSON(200, out)
	})
	h.GET("/v1/delegation/callback", func(ctx context.Context, c *app.RequestContext) {
		state := c.Query("state")
		cookie := string(c.Cookie("drive_delegation"))
		if state == "" || subtle.ConstantTimeCompare([]byte(state), []byte(cookie)) != 1 {
			fail(c, domain.NewError(domain.Unauthenticated, "invalid authorization state"))
			return
		}
		if e := a.Service.CompleteDelegation(ctx, state, c.Query("code")); e != nil {
			fail(c, e)
			return
		}
		c.SetCookie("drive_delegation", "", -1, "/v1/delegation", "", protocol.CookieSameSiteLaxMode, strings.HasPrefix(a.Service.Config.PublicURL, "https:"), true)
		c.Redirect(303, []byte(strings.TrimRight(a.Service.Config.PublicURL, "/")+"/?delegation=enabled"))
	})
	h.GET("/v1/files/:id/content", a.content)
	h.GET("/v1/audit/files/:id/versions", a.auditVersions)
	h.GET("/v1/audit/files/:id/content", a.auditContent)
	h.GET("/v1/audit/files/:id/markdown", a.auditMarkdown)
	h.GET("/v1/audit/files/:id/artifacts/:artifact/assets/:asset", a.auditAsset)
	h.GET("/v1/download/:token", a.ticket)
	h.GET("/v1/artifacts/:id/assets/:asset", a.asset)
	h.HEAD("/v1/tus/:id", a.tus)
	h.PATCH("/v1/tus/:id", a.tus)
	h.OPTIONS("/v1/tus/:id", func(_ context.Context, c *app.RequestContext) {
		c.Header("Tus-Resumable", "1.0.0")
		c.Header("Tus-Version", "1.0.0")
		c.Status(204)
	})
	h.GET("/v1/preview/:id", a.preview)
	h.GET("/v1/preview/:id/pages/:page", a.previewPage)
	h.GET("/s/:token", a.publicPreview)
	h.GET("/s/:token/pages/:page", a.previewPage)
	return h
}
func (a *API) Metadata() map[string]any {
	return map[string]any{"resource": a.Service.Config.MCPURL, "authorization_servers": []string{a.Service.Config.OIDCIssuer}, "scopes_supported": []string{"drive.read", "drive.write", "drive.delete", "drive.restore", "drive.convert", "drive.share.request", "drive.feedback"}, "bearer_methods_supported": []string{"header"}}
}
func parseRange(value string) (*ports.ByteRange, error) {
	if value == "" {
		return nil, nil
	}
	if !strings.HasPrefix(value, "bytes=") || strings.Contains(value, ",") {
		return nil, domain.NewError(domain.InvalidArgument, "only a single byte range is supported")
	}
	parts := strings.Split(strings.TrimPrefix(value, "bytes="), "-")
	if len(parts) != 2 || parts[0] == "" {
		return nil, domain.NewError(domain.InvalidArgument, "range requires start offset")
	}
	start, e := strconv.ParseInt(parts[0], 10, 64)
	if e != nil || start < 0 {
		return nil, domain.NewError(domain.InvalidArgument, "invalid range")
	}
	end := int64(-1)
	if parts[1] != "" {
		end, e = strconv.ParseInt(parts[1], 10, 64)
		if e != nil || end < start {
			return nil, domain.NewError(domain.InvalidArgument, "invalid range")
		}
	}
	return &ports.ByteRange{Start: start, End: end}, nil
}
func stream(c *app.RequestContext, v ports.Content) {
	if v.Body == nil {
		fail(c, domain.NewError(domain.DependencyUnavailable, "empty content response"))
		return
	}
	status := v.StatusCode
	if status == 0 {
		status = 200
	}
	c.Status(status)
	c.Header("Content-Type", v.ContentType)
	c.Header("Cache-Control", "private,no-store")
	c.Header("Accept-Ranges", "bytes")
	if v.ETag != "" {
		c.Header("ETag", v.ETag)
	}
	if v.ContentRange != "" {
		c.Header("Content-Range", v.ContentRange)
	}
	c.SetBodyStream(v.Body, int(v.Size))
}
func (a *API) content(ctx context.Context, c *app.RequestContext) {
	actor, e := a.actor(ctx, c)
	if e != nil {
		fail(c, e)
		return
	}
	r, e := parseRange(string(c.GetHeader("Range")))
	if e != nil {
		fail(c, e)
		return
	}
	v, e := a.Service.ReadContent(ctx, actor, c.Param("id"), c.Query("version_id"), r)
	if e != nil {
		fail(c, e)
		return
	}
	c.Header("Content-Disposition", "attachment")
	stream(c, v)
}
func (a *API) ticket(ctx context.Context, c *app.RequestContext) {
	r, e := parseRange(string(c.GetHeader("Range")))
	if e != nil {
		fail(c, e)
		return
	}
	v, e := a.Service.ReadTicket(ctx, c.Param("token"), r)
	if e != nil {
		fail(c, e)
		return
	}
	c.Header("Content-Disposition", "attachment")
	stream(c, v)
}
func (a *API) asset(ctx context.Context, c *app.RequestContext) {
	actor, e := a.actor(ctx, c)
	if e != nil {
		fail(c, e)
		return
	}
	v, e := a.Service.ReadAsset(ctx, actor, c.Param("id"), c.Param("asset"))
	if e != nil {
		fail(c, e)
		return
	}
	if v.ContentType == "image/svg+xml" {
		v.Body.Close()
		fail(c, domain.NewError(domain.AccessDenied, "active SVG must use raster preview"))
		return
	}
	stream(c, v)
}

type uploadStreamer interface {
	StreamUpload(context.Context, ports.UploadInfo, int64, io.Reader, int64, ports.Credential) (ports.UploadStatus, error)
}

func (a *API) tus(ctx context.Context, c *app.RequestContext) {
	id, method := string(c.Param("id")), string(c.Method())
	code := ""
	// Metadata only: upload id, method, status and code. Never headers, paths, tokens or bodies.
	if a.Service.Config.TusDiagnostic {
		defer func() {
			log.Printf("tus diagnostic upload_id=%s method=%s status=%d code=%s", id, method, c.Response.StatusCode(), code)
		}()
	}
	reject := func(e error) {
		code = string(domain.ErrorCode(application.Error(e)))
		fail(c, e)
	}
	actor, e := a.actor(ctx, c)
	if e != nil {
		reject(e)
		return
	}
	u, cred, e := a.Service.UploadNative(ctx, actor, id)
	if e != nil {
		reject(e)
		return
	}
	native, _ := u["native_upload_ref"].(string)
	size := int64(0)
	if v, ok := u["expected_size"].(float64); ok {
		size = int64(v)
	}
	op, _ := u["operation_id"].(string)
	info := ports.UploadInfo{NativeURL: native, OperationID: op, Length: size}
	var st ports.UploadStatus
	if method == "HEAD" {
		st, e = a.Service.Original.StatUpload(ctx, info, cred)
	} else {
		if string(c.GetHeader("Tus-Resumable")) != "1.0.0" {
			code = "MISSING_TUS_RESUMABLE"
			c.Status(412)
			return
		}
		if string(c.GetHeader("Content-Type")) != "application/offset+octet-stream" {
			code = "UNSUPPORTED_MEDIA_TYPE"
			c.Status(415)
			return
		}
		offset, er := strconv.ParseInt(string(c.GetHeader("Upload-Offset")), 10, 64)
		if er != nil || offset < 0 {
			reject(domain.NewError(domain.InvalidArgument, "valid Upload-Offset required"))
			return
		}
		length := c.Request.Header.ContentLength()
		if length < 0 || int64(length) > size-offset {
			reject(domain.NewError(domain.InvalidArgument, "chunk exceeds reserved upload length"))
			return
		}
		uploader, ok := a.Service.Original.(uploadStreamer)
		if !ok {
			reject(domain.NewError(domain.DependencyUnavailable, "streaming uploader unavailable"))
			return
		}
		epoch, err := a.Service.AdmitUploadPatch(ctx, actor, id, offset, int64(length))
		if err != nil {
			reject(err)
			return
		}
		st, e = uploader.StreamUpload(ctx, info, offset, c.RequestBodyStream(), int64(length), cred)
		e = a.Service.CompleteUploadPatch(ctx, id, epoch, st, e)
	}
	if e != nil {
		reject(e)
		return
	}
	c.Header("Tus-Resumable", "1.0.0")
	c.Header("Upload-Offset", strconv.FormatInt(st.Offset, 10))
	c.Header("Upload-Length", strconv.FormatInt(st.Length, 10))
	status := 204
	if method == "HEAD" {
		status = 200
	}
	code = "ok"
	c.Status(status)
}
func (a *API) preview(ctx context.Context, c *app.RequestContext) {
	actor, e := a.actor(ctx, c)
	if e != nil {
		fail(c, e)
		return
	}
	out, e := a.Service.PreviewInfo(ctx, actor, c.Param("id"), "", c.Query("version_id"))
	if e != nil {
		fail(c, e)
		return
	}
	c.JSON(200, out)
}
func (a *API) previewPage(ctx context.Context, c *app.RequestContext) {
	actor := application.Actor{}
	raw := c.Param("token")
	var e error
	if raw == "" {
		actor, e = a.actor(ctx, c)
		if e != nil {
			fail(c, e)
			return
		}
	}
	page, _ := strconv.Atoi(c.Param("page"))
	out, e := a.Service.PreviewPage(ctx, actor, c.Param("id"), raw, c.Query("version_id"), page)
	if e != nil {
		fail(c, e)
		return
	}
	stream(c, out)
}
func (a *API) publicPreview(ctx context.Context, c *app.RequestContext) {
	out, e := a.Service.PreviewInfo(ctx, application.Actor{}, "", c.Param("token"), c.Query("version_id"))
	if e != nil {
		fail(c, e)
		return
	}
	pm := out.(application.PreviewManifest)
	body := `<!doctype html><meta charset="utf-8"><title>CloudDrive preview</title><style>body{margin:0;background:#eee}img{display:block;max-width:100%%;margin:auto}</style>`
	for _, p := range pm.Pages {
		body += fmt.Sprintf(`<img alt="Page %d" src="%s">`, p.Page, p.URL)
	}
	c.Header("Content-Security-Policy", "default-src 'none'; img-src 'self'; style-src 'unsafe-inline'; frame-ancestors 'none'; sandbox")
	c.Data(200, "text/html; charset=utf-8", []byte(body))
}

var _ = http.StatusOK
var _ = time.Second

func (a *API) auditVersions(ctx context.Context, c *app.RequestContext) {
	actor, e := a.actor(ctx, c)
	if e != nil {
		fail(c, e)
		return
	}
	out, e := a.Service.AuditVersions(ctx, actor, c.Param("id"))
	if e != nil {
		fail(c, e)
		return
	}
	c.JSON(200, out)
}
func (a *API) auditContent(ctx context.Context, c *app.RequestContext) {
	actor, e := a.actor(ctx, c)
	if e != nil {
		fail(c, e)
		return
	}
	r, e := parseRange(string(c.GetHeader("Range")))
	if e != nil {
		fail(c, e)
		return
	}
	v, e := a.Service.AuditContent(ctx, actor, c.Param("id"), c.Query("version_id"), r)
	if e != nil {
		fail(c, e)
		return
	}
	c.Header("Content-Disposition", "attachment")
	stream(c, v)
}
func (a *API) auditMarkdown(ctx context.Context, c *app.RequestContext) {
	actor, e := a.actor(ctx, c)
	if e != nil {
		fail(c, e)
		return
	}
	v, e := a.Service.AuditMarkdown(ctx, actor, c.Param("id"), c.Query("artifact_set_id"))
	if e != nil {
		fail(c, e)
		return
	}
	c.Header("Content-Disposition", "attachment")
	stream(c, v)
}

func (a *API) auditAsset(ctx context.Context, c *app.RequestContext) {
	actor, e := a.actor(ctx, c)
	if e != nil {
		fail(c, e)
		return
	}
	v, e := a.Service.AuditAsset(ctx, actor, c.Param("id"), c.Param("artifact"), c.Param("asset"))
	if e != nil {
		fail(c, e)
		return
	}
	c.Header("Content-Disposition", "attachment")
	v.ContentType = "application/octet-stream"
	stream(c, v)
}
