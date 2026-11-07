package watchdog

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Metrics never carry a per-target label, so cardinality stays bounded by
// the number of checks.
type Metrics struct {
	targets     *prometheus.GaugeVec
	checks      *prometheus.GaugeVec
	transitions *prometheus.CounterVec
	lastEval    prometheus.Gauge
	rejected    *prometheus.CounterVec
	writeErrors prometheus.Counter
}

func NewMetrics(reg prometheus.Registerer) *Metrics {
	f := promauto.With(reg)
	return &Metrics{
		targets: f.NewGaugeVec(prometheus.GaugeOpts{
			Name: "watchdog_targets", Help: "Tracked targets by check and state.",
		}, []string{"check", "state"}),
		checks: f.NewGaugeVec(prometheus.GaugeOpts{
			Name: "watchdog_checks", Help: "Checks in the checks bucket by status (ok, invalid).",
		}, []string{"status"}),
		transitions: f.NewCounterVec(prometheus.CounterOpts{
			Name: "watchdog_transitions_total", Help: "State changes by check and new state.",
		}, []string{"check", "to"}),
		lastEval: f.NewGauge(prometheus.GaugeOpts{
			Name: "watchdog_last_eval_timestamp_seconds", Help: "Unix time of the last evaluation. Alert when it goes stale.",
		}),
		rejected: f.NewCounterVec(prometheus.CounterOpts{
			Name: "watchdog_targets_rejected_total", Help: "Messages whose target was not tracked, by check and reason (max_targets, invalid_key).",
		}, []string{"check", "reason"}),
		writeErrors: f.NewCounter(prometheus.CounterOpts{
			Name: "watchdog_state_write_errors_total", Help: "Failed writes to the state bucket. Each is retried on the next tick.",
		}),
	}
}
