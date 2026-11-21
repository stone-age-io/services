package main

import (
	"log/slog"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/stone-age-io/kit/health"
	"github.com/stone-age-io/kit/health/healthtest"
	"github.com/stone-age-io/services/watchdog"
)

// Not started, so nats, buckets and evaluation fail: a 503 that must still
// follow the wire format.
func TestReadinessConforms(t *testing.T) {
	w := watchdog.New(watchdog.Config{EvalInterval: time.Second}, slog.New(slog.DiscardHandler), watchdog.NewMetrics(prometheus.NewRegistry()))
	var reg health.Registry
	w.RegisterChecks(&reg)
	healthtest.Conform(t, httpHandler(&reg, ""))
}
