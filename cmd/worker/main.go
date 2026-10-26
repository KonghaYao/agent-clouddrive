package main

import (
	"context"
	"github.com/agentclouddrive/agent-clouddrive/internal/bootstrap"
	"log"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	svc, closeFn, e := bootstrap.Open(ctx)
	if e != nil {
		log.Fatal("worker startup dependency/configuration failed")
	}
	defer closeFn()
	if e = svc.RunWorker(ctx); e != nil && ctx.Err() == nil {
		log.Fatal("worker stopped; inspect durable job states")
	}
}
