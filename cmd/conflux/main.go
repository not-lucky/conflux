// Command conflux is the entry point for the Conflux LLM gateway v3.0.
//
// conflux loads config.yaml, builds the runtime through the app composition
// root, and runs the HTTP server until it receives SIGINT or SIGTERM.
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/not-lucky/conflux/internal/app"
	"github.com/not-lucky/conflux/internal/config"
	"github.com/not-lucky/conflux/internal/server"
	"github.com/not-lucky/conflux/internal/version"
)

func main() {
	var cfgPath string
	flag.StringVar(&cfgPath, "config", "config.yaml", "path to config.yaml")
	flag.Parse()

	cfg, err := config.Load(cfgPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	a, err := app.Build(cfg)
	if err != nil {
		log.Fatalf("build: %v", err)
	}

	// Server seam: app supplies the proxyHealth closure so server stays
	// decoupled from proxy.
	srv := server.New(cfg, a.Registry, a.Forwarder, a.Limiter, a.Metrics, a.Tracer, a.ProxyHealthSnapshot)

	addr := ":" + strconv.Itoa(cfg.Server.Port)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Start the persistence flusher in the background.
	go a.Store.StartFlusher(ctx)

	log.Printf("conflux v%s listening on %s", version.Version, addr)
	if err := srv.Serve(ctx, addr); err != nil {
		log.Fatalf("serve: %v", err)
	}
	a.Store.Stop()
}
