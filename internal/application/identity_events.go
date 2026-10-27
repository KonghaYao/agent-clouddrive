package application

import (
	"context"
	"errors"

	"github.com/agentclouddrive/agent-clouddrive/internal/ports"
	"github.com/jackc/pgx/v5"
)

// syncIdentityEvents 同时用于维护和分享入口；不能在维护尚未追平时接受旧分享。
func (s *Service) syncIdentityEvents(ctx context.Context) error {
	source, ok := s.Identity.(ports.IdentityEventSource)
	if !ok {
		return ports.ErrUnverified
	}
	rows, e := s.DB.Query(ctx, `SELECT DISTINCT issuer FROM principals ORDER BY issuer`)
	if e != nil {
		return e
	}
	var issuers []string
	for rows.Next() {
		var issuer string
		if e = rows.Scan(&issuer); e != nil {
			rows.Close()
			return e
		}
		issuers = append(issuers, issuer)
	}
	rows.Close()
	if e = rows.Err(); e != nil {
		return e
	}
	for _, issuer := range issuers {
		if e = s.syncIdentityIssuer(ctx, source, issuer); e != nil {
			return e
		}
	}
	return nil
}

func (s *Service) syncIdentityIssuer(ctx context.Context, source ports.IdentityEventSource, issuer string) error {
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('identity-events:'||$1,0))`, issuer); e != nil {
		return e
	}
	var anchor string
	e = tx.QueryRow(ctx, `SELECT anchor FROM identity_event_cursors WHERE source=$1 FOR UPDATE`, issuer).Scan(&anchor)
	if e != nil && !errors.Is(e, pgx.ErrNoRows) {
		return e
	}
	batch, fetchErr := source.IdentityEvents(ctx, issuer, anchor)
	if fetchErr != nil || batch.Anchor == "" {
		batch.Events = nil // 未完成批次不能当作已验证事件提交。
		batch.Continuous = false
		batch.Anchor = "unverified"
	}
	gap := !batch.Continuous
	if gap && anchor != "unverified" {
		// 初次接入、事件清理/丢失或设置不可信均无法证明旧授权连续性。
		if _, e = tx.Exec(ctx, `UPDATE principals SET identity_epoch=identity_epoch+1 WHERE issuer=$1`, issuer); e != nil {
			return e
		}
	}
	for _, event := range batch.Events {
		if event.ID == "" || event.Time.IsZero() {
			return ports.ErrIntegrity
		}
		result, err := tx.Exec(ctx, `INSERT INTO identity_events(source,event_id,subject,disabled,occurred_at) VALUES($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`, issuer, event.ID, event.Subject, event.Disable, event.Time)
		if err != nil {
			return err
		}
		if result.RowsAffected() == 1 && event.Disable {
			if _, e = tx.Exec(ctx, `UPDATE principals SET identity_epoch=identity_epoch+1 WHERE issuer=$1 AND (subject=$2 OR $2='')`, issuer, event.Subject); e != nil {
				return e
			}
		}
	}
	_, e = tx.Exec(ctx, `INSERT INTO identity_event_cursors(source,anchor,gap_count) VALUES($1,$2,$3) ON CONFLICT(source) DO UPDATE SET anchor=excluded.anchor,updated_at=now(),gap_count=identity_event_cursors.gap_count+excluded.gap_count`, issuer, batch.Anchor, boolInt(gap))
	if e != nil {
		return e
	}
	if e = tx.Commit(ctx); e != nil {
		return e
	}
	if fetchErr != nil {
		return fetchErr
	}
	if batch.Anchor == "unverified" {
		return ports.ErrUnverified
	}
	return nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
