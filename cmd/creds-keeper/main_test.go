package main

import (
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/stone-age-io/kit/credskeeper"
	"github.com/stone-age-io/kit/health"
	"github.com/stone-age-io/kit/health/healthtest"
)

// No sync has run, so there is no creds file: a 503 that must still follow the
// wire format.
func TestReadinessConforms(t *testing.T) {
	cfg := credskeeper.Config{
		URL:           "http://127.0.0.1:1",
		Identity:      "kit-test@things.example.com",
		CredsFile:     filepath.Join(t.TempDir(), "user.creds"),
		SyncInterval:  time.Minute,
		AllowInsecure: true,
	}
	k, err := credskeeper.New(cfg, slog.New(slog.DiscardHandler), credskeeper.NewMetrics(prometheus.NewRegistry()))
	if err != nil {
		t.Fatal(err)
	}
	var reg health.Registry
	k.RegisterChecks(&reg)
	healthtest.Conform(t, httpHandler(&reg, ""))
}
