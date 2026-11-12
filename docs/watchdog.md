# Watchdog

The watchdog reports when targets stop sending messages: a gateway goes quiet, a sensor dies, a site loses its uplink. Silence produces no message, so nothing on the bus can react to it. The watchdog remembers what it expects to see and notices when it stops arriving.

It subscribes to the subjects your checks name and records when it last saw each target. A ticker evaluates those times, and state changes are written to a KV bucket. The console reads that bucket, and rule-router turns its changes into notifications.

The design and the alternatives it rejected are in [ADR 0005](decisions/0005-watchdog-service.md).

---

## 1. How It Works

```
 site.gw-01.heartbeat ──┐
 site.gw-02.heartbeat ──┼──► watchdog ──► KV watchdog_state ──► console
 site.gw-03.heartbeat ──┘       ▲              │
                                │              └──► rule-router ──► Slack, ntfy, email
                     KV watchdog_checks
```

1. **Checks** live in the `watchdog_checks` KV bucket, one key per check. Each check names a subject to watch.
2. **The watchdog subscribes** to each check's subject and records the time each target was last seen.
3. **Every `EVAL_INTERVAL`** (10s by default) it evaluates every target. A target with no message for longer than its check's `timeout` goes `down`. A message for a `down` target brings it back `up`.
4. **State changes** are written to the `watchdog_state` KV bucket. Nothing is written while a target's state stays the same.

There is one instance per organization account. It connects with that account's user creds file and needs no platform credential.

---

## 2. Run It

### Container

```bash
docker run -d --name watchdog \
  -v /etc/watchdog/user.creds:/creds/user.creds:ro \
  -e NATS_URL=nats://hub:4222 \
  -e NATS_CREDS=/creds/user.creds \
  -p 8080:8080 \
  ghcr.io/stone-age-io/watchdog:latest
```

Images are published for `linux/amd64` and `linux/arm64`. Pin a version tag in production.

### Binary

Download an archive from the [releases page](https://github.com/stone-age-io/services/releases), or build from source:

```bash
go build -o bin/watchdog ./cmd/watchdog
NATS_URL=nats://hub:4222 NATS_CREDS=/etc/watchdog/user.creds bin/watchdog
```

Run one instance under a restart policy, such as systemd's `Restart=always` or a container's `--restart unless-stopped`. Two instances in the same account would both write every state change.

### Configuration

The watchdog is configured by environment variables only.

| Variable | Default | |
|---|---|---|
| `NATS_URL` | `nats://localhost:4222` | |
| `NATS_CREDS` | none | **Required.** Path to the account user's creds file. |
| `HTTP_ADDR` | `:8080` | Serves `/api/ready` and `/metrics`. |
| `EVAL_INTERVAL` | `10s` | How often targets are evaluated. |
| `CHECKS_BUCKET` | `watchdog_checks` | |
| `STATE_BUCKET` | `watchdog_state` | |

The watchdog creates a missing bucket with one replica and never modifies an existing one. On a clustered JetStream, create the buckets yourself first with the replica count you want:

```bash
nats kv add watchdog_checks --replicas 3
nats kv add watchdog_state --replicas 3
```

You can also raise the replica count later with `nats kv edit`.

### Permissions

This is the least the watchdog's user needs, for the default bucket names and checks under `site.>`. An integration test (`TestLeastPrivilegePermissions`) runs the watchdog with exactly this list, so it stays correct.

```
publish:
  allow:
    - "$JS.API.INFO"
    - "$JS.API.STREAM.INFO.KV_watchdog_checks"
    - "$JS.API.STREAM.INFO.KV_watchdog_state"
    - "$JS.API.STREAM.CREATE.KV_watchdog_checks"
    - "$JS.API.STREAM.CREATE.KV_watchdog_state"
    - "$JS.API.CONSUMER.CREATE.KV_watchdog_checks.>"
    - "$JS.API.CONSUMER.CREATE.KV_watchdog_state.>"
    - "$JS.API.CONSUMER.DELETE.KV_watchdog_checks.>"
    - "$JS.API.CONSUMER.DELETE.KV_watchdog_state.>"
    - "$JS.FC.KV_watchdog_checks.>"
    - "$JS.FC.KV_watchdog_state.>"
    - "$KV.watchdog_state.>"
subscribe:
  allow:
    - "_INBOX.>"
    - "watchdog.>"
    - "$SRV.>"
    - "site.>"          # the subjects your checks watch
allow_responses: true
```

- **Checks on other subjects:** replace `site.>` with the subjects your checks watch.
- **Buckets created beforehand:** you can drop the two `STREAM.CREATE` lines.
- **Different bucket names:** change every `watchdog_checks` and `watchdog_state` above to match.

---

## 3. Define Checks

A check is a JSON value in `watchdog_checks`. Its key is the check's name, which may hold letters, digits, `_` and `-`.

```bash
nats kv put watchdog_checks gateways '{
  "subject": "site.*.heartbeat",
  "target_token": 1,
  "timeout": "5m",
  "expected": ["gw-01", "gw-02"]
}'
```

| Field | | |
|---|---|---|
| `subject` | required | The subject to watch. Wildcards are allowed. |
| `target_token` | required when `subject` has a wildcard | The 0-based index of the subject token that names the target. It must point at a `*` token. Without a wildcard, the check has one target, named after the check. |
| `timeout` | required | A target goes `down` after this long with no message. A Go duration such as `90s` or `5m`. |
| `expected` | optional | Targets that should exist. They start `pending` and go `down` if they never report. |
| `max_targets` | optional, default 1000 | The most targets this check tracks. |

### Choosing a timeout

Set `timeout` to about **3× the sender's interval**. A device that reports every minute gets `"3m"`. One late message then never raises an alarm, and a dead device is caught within three minutes. The timeout is the only protection against flapping, so don't set it tight.

A timeout must be at least 3× `EVAL_INTERVAL` (30s with the default interval), or the check is rejected.

### Learned and expected targets

Each message's subject names a target, through `target_token`. A target the watchdog hasn't seen before is **learned** the first time it reports, and it is expected from then on.

A learned target can't catch a device that never comes online, because there's nothing to learn from. List those in `expected`. An expected target starts `pending`. If no message arrives within `timeout`, it goes `down` with `last_seen: null`, which is how "never reported" shows up.

`max_targets` stops a mistake like a subject of `>` from learning the whole bus. When the limit is reached, new targets are ignored. The first rejection is logged, and every rejection is counted in `watchdog_targets_rejected_total`.

A target whose name holds characters a KV key can't (anything outside `A-Z a-z 0-9 - _ / =`) is skipped and counted the same way.

### Changing and removing checks

Changes take effect immediately, with no restart.

| You | The watchdog |
|---|---|
| put a new check | subscribes, and writes `pending` for each `expected` target that has no state yet |
| edit a check | resubscribes, keeps every target's state, and gives the check's targets a fresh timeout |
| delete a check | unsubscribes and **purges** all of that check's state keys |
| put an invalid check | doesn't load it, and keeps any state it already had until you fix or delete it |

Unknown fields are errors, so a misspelled `timout` is rejected rather than ignored. To check a check before writing it:

```bash
nats req watchdog.validate '{"name": "gateways", "check": {"subject": "site.*.heartbeat", "timeout": "5m"}}'
```

```json
{"ok": false, "errors": ["target_token is required when subject has a wildcard"]}
```

To see which loaded checks are invalid, and why:

```bash
nats req watchdog.checks ''
```

---

## 4. Read State

`watchdog_state` holds one key per target, named `{check}.{target}`:

```bash
nats kv get watchdog_state gateways.gw-01
```

```json
{"check": "gateways", "target": "gw-01", "state": "down",
 "since": "2026-11-07T10:15:00Z", "last_seen": "2026-11-07T10:09:58Z"}
```

| Field | |
|---|---|
| `state` | `pending` (expected, hasn't reported yet), `up` or `down` |
| `since` | When the target entered this state. |
| `last_seen` | The last message, as of the state change. `null` if the target never reported. |

Keys are written only when a target's state changes, so `last_seen` in KV is not live. For the live value, ask the watchdog:

```bash
nats req watchdog.status '{"state": "down"}'
nats req watchdog.status '{"check": "gateways"}'
```

### Forget a target

A decommissioned device would otherwise stay `down` forever. Delete its key:

```bash
nats kv del watchdog_state gateways.gw-01
```

The watchdog drops it from memory. If it reports again, it's learned again. If it's in `expected`, remove it from the check too.

---

## 5. Alert On State Changes

State changes are KV writes, so a rule-router rule can trigger on `$KV.watchdog_state.>`. The KV bucket is a JetStream stream, so the rule's consumer is durable: a rule-router that was down catches up on the changes it missed.

```yaml
- trigger:
    nats:
      subject: "$KV.watchdog_state.>"
  conditions:
    operator: or
    items:
      - field: "{state}"
        operator: eq
        value: "down"
      - field: "{state}"
        operator: eq
        value: "up"
  action:
    http:
      url: "https://ntfy.sh/your-topic"
      method: "POST"
      payload: "{check}/{target} is {state} since {since}"
      retry:
        maxAttempts: 5
        initialDelay: "1s"
        maxDelay: "30s"
```

- **Pending and purges are skipped.** `pending` matches neither condition. A purge, from a deleted check or a forgotten target, has an empty body, so neither condition matches it either.
- **Recovery included.** The rule sends both `down` and `up`, so whoever was told something went down is also told when it comes back. Drop the `up` condition if you only want failures.
- **Repeats and escalation** belong in rule-router too. See its `throttle` block and the KV alarm patterns in [Automation](https://github.com/stone-age-io/platform-docs/blob/main/docs/automation.md).

The rule-router user needs permission to create a consumer on the `KV_watchdog_state` stream.

---

## 6. Endpoints

The watchdog is a NATS micro service, so `nats micro ls` lists it, and `$SRV.PING`, `$SRV.INFO` and `$SRV.STATS` work as for any micro service. Its own endpoints are read-only. KV is the only way to change anything.

| Subject | Request | Returns |
|---|---|---|
| `watchdog.status` | optional `{"check": "...", "state": "..."}` | `{"targets": [...]}`, each with its live `last_seen` |
| `watchdog.checks` | empty | `{"checks": [...]}`, each `ok` with its subject and timeout, or `invalid` with its error |
| `watchdog.validate` | `{"name": "...", "check": {...}}` | `{"ok": true}`, or `{"ok": false, "errors": [...]}` |

A malformed request gets a standard micro error (`Nats-Service-Error-Code: 400`).

`$SRV.STATS.watchdog` carries the same totals as `/metrics` in each endpoint's `data` field: loaded and invalid checks, targets by state, and the last evaluation time. You can see them without a Prometheus server.

---

## 7. Restarts, Outages and Time

The watchdog is built so that its own problems never look like target failures.

- **On restart,** it reloads state from `watchdog_state`.
  - A target that was `down` stays down, so there's no duplicate alert.
  - Every other target gets a full `timeout` before it can go down.
  - The price is up to one `timeout` of blindness after each restart.
- **During a NATS disconnect,** evaluation pauses. On reconnect every target gets a full `timeout` of grace, so a network blip never marks the fleet down. Silence during a disconnect is the watchdog's, not the targets'.
- **After a NATS server restart,** the KV watchers take about 15–20 seconds to resubscribe, while nats.go detects the missed heartbeats. A check changed in that window takes effect late but is never lost. Target tracking isn't affected.
- **A check deleted while the watchdog was down** has its state purged when the watchdog starts.
- **Time** comes from the watchdog's own clock when a message arrives, never from the payload, so a device with a wrong clock can't affect it.
- **Failed state writes** are retried on every tick until they succeed. A tick stops writing at its first failure, so a stalled JetStream costs one write timeout (5s) per tick, not one per target. Each failure is counted in `watchdog_state_write_errors_total`.

---

## 8. Health and Metrics

### `GET /api/ready`

This follows the same contract as every Stone-Age.io binary: `200` when ready, `503` when not, with a JSON report.

| Check | Fails when |
|---|---|
| `nats` | Not connected. |
| `buckets` | The buckets aren't bound yet (startup). |
| `checks` | Never fails. It **warns** when any check is invalid and names them. |
| `evaluation` | The last evaluation is older than 3× `EVAL_INTERVAL`. |

### `GET /metrics`

| Metric | Type | Labels |
|---|---|---|
| `watchdog_targets` | gauge | `check`, `state` |
| `watchdog_checks` | gauge | `status` (`ok`, `invalid`) |
| `watchdog_transitions_total` | counter | `check`, `to` |
| `watchdog_last_eval_timestamp_seconds` | gauge | |
| `watchdog_targets_rejected_total` | counter | `check`, `reason` (`max_targets`, `invalid_key`) |
| `watchdog_state_write_errors_total` | counter | |

No metric has a per-target label, so the number of series grows with checks, not with devices.

### Watching the watchdog

A crashed or stuck watchdog detects nothing, so something outside it must watch it. With VictoriaMetrics or Prometheus:

```yaml
- alert: WatchdogStalled
  expr: time() - watchdog_last_eval_timestamp_seconds > 60
  for: 2m
- alert: WatchdogDown
  expr: up{job="watchdog"} == 0
  for: 2m
```

---

## 9. Troubleshooting

| Symptom | Likely cause |
|---|---|
| A check never loads | It's invalid. `nats req watchdog.checks ''` shows the error. |
| A target never appears | Its messages don't match the check's `subject`, the target name holds a character a KV key can't, or `max_targets` is reached. Check `watchdog_targets_rejected_total`. |
| Targets flap between `up` and `down` | `timeout` is too close to the sender's interval. Use about 3×. |
| Everything went `down` at once | The senders lost connectivity, not the watchdog: a watchdog disconnect never causes this. Check the network between the sites and the hub. |
| A removed device stays `down` | Forget it: `nats kv del watchdog_state {check}.{target}`, and remove it from `expected`. |
| `Permissions Violation` in the log | The user is missing a subject from [Permissions](#permissions). |
