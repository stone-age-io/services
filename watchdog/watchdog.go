// Package watchdog reports when targets stop sending messages.
//
// It subscribes to the subjects named by checks in a KV bucket, keeps a map of
// when each target was last seen, evaluates the map on a ticker, and writes
// state changes to a second KV bucket. See ADR 0005 in platform-docs.
package watchdog

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/stone-age-io/services/internal/health"
)

type Config struct {
	EvalInterval time.Duration
	ChecksBucket string
	StateBucket  string
}

// checkEntry is one key in the checks bucket, valid or not.
type checkEntry struct {
	check *Check // nil when invalid
	err   error
	sub   *nats.Subscription
}

type Watchdog struct {
	cfg Config
	log *slog.Logger
	m   *Metrics
	tr  *Tracker

	nc       *nats.Conn
	checksKV jetstream.KeyValue
	stateKV  jetstream.KeyValue

	// mu guards checks, and serialises everything that writes the state
	// bucket, so a tick can never re-create a key a check removal just purged.
	mu     sync.Mutex
	checks map[string]*checkEntry

	paused   atomic.Bool
	bound    atomic.Bool
	lastEval atomic.Int64 // unix nanoseconds
	rejected sync.Map     // "check/reason" already logged

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func New(cfg Config, log *slog.Logger, m *Metrics) *Watchdog {
	return &Watchdog{cfg: cfg, log: log, m: m, tr: NewTracker(), checks: map[string]*checkEntry{}}
}

// Start binds the buckets, loads state then checks, purges state left behind
// by checks deleted while the watchdog was down, and starts the watchers and
// the ticker. It returns once the initial load is done.
func (w *Watchdog) Start(ctx context.Context, nc *nats.Conn) error {
	w.nc = nc
	js, err := jetstream.New(nc)
	if err != nil {
		return err
	}
	if w.checksKV, err = Bucket(ctx, js, w.cfg.ChecksBucket, "watchdog checks, one key per check"); err != nil {
		return err
	}
	if w.stateKV, err = Bucket(ctx, js, w.cfg.StateBucket, "watchdog state, one key per check.target"); err != nil {
		return err
	}
	w.bound.Store(true)

	runCtx, cancel := context.WithCancel(context.Background())
	w.cancel = cancel

	stateW, err := w.stateKV.WatchAll(runCtx)
	if err != nil {
		cancel()
		return fmt.Errorf("watch %s: %w", w.cfg.StateBucket, err)
	}
	now := time.Now()
	restored := 0
	for e := range stateW.Updates() {
		if e == nil {
			break // initial values done
		}
		if e.Operation() != jetstream.KeyValuePut {
			continue
		}
		t, err := fromJSON(e.Value())
		if err != nil || stateKey(t.Check, t.Target) != e.Key() {
			w.log.Warn("skipping unreadable state key", "key", e.Key(), "error", err)
			continue
		}
		w.tr.Restore(t, now)
		restored++
	}

	checksW, err := w.checksKV.WatchAll(runCtx)
	if err != nil {
		cancel()
		return fmt.Errorf("watch %s: %w", w.cfg.ChecksBucket, err)
	}
	for e := range checksW.Updates() {
		if e == nil {
			break
		}
		if e.Operation() == jetstream.KeyValuePut {
			w.applyCheck(e.Key(), e.Value())
		}
	}

	// State whose check key no longer exists at all is purged. State of an
	// invalid check is kept until the check is fixed or deleted.
	for _, name := range w.tr.Checks() {
		w.mu.Lock()
		_, exists := w.checks[name]
		w.mu.Unlock()
		if !exists {
			w.removeCheck(ctx, name)
		}
	}
	w.log.Info("watchdog started", "restored_targets", restored, "checks", len(w.checks))

	w.wg.Add(3)
	go w.watchChecks(runCtx, checksW)
	go w.watchState(runCtx, stateW)
	go w.tickLoop(runCtx)
	return nil
}

// Stop stops the watchers, the ticker and every subscription.
func (w *Watchdog) Stop() {
	if w.cancel != nil {
		w.cancel()
	}
	w.wg.Wait()
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, e := range w.checks {
		if e.sub != nil {
			_ = e.sub.Unsubscribe()
		}
	}
}

// Pause stops evaluation. It is called on disconnect.
func (w *Watchdog) Pause() { w.paused.Store(true) }

// Resume gives every target a fresh grace period, then resumes evaluation.
// It is called on reconnect.
func (w *Watchdog) Resume() {
	w.tr.ResetGrace(time.Now())
	w.paused.Store(false)
}

func (w *Watchdog) watchChecks(ctx context.Context, kw jetstream.KeyWatcher) {
	defer w.wg.Done()
	defer kw.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case e, ok := <-kw.Updates():
			if !ok {
				return
			}
			if e == nil {
				continue
			}
			if e.Operation() == jetstream.KeyValuePut {
				w.applyCheck(e.Key(), e.Value())
			} else {
				w.removeCheck(ctx, e.Key())
			}
		}
	}
}

// watchState drops a target from memory when its key is deleted or purged.
// Puts are the watchdog's own writes and are ignored.
func (w *Watchdog) watchState(ctx context.Context, kw jetstream.KeyWatcher) {
	defer w.wg.Done()
	defer kw.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case e, ok := <-kw.Updates():
			if !ok {
				return
			}
			if e == nil || e.Operation() == jetstream.KeyValuePut {
				continue
			}
			if c, t, ok := splitStateKey(e.Key()); ok {
				w.mu.Lock()
				w.tr.Forget(c, t)
				w.mu.Unlock()
			}
		}
	}
}

func (w *Watchdog) applyCheck(name string, data []byte) {
	c, err := ParseCheck(name, data, w.cfg.EvalInterval)

	w.mu.Lock()
	defer w.mu.Unlock()
	e := w.checks[name]
	if e == nil {
		e = &checkEntry{}
		w.checks[name] = e
	}
	if e.sub != nil {
		_ = e.sub.Unsubscribe()
		e.sub = nil
	}
	if err != nil {
		e.check, e.err = nil, err
		w.tr.UnloadCheck(name)
		w.log.Warn("invalid check", "check", name, "error", err)
		return
	}

	w.tr.SetCheck(c, time.Now())
	sub, err := w.nc.Subscribe(c.Subject, func(msg *nats.Msg) {
		target := c.Target(msg.Subject)
		if reason := w.tr.Seen(c.Name, target, time.Now()); reason != "" {
			w.m.rejected.WithLabelValues(c.Name, reason).Inc()
			if _, logged := w.rejected.LoadOrStore(c.Name+"/"+reason, true); !logged {
				w.log.Warn("target rejected; further rejections are only counted", "check", c.Name, "target", target, "reason", reason)
			}
		}
	})
	if err != nil {
		e.check, e.err = nil, fmt.Errorf("subscribe %q: %w", c.Subject, err)
		w.tr.UnloadCheck(name)
		w.log.Error("check not loaded", "check", name, "error", e.err)
		return
	}
	e.check, e.err, e.sub = &c, nil, sub
	w.log.Info("check loaded", "check", name, "subject", c.Subject, "timeout", c.Timeout)
}

// removeCheck unsubscribes, forgets a check's targets and purges its state.
func (w *Watchdog) removeCheck(ctx context.Context, name string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if e := w.checks[name]; e != nil && e.sub != nil {
		_ = e.sub.Unsubscribe()
	}
	delete(w.checks, name)
	w.tr.RemoveCheck(name)

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	n, err := purgeCheckState(ctx, w.stateKV, name)
	if err != nil {
		w.log.Error("purging state of removed check", "check", name, "error", err)
		return
	}
	w.log.Info("check removed", "check", name, "purged_keys", n)
}

func (w *Watchdog) tickLoop(ctx context.Context) {
	defer w.wg.Done()
	t := time.NewTicker(w.cfg.EvalInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if !w.paused.Load() {
				w.tick(ctx)
			}
		}
	}
}

func (w *Watchdog) tick(ctx context.Context) {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := time.Now()

	changed, dirty := w.tr.Evaluate(now)
	for _, t := range changed {
		w.m.transitions.WithLabelValues(t.Check, string(t.State)).Inc()
		w.log.Info("state change", "check", t.Check, "target", t.Target, "state", t.State, "last_seen", t.LastSeen)
	}
	for _, t := range dirty {
		wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := putState(wctx, w.stateKV, t)
		cancel()
		if err != nil {
			w.m.writeErrors.Inc()
			w.log.Warn("state write failed; retrying next tick", "check", t.Check, "target", t.Target, "error", err)
			continue
		}
		w.tr.Written(t)
	}

	w.m.targets.Reset()
	for c, states := range w.tr.Counts() {
		for s, n := range states {
			w.m.targets.WithLabelValues(c, string(s)).Set(float64(n))
		}
	}
	ok, invalid := w.checkCounts()
	w.m.checks.WithLabelValues("ok").Set(float64(ok))
	w.m.checks.WithLabelValues("invalid").Set(float64(invalid))
	w.m.lastEval.Set(float64(now.Unix()))
	w.lastEval.Store(now.UnixNano())
}

// checkCounts must be called with mu held.
func (w *Watchdog) checkCounts() (ok, invalid int) {
	for _, e := range w.checks {
		if e.check != nil {
			ok++
		} else {
			invalid++
		}
	}
	return ok, invalid
}

// Ready returns the readiness checks for /api/ready.
func (w *Watchdog) Ready() []health.Result {
	var out []health.Result

	switch {
	case w.nc == nil:
		out = append(out, health.Result{Name: "nats", State: health.Fail, Detail: "not connected yet"})
	case !w.nc.IsConnected():
		out = append(out, health.Result{Name: "nats", State: health.Fail, Detail: "disconnected: " + w.nc.Status().String(),
			Fix: "Check NATS_URL and that the server is reachable"})
	default:
		out = append(out, health.Result{Name: "nats", State: health.OK, Detail: "connected to " + w.nc.ConnectedUrlRedacted()})
	}

	if w.bound.Load() {
		out = append(out, health.Result{Name: "buckets", State: health.OK, Detail: w.cfg.ChecksBucket + ", " + w.cfg.StateBucket})
	} else {
		out = append(out, health.Result{Name: "buckets", State: health.Fail, Detail: "not bound yet"})
	}

	w.mu.Lock()
	var bad []string
	for name, e := range w.checks {
		if e.check == nil {
			bad = append(bad, name)
		}
	}
	total := len(w.checks)
	w.mu.Unlock()
	if len(bad) > 0 {
		sort.Strings(bad)
		out = append(out, health.Result{Name: "checks", State: health.Warn,
			Detail: fmt.Sprintf("%d of %d invalid: %s", len(bad), total, strings.Join(bad, ", ")),
			Fix:    "nats req watchdog.checks '' shows each error"})
	} else {
		out = append(out, health.Result{Name: "checks", State: health.OK, Detail: fmt.Sprintf("%d loaded", total)})
	}

	last := w.lastEval.Load()
	switch age := time.Since(time.Unix(0, last)); {
	case last == 0:
		out = append(out, health.Result{Name: "evaluation", State: health.Fail, Detail: "not evaluated yet"})
	case age > 3*w.cfg.EvalInterval:
		out = append(out, health.Result{Name: "evaluation", State: health.Fail,
			Detail: fmt.Sprintf("last evaluation %s ago", age.Round(time.Second))})
	default:
		out = append(out, health.Result{Name: "evaluation", State: health.OK,
			Detail: fmt.Sprintf("last evaluation %s ago", age.Round(time.Millisecond))})
	}
	return out
}
