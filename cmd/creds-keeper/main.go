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

	"github.com/stone-age-io/services/credskeeper"
	"github.com/stone-age-io/services/internal/health"
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

	srv := &http.Server{
		Addr:              httpAddr,
		Handler:           health.Handler(version, time.Now(), k.Ready, prometheus.DefaultGatherer),
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
