package application

import (
	"context"
	"errors"
	"time"

	"github.com/agentclouddrive/agent-clouddrive/internal/domain"
	"github.com/agentclouddrive/agent-clouddrive/internal/ports"
	"github.com/jackc/pgx/v5"
)

const (
	markdownPageBytes = int64(64 << 10)
	// readCursorTTL bounds how long a continuation can be resumed. Cursors are
	// cheap rows and expired rows are reclaimed opportunistically on creation.
	readCursorTTL    = time.Hour
	readCursorFormat = 1
)

type readCursor struct {
	PrincipalID     string
	FileID          string
	SourceVersionID string
	ArtifactSetID   string
	Scope           string
	Offset          int64
	ExpiresAt       time.Time
}

func invalidReadCursor() error {
	return domain.NewError(domain.InvalidArgument, "invalid or expired read cursor; start a new read")
}

func (s *Service) loadReadCursor(ctx context.Context, raw string) (readCursor, error) {
	var c readCursor
	if raw == "" || len(raw) > 128 {
		return c, invalidReadCursor()
	}
	var format int
	var scope string
	e := s.DB.QueryRow(ctx, `SELECT format_version,principal_id::text,file_id::text,source_version_id::text,artifact_set_id::text,read_scope,byte_offset,expires_at FROM read_cursors WHERE token_hash=$1`, digest(raw)).Scan(&format, &c.PrincipalID, &c.FileID, &c.SourceVersionID, &c.ArtifactSetID, &scope, &c.Offset, &c.ExpiresAt)
	if e != nil {
		if errors.Is(e, pgx.ErrNoRows) {
			return c, invalidReadCursor()
		}
		return c, normalizeError(e)
	}
	if format != readCursorFormat || (scope != "current" && scope != "history") || c.Offset < 0 || !time.Now().Before(c.ExpiresAt) {
		return c, invalidReadCursor()
	}
	c.Scope = scope
	return c, nil
}

func (s *Service) createReadCursor(ctx context.Context, a Actor, fileID, versionID, artifactID, scope string, offset int64) (string, error) {
	if offset < 0 || (scope != "current" && scope != "history") {
		return "", invalidReadCursor()
	}
	raw, hash, e := token()
	if e != nil {
		return "", dependency()
	}
	if _, e = s.DB.Exec(ctx, `INSERT INTO read_cursors(token_hash,format_version,principal_id,file_id,source_version_id,artifact_set_id,read_scope,byte_offset,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,now()+make_interval(secs=>$9))`, hash, readCursorFormat, a.Principal.ID, fileID, versionID, artifactID, scope, offset, int64(readCursorTTL.Seconds())); e != nil {
		return "", normalizeError(e)
	}
	// Bounded maintenance; a failure here must not fail a correct page. The
	// expiry index keeps this to a small prefix scan.
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	_, _ = s.DB.Exec(cleanup, `DELETE FROM read_cursors WHERE token_hash IN(SELECT token_hash FROM read_cursors WHERE expires_at<now() LIMIT 64)`)
	return raw, nil
}

// readMarkdownPage reads a bounded range instead of the whole artifact. It
// returns the page bytes, the exclusive end offset and the immutable object
// size. The page never splits a UTF-8 sequence: it may be shortened by at most
// three bytes, and the next offset resumes exactly at the skipped character.
func (s *Service) readMarkdownPage(ctx context.Context, key string, offset int64) ([]byte, int64, int64, error) {
	if offset < 0 || offset > 1<<62 {
		return nil, 0, 0, domain.NewError(domain.InvalidArgument, "invalid offset")
	}
	limit := markdownPageBytes + 3
	c, e := s.Artifacts.Open(ctx, key, &ports.ByteRange{Start: offset, End: offset + limit - 1})
	if e != nil {
		if !errors.Is(e, ports.ErrInvalid) {
			return nil, 0, 0, normalizeError(e)
		}
		// The store rejects a range starting at or past EOF. Probe the total size
		// with a one-byte (or empty-object) read and answer EOF explicitly.
		total, pe := s.markdownObjectSize(ctx, key)
		if pe != nil {
			return nil, 0, 0, pe
		}
		if offset == total {
			return []byte{}, total, total, nil
		}
		return nil, 0, 0, domain.NewError(domain.InvalidArgument, "offset beyond end of artifact")
	}
	defer c.Body.Close()
	total := c.TotalSize
	if c.Size < 0 || c.Size > limit || c.Size > total-offset {
		return nil, 0, 0, dependency()
	}
	body, e := readBounded(c.Body, c.Size)
	if e != nil {
		return nil, 0, 0, normalizeError(e)
	}
	if int64(len(body)) != c.Size {
		return nil, 0, 0, dependency()
	}
	if offset > 0 && len(body) > 0 && body[0]&0xc0 == 0x80 {
		return nil, 0, 0, domain.NewError(domain.InvalidArgument, "offset is not a UTF-8 boundary")
	}
	take := len(body)
	if int64(take) > markdownPageBytes {
		take = int(markdownPageBytes)
	}
	for take > 0 && take < len(body) && body[take]&0xc0 == 0x80 {
		take--
	}
	if take == 0 && offset < total {
		return nil, 0, 0, domain.NewError(domain.InvalidArgument, "offset is not a UTF-8 boundary")
	}
	end := offset + int64(take)
	if end > total {
		return nil, 0, 0, dependency()
	}
	return body[:take], end, total, nil
}

func (s *Service) markdownObjectSize(ctx context.Context, key string) (int64, error) {
	c, e := s.Artifacts.Open(ctx, key, &ports.ByteRange{Start: 0, End: 0})
	if e == nil {
		total := c.TotalSize
		c.Body.Close()
		return total, nil
	}
	if !errors.Is(e, ports.ErrInvalid) {
		return 0, normalizeError(e)
	}
	// Empty objects reject every range, including 0-0. An un-ranged open still
	// exposes metadata without transferring a body.
	c, e = s.Artifacts.Open(ctx, key, nil)
	if e != nil {
		return 0, normalizeError(e)
	}
	total := c.TotalSize
	c.Body.Close()
	return total, nil
}
