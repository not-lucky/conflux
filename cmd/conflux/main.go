// Command conflux is the entry point for the Conflux LLM gateway v3.0.
//
// conflux loads config.yaml, builds the runtime through the app composition
// root, wires the management dashboard into the server, and runs the HTTP
// server until it receives SIGINT or SIGTERM.
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/not-lucky/conflux/internal/app"
	"github.com/not-lucky/conflux/internal/config"
	"github.com/not-lucky/conflux/internal/dashboard"
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

	// Management dashboard: reads the live runtime snapshot, the stable
	// metrics/tracer, and a reload closure that re-reads config.yaml and
	// rebuilds the runtime, then publishes the new snapshot to both the
	// dashboard (via its store) and the server (via SetLive). Mounted at
	// /_dashboard/ and gated by server.admin_token inside the dashboard.
	dash := dashboard.New(a.Live, a.Metrics, a.Tracer, func() error {
		if err := a.Reload(cfgPath); err != nil {
			return err
		}
		srv.SetLive(a.Live.Load())
		return nil
	})
	// Mount the dashboard at /_dashboard/, stripping the prefix so the
	// dashboard mux matches on the suffix (e.g. /keys/reset) while the
	// browser-facing links and redirects keep the full /_dashboard/... path.
	srv.Dashboard = http.StripPrefix("/_dashboard", dash.Routes())

	addr := ":" + strconv.Itoa(cfg.Server.Port)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Start the persistence flusher in the background.
	go a.Store.StartFlusher(ctx)

	log.Printf("conflux v%s listening on %s", version.Version, addr)
	if cfg.Server.AdminToken != "" {
		log.Printf("dashboard at http://%s/_dashboard/ (admin token required)", addr)
	} else {
		log.Printf("dashboard disabled (set server.admin_token to enable /_dashboard)")
	}
	if err := srv.Serve(ctx, addr); err != nil {
		log.Fatalf("serve: %v", err)
	}
	a.Store.Stop()
}
