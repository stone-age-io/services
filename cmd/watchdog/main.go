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

	"github.com/stone-age-io/kit/credskeeper"
	"github.com/stone-age-io/kit/health"
	"github.com/stone-age-io/kit/natsx"

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
	thingCode := os.Getenv("THING_CODE")
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

	var reg health.Registry
	w.RegisterChecks(&reg)

	// With PLATFORM_URL set, the watchdog keeps its own Thing's creds file
	// current (ADR 0009 §2). Without it, it connects with the creds file it
	// was given, as before.
	var keeper *credskeeper.Keeper
	if os.Getenv("PLATFORM_URL") != "" {
		kcfg, err := credskeeper.ConfigFromEnv(os.Getenv)
		if err != nil {
			return err
		}
		keeper, err = credskeeper.New(kcfg, log.With("component", "credskeeper"), credskeeper.NewMetrics(prometheus.DefaultRegisterer))
		if err != nil {
			return err
		}
		keeper.RegisterChecks(&reg)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// HTTP comes up first, so /api/ready answers "not ready" during startup.
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

	if keeper != nil {
		go keeper.Run(ctx)
		// nats.go reads the creds file before it dials, so a first start
		// waits for the first sync rather than fail the connect.
		if err := keeper.WaitForCreds(ctx); err != nil {
			return err
		}
	}

	nc, err := natsx.Connect(natsx.Options{
		URLs:  []string{natsURL},
		Creds: creds,
		Name:  "watchdog",
		Log:   log,
		Hooks: natsx.Hooks{Disconnected: w.Pause, Reconnected: w.Resume},
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
	if thingCode == "" {
		log.Warn("THING_CODE is not set: the discovery entry carries no Thing code, so the console cannot link this process to its Thing")
	}
	svc, err := w.AddService(version, thingCode)
	if err != nil {
		w.Stop()
		nc.Close()
		return fmt.Errorf("micro service: %w", err)
	}
	log.Info("running", "version", version, "thing_code", thingCode, "http", httpAddr, "eval_interval", interval)

	<-ctx.Done()
	log.Info("shutting down")

	_ = svc.Stop()
	w.Stop()
	_ = nc.Drain()
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

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
