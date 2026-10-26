package main

import (
	"context"
	"github.com/agentclouddrive/agent-clouddrive/internal/bootstrap"
	"github.com/agentclouddrive/agent-clouddrive/internal/config"
	"github.com/agentclouddrive/agent-clouddrive/internal/migrate"
	"github.com/jackc/pgx/v5/pgxpool"
	"log"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if len(os.Args) > 1 && os.Args[1] == "migrate" {
		cfg, e := config.Load()
		if e != nil {
			log.Fatal("database config missing")
		}
		pool, e := pgxpool.New(ctx, cfg.DatabaseURL)
		if e != nil {
			log.Fatal("database connection failed")
		}
		defer pool.Close()
		dir := os.Getenv("DRIVE_MIGRATIONS_DIR")
		if dir == "" {
			dir = "db/migrations"
		}
		if e = migrate.Apply(ctx, pool, dir); e != nil {
			log.Fatalf("migration failed: %v", e)
		}
		log.Print("migrations applied")
		return
	}
	svc, closeFn, e := bootstrap.Open(ctx)
	if e != nil {
		log.Fatal("maintenance startup configuration failed")
	}
	defer closeFn()
	if e = svc.RunMaintenance(ctx); e != nil && ctx.Err() == nil {
		log.Fatal("maintenance stopped; inspect outbox and operation states")
	}
}
