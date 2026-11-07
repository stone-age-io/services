package watchdog

import (
	"context"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	natstest "github.com/nats-io/nats-server/v2/test"
	"github.com/nats-io/nats.go"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/stone-age-io/services/internal/natsconn"
)

// newWatchdog returns a watchdog that has not been started, so a test can
// connect it with its own hooks first.
func newWatchdog(t *testing.T, logOut testWriter) *Watchdog {
	return New(Config{EvalInterval: evalInterval, ChecksBucket: "watchdog_checks", StateBucket: "watchdog_state"},
		slog.New(slog.NewTextHandler(logOut, nil)), NewMetrics(prometheus.NewRegistry()))
}

func waitConnected(t *testing.T, nc *nats.Conn) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !nc.IsConnected() {
		if time.Now().After(deadline) {
			t.Fatal("did not reconnect")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The design's most important promise: a NATS outage longer than a check's
// timeout marks nothing down, and the KV watchers keep working afterwards.
func TestReconnectCausesNoFalseDowns(t *testing.T) {
	opts := natstest.DefaultTestOptions
	opts.Port = -1
	opts.JetStream = true
	opts.StoreDir = t.TempDir()
	s := natstest.RunServer(&opts)
	port := s.Addr().(*net.TCPAddr).Port
	e := newEnvFor(t, s)
	e.putCheck("gw", `{"subject":"site.*.hb","target_token":1,"timeout":"100ms"}`)

	w := newWatchdog(t, testWriter{t})
	nc, err := natsconn.Connect(s.ClientURL(), "", "watchdog-test", w.log, natsconn.Hooks{
		Disconnected: w.Pause,
		Reconnected:  w.Resume,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	if err := w.Start(context.Background(), nc); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(w.Stop)

	// Beat on the watchdog's own connection, so the sender and the watchdog
	// lose and regain the server together.
	stop := (&env{t: t, nc: nc}).beat("site.gw-01.hb")
	defer stop()
	e.waitState("gw.gw-01", Up)
	_, rev := e.state("gw.gw-01")

	// Outage: 5x the timeout.
	s.Shutdown()
	s.WaitForShutdown()
	time.Sleep(500 * time.Millisecond)
	opts.Port = port
	s2 := natstest.RunServer(&opts)
	t.Cleanup(s2.Shutdown)
	waitConnected(t, nc)
	waitConnected(t, e.nc)

	time.Sleep(20 * evalInterval)
	if st, r := e.state("gw.gw-01"); st.State != Up || r != rev {
		t.Fatalf("gw-01 must stay up with no new write across the outage, got %+v rev %d (was %d)", st, r, rev)
	}

	// The checks watcher survives the restart. nats.go's ordered consumer
	// recreates itself after it misses heartbeats, about 15-20s after a
	// server restart, and resumes from the last sequence it delivered: a
	// change made in that window is delayed, never lost.
	e.putCheck("new", `{"subject":"new.*.hb","target_token":1,"timeout":"100ms","expected":["x"]}`)
	deadline := time.Now().Add(30 * time.Second)
	for {
		if st, _ := e.state("new.x"); st.State == Down {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("a check added after the restart was not picked up within 30s")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// permWriter logs like testWriter and fails the test on any permissions
// violation the client reports.
type permWriter struct{ t *testing.T }

func (w permWriter) Write(p []byte) (int, error) {
	line := strings.TrimSpace(string(p))
	w.t.Log(line)
	if strings.Contains(strings.ToLower(line), "permissions violation") {
		w.t.Errorf("permissions violation: %s", line)
	}
	return len(p), nil
}

// watchdogPermissions is the least a watchdog user needs, for the default
// bucket names and checks under site.>. docs/watchdog.md copies this list;
// keep the two in step.
var watchdogPermissions = &server.Permissions{
	Publish: &server.SubjectPermission{Allow: []string{
		"$JS.API.INFO",
		"$JS.API.STREAM.INFO.KV_watchdog_checks",
		"$JS.API.STREAM.INFO.KV_watchdog_state",
		"$JS.API.STREAM.CREATE.KV_watchdog_checks",
		"$JS.API.STREAM.CREATE.KV_watchdog_state",
		"$JS.API.CONSUMER.CREATE.KV_watchdog_checks.>",
		"$JS.API.CONSUMER.CREATE.KV_watchdog_state.>",
		"$JS.API.CONSUMER.DELETE.KV_watchdog_checks.>",
		"$JS.API.CONSUMER.DELETE.KV_watchdog_state.>",
		"$JS.FC.KV_watchdog_checks.>",
		"$JS.FC.KV_watchdog_state.>",
		"$KV.watchdog_state.>",
	}},
	Subscribe: &server.SubjectPermission{Allow: []string{
		"_INBOX.>",
		"watchdog.>",
		"$SRV.>",
		"site.>",
	}},
	Response: &server.ResponsePermission{MaxMsgs: 1},
}

// The documented permissions are enough for everything the watchdog does:
// creating its buckets, loading checks, writing and purging state, and
// serving its endpoints.
func TestLeastPrivilegePermissions(t *testing.T) {
	opts := natstest.DefaultTestOptions
	opts.Port = -1
	opts.JetStream = true
	opts.StoreDir = t.TempDir()
	opts.Users = []*server.User{
		{Username: "admin", Password: "admin"},
		{Username: "watchdog", Password: "watchdog", Permissions: watchdogPermissions},
	}
	s := natstest.RunServer(&opts)
	t.Cleanup(s.Shutdown)

	// The watchdog starts first, so it is the one that creates the buckets.
	w := newWatchdog(t, testWriter{t})
	nc, err := nats.Connect(s.ClientURL(), nats.UserInfo("watchdog", "watchdog"),
		nats.ErrorHandler(func(_ *nats.Conn, _ *nats.Subscription, err error) { _, _ = permWriter{t}.Write([]byte(err.Error())) }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	if err := w.Start(context.Background(), nc); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(w.Stop)
	svc, err := w.AddService("0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Stop() })

	admin, err := nats.Connect(s.ClientURL(), nats.UserInfo("admin", "admin"))
	if err != nil {
		t.Fatal(err)
	}
	e := newEnvConn(t, admin)

	e.putCheck("gw", gwCheck)
	stop := e.beat("site.gw-01.hb")
	e.waitState("gw.gw-01", Up)
	stop()
	e.waitState("gw.gw-02", Down)

	for _, subject := range []string{"watchdog.status", "watchdog.checks", "$SRV.PING.watchdog"} {
		msg, err := admin.Request(subject, nil, time.Second)
		if err != nil {
			t.Fatalf("%s: %v", subject, err)
		}
		if code := msg.Header.Get("Nats-Service-Error-Code"); code != "" {
			t.Fatalf("%s: error %s", subject, code)
		}
	}

	if err := e.checksKV.Delete(context.Background(), "gw"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, r := e.state("gw.gw-02"); r == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("state not purged with least-privilege permissions")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
