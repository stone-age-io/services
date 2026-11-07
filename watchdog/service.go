package watchdog

import (
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/nats-io/nats.go/micro"
)

// AddService registers the watchdog as a NATS micro service. Its endpoints
// are read-only: KV is the only way to change anything.
func (w *Watchdog) AddService(version string) (micro.Service, error) {
	svc, err := micro.AddService(w.nc, micro.Config{
		Name:         "watchdog",
		Version:      version,
		Description:  "Reports when targets stop sending messages",
		StatsHandler: func(*micro.Endpoint) any { return w.stats() },
	})
	if err != nil {
		return nil, err
	}
	g := svc.AddGroup("watchdog")
	for name, h := range map[string]micro.HandlerFunc{
		"status":   w.handleStatus,
		"checks":   w.handleChecks,
		"validate": w.handleValidate,
	} {
		if err := g.AddEndpoint(name, h); err != nil {
			_ = svc.Stop()
			return nil, err
		}
	}
	return svc, nil
}

type statusRequest struct {
	Check string `json:"check"`
	State State  `json:"state"`
}

// handleStatus returns targets with their live last_seen, which the state
// bucket does not hold.
func (w *Watchdog) handleStatus(req micro.Request) {
	var r statusRequest
	if len(req.Data()) > 0 {
		if err := json.Unmarshal(req.Data(), &r); err != nil {
			_ = req.Error("400", "invalid JSON: "+err.Error(), nil)
			return
		}
	}
	switch r.State {
	case "", Pending, Up, Down:
	default:
		_ = req.Error("400", "state must be pending, up or down", nil)
		return
	}
	targets := []stateJSON{}
	for _, t := range w.tr.Targets(r.Check, r.State) {
		targets = append(targets, toJSON(t))
	}
	_ = req.RespondJSON(map[string]any{"targets": targets})
}

type checkStatus struct {
	Name    string `json:"name"`
	Status  string `json:"status"` // ok, invalid
	Subject string `json:"subject,omitempty"`
	Timeout string `json:"timeout,omitempty"`
	Error   string `json:"error,omitempty"`
}

func (w *Watchdog) handleChecks(req micro.Request) {
	w.mu.Lock()
	out := []checkStatus{}
	for name, e := range w.checks {
		if e.check != nil {
			out = append(out, checkStatus{Name: name, Status: "ok", Subject: e.check.Subject, Timeout: e.check.Timeout.String()})
		} else {
			out = append(out, checkStatus{Name: name, Status: "invalid", Error: e.err.Error()})
		}
	}
	w.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	_ = req.RespondJSON(map[string]any{"checks": out})
}

type validateRequest struct {
	Name  string          `json:"name"`
	Check json.RawMessage `json:"check"`
}

// handleValidate parses a check without loading it.
func (w *Watchdog) handleValidate(req micro.Request) {
	var r validateRequest
	if err := json.Unmarshal(req.Data(), &r); err != nil {
		_ = req.Error("400", `request must be {"name": "...", "check": {...}}: `+err.Error(), nil)
		return
	}
	if _, err := ParseCheck(r.Name, r.Check, w.cfg.EvalInterval); err != nil {
		_ = req.RespondJSON(map[string]any{"ok": false, "errors": strings.Split(err.Error(), "\n")})
		return
	}
	_ = req.RespondJSON(map[string]any{"ok": true})
}

type statsData struct {
	ChecksOK      int           `json:"checks_ok"`
	ChecksInvalid int           `json:"checks_invalid"`
	Targets       map[State]int `json:"targets"`
	LastEval      *time.Time    `json:"last_eval"`
}

// stats is the custom data in $SRV.STATS, the same numbers as /metrics.
func (w *Watchdog) stats() statsData {
	w.mu.Lock()
	ok, invalid := w.checkCounts()
	w.mu.Unlock()
	s := statsData{ChecksOK: ok, ChecksInvalid: invalid, Targets: map[State]int{Pending: 0, Up: 0, Down: 0}}
	for _, states := range w.tr.Counts() {
		for st, n := range states {
			s.Targets[st] += n
		}
	}
	if last := w.lastEval.Load(); last != 0 {
		t := time.Unix(0, last).UTC()
		s.LastEval = &t
	}
	return s
}
