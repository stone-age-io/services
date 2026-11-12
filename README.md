# Services

Small, account-scoped NATS services for the [Stone-Age.io](https://stone-age.io) platform.

| Service | What it does |
|---|---|
| [Watchdog](docs/watchdog.md) | Reports when targets stop sending messages. |
| [creds-keeper](docs/creds-keeper.md) | Keeps a service's creds file current from the platform, so Regenerate and Revoke reach it. |

---

## What every service here has in common

creds-keeper is the one exception to this list. It talks only HTTPS to the platform, with a Thing's login, and never NATS ([ADR 0007](docs/decisions/0007-creds-keeper.md) §7). It shares the health contract, the configuration rule and the image.

- **Account-scoped.** It connects with one account user's **creds file** and never needs a platform credential.
- **Environment variables only.** There are no config files.
- **The platform's health contract.** `GET /api/ready` returns `200` or `503` with a JSON report, and `GET /metrics` serves Prometheus metrics. Neither needs a login.
- **A NATS micro service.** `nats micro ls` finds it, and `$SRV.PING`, `$SRV.INFO` and `$SRV.STATS` work.
- **One container image** per service, built from the shared `Dockerfile`.

Shared code lives in `internal/`: `natsconn` (connect, creds, reconnect hooks) and `health` (`/api/ready` and `/metrics`). A service never keeps its own copy of either.

## What belongs here

Simple and easy to maintain beats clever. A service belongs here only if:

1. it needs state over time, arithmetic, or the absence of a message, which rule-router's stateless rules can't express;
2. rule-router, Benthos or NATS itself doesn't already do the job;
3. it's useful to any tenant, not one project;
4. it runs inside a tenant's account with a user creds file.

creds-keeper is admitted on different grounds: it is what lets the services above get their creds file from the platform.

## Decisions

- [ADR 0005: A Watchdog Service for Message Absence](docs/decisions/0005-watchdog-service.md)
- [ADR 0006: History Services for Metrics and Logs](docs/decisions/0006-history-services.md) (deferred, nothing built)
- [ADR 0007: creds-keeper, Platform-Managed Credentials for Account Services](docs/decisions/0007-creds-keeper.md)

The platform's own decisions (0001 to 0004) are in [platform-docs](https://github.com/stone-age-io/platform-docs/blob/main/docs/decisions.md). The numbers are shared, so a new one here takes the next free number across both.

## Releases

One tag releases every service. Pushing `v1.2.3` publishes:

- a Linux `amd64` and `arm64` binary archive per service on the [releases page](https://github.com/stone-age-io/services/releases);
- a container image per service, such as `ghcr.io/stone-age-io/watchdog:v1.2.3`.

`:latest` moves only for a tag without a prerelease suffix, so `v1.3.0-rc1` never becomes `:latest`.

## Develop

```bash
go test -race ./...
docker build --build-arg SERVICE=watchdog -t watchdog .
```

The integration tests run an embedded `nats-server`, so you don't need a server.

To add a service:

1. Add `cmd/<service>/main.go` and a `<service>/` package.
2. Add a build and an archive to `.goreleaser.yaml`.
3. Add the service to the image matrix in `.github/workflows/release.yml`.
4. Write `docs/<service>.md` and list it in the table above.
