// Package credskeeper keeps one NATS creds file current for one Thing on the
// Stone-Age.io platform. See ADR 0007 in platform-docs.
//
// It talks only HTTPS to the platform, never NATS. Every sync does the same
// three steps, which is also how it bootstraps:
//
//  1. Authenticate: auth-refresh with the saved session token. Only when the
//     platform rejects the token, sign in with the password, if one is set.
//  2. Probe: read only the credential's `updated` time (?fields=updated).
//  3. Fetch: read the full credential and write the creds file, but only
//     when the revision changed or the file is missing.
//
// So the private key crosses the network only when it changed. No call
// expands a relation: PocketBase doesn't apply ?fields= to auth responses,
// so an auth call with expand=nats_user would return the key every time.
package credskeeper

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/stone-age-io/services/internal/health"
)

// The platform's schema: a Thing authenticates against `things`, and its
// NATS identity is the `nats_users` record its `nats_user` relation names.
const (
	thingsPath    = "/api/collections/things"
	natsUsersPath = "/api/collections/nats_users/records/"
)

// ErrNeedPassword means the platform rejected the saved session and no
// password is configured. Setting PLATFORM_PASSWORD for one start fixes it.
var ErrNeedPassword = errors.New("the platform session has lapsed and PLATFORM_PASSWORD is not set")

type Config struct {
	URL          string // platform base URL
	Identity     string // the Thing's login email
	Password     string // optional after the first run
	CredsFile    string
	SessionFile  string
	CredsMode    os.FileMode
	SyncInterval time.Duration
}

type Keeper struct {
	cfg  Config
	log  *slog.Logger
	m    *Metrics
	http *http.Client

	mu       sync.Mutex // guards lastSync and lastErr
	lastSync time.Time  // last successful sync
	lastErr  error      // error of the last sync, nil if it succeeded
}

func New(cfg Config, log *slog.Logger, m *Metrics) *Keeper {
	return &Keeper{cfg: cfg, log: log, m: m, http: &http.Client{Timeout: 15 * time.Second}}
}

// retryAfterFailure is when a failed sync is tried again, if SyncInterval
// is longer. It lets a first start recover quickly from a short platform
// outage, without a mode of its own.
const retryAfterFailure = 30 * time.Second

// Run syncs at once, then after every SyncInterval, until ctx is done. A
// failed sync is tried again after 30 seconds.
func (k *Keeper) Run(ctx context.Context) {
	for {
		next := k.cfg.SyncInterval
		if err := k.Sync(ctx); err != nil && retryAfterFailure < next {
			next = retryAfterFailure
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(next):
		}
	}
}

// Sync runs one sync and records its result for readiness and metrics.
func (k *Keeper) Sync(ctx context.Context) error {
	changed, err := k.sync(ctx)

	k.mu.Lock()
	k.lastErr = err
	if err == nil {
		k.lastSync = time.Now()
	}
	k.mu.Unlock()

	if err != nil {
		k.m.syncErrors.Inc()
		k.log.Warn("sync failed", "error", err)
		return err
	}
	k.m.lastSync.SetToCurrentTime()
	if changed {
		k.m.changes.Inc()
		k.log.Info("creds file written", "path", k.cfg.CredsFile)
	}
	return nil
}

func (k *Keeper) sync(ctx context.Context) (changed bool, err error) {
	prev := loadSession(k.cfg.SessionFile, k.log)

	token, natsUserID, err := k.authenticate(ctx, prev.Token)
	if err != nil {
		return false, err
	}
	if natsUserID == "" {
		return false, errors.New("the Thing has no NATS identity (nats_user) assigned")
	}

	var probe natsUser
	if err := k.get(ctx, token, natsUsersPath+natsUserID+"?fields=updated", &probe); err != nil {
		return false, fmt.Errorf("probe credential revision: %w", err)
	}

	next := session{Token: token, CredsRevision: probe.Updated}
	if probe.Updated != "" && probe.Updated == prev.CredsRevision && fileExists(k.cfg.CredsFile) {
		// Unchanged. Apply CREDS_FILE_MODE in case it was changed since the
		// file was written.
		if err := os.Chmod(k.cfg.CredsFile, k.cfg.CredsMode); err != nil {
			return false, err
		}
		return false, saveSession(k.cfg.SessionFile, next)
	}

	var user natsUser
	if err := k.get(ctx, token, natsUsersPath+natsUserID, &user); err != nil {
		return false, fmt.Errorf("read credential: %w", err)
	}
	if user.CredsFile == "" {
		return false, errors.New("the NATS identity has no credential; generate one on the platform")
	}
	next.CredsRevision = user.Updated

	changed, err = writeIfChanged(k.cfg.CredsFile, []byte(user.CredsFile), k.cfg.CredsMode)
	if err != nil {
		return false, err
	}
	return changed, saveSession(k.cfg.SessionFile, next)
}

// authenticate returns a fresh session token and the id of the Thing's NATS
// identity. It uses the password only when the platform rejects the saved
// token. When the platform can't be reached, it returns that error, so a
// platform outage is never reported as a lapsed session.
func (k *Keeper) authenticate(ctx context.Context, token string) (string, string, error) {
	if token != "" {
		t, id, err := k.auth(ctx, "/auth-refresh", token, nil)
		if err == nil {
			return t, id, nil
		}
		if !isRejected(err) {
			return "", "", fmt.Errorf("auth-refresh: %w", err)
		}
		k.log.Info("platform rejected the saved session", "error", err)
	}
	if k.cfg.Password == "" {
		return "", "", ErrNeedPassword
	}
	t, id, err := k.auth(ctx, "/auth-with-password", "",
		map[string]string{"identity": k.cfg.Identity, "password": k.cfg.Password})
	if err != nil {
		return "", "", fmt.Errorf("sign in as %s: %w", k.cfg.Identity, err)
	}
	return t, id, nil
}

type natsUser struct {
	CredsFile string `json:"creds_file"`
	Updated   string `json:"updated"`
}

func (k *Keeper) auth(ctx context.Context, path, token string, body any) (string, string, error) {
	var resp struct {
		Token  string `json:"token"`
		Record struct {
			NATSUser string `json:"nats_user"`
		} `json:"record"`
	}
	if err := k.do(ctx, http.MethodPost, thingsPath+path, token, body, &resp); err != nil {
		return "", "", err
	}
	if resp.Token == "" {
		return "", "", errors.New("the platform returned no token")
	}
	return resp.Token, resp.Record.NATSUser, nil
}

func (k *Keeper) get(ctx context.Context, token, path string, out any) error {
	return k.do(ctx, http.MethodGet, path, token, nil, out)
}

func (k *Keeper) do(ctx context.Context, method, path, token string, body, out any) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(k.cfg.URL, "/")+path, r)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", token)
	}
	resp, err := k.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// An error body is safe to show. A success body may hold the
		// private key, so it never goes into an error or a log.
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return &httpError{status: resp.StatusCode, body: strings.TrimSpace(string(msg))}
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

type httpError struct {
	status int
	body   string
}

func (e *httpError) Error() string { return fmt.Sprintf("platform returned %d: %s", e.status, e.body) }

// isRejected reports whether the platform refused the token, as opposed to
// failing to answer.
func isRejected(err error) bool {
	var he *httpError
	return errors.As(err, &he) && (he.status == http.StatusUnauthorized || he.status == http.StatusForbidden)
}

// Ready returns the readiness checks for /api/ready.
func (k *Keeper) Ready() []health.Result {
	var out []health.Result

	if fileExists(k.cfg.CredsFile) {
		out = append(out, health.Result{Name: "credentials", State: health.OK, Detail: k.cfg.CredsFile})
	} else {
		out = append(out, health.Result{Name: "credentials", State: health.Fail,
			Detail: "no creds file yet at " + k.cfg.CredsFile,
			Fix:    "See the platform check for why the first sync has not succeeded"})
	}

	k.mu.Lock()
	last, lastErr := k.lastSync, k.lastErr
	k.mu.Unlock()

	// The platform check never fails: the file on disk usually still works,
	// and a platform outage must not take services out of rotation.
	p := health.Result{Name: "platform", State: health.OK}
	switch {
	case errors.Is(lastErr, ErrNeedPassword):
		p.State, p.Detail = health.Warn, lastErr.Error()
		p.Fix = "Set PLATFORM_PASSWORD for one start. If the Thing was deactivated and reactivated, an Owner or Admin sets a new password first."
	case lastErr != nil:
		p.State, p.Detail = health.Warn, "last sync failed: "+lastErr.Error()
	case last.IsZero():
		p.State, p.Detail = health.Warn, "no sync has completed yet"
	case time.Since(last) > 3*k.cfg.SyncInterval:
		p.State, p.Detail = health.Warn, fmt.Sprintf("last successful sync %s ago", time.Since(last).Round(time.Second))
	default:
		p.Detail = fmt.Sprintf("last sync %s ago", time.Since(last).Round(time.Second))
	}
	return append(out, p)
}
