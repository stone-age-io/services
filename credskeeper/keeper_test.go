package credskeeper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/stone-age-io/services/internal/health"
)

const (
	identity   = "watchdog-01@things.example.com"
	password   = "s3cret"
	natsUserID = "nu123"
)

// fakePlatform serves the four PocketBase routes creds-keeper uses, with the
// platform's access rules reduced to "a valid token reads its own identity".
type fakePlatform struct {
	mu      sync.Mutex
	tokens  map[string]bool
	n       int
	creds   string
	updated string
	down    bool
	calls   map[string]int
}

func newFakePlatform(t *testing.T) (*fakePlatform, *httptest.Server) {
	p := &fakePlatform{tokens: map[string]bool{}, creds: "CREDS-1", updated: "2026-11-06 10:00:00.000Z", calls: map[string]int{}}
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	return p, srv
}

func (p *fakePlatform) newToken() string {
	p.n++
	tok := fmt.Sprintf("token-%d", p.n)
	p.tokens[tok] = true
	return tok
}

func (p *fakePlatform) authReply(w http.ResponseWriter) {
	_ = json.NewEncoder(w).Encode(map[string]any{
		"token":  p.newToken(),
		"record": map[string]string{"id": "th1", "nats_user": natsUserID},
	})
}

func (p *fakePlatform) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.down {
		http.Error(w, `{"message":"down"}`, http.StatusServiceUnavailable)
		return
	}
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/api/collections/things/auth-with-password":
		p.calls["password"]++
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["identity"] != identity || body["password"] != password {
			http.Error(w, `{"message":"Failed to authenticate."}`, http.StatusBadRequest)
			return
		}
		if r.URL.Query().Get("expand") != "" {
			http.Error(w, "test: no call may expand a relation", http.StatusBadRequest)
			return
		}
		p.authReply(w)
	case r.Method == http.MethodPost && r.URL.Path == "/api/collections/things/auth-refresh":
		p.calls["refresh"]++
		if !p.tokens[r.Header.Get("Authorization")] {
			http.Error(w, `{"message":"The request requires valid record authorization token."}`, http.StatusUnauthorized)
			return
		}
		p.authReply(w)
	case r.Method == http.MethodGet && r.URL.Path == "/api/collections/nats_users/records/"+natsUserID:
		if !p.tokens[r.Header.Get("Authorization")] {
			http.Error(w, `{"message":"forbidden"}`, http.StatusForbidden)
			return
		}
		if r.URL.Query().Get("fields") == "updated" {
			p.calls["probe"]++
			_ = json.NewEncoder(w).Encode(map[string]string{"updated": p.updated})
			return
		}
		p.calls["fetch"]++
		_ = json.NewEncoder(w).Encode(map[string]string{"creds_file": p.creds, "updated": p.updated})
	default:
		http.NotFound(w, r)
	}
}

// set changes the fake's state under its lock.
func (p *fakePlatform) set(f func(p *fakePlatform)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	f(p)
}

func (p *fakePlatform) count(name string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls[name]
}

func newKeeper(t *testing.T, url, dir, pw string) *Keeper {
	t.Helper()
	cfg := Config{
		URL:          url,
		Identity:     identity,
		Password:     pw,
		CredsFile:    filepath.Join(dir, "user.creds"),
		SessionFile:  filepath.Join(dir, "platform-session.json"),
		CredsMode:    0o600,
		SyncInterval: time.Minute,
	}
	return New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), NewMetrics(prometheus.NewRegistry()))
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func check(t *testing.T, results []health.Result, name string) health.Result {
	t.Helper()
	for _, r := range results {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("no %q check in %+v", name, results)
	return health.Result{}
}

// bootstrap runs a first sync with the password, as on a first start.
func bootstrap(t *testing.T, url, dir string) {
	t.Helper()
	if err := newKeeper(t, url, dir, password).Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestBootstrapWritesCredsAndSession(t *testing.T) {
	p, srv := newFakePlatform(t)
	dir := t.TempDir()
	k := newKeeper(t, srv.URL, dir, password)

	if err := k.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, k.cfg.CredsFile); got != "CREDS-1" {
		t.Fatalf("creds file = %q", got)
	}
	if s := readFile(t, k.cfg.SessionFile); strings.Contains(s, "CREDS") || !strings.Contains(s, "token-") {
		t.Fatalf("session file must hold the token and never the credential: %s", s)
	}
	if runtime.GOOS != "windows" {
		for _, f := range []string{k.cfg.CredsFile, k.cfg.SessionFile} {
			if fi, _ := os.Stat(f); fi.Mode().Perm() != 0o600 {
				t.Fatalf("%s mode = %04o, want 0600", f, fi.Mode().Perm())
			}
		}
	}
	if p.count("password") != 1 || p.count("fetch") != 1 {
		t.Fatalf("calls = %v", p.calls)
	}
	if testutil.ToFloat64(k.m.changes) != 1 {
		t.Fatal("credential change not counted")
	}
	if got := check(t, k.Ready(), "credentials"); got.State != health.OK {
		t.Fatalf("credentials = %+v", got)
	}
}

// The routine sync renews the session without the password, and the private
// key stays on the platform while its revision is unchanged.
func TestUnchangedSyncDoesNotFetchTheKey(t *testing.T) {
	p, srv := newFakePlatform(t)
	dir := t.TempDir()
	bootstrap(t, srv.URL, dir)

	k := newKeeper(t, srv.URL, dir, "") // password removed after the first run
	for range 3 {
		if err := k.Sync(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if p.count("password") != 1 || p.count("refresh") != 3 || p.count("probe") != 4 || p.count("fetch") != 1 {
		t.Fatalf("calls = %v", p.calls)
	}
	if testutil.ToFloat64(k.m.changes) != 0 {
		t.Fatal("an unchanged credential was counted as a change")
	}
}

// Regenerate or Revoke on the platform changes the revision, and the next
// sync writes the new credential.
func TestNewRevisionRewritesTheFile(t *testing.T) {
	p, srv := newFakePlatform(t)
	dir := t.TempDir()
	bootstrap(t, srv.URL, dir)

	p.set(func(p *fakePlatform) { p.creds, p.updated = "CREDS-2", "2026-11-06 11:00:00.000Z" })
	k := newKeeper(t, srv.URL, dir, "")
	if err := k.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, k.cfg.CredsFile); got != "CREDS-2" {
		t.Fatalf("creds file = %q", got)
	}
	if testutil.ToFloat64(k.m.changes) != 1 {
		t.Fatal("change not counted")
	}
}

// Deleting the creds file alone is always safe: the session restores it
// without the password.
func TestDeletedCredsRestoredFromSession(t *testing.T) {
	p, srv := newFakePlatform(t)
	dir := t.TempDir()
	bootstrap(t, srv.URL, dir)

	k := newKeeper(t, srv.URL, dir, "")
	if err := os.Remove(k.cfg.CredsFile); err != nil {
		t.Fatal(err)
	}
	if err := k.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, k.cfg.CredsFile); got != "CREDS-1" {
		t.Fatalf("creds file = %q", got)
	}
	if p.count("password") != 1 {
		t.Fatalf("the password was used again: %v", p.calls)
	}
}

// A lapsed session (7 days offline, or the Thing deactivated) with no
// password: the sync fails, readiness says how to fix it, and the file on
// disk keeps the service in rotation.
func TestLapsedSessionWithoutPassword(t *testing.T) {
	p, srv := newFakePlatform(t)
	dir := t.TempDir()
	bootstrap(t, srv.URL, dir)
	p.set(func(p *fakePlatform) { p.tokens = map[string]bool{} })

	k := newKeeper(t, srv.URL, dir, "")
	if err := k.Sync(context.Background()); !errors.Is(err, ErrNeedPassword) {
		t.Fatalf("err = %v, want ErrNeedPassword", err)
	}
	r := k.Ready()
	if got := check(t, r, "platform"); got.State != health.Warn || !strings.Contains(got.Fix, "PLATFORM_PASSWORD") {
		t.Fatalf("platform = %+v", got)
	}
	if got := check(t, r, "credentials"); got.State != health.OK {
		t.Fatalf("credentials = %+v", got)
	}
	if testutil.ToFloat64(k.m.syncErrors) != 1 {
		t.Fatal("sync error not counted")
	}
}

func TestLapsedSessionFallsBackToPassword(t *testing.T) {
	p, srv := newFakePlatform(t)
	dir := t.TempDir()
	bootstrap(t, srv.URL, dir)
	p.set(func(p *fakePlatform) { p.tokens = map[string]bool{} })

	k := newKeeper(t, srv.URL, dir, password)
	if err := k.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p.count("password") != 2 {
		t.Fatalf("calls = %v", p.calls)
	}
}

// A platform outage is not a lapsed session: the keeper must not try the
// password, and must not tell the operator to set one.
func TestPlatformDownIsNotALapsedSession(t *testing.T) {
	p, srv := newFakePlatform(t)
	dir := t.TempDir()
	bootstrap(t, srv.URL, dir)
	p.set(func(p *fakePlatform) { p.down = true })

	k := newKeeper(t, srv.URL, dir, password)
	err := k.Sync(context.Background())
	if err == nil || errors.Is(err, ErrNeedPassword) {
		t.Fatalf("err = %v", err)
	}
	if p.count("password") != 1 {
		t.Fatalf("the password was tried during an outage: %v", p.calls)
	}
	if got := check(t, k.Ready(), "platform"); got.State != health.Warn || got.Fix != "" {
		t.Fatalf("platform = %+v", got)
	}
}

// Readiness fails until a creds file exists, so a process gated on it never
// starts without one.
func TestReadyFailsWithoutCredsFile(t *testing.T) {
	p, srv := newFakePlatform(t)
	p.set(func(p *fakePlatform) { p.down = true })
	k := newKeeper(t, srv.URL, t.TempDir(), password)

	if got := check(t, k.Ready(), "credentials"); got.State != health.Fail {
		t.Fatalf("credentials = %+v", got)
	}
	_ = k.Sync(context.Background())
	if got := check(t, k.Ready(), "credentials"); got.State != health.Fail {
		t.Fatalf("credentials = %+v", got)
	}
}

func TestNoNATSIdentity(t *testing.T) {
	// A platform whose Thing has no nats_user.
	bare := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "t", "record": map[string]string{"id": "th1"}})
	}))
	t.Cleanup(bare.Close)

	err := newKeeper(t, bare.URL, t.TempDir(), password).Sync(context.Background())
	if err == nil || !strings.Contains(err.Error(), "nats_user") {
		t.Fatalf("err = %v", err)
	}
}

func TestConfigFromEnv(t *testing.T) {
	base := map[string]string{
		"PLATFORM_URL":      "https://platform.example.com",
		"PLATFORM_IDENTITY": identity,
		"NATS_CREDS":        "/creds/user.creds",
	}
	cfg, err := ConfigFromEnv(func(k string) string { return base[k] })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SessionFile != filepath.Join("/creds", "platform-session.json") || cfg.CredsMode != 0o600 || cfg.SyncInterval != 5*time.Minute {
		t.Fatalf("defaults = %+v", cfg)
	}

	for _, tc := range []struct {
		name, key, value, wantErr string
	}{
		{"http refused", "PLATFORM_URL", "http://platform.example.com", "https"},
		{"no identity", "PLATFORM_IDENTITY", "", "PLATFORM_IDENTITY"},
		{"no creds path", "NATS_CREDS", "", "NATS_CREDS"},
		{"mode not octal", "CREDS_FILE_MODE", "rw", "CREDS_FILE_MODE"},
		{"world readable", "CREDS_FILE_MODE", "0644", "other users"},
		{"bad interval", "SYNC_INTERVAL", "0s", "SYNC_INTERVAL"},
		{"session is creds", "SESSION_FILE", "/creds/user.creds", "SESSION_FILE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{}
			for k, v := range base {
				env[k] = v
			}
			env[tc.key] = tc.value
			_, err := ConfigFromEnv(func(k string) string { return env[k] })
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.wantErr)
			}
		})
	}

	env := map[string]string{"PLATFORM_ALLOW_INSECURE": "true", "CREDS_FILE_MODE": "0640"}
	for k, v := range base {
		env[k] = v
	}
	env["PLATFORM_URL"] = "http://localhost:8090"
	cfg, err = ConfigFromEnv(func(k string) string { return env[k] })
	if err != nil || cfg.CredsMode != 0o640 {
		t.Fatalf("cfg = %+v, err = %v", cfg, err)
	}
}
