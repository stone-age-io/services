// Command watchdog reports when targets stop sending messages. It is
// configured by environment variables only; see the README.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/stone-age-io/services/internal/health"
	"github.com/stone-age-io/services/internal/natsconn"
	"github.com/stone-age-io/services/watchdog"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "0.0.0-dev"

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("service", "watchdog")
	if err := run(log); err != nil {
		log.Error("exiting", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	natsURL := env("NATS_URL", "nats://localhost:4222")
	creds := os.Getenv("NATS_CREDS")
	httpAddr := env("HTTP_ADDR", ":8080")
	interval, err := time.ParseDuration(env("EVAL_INTERVAL", "10s"))
	if err != nil || interval <= 0 {
		return fmt.Errorf("EVAL_INTERVAL must be a positive duration, got %q", os.Getenv("EVAL_INTERVAL"))
	}
	if creds == "" {
		return errors.New("NATS_CREDS is required: the path to the account user's creds file")
	}
	cfg := watchdog.Config{
		EvalInterval: interval,
		ChecksBucket: env("CHECKS_BUCKET", "watchdog_checks"),
		StateBucket:  env("STATE_BUCKET", "watchdog_state"),
	}

	w := watchdog.New(cfg, log, watchdog.NewMetrics(prometheus.DefaultRegisterer))

	// HTTP comes up first, so /api/ready answers "not ready" during startup.
	srv := &http.Server{
		Addr:              httpAddr,
		Handler:           health.Handler(version, time.Now(), w.Ready, prometheus.DefaultGatherer),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http server", "error", err)
			os.Exit(1)
		}
	}()

	nc, err := natsconn.Connect(natsURL, creds, "watchdog", log, natsconn.Hooks{
		Disconnected: w.Pause,
		Reconnected:  w.Resume,
	})
	if err != nil {
		return fmt.Errorf("connect %s: %w", natsURL, err)
	}

	startCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	err = w.Start(startCtx, nc)
	cancel()
	if err != nil {
		nc.Close()
		return err
	}
	svc, err := w.AddService(version)
	if err != nil {
		w.Stop()
		nc.Close()
		return fmt.Errorf("micro service: %w", err)
	}
	log.Info("running", "version", version, "http", httpAddr, "eval_interval", interval)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Info("shutting down")

	_ = svc.Stop()
	w.Stop()
	_ = nc.Drain()
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(shutCtx)
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
