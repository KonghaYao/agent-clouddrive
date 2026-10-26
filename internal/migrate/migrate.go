package migrate

import (
	"context"
	"fmt"
	"github.com/jackc/pgx/v5/pgxpool"
	"os"
	"path/filepath"
	"sort"
)

func Apply(ctx context.Context, db *pgxpool.Pool, dir string) error {
	conn, e := db.Acquire(ctx)
	if e != nil {
		return e
	}
	defer conn.Release()
	if _, e = conn.Exec(ctx, `SELECT pg_advisory_lock(5104779081)`); e != nil {
		return e
	}
	defer conn.Exec(context.Background(), `SELECT pg_advisory_unlock(5104779081)`)
	if _, e = conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations(version text PRIMARY KEY,applied_at timestamptz NOT NULL DEFAULT now())`); e != nil {
		return e
	}
	files, e := filepath.Glob(filepath.Join(dir, "*.up.sql"))
	if e != nil {
		return e
	}
	sort.Strings(files)
	if len(files) == 0 {
		return fmt.Errorf("no migrations in %s", dir)
	}
	for _, path := range files {
		version := filepath.Base(path)
		var exists bool
		if e = conn.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=$1)`, version).Scan(&exists); e != nil {
			return e
		}
		if exists {
			continue
		}
		data, e := os.ReadFile(path)
		if e != nil {
			return e
		}
		tx, e := conn.Begin(ctx)
		if e != nil {
			return e
		}
		if _, e = tx.Exec(ctx, string(data)); e != nil {
			tx.Rollback(ctx)
			return fmt.Errorf("migration %s: %w", version, e)
		}
		if _, e = tx.Exec(ctx, `INSERT INTO schema_migrations(version) VALUES($1)`, version); e != nil {
			tx.Rollback(ctx)
			return e
		}
		if e = tx.Commit(ctx); e != nil {
			return e
		}
	}
	return nil
}
