package application

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	converteradapter "github.com/agentclouddrive/agent-clouddrive/internal/adapters/converters"
	searchadapter "github.com/agentclouddrive/agent-clouddrive/internal/adapters/search"
	"github.com/agentclouddrive/agent-clouddrive/internal/application/access"
	"github.com/agentclouddrive/agent-clouddrive/internal/dbgen"
	"github.com/agentclouddrive/agent-clouddrive/internal/domain"
	"github.com/agentclouddrive/agent-clouddrive/internal/ports"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/nats-io/nats.go"
)

const eventPrefix = "drive.events."

func pgUUID(s string) pgtype.UUID { var u pgtype.UUID; _ = u.Scan(s); return u }
func uuidText(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	b := u.Bytes
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
func contentHash(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func waitContext(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
func (s *Service) leaseSeconds() int32 {
	n := int64(s.Config.Lease / time.Second)
	if n < 3 {
		n = 300
	}
	if n > 86400 {
		n = 86400
	}
	return int32(n)
}
func (s *Service) openJetStream(ctx context.Context) (*nats.Conn, nats.JetStreamContext, error) {
	for {
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		nc, e := nats.Connect(s.Config.NATSURL, nats.Name("clouddrive-"+s.Config.NodeID), nats.Timeout(5*time.Second), nats.MaxReconnects(-1), nats.ReconnectWait(time.Second))
		if e != nil {
			if !waitContext(ctx, 2*time.Second) {
				return nil, nil, ctx.Err()
			}
			continue
		}
		js, e := nc.JetStream(nats.MaxWait(10 * time.Second))
		if e != nil {
			nc.Close()
			return nil, nil, e
		}
		_, e = js.StreamInfo(s.Config.NATSStream, nats.Context(ctx))
		if errors.Is(e, nats.ErrStreamNotFound) {
			_, e = js.AddStream(&nats.StreamConfig{Name: s.Config.NATSStream, Subjects: []string{eventPrefix + ">"}, Storage: nats.FileStorage, Retention: nats.LimitsPolicy, MaxAge: 7 * 24 * time.Hour, Duplicates: 24 * time.Hour}, nats.Context(ctx))
		}
		if e != nil {
			nc.Close()
			if !waitContext(ctx, 2*time.Second) {
				return nil, nil, ctx.Err()
			}
			continue
		}
		return nc, js, nil
	}
}

// RunWorker consumes persistent JetStream notifications. PostgreSQL remains the
// scheduler of record, so retries and a lost/redelivered message cannot lose work.
func (s *Service) RunWorker(ctx context.Context) error {
	if s.DB == nil || s.Original == nil || s.Identity == nil || s.Artifacts == nil || s.Search == nil {
		return ports.ErrUnavailable
	}
	nc, stream, e := s.openJetStream(ctx)
	if e != nil {
		return e
	}
	defer nc.Close()
	if e = s.Search.Configure(ctx); e != nil {
		return e
	}
	const durable = "clouddrive-converter"
	// Create explicitly, then bind. A library-owned implicit consumer would be
	// deleted by Unsubscribe on shutdown, losing durable acknowledgement state.
	if _, e = stream.ConsumerInfo(s.Config.NATSStream, durable, nats.Context(ctx)); errors.Is(e, nats.ErrConsumerNotFound) {
		_, e = stream.AddConsumer(s.Config.NATSStream, &nats.ConsumerConfig{Durable: durable, AckPolicy: nats.AckExplicitPolicy, DeliverPolicy: nats.DeliverAllPolicy, FilterSubject: eventPrefix + "conversion_requested", AckWait: time.Duration(s.leaseSeconds()) * time.Second, MaxAckPending: 64}, nats.Context(ctx))
		if e != nil {
			_, e = stream.ConsumerInfo(s.Config.NATSStream, durable, nats.Context(ctx))
		}
	}
	if e != nil {
		return e
	}
	sub, e := stream.PullSubscribe(eventPrefix+"conversion_requested", durable, nats.Bind(s.Config.NATSStream, durable), nats.ManualAck())
	if e != nil {
		return e
	}
	defer sub.Unsubscribe()
	q := dbgen.New(s.DB)
	for ctx.Err() == nil {
		messages, fetchErr := sub.Fetch(1, nats.MaxWait(time.Second))
		if fetchErr != nil && !errors.Is(fetchErr, nats.ErrTimeout) {
			if ctx.Err() != nil {
				return nil
			}
			if !waitContext(ctx, time.Second) {
				break
			}
		}
		for _, msg := range messages {
			var payload struct {
				JobID string `json:"job_id"`
			}
			if json.Unmarshal(msg.Data, &payload) != nil || !pgUUID(payload.JobID).Valid {
				_ = msg.Term()
				continue
			}
			job, claimErr := q.ClaimConversionJob(ctx, dbgen.ClaimConversionJobParams{JobID: pgUUID(payload.JobID), WorkerID: s.Config.NodeID, LeaseSeconds: s.leaseSeconds()})
			if errors.Is(claimErr, pgx.ErrNoRows) {
				saved, readErr := q.GetConversionJob(ctx, pgUUID(payload.JobID))
				if readErr == nil && saved.State != "processing" {
					_ = msg.AckSync(nats.Context(ctx))
				} else {
					_ = msg.NakWithDelay(10 * time.Second)
				}
				continue
			}
			if claimErr != nil {
				_ = msg.NakWithDelay(5 * time.Second)
				continue
			}
			e = s.processConversion(ctx, job, msg)
			if e == nil {
				_ = msg.AckSync(nats.Context(ctx))
			} else {
				_ = msg.NakWithDelay(10 * time.Second)
				slog.Warn("conversion attempt deferred", "job_id", payload.JobID)
			}
		}
		// Delayed retries are persisted in DB and need no duplicate queue event.
		jobs, claimErr := q.ClaimConversionJobs(ctx, dbgen.ClaimConversionJobsParams{WorkerID: s.Config.NodeID, LeaseSeconds: s.leaseSeconds(), BatchSize: 1})
		if claimErr != nil {
			if !waitContext(ctx, time.Second) {
				break
			}
			continue
		}
		for _, job := range jobs {
			if e = s.processConversion(ctx, job, nil); e != nil {
				slog.Warn("conversion attempt deferred", "job_id", uuidText(job.ID))
			}
		}
	}
	return nil
}

func (s *Service) processConversion(parent context.Context, job dbgen.ConversionJob, msg *nats.Msg) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	heartbeatDone := make(chan struct{})
	q := dbgen.New(s.DB)
	go func() {
		defer close(heartbeatDone)
		ticker := time.NewTicker(time.Duration(s.leaseSeconds()) * time.Second / 3)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				n, e := q.RenewConversionLease(ctx, dbgen.RenewConversionLeaseParams{JobID: job.ID, LeaseToken: job.LeaseToken, WorkerID: s.Config.NodeID, LeaseSeconds: s.leaseSeconds()})
				if e != nil || n != 1 {
					cancel()
					return
				}
				if msg != nil {
					_ = msg.InProgress()
				}
			}
		}
	}()
	defer func() { cancel(); <-heartbeatDone }()
	resultErr := s.convertAndPublish(ctx, job)
	if resultErr == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	state, code, retry := classifyConversionError(resultErr)
	var cf *conversionFailure
	if errors.As(resultErr, &cf) {
		state, code, retry = cf.state, cf.code, cf.retry
	}
	if retry && job.Attempt < job.MaxAttempts {
		delay := time.Second * time.Duration(1<<min(int(job.Attempt), 8))
		n, e := q.RetryConversion(ctx, dbgen.RetryConversionParams{JobID: job.ID, LeaseToken: job.LeaseToken, RetryAt: pgtype.Timestamptz{Time: time.Now().Add(delay), Valid: true}, ErrorCode: code})
		if e != nil {
			return e
		}
		if n != 1 {
			return ports.ErrConflict
		}
		return nil
	}
	n, e := q.FinishFailedConversion(ctx, dbgen.FinishFailedConversionParams{JobID: job.ID, LeaseToken: job.LeaseToken, State: state, ErrorCode: code, ErrorDetail: []byte(`{}`)})
	if e != nil {
		return e
	}
	if n != 1 {
		return ports.ErrConflict
	}
	return nil
}

type conversionFailure struct {
	state, code string
	retry       bool
}

func (e *conversionFailure) Error() string { return e.code }
func classifyConversionError(e error) (state, code string, retry bool) {
	switch {
	case errors.Is(e, ports.ErrLimit):
		return "not_converted", "limit_exceeded", false
	case errors.Is(e, ports.ErrUnverified), errors.Is(e, ports.ErrUnauthorized):
		return "blocked", "delegation_unavailable", false
	case errors.Is(e, ports.ErrIntegrity), errors.Is(e, ports.ErrInvalid):
		return "failed", "malformed", false
	case errors.Is(e, context.DeadlineExceeded):
		return "failed", "timeout", true
	default:
		return "failed", "temporary_failure", true
	}
}

type conversionSource struct {
	FileID, OwnerID, SpaceID, Name, Path, Issuer, Subject string
	Version                                               dbgen.FileVersion
	Ref                                                   ports.RevisionRef
}

func (s *Service) conversionSource(ctx context.Context, job dbgen.ConversionJob) (conversionSource, error) {
	return s.conversionSourceWithDB(ctx, s.DB, job)
}

func (s *Service) conversionSourceWithDB(ctx context.Context, database dbgen.DBTX, job dbgen.ConversionJob) (conversionSource, error) {
	var src conversionSource
	var e error
	e = database.QueryRow(ctx, `SELECT f.id::text,f.owner_id::text,f.space_id::text,f.name,f.path,p.issuer,p.subject FROM files f JOIN file_versions v ON v.file_id=f.id JOIN principals p ON p.id=f.owner_id WHERE v.id=$1 AND p.status='active' AND file_is_active(f.id)`, job.SourceVersionID).Scan(&src.FileID, &src.OwnerID, &src.SpaceID, &src.Name, &src.Path, &src.Issuer, &src.Subject)
	if e != nil {
		return src, e
	}
	src.Version, e = dbgen.New(database).GetOwnedVersion(ctx, dbgen.GetOwnedVersionParams{ID: job.SourceVersionID, OwnerID: pgUUID(src.OwnerID)})
	if e != nil {
		return src, e
	}
	if e = json.Unmarshal(src.Version.NativeRevisionRef, &src.Ref); e != nil || src.Ref.RevisionID == "" || src.Ref.ResourceID == "" {
		return src, ports.ErrIntegrity
	}
	status, e := s.Identity.SubjectStatus(ctx, src.Issuer, src.Subject)
	if e != nil {
		return src, e
	}
	if !status.Enabled {
		_, _ = database.Exec(ctx, `UPDATE principals SET status='disabled',disabled_reason=CASE WHEN status='active' THEN 'identity' ELSE disabled_reason END,updated_at=now() WHERE id=$1`, src.OwnerID)
		return src, &conversionFailure{"blocked", "account_disabled", false}
	}
	return src, nil
}

type manifestAsset struct {
	ID        string `json:"id"`
	Key       string `json:"key"`
	SHA256    string `json:"sha256"`
	MediaType string `json:"media_type"`
	Page      int    `json:"page"`
}
type conversionManifest struct {
	SchemaVersion     int                    `json:"schema_version"`
	FileID            string                 `json:"file_id"`
	SourceVersionID   string                 `json:"source_version_id"`
	ArtifactSetID     string                 `json:"artifact_set_id"`
	RecipeFingerprint string                 `json:"recipe_fingerprint"`
	Recipe            ports.Recipe           `json:"recipe"`
	Engine            ports.EngineInfo       `json:"engine"`
	Markdown          ports.Object           `json:"markdown"`
	Assets            []manifestAsset        `json:"assets"`
	Completeness      ports.Completeness     `json:"completeness"`
	Locations         []ports.Location       `json:"locations"`
	Pages             []ports.ConversionPage `json:"pages"`
}

func (s *Service) convertAndPublish(ctx context.Context, job dbgen.ConversionJob) error {
	src, e := s.conversionSource(ctx, job)
	if e != nil {
		return e
	}
	var recipe ports.Recipe
	if e = json.Unmarshal(job.RecipeSnapshot, &recipe); e != nil {
		return ports.ErrIntegrity
	}
	if recipe.TimeoutSeconds <= 0 || recipe.MaxPages <= 0 || recipe.MaxOutputBytes <= 0 {
		return ports.ErrIntegrity
	}
	recipe.Fingerprint = job.RecipeFingerprint
	ctx, cancel := context.WithTimeout(ctx, time.Duration(recipe.TimeoutSeconds)*time.Second)
	defer cancel()
	q := dbgen.New(s.DB)
	// A prior attempt may have saved immutable output but crashed while indexing.
	existing, e := q.GetArtifactForGeneration(ctx, dbgen.GetArtifactForGenerationParams{SourceVersionID: job.SourceVersionID, Generation: job.Generation})
	if e == nil {
		return s.resumeArtifact(ctx, job, src, existing)
	}
	if !errors.Is(e, pgx.ErrNoRows) {
		return e
	}
	limit := s.Config.ConvertMax
	if configured, ok := recipe.Config["convert_max_bytes"].(float64); ok && configured >= 0 {
		limit = int64(configured)
	}
	if src.Version.Size > limit {
		return &conversionFailure{"not_converted", "limit_exceeded", false}
	}
	cred, e := s.Identity.DelegatedCredential(ctx, ports.DelegationRequest{Subject: ports.Subject{Issuer: src.Issuer, Subject: src.Subject}, Audience: s.Config.OCISAudience, Source: src.Ref, JobID: uuidText(job.ID)})
	if e != nil {
		return e
	}
	original, e := s.Original.OpenRevision(ctx, src.Ref, nil, cred)
	if e != nil {
		return e
	}
	data, e := readBounded(original.Body, limit)
	_ = original.Body.Close()
	if e != nil {
		return e
	}
	if int64(len(data)) != src.Version.Size || contentHash(data) != src.Version.Sha256 {
		return ports.ErrIntegrity
	}
	attemptID := strconv.FormatInt(job.LeaseToken, 10)
	var result ports.ConversionResult
	if isPlainText(src.Name, src.Version.ContentType) {
		result, e = directText(data)
	} else {
		if s.Converter == nil {
			return &conversionFailure{"blocked", "provider_not_configured", false}
		}
		result, e = s.Converter.Execute(ctx, ports.ConversionRequest{JobID: uuidText(job.ID), AttemptID: attemptID, Capability: "document.convert", Input: ports.ConversionInput{ContentBase64: base64.StdEncoding.EncodeToString(data), MIMEType: src.Version.ContentType, Name: src.Name, FileID: src.FileID, SourceVersionID: uuidText(job.SourceVersionID), SHA256: src.Version.Sha256, Size: src.Version.Size}, Recipe: recipe, OutputScope: "tmp/" + uuidText(job.ID) + "/" + attemptID + "/"})
	}
	if e != nil {
		return e
	}
	if result.Status != "ready" && result.Status != "partial" {
		code := "conversion_failed"
		retry := false
		if len(result.Errors) > 0 {
			code = result.Errors[0].Code
			retry = result.Errors[0].Retryable
		}
		return &conversionFailure{result.Status, code, retry}
	}
	if e = converteradapter.ValidateResult(result, recipe.MaxPages, recipe.MaxOutputBytes); e != nil {
		return e
	}
	complete := domain.Completeness{Status: domain.ConversionStatus(result.Completeness.Status), TotalPages: result.Completeness.TotalPages, MissingTextPages: result.Completeness.MissingTextPages, MissingDescriptionPages: result.Completeness.MissingDescriptionPages}
	if e = complete.Validate(); e != nil {
		return ports.ErrIntegrity
	}
	aid := id()
	prefix := "tmp/" + uuidText(job.ID) + "/" + attemptID + "/"
	manifest := conversionManifest{SchemaVersion: 1, FileID: src.FileID, SourceVersionID: uuidText(job.SourceVersionID), ArtifactSetID: aid, RecipeFingerprint: job.RecipeFingerprint, Recipe: recipe, Engine: result.Engine, Assets: []manifestAsset{}, Completeness: result.Completeness, Locations: result.Locations, Pages: result.Pages}
	for _, asset := range result.Assets {
		if !safeAssetID.MatchString(asset.ID) || !safeImageType(asset.MediaType) {
			return ports.ErrIntegrity
		}
		raw, decodeErr := base64.StdEncoding.DecodeString(asset.ContentBase64)
		if decodeErr != nil || contentHash(raw) != asset.SHA256 {
			return ports.ErrIntegrity
		}
		key := "images/" + aid + "/" + asset.ID + imageExtension(asset.MediaType)
		_, e = s.publishBytes(ctx, prefix+asset.ID, key, asset.MediaType, raw)
		if e != nil {
			return e
		}
		manifest.Assets = append(manifest.Assets, manifestAsset{asset.ID, key, asset.SHA256, asset.MediaType, asset.Page})
		result.Markdown = strings.ReplaceAll(result.Markdown, "asset:"+asset.ID+")",
			"/v1/artifacts/"+aid+"/assets/"+asset.ID+")")
	}
	if strings.Contains(result.Markdown, "asset:") {
		return ports.ErrIntegrity
	}
	md, e := s.publishBytes(ctx, prefix+"content.md", "markdown/"+aid+"/content.md", "text/markdown; charset=utf-8", []byte(result.Markdown))
	if e != nil {
		return e
	}
	manifest.Markdown = md
	mf, e := s.publishBytes(ctx, prefix+"manifest.json", "manifests/"+aid+"/manifest.json", "application/json", js(manifest))
	if e != nil {
		return e
	}
	a, e := q.CreateArtifactSet(ctx, dbgen.CreateArtifactSetParams{ID: pgUUID(aid), SourceVersionID: job.SourceVersionID, Generation: job.Generation, RecipeFingerprint: job.RecipeFingerprint, RecipeSnapshot: job.RecipeSnapshot, ManifestKey: mf.Key, MarkdownKey: md.Key, ManifestSha256: mf.SHA256, MarkdownSha256: md.SHA256, Completeness: js(result.Completeness)})
	if e != nil {
		return e
	}
	if e = s.savePageResults(ctx, job, manifest); e != nil {
		return e
	}
	return s.indexAndPublish(ctx, job, src, a, result.Markdown, manifest)
}

var safeAssetID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

func safeImageType(v string) bool {
	return v == "image/png" || v == "image/jpeg" || v == "image/webp" || v == "image/gif"
}
func imageExtension(v string) string {
	switch v {
	case "image/jpeg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	default:
		return ".png"
	}
}
func isPlainText(name, media string) bool {
	ext := strings.ToLower(path.Ext(name))
	return media == "text/plain" || strings.HasPrefix(media, "text/plain;") || strings.HasPrefix(media, "text/markdown") || ext == ".txt" || ext == ".md" || ext == ".markdown"
}
func directText(data []byte) (ports.ConversionResult, error) {
	if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return ports.ConversionResult{}, ports.ErrIntegrity
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return ports.ConversionResult{}, &conversionFailure{"not_converted", "empty_document", false}
	}
	return ports.ConversionResult{Status: "ready", Engine: ports.EngineInfo{Name: "utf8-text", Version: "1"}, Markdown: string(data), Pages: []ports.ConversionPage{{Page: 1, Block: "p1-b01", TextStatus: "ready", DescriptionStatus: "not_required"}}, Completeness: ports.Completeness{Status: "ready", TotalPages: 1, MissingTextPages: []int{}, MissingDescriptionPages: []int{}, Warnings: []string{}}, Assets: []ports.ConversionAsset{}, Errors: []ports.ConversionError{}}, nil
}
func (s *Service) publishBytes(ctx context.Context, tmp, key, media string, data []byte) (ports.Object, error) {
	h := contentHash(data)
	if _, e := s.Artifacts.PutAttempt(ctx, ports.ArtifactWrite{Key: tmp, Body: bytes.NewReader(data), Size: int64(len(data)), SHA256: h, ContentType: media}); e != nil {
		return ports.Object{}, e
	}
	obj, e := s.Artifacts.Publish(ctx, tmp, key, h)
	if e != nil {
		return obj, e
	}
	return s.Artifacts.Verify(ctx, key, h)
}
func (s *Service) savePageResults(ctx context.Context, job dbgen.ConversionJob, manifest conversionManifest) error {
	q := dbgen.New(s.DB)
	for _, page := range manifest.Pages {
		for _, stage := range []struct{ name, state string }{{"text", page.TextStatus}, {"description", page.DescriptionStatus}} {
			state := stage.state
			if state == "" {
				state = "failed"
			}
			var code *string
			if state == "failed" {
				v := "stage_failed"
				code = &v
			}
			_, e := q.SavePageResult(ctx, dbgen.SavePageResultParams{JobID: job.ID, PageNo: int32(page.Page), Stage: stage.name, Attempt: job.Attempt, LeaseToken: job.LeaseToken, State: state, ResultRef: js(Object{"manifest_key": "manifests/" + manifest.ArtifactSetID + "/manifest.json", "block": page.Block}), ErrorCode: code})
			if e != nil {
				return e
			}
		}
	}
	return nil
}
func (s *Service) resumeArtifact(ctx context.Context, job dbgen.ConversionJob, src conversionSource, a dbgen.ArtifactSet) error {
	if _, e := s.Artifacts.Verify(ctx, a.ManifestKey, a.ManifestSha256); e != nil {
		return e
	}
	if _, e := s.Artifacts.Verify(ctx, a.MarkdownKey, a.MarkdownSha256); e != nil {
		return e
	}
	c, e := s.Artifacts.Open(ctx, a.ManifestKey, nil)
	if e != nil {
		return e
	}
	b, e := readBounded(c.Body, 4<<20)
	_ = c.Body.Close()
	if e != nil {
		return e
	}
	var manifest conversionManifest
	if e = json.Unmarshal(b, &manifest); e != nil {
		return ports.ErrIntegrity
	}
	if manifest.ArtifactSetID != uuidText(a.ID) || manifest.SourceVersionID != uuidText(job.SourceVersionID) {
		return ports.ErrIntegrity
	}
	for _, asset := range manifest.Assets {
		if _, e = s.Artifacts.Verify(ctx, asset.Key, asset.SHA256); e != nil {
			return e
		}
	}
	c, e = s.Artifacts.Open(ctx, a.MarkdownKey, nil)
	if e != nil {
		return e
	}
	b, e = readBounded(c.Body, 64<<20)
	_ = c.Body.Close()
	if e != nil {
		return e
	}
	if e = s.savePageResults(ctx, job, manifest); e != nil {
		return e
	}
	return s.indexAndPublish(ctx, job, src, a, string(b), manifest)
}

var pageMarker = regexp.MustCompile(`<!--\s*page:(\d+)\s+block:([A-Za-z0-9_-]+)\s*-->`)

func conversionBlocks(markdown string, manifest conversionManifest) []searchadapter.TextBlock {
	unmarked := func() []searchadapter.TextBlock {
		page, block := 0, "document"
		if manifest.Completeness.TotalPages == 1 {
			page = 1
			if len(manifest.Pages) > 0 {
				block = manifest.Pages[0].Block
			}
		}
		return []searchadapter.TextBlock{{Text: markdown, Page: page, BlockID: block}}
	}
	// Raw text/Markdown comments are content, not converter-generated page
	// provenance. Never let them forge page numbers or structural allocations.
	if manifest.Engine.Name == "utf8-text" {
		return unmarked()
	}
	allowed := make(map[string]int, len(manifest.Pages))
	for _, page := range manifest.Pages {
		if page.Page < 1 || page.Page > manifest.Completeness.TotalPages || page.Block == "" {
			return unmarked()
		}
		if _, duplicate := allowed[page.Block]; duplicate {
			return unmarked()
		}
		allowed[page.Block] = page.Page
	}
	if len(allowed) == 0 {
		return unmarked()
	}
	// Bound regex allocations by the validated manifest, not by untrusted text.
	// One extra result detects overproduction; ambiguous structure is retained
	// as unpaged content rather than silently discarded or assigned fake pages.
	matches := pageMarker.FindAllStringSubmatchIndex(markdown, len(allowed)+1)
	if len(matches) == 0 || len(matches) > len(allowed) {
		return unmarked()
	}
	seen := make(map[string]bool, len(matches))
	for _, m := range matches {
		p, err := strconv.Atoi(markdown[m[2]:m[3]])
		block := markdown[m[4]:m[5]]
		expected, exists := allowed[block]
		if err != nil || !exists || p != expected || seen[block] {
			return unmarked()
		}
		seen[block] = true
	}
	out := make([]searchadapter.TextBlock, 0, len(matches)+1)
	if strings.TrimSpace(markdown[:matches[0][0]]) != "" {
		out = append(out, searchadapter.TextBlock{Text: markdown[:matches[0][0]], BlockID: "preface"})
	}
	for i, m := range matches {
		end := len(markdown)
		if i+1 < len(matches) {
			end = matches[i+1][0]
		}
		p, _ := strconv.Atoi(markdown[m[2]:m[3]])
		assets := []string{}
		for _, asset := range manifest.Assets {
			if asset.Page == p {
				assets = append(assets, asset.ID)
			}
		}
		out = append(out, searchadapter.TextBlock{Text: markdown[m[1]:end], Page: p, BlockID: markdown[m[4]:m[5]], AssetIDs: assets})
	}
	return out
}

func (s *Service) indexAndPublish(ctx context.Context, job dbgen.ConversionJob, src conversionSource, a dbgen.ArtifactSet, markdown string, manifest conversionManifest) error {
	q := dbgen.New(s.DB)
	base := ports.SearchDocument{OwnerID: src.OwnerID, SpaceID: src.SpaceID, FileID: src.FileID, VersionID: uuidText(job.SourceVersionID), ArtifactSetID: uuidText(a.ID), SourcePath: src.Path, VersionCreatedAt: src.Version.CommittedAt.Time, Title: src.Name, Completeness: manifest.Completeness.Status, MissingPages: manifest.Completeness.MissingTextPages}
	docs, e := searchadapter.Chunk(base, conversionBlocks(markdown, manifest), 3072)
	if e != nil {
		return e
	}
	if a.IndexState != "succeeded" {
		task, e := s.upsertSearchDocuments(ctx, docs, func(task ports.SearchTask) error {
			_, err := q.SetArtifactIndexTask(ctx, dbgen.SetArtifactIndexTaskParams{ID: a.ID, IndexTaskID: &task.UID})
			return err
		})
		if e != nil {
			return e
		}
		if _, e = q.MarkArtifactIndexed(ctx, dbgen.MarkArtifactIndexedParams{ID: a.ID, IndexTaskID: &task.UID}); e != nil {
			return e
		}
	}
	status, e := s.Identity.SubjectStatus(ctx, src.Issuer, src.Subject)
	if e != nil {
		return e
	}
	if !status.Enabled {
		_, _ = s.DB.Exec(ctx, `UPDATE principals SET status='disabled',disabled_reason=CASE WHEN status='active' THEN 'identity' ELSE disabled_reason END,updated_at=now() WHERE id=$1`, src.OwnerID)
		return &conversionFailure{"blocked", "account_disabled", false}
	}
	_, e = q.PublishArtifact(ctx, dbgen.PublishArtifactParams{JobID: job.ID, LeaseToken: job.LeaseToken, ArtifactSetID: a.ID})
	return e
}

// RunMaintenance does not purge history or final artifacts. It publishes durable
// outbox rows, handles rebuildable index metadata, and reconciles native outcomes.
func (s *Service) RunMaintenance(ctx context.Context) error {
	if s.DB == nil {
		return ports.ErrUnavailable
	}
	nc, stream, e := s.openJetStream(ctx)
	if e != nil {
		return e
	}
	defer nc.Close()
	s.runMaintenance(ctx, stream)
	return nil
}

// Each reconciliation lane is independent of outbox delivery and of the other
// dependencies. A slow identity provider must not hold committed path updates
// behind it, and a continuous outbox backlog must not starve account revocation.
// All lanes are canceled and joined before their caller returns. Do not impose
// a pass-wide deadline on a preclaimed batch or a potentially large upload hash:
// doing so could repeatedly skip later items or prevent large-file recovery.
func (s *Service) runMaintenance(ctx context.Context, stream nats.JetStreamContext) {
	q := dbgen.New(s.DB)
	tasks := []struct {
		warning string
		run     func(context.Context) error
	}{
		{"maintenance cleanup pending", func(ctx context.Context) error {
			if _, e := q.ExpireExhaustedJobs(ctx); e != nil {
				return e
			}
			if _, e := s.DB.Exec(ctx, `DELETE FROM web_sessions WHERE expires_at<now()`); e != nil {
				return e
			}
			_, e := s.DB.Exec(ctx, `DELETE FROM access_tickets WHERE expires_at<now()`)
			return e
		}},
		{"identity status reconciliation pending", s.syncPrincipalStatuses},
		{"native metadata reconciliation pending", s.ReconcileMutations},
		{"native upload reconciliation pending", s.reconcileUploads},
	}
	var workers sync.WaitGroup
	for _, task := range tasks {
		workers.Go(func() {
			for ctx.Err() == nil {
				err := task.run(ctx)
				if err != nil && ctx.Err() == nil {
					slog.Warn(task.warning)
				}
				if !waitContext(ctx, time.Second) {
					return
				}
			}
		})
	}
	s.drainOutbox(ctx, stream)
	workers.Wait()
}

func (s *Service) drainOutbox(ctx context.Context, stream nats.JetStreamContext) {
	q := dbgen.New(s.DB)
	for ctx.Err() == nil {
		// Claim only the next row so a slow index update cannot exhaust the
		// leases of a preclaimed batch. Keep draining without a per-row sleep.
		rows, claimErr := q.ClaimOutbox(ctx, dbgen.ClaimOutboxParams{WorkerID: s.Config.NodeID + "-maintenance", LeaseSeconds: s.leaseSeconds(), BatchSize: 1})
		if claimErr != nil {
			if !waitContext(ctx, time.Second) {
				break
			}
			continue
		}
		for _, row := range rows {
			e := s.publishOutbox(ctx, stream, row)
			if e != nil {
				_, _ = q.RetryOutbox(ctx, dbgen.RetryOutboxParams{ID: row.ID, LeaseToken: row.LeaseToken, RetryAt: pgtype.Timestamptz{Time: time.Now().Add(10 * time.Second), Valid: true}})
				slog.Warn("outbox retry scheduled", "event_id", uuidText(row.ID))
			}
		}
		if len(rows) == 0 && !waitContext(ctx, time.Second) {
			break
		}
	}
}
func (s *Service) publishOutbox(parent context.Context, stream nats.JetStreamContext, row dbgen.Outbox) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	// Index maintenance may hold the only pool connection while waiting for
	// external tasks. Use one bounded, dedicated connection for this single
	// claimed row's heartbeat; never consume a second business pool slot.
	connectCtx, connectCancel := context.WithTimeout(ctx, 5*time.Second)
	leaseDB, err := pgx.ConnectConfig(connectCtx, s.DB.Config().ConnConfig.Copy())
	connectCancel()
	if err != nil {
		return err
	}
	defer func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		_ = leaseDB.Close(closeCtx)
	}()
	lease := time.Duration(s.leaseSeconds()) * time.Second
	renew := func() error {
		renewCtx, renewCancel := context.WithTimeout(ctx, lease/3)
		defer renewCancel()
		n, e := dbgen.New(leaseDB).RenewOutboxLease(renewCtx, dbgen.RenewOutboxLeaseParams{
			ID: row.ID, LeaseToken: row.LeaseToken, LeaseSeconds: s.leaseSeconds(),
		})
		if e != nil {
			return e
		}
		if n != 1 {
			return ports.ErrConflict
		}
		return nil
	}
	// Reject an already expired or reclaimed lease before any external write.
	if err = renew(); err != nil {
		return err
	}
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		ticker := time.NewTicker(lease / 3)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if renew() != nil {
					cancel()
					return
				}
			}
		}
	}()
	defer func() { cancel(); <-heartbeatDone }()
	if e := s.indexMaintenance(ctx, row); e != nil {
		return e
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	subject := eventPrefix + strings.ReplaceAll(row.EventType, ".", "_")
	if _, e := stream.Publish(subject, row.Payload, nats.MsgId(uuidText(row.ID)), nats.Context(ctx)); e != nil {
		return e
	}
	n, e := dbgen.New(s.DB).MarkOutboxPublished(ctx, dbgen.MarkOutboxPublishedParams{ID: row.ID, LeaseToken: row.LeaseToken})
	if e != nil {
		return e
	}
	if n != 1 {
		return ports.ErrConflict
	}
	return nil
}
func (s *Service) indexMaintenance(ctx context.Context, row dbgen.Outbox) error {
	var payload Object
	if e := json.Unmarshal(row.Payload, &payload); e != nil {
		return ports.ErrIntegrity
	}
	switch row.EventType {
	case "search.published", "file.move", "file.rename", "file.restore", "file.delete":
	default:
		return nil
	}
	if s.Search == nil {
		return ports.ErrUnavailable
	}
	// Serialize index maintenance across processes before reading any path or
	// lifecycle snapshot. A stale lease holder must not overwrite a newer MOVE,
	// or delete documents rebuilt by RESTORE. Try-lock avoids occupying every
	// pool connection with waiters. ALL nested DB reads use this transaction,
	// so even a one-connection pool cannot self-deadlock.
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cleanupCancel()
		_ = tx.Rollback(cleanupCtx)
	}()
	err = s.indexMaintenanceLocked(ctx, tx, row, payload)
	// The only writes inside this transaction are authoritative security-state
	// updates (e.g. a newly detected disabled owner). Preserve those even when
	// indexing is deferred. External search effects remain retryable; no outbox
	// acknowledgement is committed here. A SQL-aborted transaction cannot commit.
	commitCtx, commitCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer commitCancel()
	if commitErr := tx.Commit(commitCtx); commitErr != nil {
		return commitErr
	}
	return err
}

func (s *Service) indexMaintenanceLocked(ctx context.Context, tx pgx.Tx, row dbgen.Outbox, payload Object) error {
	var err error
	var owner string
	if err = tx.QueryRow(ctx, `SELECT owner_id::text FROM files WHERE id=$1`, row.AggregateID).Scan(&owner); err != nil {
		return err
	}
	var locked bool
	if err = tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended('search-owner:'||$1,0))`, owner).Scan(&locked); err != nil {
		return err
	}
	if !locked {
		return ports.ErrConflict
	}
	if row.EventType == "search.published" {
		previous := str(payload, "previous_artifact_set_id")
		var owner, current, currentPath, fileID string
		if e := tx.QueryRow(ctx, `SELECT owner_id::text,COALESCE(search_artifact_set_id::text,''),path,id::text FROM files WHERE id=$1`, row.AggregateID).Scan(&owner, &current, &currentPath, &fileID); e != nil {
			return e
		}
		if s.Search == nil {
			return ports.ErrUnavailable
		}
		if previous != "" && previous != current {
			task, e := s.Search.DeleteArtifactSet(ctx, owner, previous)
			if e != nil {
				return e
			}
			if e = s.Search.Wait(ctx, task); e != nil {
				return e
			}
		}
		// Conversion may race a rename. This durable post-publication event
		// refreshes path labels even if an earlier move event ran before Upsert.
		task, e := s.Search.UpdatePaths(ctx, owner, map[string]string{fileID: currentPath})
		if e != nil {
			return e
		}
		return s.Search.Wait(ctx, task)
	}

	switch row.EventType {
	case "file.move", "file.rename", "file.restore", "file.delete":
	default:
		return nil
	}
	if s.Search == nil {
		return ports.ErrUnavailable
	}
	rows, e := tx.Query(ctx, `WITH RECURSIVE tree AS(SELECT id,parent_id,path,owner_id,search_artifact_set_id FROM files WHERE id=$1 UNION ALL SELECT f.id,f.parent_id,f.path,f.owner_id,f.search_artifact_set_id FROM files f JOIN tree p ON f.parent_id=p.id) SELECT id::text,path,owner_id::text,COALESCE(search_artifact_set_id::text,'') FROM tree`, row.AggregateID)
	if e != nil {
		return e
	}
	type entry struct{ fileID, path, owner, artifactID string }
	entries := []entry{}
	for rows.Next() {
		var item entry
		if e = rows.Scan(&item.fileID, &item.path, &item.owner, &item.artifactID); e != nil {
			rows.Close()
			return e
		}
		entries = append(entries, item)
	}
	rows.Close()
	if e = rows.Err(); e != nil {
		return e
	}
	paths := map[string]map[string]string{}
	for _, item := range entries {
		var active bool
		if e = tx.QueryRow(ctx, `SELECT file_is_active($1)`, item.fileID).Scan(&active); e != nil {
			return e
		}
		if !active {
			if item.artifactID != "" {
				task, e := s.Search.DeleteArtifactSet(ctx, item.owner, item.artifactID)
				if e != nil {
					return e
				}
				if e = s.Search.Wait(ctx, task); e != nil {
					return e
				}
			}
			continue
		}
		if paths[item.owner] == nil {
			paths[item.owner] = map[string]string{}
		}
		paths[item.owner][item.fileID] = item.path
		// Restoring an item must repopulate blocks removed by deletion without
		// re-running OCR. Late delete events also obey the current lifecycle.
		if (row.EventType == "file.restore" || row.EventType == "file.delete") && item.artifactID != "" {
			if e = s.rebuildSearchArtifactWithDB(ctx, tx, item.artifactID); e != nil {
				return e
			}
		}
	}
	for owner, ownerPaths := range paths {
		if e = s.updateSearchPaths(ctx, owner, ownerPaths); e != nil {
			return e
		}
	}
	return nil
}

// UpdatePaths accepts at most 100 files. A directory mutation can affect many
// more descendants; acknowledge its outbox event only after every owner-scoped
// batch finishes. A failed partial pass is safely retried from current DB paths.
func (s *Service) updateSearchPaths(ctx context.Context, owner string, paths map[string]string) error {
	batch := map[string]string{}
	flush := func() error {
		task, e := s.Search.UpdatePaths(ctx, owner, batch)
		if e != nil {
			return e
		}
		return s.Search.Wait(ctx, task)
	}
	for fileID, sourcePath := range paths {
		batch[fileID] = sourcePath
		if len(batch) == 100 {
			if e := flush(); e != nil {
				return e
			}
			batch = map[string]string{}
		}
	}
	if len(batch) > 0 {
		return flush()
	}
	return nil
}

func (s *Service) rebuildSearchArtifact(ctx context.Context, aid string) error {
	return s.rebuildSearchArtifactWithDB(ctx, s.DB, aid)
}

func (s *Service) rebuildSearchArtifactWithDB(ctx context.Context, database dbgen.DBTX, aid string) error {
	a, e := dbgen.New(database).GetArtifactSet(ctx, pgUUID(aid))
	if e != nil {
		return e
	}
	source, e := s.conversionSourceWithDB(ctx, database, dbgen.ConversionJob{SourceVersionID: a.SourceVersionID})
	if e != nil {
		return e
	}
	c, e := s.Artifacts.Open(ctx, a.ManifestKey, nil)
	if e != nil {
		return e
	}
	raw, e := readBounded(c.Body, 4<<20)
	_ = c.Body.Close()
	if e != nil {
		return e
	}
	if contentHash(raw) != a.ManifestSha256 {
		return ports.ErrIntegrity
	}
	var manifest conversionManifest
	if e = json.Unmarshal(raw, &manifest); e != nil {
		return e
	}
	c, e = s.Artifacts.Open(ctx, a.MarkdownKey, nil)
	if e != nil {
		return e
	}
	raw, e = readBounded(c.Body, 64<<20)
	_ = c.Body.Close()
	if e != nil {
		return e
	}
	if contentHash(raw) != a.MarkdownSha256 {
		return ports.ErrIntegrity
	}
	docs, e := searchadapter.Chunk(ports.SearchDocument{OwnerID: source.OwnerID, SpaceID: source.SpaceID, FileID: source.FileID, VersionID: uuidText(a.SourceVersionID), ArtifactSetID: aid, SourcePath: source.Path, VersionCreatedAt: source.Version.CommittedAt.Time, Title: source.Name, Completeness: manifest.Completeness.Status, MissingPages: manifest.Completeness.MissingTextPages}, conversionBlocks(string(raw), manifest), 3072)
	if e != nil {
		return e
	}
	_, e = s.upsertSearchDocuments(ctx, docs, nil)
	return e
}

// The Meilisearch adapter accepts at most 1000 documents per request. Stable
// chunk IDs make partial retries idempotent. No artifact becomes indexed or
// published until EVERY submitted batch has successfully completed.
func (s *Service) upsertSearchDocuments(ctx context.Context, docs []ports.SearchDocument, submitted func(ports.SearchTask) error) (ports.SearchTask, error) {
	last := ports.SearchTask{UID: -1}
	if len(docs) == 0 {
		return last, ports.ErrInvalid
	}
	for offset := 0; offset < len(docs); offset += 1000 {
		task, err := s.Search.Upsert(ctx, docs[offset:min(offset+1000, len(docs))])
		if err != nil {
			return last, err
		}
		if submitted != nil {
			if err = submitted(task); err != nil {
				return task, err
			}
		}
		if err = s.Search.Wait(ctx, task); err != nil {
			return task, err
		}
		last = task
	}
	return last, nil
}

func (s *Service) reconcileUploads(ctx context.Context) error {
	if e := s.expireUnstartedUploads(ctx); e != nil {
		return e
	}
	// 原子领取即推进 retry_at，任何身份/凭据/HEAD 失败分支都不会独占队首。
	rows, e := s.DB.Query(ctx, `WITH candidates AS (
		SELECT o.id FROM file_operations o JOIN upload_sessions u ON u.operation_id=o.id
		WHERE o.kind IN ('create','overwrite') AND o.state IN ('executing','outcome_unknown','remote_confirmed')
		AND o.updated_at<now()-interval '30 seconds' AND o.retry_at<=now() AND (o.lease_until IS NULL OR o.lease_until<=now())
		ORDER BY (u.transfer_completed OR o.state='remote_confirmed') DESC,o.retry_at,o.id LIMIT 10 FOR UPDATE OF o SKIP LOCKED
	), claimed AS (
		UPDATE file_operations o SET retry_at=now()+interval '30 seconds',lease_until=now()+interval '2 minutes',lease_owner=$1,lease_token=o.lease_token+1
		FROM candidates c WHERE o.id=c.id RETURNING o.*
	) SELECT u.id::text,o.actor_principal_id::text,p.issuer,p.subject,COALESCE(u.native_upload_ref,''),u.expected_size,o.state,u.transfer_completed,o.lease_token
	FROM claimed o JOIN upload_sessions u ON u.operation_id=o.id JOIN principals p ON p.id=o.actor_principal_id`, s.Config.NodeID)
	if e != nil {
		return e
	}
	type task struct {
		upload, owner, issuer, subject, nativeURL, operationState string
		expectedSize, token                                       int64
		completed                                                 bool
	}
	tasks := []task{}
	for rows.Next() {
		var t task
		if e = rows.Scan(&t.upload, &t.owner, &t.issuer, &t.subject, &t.nativeURL, &t.expectedSize, &t.operationState, &t.completed, &t.token); e != nil {
			rows.Close()
			return e
		}
		tasks = append(tasks, t)
	}
	rows.Close()
	if e = rows.Err(); e != nil {
		return e
	}
	for _, t := range tasks {
		func() {
			reason := "upload_reconcile_retry"
			defer func() { s.releaseUploadLease(ctx, t.upload, t.token, reason) }()
			probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			status, err := s.Identity.SubjectStatus(probeCtx, t.issuer, t.subject)
			if err != nil || !status.Enabled {
				reason = "upload_identity_unavailable"
				return
			}
			actor := Actor{Principal: domain.Principal{ID: t.owner, Type: domain.ActorUser, Active: true, StatusVerified: true, Scopes: domain.AllScopes()}, Subject: ports.Subject{Issuer: t.issuer, Subject: t.subject}}
			// 持久完成证据走稳定资源；临时 tus URL 只用于仍在传输者。
			if !t.completed && t.operationState != "remote_confirmed" {
				if t.nativeURL == "" {
					reason = "upload_session_unknown"
					return
				}
				cred, err := access.New(s.deps()).Credential(probeCtx, actor, ports.RevisionRef{})
				if err != nil {
					reason = "upload_credential_unavailable"
					return
				}
				remote, err := s.Original.StatUpload(probeCtx, ports.UploadInfo{NativeURL: t.nativeURL, Length: t.expectedSize}, cred)
				if err != nil {
					reason = "upload_status_unknown"
					return
				}
				if err = s.observeUploadTransfer(ctx, t.upload, t.token, remote); err != nil {
					reason = "upload_progress_conflict"
					return
				}
				if !remote.Complete {
					reason = "upload_transfer_incomplete"
					return
				}
			}
			if _, err = s.finishUpload(ctx, actor, t.upload, t.token); err == nil {
				reason = "upload_reconciled"
			}
		}()
	}
	return nil
}

// syncPrincipalStatuses is independent of request traffic. It is NOT an access
// cache: every protected request still asks the authoritative provider. Failed
// provider reads never enable an account or reactivate a share.
func (s *Service) syncPrincipalStatuses(ctx context.Context) error {
	if e := s.syncIdentityEvents(ctx); e != nil {
		return e
	}
	if s.Identity == nil {
		return ports.ErrUnavailable
	}
	rows, e := s.DB.Query(ctx, `WITH due AS (SELECT id,updated_at FROM principals WHERE updated_at<now()-interval '1 minute' ORDER BY updated_at,id FOR UPDATE SKIP LOCKED LIMIT 50), claimed AS (UPDATE principals p SET updated_at=now() FROM due WHERE p.id=due.id RETURNING p.id,p.issuer,p.subject) SELECT c.id::text,c.issuer,c.subject FROM claimed c JOIN due d ON d.id=c.id ORDER BY d.updated_at,d.id`)
	if e != nil {
		return e
	}
	type principal struct{ id, issuer, subject string }
	principals := []principal{}
	for rows.Next() {
		var p principal
		if e = rows.Scan(&p.id, &p.issuer, &p.subject); e != nil {
			rows.Close()
			return e
		}
		principals = append(principals, p)
	}
	rows.Close()
	if e = rows.Err(); e != nil {
		return e
	}
	var firstError error
	for _, p := range principals {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Bound each provider read, not the entire preclaimed batch. One slow
		// account must not consume the deadline of every later account and
		// permanently starve their disable/share-revocation checks.
		statusCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		status, err := s.Identity.SubjectStatus(statusCtx, p.issuer, p.subject)
		cancel()
		if err != nil {
			if firstError == nil {
				firstError = err
			}
			continue
		}
		if !status.Enabled {
			_, err = s.DB.Exec(ctx, `UPDATE principals SET status='disabled',disabled_reason=CASE WHEN status='active' THEN 'identity' ELSE disabled_reason END,updated_at=now() WHERE id=$1`, p.id)
		} else {
			_, err = s.DB.Exec(ctx, `UPDATE principals SET status=CASE WHEN status='disabled' AND disabled_reason='identity' THEN 'active' ELSE status END,disabled_reason=CASE WHEN disabled_reason='identity' THEN NULL ELSE disabled_reason END,updated_at=now() WHERE id=$1`, p.id)
		}
		if err != nil && firstError == nil {
			firstError = err
		}
	}
	return firstError
}

// Only prepared admissions are provably not dispatched. Transition under the
// row lock races safely with the API's prepared->executing CAS; executing or
// unknown operations are never reclaimed on a wall-clock assumption.
func (s *Service) expireUnstartedUploads(ctx context.Context) error {
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	rows, e := tx.Query(ctx, `SELECT o.id::text,u.id::text FROM file_operations o JOIN upload_sessions u ON u.operation_id=o.id WHERE o.kind IN('create','overwrite') AND o.state='prepared' AND o.updated_at<now()-interval '10 minutes' ORDER BY o.updated_at LIMIT 20 FOR UPDATE OF o SKIP LOCKED`)
	if e != nil {
		return e
	}
	type pending struct{ op, upload string }
	todo := []pending{}
	for rows.Next() {
		var p pending
		if e = rows.Scan(&p.op, &p.upload); e != nil {
			rows.Close()
			return e
		}
		todo = append(todo, p)
	}
	rows.Close()
	if e = rows.Err(); e != nil {
		return e
	}
	for _, p := range todo {
		if _, e = tx.Exec(ctx, `UPDATE file_operations SET state='failed_confirmed',error_code='admission_expired_before_dispatch',updated_at=now() WHERE id=$1 AND state='prepared'`, p.op); e != nil {
			return e
		}
		if _, e = tx.Exec(ctx, `UPDATE files SET active_operation_id=NULL,lifecycle=CASE WHEN native_resource_id IS NULL THEN 'trashed' ELSE lifecycle END WHERE active_operation_id=$1`, p.op); e != nil {
			return e
		}
		if _, e = tx.Exec(ctx, `UPDATE upload_sessions SET state='failed_confirmed' WHERE id=$1`, p.upload); e != nil {
			return e
		}
		if _, e = tx.Exec(ctx, `SELECT release_upload_quota($1)`, p.upload); e != nil {
			return e
		}
	}
	return tx.Commit(ctx)
}
