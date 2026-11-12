# ADR 0006: History Services for Metrics and Logs

**Status:** Deferred. Nothing in this ADR is built. See [Why deferred](#why-deferred).
**Date:** 2026-11-07 (deferred 2026-11-06)

> This ADR decides how the console and other NATS clients read **long-term** history: months of metrics, and later, searchable logs. It adds two small services to [`stone-age-io/services`](https://github.com/stone-age-io/services), built to the same rules as the [Watchdog (ADR 0005)](./0005-watchdog-service.md). `history-metrics` and `history-logs` are both designed here so the two match. Neither is built until a real need appears.

---

## Why deferred

The design stays here for when it is needed. We do not build it now, for three reasons:

1. **No tenant has asked for it.** `history-logs` waits for a real need. `history-metrics` must meet the same rule.
2. **It moves a boundary that the docs state.** [Observability](https://github.com/stone-age-io/platform-docs/blob/main/docs/observability.md) says the console's dashboards are for live operation, and Perses or Grafana is for history. [ADR 0004](https://github.com/stone-age-io/platform-docs/blob/main/docs/decisions/0004-long-term-data-and-location-path.md) says the same of the dashboard widgets. This ADR reverses both, and the reversal must be a decision of its own.
3. **It changes the documented TSDB layout.** [Observability](https://github.com/stone-age-io/platform-docs/blob/main/docs/observability.md) shows one shared VictoriaMetrics with no auth proxy. §2 is safe only with one database per account, or a per-account token behind vmauth. If someone points `history-metrics` at the documented shared database, one tenant can read the data of another tenant.

Until then, Perses or Grafana shows long-term metrics, and JetStream with retention keeps event history (§9).

**Before this ADR is built, do these:**

- Name the tenant need.
- Update [Observability](https://github.com/stone-age-io/platform-docs/blob/main/docs/observability.md) and [ADR 0004](https://github.com/stone-age-io/platform-docs/blob/main/docs/decisions/0004-long-term-data-and-location-path.md) to the new boundary.
- Make the documented TSDB layout one of the two layouts in §2.
- Set `MAX_POINTS` to a value that fits the 1 MB payload limit for a chart with several series. 11,000 points of Prometheus JSON is approximately 240 KB for each series, so approximately four series reach the limit. A chart is approximately 1,000 pixels wide, so a default near 1,500 is sufficient.
- Say which console widget uses the service, and what makes the PromQL.

---

## Context

The console talks only NATS. Its widgets read history by replaying a JetStream stream ([Dashboards](https://github.com/stone-age-io/platform-docs/blob/main/docs/dashboards.md)), which covers minutes to days. Long-term metrics live in the [Layer 3](https://github.com/stone-age-io/platform-docs/blob/main/docs/observability.md) time-series database, VictoriaMetrics by default, fed by Telegraf. Today the only way to see them is Grafana or Perses.

That leaves a gap. A tenant looking at a Thing in the console can see the last hour of its temperature but not the last 90 days, unless they switch to another tool that has its own login and its own idea of which data they may see.

Nothing on the bus can fill it today:

- **rule-router can't.** Its NATS responder (`reply: true` with `respond`) answers from templates and KV lookups. It can't make an HTTP call and return the result as the reply.
- **The browser can't call the database directly.** That would put a database credential in the browser, need CORS, and move the tenant boundary from NATS accounts to whatever the database's own auth allows.

Three decisions were made in discussion before this ADR:

1. **Events stay in JetStream.** Discrete, low-volume events (state changes, alarms, door events) are kept in streams with time-based retention, which the console already replays. No new service is involved.
2. **Telegraf runs per account.** Ingestion is already account-scoped.
3. **Packaging waits.** There is no appliance image yet. Each service ships as its own binary and image until it's clear what usually runs together in one account.

---

## Decision

**Two read-only request/reply services, one per backend, each running inside one account.** They forward a query to the account's own database, apply limits, and return the backend's answer unchanged.

| Service (micro name) | Backend | Endpoints | Status |
|---|---|---|---|
| `history-metrics` | Any Prometheus query API: VictoriaMetrics, Prometheus, Mimir | `history.metrics.query`, `history.metrics.query_range` | Deferred (see [Why deferred](#why-deferred)) |
| `history-logs` | VictoriaLogs | `history.logs.query` | Deferred until needed |

### 1. Account-scoped, like the Watchdog

Each service runs inside one organization account with that account's user creds file. A console user, signed in to the same account, sends requests over the connection the console already has. No import, no export, no new credential in the browser.

### 2. The database enforces tenancy, not the service

The services contain **no tenant logic**. Each is given a URL and an optional bearer token, and forwards queries there. Isolation is decided by how the database is deployed:

| Deployment | How tenants are kept apart | When |
|---|---|---|
| **One database per account** | Separate processes and separate data. Nothing to configure. | The default. A single-node VictoriaMetrics is light enough to run per account. |
| **A shared database behind an auth proxy** | Each account's Telegraf and history service get their own token. The proxy (vmauth for VictoriaMetrics) maps each token to that account's tenant, for writes and reads alike. | Many small accounts on one host. |

**A tenant that edits its own service's config still reaches only its own data,** because the token is what decides, and the token belongs to the account. The service never adds label filters or rewrites queries. A filter we inject is a filter we can get wrong. The database's auth layer already does this job.

### 3. Requests mirror the backend's own API

There is no query language or request format of our own. Request fields are the backend's HTTP query parameters, as JSON strings:

```
history.metrics.query        {"query": "...", "time": "..."}
history.metrics.query_range  {"query": "...", "start": "...", "end": "...", "step": "..."}
```

- **The fields match the Prometheus HTTP API.** `query` is PromQL (or MetricsQL on VictoriaMetrics). `time`, `start` and `end` are RFC 3339 or Unix seconds. `step` is a duration (`60s`, `5m`) or seconds.
- **The reply is the backend's JSON body unchanged:** `{"status": "success", "data": {"resultType": ..., "result": [...]}}`. Any Prometheus client library or chart code can read it.
- **Why mirror and not abstract:** PromQL is already the standard, and anyone who knows the Prometheus API knows ours. A translation layer would be a second query language to document, test and keep in step with the backend.
- **`history-logs` will do the same** with VictoriaLogs' `/select/logsql/query` parameters, passing LogsQL through. There is no standard query language for logs, so the backend is fixed there and that is accepted.

### 4. Limits protect the shared database

Each request is checked before it reaches the backend:

| Variable | Default | Check |
|---|---|---|
| `MAX_RANGE` | `2160h` (90 days) | `end - start` of a range query |
| `MAX_POINTS` | `11000` | `(end - start) / step`, the points per series. 11,000 is Prometheus's own limit. |
| `QUERY_TIMEOUT` | `30s` | Passed to the backend as `timeout` and enforced on the HTTP call |
| `MAX_CONCURRENT` | `4` | Queries in flight at once. A request beyond it gets an immediate `503` rather than waiting. |

- **Response size has a hard ceiling.** A NATS message can't exceed the server's `max_payload` (1 MB by default). A reply that would be larger is refused with an error that says to narrow the query or raise the step. It is never truncated, because a cut-off result looks like real data.
- **The database's own limits are the second guard.** VictoriaMetrics has `-search.maxQueryDuration`, `-search.maxSeries` and `-search.maxPointsPerTimeseries`, and an operator should set them as well.
- **Concurrency is explicit.** A micro endpoint handles its requests one at a time, so a single slow query would block every widget in the account. Each request runs in its own goroutine, bounded by `MAX_CONCURRENT`.

### 5. Errors

Errors use the standard micro error headers (`Nats-Service-Error`, `Nats-Service-Error-Code`):

| Code | When |
|---|---|
| `400` | Invalid request JSON, a missing `query`, or an unparseable time or step. Also a PromQL error the backend reports as `bad_data`. |
| `413` | The result exceeds the NATS payload limit. |
| `422` | The request exceeds `MAX_RANGE` or `MAX_POINTS`. The error names the limit. |
| `503` | The backend is unreachable, or `MAX_CONCURRENT` is reached. |
| `504` | The query exceeded `QUERY_TIMEOUT`. |

When the backend returned an error body, it is passed back as the error's data, so the caller sees the database's own message.

### 6. Health, metrics and micro

The same contract as every service in the repository: `GET /api/ready` and `GET /metrics` with no login, and registration as a NATS micro service.

- **Readiness:** two checks, `nats` and `backend`.
  - **How `backend` is probed:** it runs the query `1` through `/api/v1/query`. That works on any Prometheus-compatible backend, where health endpoints differ.
  - **The probe runs on each readiness request,** with a 2-second timeout, and nothing is cached. [Health & Metrics §2](https://github.com/stone-age-io/platform-docs/blob/main/docs/health-metrics.md) caches the Control Plane's checks because they open connections and query the database. One trivial query every 10 to 30 seconds, the usual orchestrator interval, doesn't need a background goroutine and a cache. The timeout keeps a slow backend from making the endpoint slow.
- **Metrics:**
  - `history_metrics_requests_total{endpoint, outcome}`
  - `history_metrics_request_duration_seconds{endpoint}`
  - `history_metrics_backend_up`

  No metric carries the query text.
- **No endpoints beyond the queries.** Health is in `/api/ready` and `$SRV.STATS`.

### 7. Configuration

| Variable | Default | |
|---|---|---|
| `NATS_URL` | `nats://localhost:4222` | |
| `NATS_CREDS` | none | Required. |
| `HTTP_ADDR` | `:8080` | |
| `METRICS_URL` | none | Required. The base URL under which `/api/v1/query` is served: `http://vm:8428` for a single-node VictoriaMetrics, or the vmauth URL for a shared one. |
| `METRICS_TOKEN` | none | Optional bearer token for the backend. |
| `MAX_RANGE`, `MAX_POINTS`, `QUERY_TIMEOUT`, `MAX_CONCURRENT` | see §4 | |

The NATS permissions are short, because the service uses no JetStream:

```
subscribe: ["history.metrics.>", "$SRV.>"]
allow_responses: true
```

A console user needs `publish` on `history.>`, which covers both services and whichever one the account runs.

### 8. Subject names

The endpoints sit under a shared `history.` prefix, not bare `metrics.` or `logs.`. Tenants often publish telemetry on `metrics.>` or `logs.>`, and a Telegraf or rule-router subscribed there would receive query requests, or a tenant subject would collide with an endpoint. `history.` marks the subjects as platform services and gives console users one permission to grant. The Watchdog keeps `watchdog.*`, which is specific enough not to collide.

### 9. Event history stays in JetStream

Discrete events need no history service. A stream with `max_age` over the event subjects keeps them, and the console replays them with *By Time Window*. One example: `watchdog_state` keeps only the current state (`history: 1`). A stream that sources `$KV.watchdog_state.>` with `max_age: 30d` gives every target a 30-day timeline of its state changes, without code.

JetStream stops being enough when the data is high-volume and needs searching by content. Scanning a stream doesn't scale. That is the point at which `history-logs` gets built.

---

## Alternatives Considered

**One service in the operator's hub account, serving every tenant.** Tenants would import `history.query`, and the operator-signed import would add the org code to the subject, as the Helpdesk's ticket intake does ([ADR 0002](https://github.com/stone-age-io/platform-docs/blob/main/docs/decisions/0002-organization-code-namespace.md)). It's one process instead of one per account. We rejected it for two reasons:

- The service would have to enforce tenancy itself by adding label filters to every query, which is exactly the logic §2 keeps out.
- It doesn't match where ingestion runs: Telegraf is per account, so the read side is too.

**One `history` service with an optional logs backend.** We rejected it because:

- It has two modes, with different endpoints and readiness depending on whether `LOGS_URL` is set.
- A logs outage would make the metrics side look half-broken.
- The two backends share almost nothing: query language, result shape, the limits that matter, and credentials all differ.

Two services each do one thing in every deployment. The shared part, NATS and health, is already in `internal/`.

**A query format of our own** (for example `{"thing": "...", "metric": "...", "range": "7d"}`). Easier for a widget to build. We rejected it because:

- It is a second query language that covers less than PromQL.
- Every new need would mean a new field.

If a simpler shape helps the console, the console can build PromQL from it.

**The browser queries the database directly.** We rejected it because:

- It puts a database credential in the browser.
- It needs CORS.
- It replaces the NATS account boundary with the database's auth as the only thing between tenants.

**Only Grafana or Perses.** They stay the tool for exploring and for building dashboards. The console needs history *in context*, beside the Thing being looked at, without a second login.

---

## Consequences

- **The console can show months of metrics** with the permissions and connection it already has.
- **The services hold no tenant logic,** so they can't get tenancy wrong. In exchange, isolation depends on deploying the database correctly: a per-account database, or a per-account token on a shared one.
- **Any Prometheus-compatible backend works,** so VictoriaMetrics is the default, not a requirement.
- **Logs are not covered until `history-logs` is built.** Until then, event history is JetStream with retention.
- **Large results are refused, not truncated,** so a widget must choose a sensible `step`. The 1 MB NATS payload limit is the hard ceiling.
- **One more process per account** that wants long-term charts, about the size of the Watchdog, without its state.

## Out of Scope

- **`history-logs`.** It is designed in §3 and built when a real need appears.
- **Metadata endpoints** (`/api/v1/labels`, `/api/v1/label/<name>/values`, `/api/v1/series`). These are the likely first addition, so a console query builder can offer metric and label dropdowns.
- **Caching query results.**
- **Streaming or live tail.** Live data is already on the bus.
- **Writing data.** Telegraf owns the write path.
- **Packaging** several services into one image or install.
