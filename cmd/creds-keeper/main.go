// Command creds-keeper keeps one NATS creds file current for one Thing on
// the Stone-Age.io platform. It is configured by environment variables only;
// see docs/creds-keeper.md.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/stone-age-io/kit/health"

	"github.com/stone-age-io/services/credskeeper"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "0.0.0-dev"

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("service", "creds-keeper")
	if err := run(log); err != nil {
		log.Error("exiting", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := credskeeper.ConfigFromEnv(os.Getenv)
	if err != nil {
		return err
	}
	httpAddr := os.Getenv("HTTP_ADDR")
	if httpAddr == "" {
		httpAddr = ":8080"
	}

	k := credskeeper.New(cfg, log, credskeeper.NewMetrics(prometheus.DefaultRegisterer))

	var reg health.Registry
	k.RegisterChecks(&reg)
	srv := &http.Server{
		Addr:              httpAddr,
		Handler:           httpHandler(&reg, os.Getenv("METRICS_TOKEN")),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http server", "error", err)
			os.Exit(1)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	log.Info("running", "version", version, "http", httpAddr, "platform", cfg.URL,
		"identity", cfg.Identity, "creds", cfg.CredsFile, "sync_interval", cfg.SyncInterval,
		"password_set", cfg.Password != "")
	k.Run(ctx)
	log.Info("shutting down")

	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(shutCtx)
}

// httpHandler serves the process contract. The checks run on each request:
// each only reads memory, so a cached prober would save nothing (ADR 0005,
// As implemented, item 3).
func httpHandler(reg *health.Registry, metricsToken string) http.Handler {
	started := time.Now()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/ready", func(w http.ResponseWriter, r *http.Request) {
		health.WriteJSON(w, reg.Run(r.Context(), version, time.Since(started)))
	})
	mux.Handle("GET /metrics", health.MetricsHandler(prometheus.DefaultGatherer, metricsToken))
	return mux
}
