package credskeeper

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

type Metrics struct {
	lastSync   prometheus.Gauge
	syncErrors prometheus.Counter
	changes    prometheus.Counter
}

func NewMetrics(reg prometheus.Registerer) *Metrics {
	f := promauto.With(reg)
	return &Metrics{
		lastSync: f.NewGauge(prometheus.GaugeOpts{
			Name: "creds_keeper_last_sync_timestamp_seconds", Help: "Unix time of the last successful sync. Alert when it goes stale.",
		}),
		syncErrors: f.NewCounter(prometheus.CounterOpts{
			Name: "creds_keeper_sync_errors_total", Help: "Failed syncs with the platform.",
		}),
		changes: f.NewCounter(prometheus.CounterOpts{
			Name: "creds_keeper_credential_changes_total", Help: "Times the creds file was written with a new credential.",
		}),
	}
}
