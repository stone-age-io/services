// Package health serves the Stone-Age.io process contract: GET /api/ready
// and GET /metrics, with no login.
//
// Checks run on each request. Every check a service in this repository
// reports is an in-memory read, so there is nothing to cache.
package health

import (
	"encoding/json"
	"net/http"
	"sort"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type State string

const (
	OK      State = "ok"
	Warn    State = "warn" // running, but a person should look
	Fail    State = "fail" // stop sending this process traffic
	Skipped State = "skipped"
)

// rank orders states for the top-level state. skipped ranks below ok: it
// means "no answer", so an all-skipped report does not look healthy.
var rank = map[State]int{OK: 0, Skipped: 1, Warn: 2, Fail: 3}

type Result struct {
	Name   string `json:"name"`
	State  State  `json:"state"`
	Detail string `json:"detail,omitempty"`
	Fix    string `json:"fix,omitempty"`
}

type report struct {
	Ready   bool     `json:"ready"`
	State   State    `json:"state"`
	Version string   `json:"version"`
	Uptime  string   `json:"uptime"`
	Checked string   `json:"checked"`
	Checks  []Result `json:"checks"`
}

// Handler returns a mux serving /api/ready from checks and /metrics from
// gatherer.
func Handler(version string, started time.Time, checks func() []Result, gatherer prometheus.Gatherer) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(gatherer, promhttp.HandlerOpts{}))
	mux.HandleFunc("GET /api/ready", func(w http.ResponseWriter, r *http.Request) {
		results := checks()
		sort.Slice(results, func(i, j int) bool { return results[i].Name < results[j].Name })
		rep := report{
			Ready:   true,
			Version: version,
			Uptime:  time.Since(started).Round(time.Second).String(),
			Checked: time.Now().UTC().Format(time.RFC3339),
			Checks:  results,
		}
		worst := -1
		for _, c := range results {
			if rank[c.State] > worst {
				worst, rep.State = rank[c.State], c.State
			}
			if c.State == Fail {
				rep.Ready = false
			}
		}
		if worst < 0 {
			rep.State = Skipped
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if !rep.Ready {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		_ = json.NewEncoder(w).Encode(rep)
	})
	return mux
}
