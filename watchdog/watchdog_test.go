package watchdog

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	natstest "github.com/nats-io/nats-server/v2/test"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/prometheus/client_golang/prometheus"
)

const evalInterval = 20 * time.Millisecond

// Timeouts in checks are 100ms: at least 3x evalInterval.

type env struct {
	t        *testing.T
	nc       *nats.Conn
	checksKV jetstream.KeyValue
	stateKV  jetstream.KeyValue
}

func newEnv(t *testing.T) *env {
	t.Helper()
	opts := natstest.DefaultTestOptions
	opts.Port = -1
	opts.JetStream = true
	opts.StoreDir = t.TempDir()
	s := natstest.RunServer(&opts)
	t.Cleanup(s.Shutdown)
	return newEnvFor(t, s)
}

func newEnvFor(t *testing.T, s *server.Server) *env {
	nc, err := nats.Connect(s.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	return newEnvConn(t, nc)
}

// newEnvConn binds the buckets over nc, creating them if needed, and closes
// nc when the test ends.
func newEnvConn(t *testing.T, nc *nats.Conn) *env {
	t.Cleanup(nc.Close)
	js, _ := jetstream.New(nc)
	ctx := context.Background()
	e := &env{t: t, nc: nc}
	var err error
	if e.checksKV, err = Bucket(ctx, js, "watchdog_checks", ""); err != nil {
		t.Fatal(err)
	}
	if e.stateKV, err = Bucket(ctx, js, "watchdog_state", ""); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *env) start() *Watchdog {
	e.t.Helper()
	w := New(Config{EvalInterval: evalInterval, ChecksBucket: "watchdog_checks", StateBucket: "watchdog_state"},
		slog.New(slog.NewTextHandler(testWriter{e.t}, nil)), NewMetrics(prometheus.NewRegistry()))
	if err := w.Start(context.Background(), e.nc); err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(w.Stop)
	return w
}

func (e *env) putCheck(name, value string) {
	e.t.Helper()
	if _, err := e.checksKV.Put(context.Background(), name, []byte(value)); err != nil {
		e.t.Fatal(err)
	}
}

// state reads a state key; "" means the key does not exist.
func (e *env) state(key string) (stateJSON, uint64) {
	entry, err := e.stateKV.Get(context.Background(), key)
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return stateJSON{}, 0
	}
	if err != nil {
		e.t.Fatal(err)
	}
	var s stateJSON
	if err := json.Unmarshal(entry.Value(), &s); err != nil {
		e.t.Fatal(err)
	}
	return s, entry.Revision()
}

func (e *env) waitState(key string, want State) stateJSON {
	e.t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if s, _ := e.state(key); s.State == want {
			return s
		}
		time.Sleep(10 * time.Millisecond)
	}
	s, _ := e.state(key)
	e.t.Fatalf("%s: want state %q, have %+v", key, want, s)
	return stateJSON{}
}

// beat publishes on subject every 10ms until the returned func is called.
func (e *env) beat(subject string) (stop func()) {
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(10 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				_ = e.nc.Publish(subject, nil)
			}
		}
	}()
	return func() { close(done) }
}

const gwCheck = `{"subject":"site.*.hb","target_token":1,"timeout":"100ms","expected":["gw-02"]}`

func TestLifecycle(t *testing.T) {
	e := newEnv(t)
	e.putCheck("gw", gwCheck)
	e.start()

	stop := e.beat("site.gw-01.hb")
	e.waitState("gw.gw-01", Up)
	if s := e.waitState("gw.gw-02", Down); s.LastSeen != nil {
		t.Fatalf("gw-02 never reported, want last_seen null, got %v", s.LastSeen)
	}

	stop()
	if s := e.waitState("gw.gw-01", Down); s.LastSeen == nil {
		t.Fatal("want last_seen on a target that reported")
	}
	stop = e.beat("site.gw-01.hb")
	defer stop()
	e.waitState("gw.gw-01", Up)

	// Deleting the check purges its state.
	if err := e.checksKV.Delete(context.Background(), "gw"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		_, r1 := e.state("gw.gw-01")
		_, r2 := e.state("gw.gw-02")
		if r1 == 0 && r2 == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("state not purged after check deletion")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// And nothing re-creates it.
	time.Sleep(10 * evalInterval)
	if _, r := e.state("gw.gw-01"); r != 0 {
		t.Fatal("state re-created after purge")
	}
}

func TestRestartKeepsDownAndPurgesOrphans(t *testing.T) {
	e := newEnv(t)
	e.putCheck("gw", gwCheck)
	e.putCheck("old", `{"subject":"old.*.hb","target_token":1,"timeout":"100ms","expected":["x"]}`)
	w := e.start()
	e.waitState("gw.gw-02", Down)
	e.waitState("old.x", Down)
	w.Stop()

	// While the watchdog is down, a check is deleted.
	if err := e.checksKV.Delete(context.Background(), "old"); err != nil {
		t.Fatal(err)
	}

	_, rev := e.state("gw.gw-02")
	e.start()
	if _, r := e.state("old.x"); r != 0 {
		t.Fatal("state of a check deleted while down must be purged at startup")
	}
	time.Sleep(10 * evalInterval)
	if s, r := e.state("gw.gw-02"); s.State != Down || r != rev {
		t.Fatalf("a down target must stay down with no new write after restart, got %+v rev %d (was %d)", s, r, rev)
	}
}

func TestInvalidCheckKeepsState(t *testing.T) {
	e := newEnv(t)
	e.putCheck("gw", gwCheck)
	e.start()
	e.waitState("gw.gw-02", Down)

	e.putCheck("gw", `{"subject":"site.*.hb","timeout":"100ms"}`) // missing target_token
	time.Sleep(10 * evalInterval)
	if s, _ := e.state("gw.gw-02"); s.State != Down {
		t.Fatalf("an invalid edit must not purge state, got %+v", s)
	}
}

func TestForgetTarget(t *testing.T) {
	e := newEnv(t)
	e.putCheck("gw", gwCheck)
	w := e.start()
	e.waitState("gw.gw-02", Down)

	if err := e.stateKV.Delete(context.Background(), "gw.gw-02"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for len(w.tr.Targets("gw", "")) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("target not forgotten")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestService(t *testing.T) {
	e := newEnv(t)
	e.putCheck("gw", gwCheck)
	e.putCheck("bad", `{"subject":"a.b"}`)
	w := e.start()
	svc, err := w.AddService("0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Stop()
	e.waitState("gw.gw-02", Down)

	req := func(subject, body string) string {
		msg, err := e.nc.Request(subject, []byte(body), time.Second)
		if err != nil {
			t.Fatalf("%s: %v", subject, err)
		}
		if code := msg.Header.Get("Nats-Service-Error-Code"); code != "" {
			return "error " + code
		}
		return string(msg.Data)
	}

	if got := req("watchdog.status", `{"state":"down"}`); !strings.Contains(got, `"target":"gw-02"`) {
		t.Fatalf("status: %s", got)
	}
	if got := req("watchdog.status", `{"state":"sideways"}`); got != "error 400" {
		t.Fatalf("status with bad state: %s", got)
	}
	got := req("watchdog.checks", "")
	if !strings.Contains(got, `"name":"bad","status":"invalid"`) || !strings.Contains(got, `"name":"gw","status":"ok"`) {
		t.Fatalf("checks: %s", got)
	}
	if got := req("watchdog.validate", `{"name":"x","check":`+gwCheck+`}`); got != `{"ok":true}` {
		t.Fatalf("validate ok: %s", got)
	}
	if got := req("watchdog.validate", `{"name":"x","check":{"subject":"a.*"}}`); !strings.Contains(got, `"ok":false`) {
		t.Fatalf("validate bad: %s", got)
	}

	stats := req("$SRV.STATS.watchdog", "")
	if !strings.Contains(stats, `"checks_invalid":1`) {
		t.Fatalf("stats: %s", stats)
	}
}

func TestReady(t *testing.T) {
	e := newEnv(t)
	w := e.start()
	time.Sleep(3 * evalInterval)
	for _, r := range w.Ready() {
		if r.State != "ok" {
			t.Fatalf("want all ok, got %+v", r)
		}
	}
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimSpace(string(p)))
	return len(p), nil
}
