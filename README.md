# services

Small, account-scoped NATS services for the [Stone-Age.io](https://stone-age.io) platform.

Every service here:

- connects with one account user's **creds file** and never needs a platform credential;
- is configured by **environment variables** only;
- serves **`GET /api/ready`** and **`GET /metrics`** with no login;
- registers as a **NATS micro service**, so `nats micro ls` finds it.

Shared code lives in `internal/`: `natsconn` (connect, creds, reconnect hooks) and `health` (`/api/ready` and `/metrics`). A service never keeps its own copy of either.

The rule for what goes here: simple and easy to maintain beats clever. If rule-router, Benthos or NATS itself already does the job, it doesn't belong here.

| Service | What it does |
|---|---|
| [`watchdog`](#watchdog) | Reports when targets stop sending messages |

---

## watchdog

The design is [ADR 0005](https://github.com/stone-age-io/platform-docs/blob/main/docs/decisions/0005-watchdog-service.md) in platform-docs.

The watchdog subscribes to the subjects your checks name and remembers when it last saw each target. It evaluates that on a ticker and writes state changes to KV. Run **one instance per account**.

### Run

```bash
go build -o bin/watchdog ./cmd/watchdog
NATS_URL=nats://hub:4222 NATS_CREDS=/etc/watchdog/user.creds bin/watchdog
```

| Variable | Default | |
|---|---|---|
| `NATS_URL` | `nats://localhost:4222` | |
| `NATS_CREDS` | — | required |
| `HTTP_ADDR` | `:8080` | `/api/ready` and `/metrics` |
| `EVAL_INTERVAL` | `10s` | how often targets are evaluated |
| `CHECKS_BUCKET` | `watchdog_checks` | created if missing, never modified |
| `STATE_BUCKET` | `watchdog_state` | created if missing, never modified |

The account user needs to:

- subscribe to the subjects your checks name;
- use the two KV buckets (`$KV.watchdog_checks.>`, `$KV.watchdog_state.>`, plus the JetStream API for them);
- serve `watchdog.>` and `$SRV.>`.

### Define a check

One key per check in `watchdog_checks`. The key is the check name: letters, digits, `_` and `-`.

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
| `subject` | required | Subject to watch. Wildcards allowed. |
| `target_token` | required with a wildcard | 0-based index of the `*` token that names the target. Without a wildcard, the target is the check name. |
| `timeout` | required | A target goes `down` after this long with no message. Use about 3× the sender's interval. Minimum 3× `EVAL_INTERVAL`. |
| `expected` | optional | Targets that should exist. They start `pending` and go `down` if they never report. |
| `max_targets` | optional, default 1000 | Upper limit on tracked targets. Further targets are ignored and counted. |

- **Learned targets.** Targets not in `expected` are learned from their first message and expected from then on.
- **Live changes.** Edits apply immediately.
- **Deleting a check** purges its state.
- **Invalid checks.** A check that doesn't parse isn't loaded. `nats req watchdog.checks ''` shows why.

### Read state

`watchdog_state` holds one key per `{check}.{target}`. It is written only when the state changes:

```json
{"check": "gateways", "target": "gw-01", "state": "down",
 "since": "2026-11-07T10:15:00Z", "last_seen": "2026-11-07T10:09:58Z"}
```

`state` is `pending`, `up` or `down`. `last_seen` is `null` for a target that never reported.

- **Automation.** Trigger rule-router on `$KV.watchdog_state.>`.
- **Forgetting a target.** Delete its key: `nats kv del watchdog_state gateways.gw-01`.

### Endpoints

All endpoints are read-only. KV is the only way to change anything.

| Subject | Request | Returns |
|---|---|---|
| `watchdog.status` | optional `{"check": "...", "state": "..."}` | targets, with the live `last_seen` |
| `watchdog.checks` | empty | every check, `ok` or `invalid` with its error |
| `watchdog.validate` | `{"name": "...", "check": {...}}` | `{"ok": true}` or `{"ok": false, "errors": [...]}` |

```bash
nats micro ls
nats req watchdog.status '{"state":"down"}'
nats req watchdog.checks ''
```

### Behaviour worth knowing

- **Restarts.** A `down` target stays down, so you get no duplicate alerts. Every other target gets a full `timeout` before it can go down.
- **NATS disconnects.** Evaluation pauses. On reconnect, every target gets a full `timeout` of grace, so a network blip never marks the fleet down.
- **Time.** It comes from the watchdog's clock when a message arrives, never from the payload.
- **Who watches the watchdog.** Alert when `watchdog_last_eval_timestamp_seconds` goes stale.

## Develop

```bash
go test -race ./...
```

The integration tests run an embedded `nats-server`, so you don't need a server.
