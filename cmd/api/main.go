package main

import (
	"context"
	"github.com/agentclouddrive/agent-clouddrive/internal/bootstrap"
	"github.com/agentclouddrive/agent-clouddrive/internal/transport"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	svc, closeFn, e := bootstrap.Open(ctx)
	if e != nil {
		log.Fatal("startup dependency/configuration check failed; check configured services and secret files")
	}
	defer closeFn()
	api := &transport.API{Service: svc}
	mcp := &http.Server{Addr: svc.Config.MCPListen, Handler: api.MCPHandler(), ReadHeaderTimeout: 15 * time.Second, IdleTimeout: 90 * time.Second}
	go func() {
		if e := mcp.ListenAndServe(); e != nil && e != http.ErrServerClosed {
			log.Print("MCP listener failed")
			stop()
		}
	}()
	h := api.Server()
	go func() {
		if e := h.Run(); e != nil {
			log.Print("REST listener failed")
			stop()
		}
	}()
	<-ctx.Done()
	shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = mcp.Shutdown(shutdown)
	_ = h.Shutdown(shutdown)
}
