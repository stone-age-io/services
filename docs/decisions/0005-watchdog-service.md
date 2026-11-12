# ADR 0005: A Watchdog Service for Message Absence

**Status:** Accepted — implemented in [`stone-age-io/services`](https://github.com/stone-age-io/services), with three small deviations. See [As implemented](#as-implemented).
**Date:** 2026-11-07 (implemented 2026-11-07)

> This is the first service in `stone-age-io/services`, a new repository for small, account-scoped NATS services. It is written to the grug-brained standard: when a simple design and a clever design both work, take the simple one.

---

## Context

We need to know when something **stops** sending messages: a gateway goes quiet, a sensor dies, a site loses its uplink. Silence produces no message, so nothing on the bus reacts to it. Something has to remember what it expected to see and notice when it stops arriving.

We need three outcomes:

1. **A current state per target** (`up`, `down`, `pending`) that the console can show.
2. **A state change that automation can act on.** Rule-router turns it into a notification.
3. **Detection of targets that never report at all.** A device installed but never brought online is the case most often missed.

The service has to stay inside one organization's account and use that account's user creds file, like any other tenant workload. The Control Plane has no credential in tenant accounts, and this design doesn't add one.

### What we considered and rejected

**A TSDB with absence alerts (VictoriaMetrics + vmalert).** It works for operations monitoring, and we keep it for that (see [Monitoring the watchdog](#monitoring-the-watchdog)). We rejected it for tenant liveness for three reasons:

- It makes knowing a device is offline depend on Layer 3, which is optional.
- The result lands in Alertmanager, not on the bus. It would need another hop to reach the console or rule-router.
- vmalert rules are files managed by the operator, so tenants couldn't define their own checks.

**KV per-key TTL as the timer.** Each message would recreate a key with a TTL (nats-server 2.11+, `LimitMarkerTTL` on the bucket), and the expiry marker would be the "down" event. We rejected it for four reasons:

- It can't detect a target that never reports, because a key never written never expires.
- The TTL can only be set on `Create`. `Put` and `Update` drop it, so every message becomes a delete plus a create.
- In nats.go, a watcher sees TTL expiry and a manual purge as the same `KeyValuePurge` operation. `KeyValueEntry` exposes no headers, so the two can't be told apart.
- Every source would have to write KV instead of just publishing.

**Server-side message scheduling (2.12 delayed, 2.14 recurring and subject sampling).** Each message would push back a scheduled "down" message. We rejected it for four reasons:

- Every source has to publish schedule headers.
- It gives no "came back" event.
- The timing logic is split between the server and the clients.
- It is hard to debug.

**Connect/disconnect events.** A connection being up says nothing about whether messages flow. A gateway can stay connected over a leaf node while the device behind it is dead. These events also need system-account access.

We checked nats-server 2.15.0–2.15.2 for anything new that would help: absence detection, KV watcher or marker changes, new scheduling, new advisories. There is none. 2.15 is a JetStream operations release.

---

## Decision

**A single Go binary that subscribes to subjects, keeps a map of when each target was last seen, checks the map on a ticker, and writes state changes to KV.** Checks are defined in KV. The process clock is the only timer.

### 1. One instance per account, user creds only

- **Where it runs:** one `watchdog` process per organization account, connected with a user creds file scoped to that account.
- **No HA in v1:** run one instance under a restart policy. Two instances would both write every state change. If HA becomes necessary, the obvious next step is a leader lease in KV.
- **Server version:** the service uses no server feature newer than 2.12. Run the current release (2.15.x).

### 2. Checks live in the `watchdog_checks` KV bucket

One key per check. The key is the check name, limited to `[a-zA-Z0-9_-]` so that a `{check}.{target}` state key always splits cleanly. The value is JSON:

```json
{
  "subject": "site.*.heartbeat",
  "target_token": 1,
  "timeout": "5m",
  "expected": ["gw-01", "gw-02"],
  "max_targets": 1000
}
```

| Field | Required | Meaning |
|---|:-:|---|
| `subject` | yes | Subject to subscribe to. Wildcards allowed. |
| `target_token` | if `subject` has a wildcard | 0-based index of the subject token that names the target. It must point at a `*` token. Without a wildcard, the target is the check name. |
| `timeout` | yes | A target goes `down` after this long with no message. Go duration, at least 3× the evaluation interval. Set it to about 3× the sender's interval. It is the only flap protection. |
| `expected` | no | Targets that should exist. They start `pending` and go `down` if nothing arrives within `timeout`, which is how a target that never reports is detected. |
| `max_targets` | no (default 1000) | Upper limit on tracked targets. When reached, new targets are ignored, logged once and counted. It stops a typo like `>` from learning the whole bus. |

- **Learned targets.** A target not in `expected` is added the first time a message arrives for it, and is expected from then on.
- **Invalid targets.** A target whose token holds characters KV keys don't allow (anything outside `[-/_=.a-zA-Z0-9]`) is skipped and counted.
- **Live changes.** The service watches the bucket, so changes apply without a restart:
  - **Add:** subscribe, and write `pending` for each `expected` target that has no state key yet.
  - **Edit:** replace the subscription and keep existing target state. Give that check's targets a fresh grace period (§5).
  - **Delete:** unsubscribe, drop the check's targets from memory, and **purge** every `{check}.*` key in `watchdog_state`. The console then shows no orphaned targets. Rule-router sees the purges as `PURGE` operations, not as state changes.
- **Invalid checks.** A check that fails to parse or validate isn't loaded. Its error is reported by the `watchdog.checks` endpoint and the `watchdog_checks{status="invalid"}` metric. An edit that makes a loaded check invalid unloads it, without purging its state, until it's fixed.

### 3. Subscriptions and the tracker

- **Subscriptions:** one **core NATS** subscription per check, not a JetStream consumer.
  - The handler takes the target from the subject and sets `lastSeen[target] = now` under a mutex. It does nothing else.
  - Messages missed while the service is down don't matter, because §5 covers restarts.
- **Clock:** time comes from the watchdog's own clock when the message arrives, never from the payload. Device clock skew can't affect it.
- **Evaluation:** a ticker (`EVAL_INTERVAL`, default 10s) evaluates every target:
  - `up` or `pending`, and `now - lastSeen > timeout` → `down`.
  - Any message for a `down` or `pending` target → `up`, applied on the next tick.
- **Pure tracker:** the tracker has no I/O.

  ```go
  Seen(check, target string, now time.Time)
  Evaluate(now time.Time) []Transition
  ResetGrace(now time.Time)
  ```

  Tests are table tests with explicit times. No fake-clock library.

### 4. Output: state changes written to the `watchdog_state` KV bucket

The KV write is the service's only output. There is no separate event subject.

- **Key:** `{check}.{target}`.
- **History:** `history: 1`.
- **Value:**

  ```json
  {"check": "gateways", "target": "gw-01", "state": "down",
   "since": "2026-11-07T10:15:00Z", "last_seen": "2026-11-07T10:09:58Z"}
  ```

  `last_seen` is `null` for a target that has never reported.

Why KV:

- **Current state is always readable:** `kv get` returns it, and an event stream doesn't.
- **Durable and replayable:** a console or rule-router that was offline catches up from the bucket.
- **Native consumers:** the console watches the bucket, and rule-router triggers on `$KV.watchdog_state.>` with a JetStream consumer.
- **One delete path:** `kv del` is also how an operator tells the watchdog to forget a target (§6).

Writes:

- **Changes only.** The service writes on state changes, never on each message. The live `last_seen` is served by `watchdog.status` (§7), not stored.
- **Retry.** A state change marks the target dirty, and each tick writes the dirty targets. A failed write, for example during a disconnect, is retried on the next tick. No extra retry logic is needed.
- **Timeline.** If a timeline of state changes is ever needed, add a separate stream on `$KV.watchdog_state.>` with its own retention. Don't raise KV history.

### 5. Restarts and disconnects: one rule, `ResetGrace`

| Event | What happens |
|---|---|
| Start | Load `watchdog_state`, then the checks. `down` targets stay `down`, so no duplicate events. Every other target gets `lastSeen = now`: a full `timeout` before it can go `down`. |
| NATS disconnect | Stop evaluating. Silence during a disconnect is ours, not the targets'. |
| NATS reconnect | `ResetGrace(now)`, then resume. Core subscriptions are restored by the client. |
| Check edited | `ResetGrace` for that check's targets only. |

This one rule prevents the two failures that make a watchdog unusable: marking the fleet down after a network blip, and sending duplicate alerts after a restart.

### 6. Forgetting a target

`nats kv del watchdog_state {check}.{target}`. The service watches its own state bucket. It ignores puts, which are its own writes, and drops a target from memory when its key is deleted or purged. If that target sends again, it is learned again. There is no forget API. KV is the only way to change anything.

### 7. The NATS micro service

The service registers with `micro.AddService` as `watchdog`, which gives the built-in `$SRV.PING`, `$SRV.INFO` and `$SRV.STATS`, plus their `.watchdog` and `.watchdog.<id>` forms. The default queue group `q` stays in place.

The endpoints are read-only:

| Subject | Request | Returns |
|---|---|---|
| `watchdog.status` | optional `{"check": "...", "state": "..."}` | Every matching target with `state`, `since` and the **live `last_seen`** from memory. |
| `watchdog.checks` | empty | Every key in `watchdog_checks`, marked `ok` or `invalid`, with the error. |
| `watchdog.validate` | check JSON | `ok`, or a list of validation errors. Nothing is loaded. The console and CLI use it before writing a check. *(Amended: the request is `{"name": "...", "check": {...}}`. See [As implemented](#as-implemented).)* |

- **Errors:** returned with `req.Error`, which sets the standard `Nats-Service-Error` headers.
- **Stats:** the stats handler adds targets by state, loaded and invalid checks, and the last evaluation time. These are the same numbers as `/metrics`, so the CLI and console don't need Prometheus.
- **No write endpoints.** That means no forget, silence, ack or reload. Silencing and escalation belong in rule-router.

Each account is separate, so plain `watchdog.*` subjects don't collide across tenants.

### 8. Health and metrics

The service follows the platform's contract: `GET /api/ready` and `GET /metrics`, with no login. *(Amended: readiness checks run on each request, with no background prober. See [As implemented](#as-implemented).)*

| Readiness check | Fails when |
|---|---|
| `nats` | not connected |
| `buckets` | `watchdog_checks` or `watchdog_state` can't be bound |
| `checks` | never fails. Warns when any check is invalid. |
| `evaluation` | the last evaluation is older than 3× `EVAL_INTERVAL` |

| Metric | Type | Labels |
|---|---|---|
| `watchdog_targets` | gauge | `check`, `state` |
| `watchdog_checks` | gauge | `status` (`ok`, `invalid`) |
| `watchdog_transitions_total` | counter | `check`, `to` |
| `watchdog_last_eval_timestamp_seconds` | gauge | |
| `watchdog_targets_rejected_total` | counter | `check`, `reason` (`max_targets`, `invalid_key`) |
| `watchdog_state_write_errors_total` | counter | |

Metrics never carry a per-target label, so cardinality stays bounded by the number of checks.

#### Monitoring the watchdog

The watchdog doesn't watch itself. The operator's VictoriaMetrics/vmalert does, by alerting when `watchdog_last_eval_timestamp_seconds` goes stale or the scrape fails. This is the job vmalert is good at.

### 9. Configuration and startup

The process is configured by environment variables only:

| Variable | Default |
|---|---|
| `NATS_URL` | `nats://localhost:4222` |
| `NATS_CREDS` | none, required |
| `HTTP_ADDR` | `:8080` |
| `EVAL_INTERVAL` | `10s` |
| `CHECKS_BUCKET` | `watchdog_checks` |
| `STATE_BUCKET` | `watchdog_state` |

**Buckets:** they are created if missing and never modified, the same rule as rule-router's `autoProvision`.

**Startup order:**

1. Connect.
2. Bind the buckets.
3. Load the state.
4. Load the checks.
5. Subscribe.
6. Start the ticker.
7. Start the micro service and HTTP.

**Connection settings:** connect with `nats.UserCredentials`, `nats.Name("watchdog")` and `nats.MaxReconnects(-1)`. Its disconnect and reconnect handlers drive §5. Use the `jetstream` package (`jetstream.New`), not the legacy `nats.JetStream()`. Load state and checks with `kv.WatchAll`, where a `nil` entry marks the end of the initial values. On shutdown, `nc.Drain()`.

**Consumer limit:** nats-server 2.15 defaults to 1000 consumers per stream, and KV watchers count toward it. The service uses two watchers, one per bucket.

### 10. Repository layout

```
stone-age-io/services
  go.mod
  internal/natsconn/     connect, creds, reconnect hooks
  internal/health/       /api/ready and /metrics
  watchdog/
    tracker.go           pure logic, §3
    tracker_test.go
    checks.go            parse and validate check JSON
    store.go             KV reads and writes
    service.go           micro endpoints
  cmd/watchdog/main.go
```

- **Shared code:** `internal/` holds what every service in the repository shares. A service doesn't get its own copy of connection or health code.
- **Integration tests:** they use an embedded `nats-server` from `server/test`.

---

## Consequences

- **One answer per question.** Tenants define, view and forget checks with KV alone. The console reads one bucket, and rule-router triggers on one subject.
- **Never-reported targets are detected,** which a TTL-based design couldn't do.
- **No duplicate alerts.** Restarts and network blips don't cause false or repeated alerts, at the cost of up to one `timeout` of blindness after each.
- **No HA.** A crashed watchdog detects nothing until it restarts. vmalert detects the crash.
- **In-memory tracking.** Memory grows with targets. `max_targets` bounds each check.
- **Delayed "up".** Recovery is reported on the next tick, up to `EVAL_INTERVAL` late. That's acceptable for liveness.

## Out of scope for v1

- HA or leader election.
- Per-message `last_seen` writes to KV.
- A separate event stream.
- Write endpoints: forget, silence, ack.
- Notification and escalation, which belong to rule-router.
- Payload inspection. A message's arrival is all that counts, not its content.

---

## As implemented

The watchdog shipped in [`stone-age-io/services`](https://github.com/stone-age-io/services) at commit `20b4bc2`, built on nats.go v1.55.0 and tested against an embedded nats-server 2.15.2. It follows the decision above, with three deviations, all found while building it:

1. **The default version is `0.0.0-dev`, not `dev`.** `micro.AddService` rejects a version that isn't SemVer, so a build without `-ldflags "-X main.version=..."` couldn't register the service. Release builds set a real version.
2. **`watchdog.validate` takes `{"name": "...", "check": {...}}`.** §7 says it takes the check JSON. A check's name is its KV key, so it isn't inside the JSON, and the name has its own rule (`[a-zA-Z0-9_-]+`). Without the name in the request, the endpoint couldn't validate it.
3. **Readiness runs on each request, with no cached prober.** The Control Plane caches its checks because they open connections and query the database ([Health & Metrics §2](https://github.com/stone-age-io/platform-docs/blob/main/docs/health-metrics.md)). Each of the watchdog's four checks only reads memory (connection status, an atomic flag, the check map, the last evaluation time), so a prober would add a goroutine and a cache with nothing to save. The response format and the `ok`/`warn`/`fail`/`skipped` rules are unchanged.

One known gap: if an operator deletes a target's state key while a write for that same target is in progress, the key can be recreated. This affects only a target being forgotten, and deleting the key again fixes it. Closing the gap would need more locking than it's worth.
