# Changelog

All notable changes to the services are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/). One tag releases
every service in the repository, so one version covers them all.

The services are **before 1.0**, so a minor version can change any surface,
and each entry says when one did. The surfaces worth pinning against: the
environment variables, the KV bucket formats and the micro endpoints in
[`docs/watchdog.md`](docs/watchdog.md) and
[`docs/creds-keeper.md`](docs/creds-keeper.md), and the NATS roles in
[`contract/`](contract/).

> [!NOTE]
> **Keep `## [Unreleased]` empty until it has real entries.**
> `scripts/release-notes.sh` builds the GitHub release body from the section
> matching the tag, and a prerelease tag reads `## [Unreleased]`. It refuses an
> empty section, which is what stops a release shipping with no notes, so a
> placeholder line turns that guard off.

## [Unreleased]

### watchdog

- **Its discovery entry carries its Thing code.** Set `THING_CODE` to the code
  of the watchdog's Thing and `$SRV.INFO` reports it as `metadata.code`, the
  key the console uses to link a running process to its Thing (ADR 0008 rule
  4). The metadata also gains `version`. Unset, the watchdog still runs, and
  logs a warning at startup.

### Repository

- **Release notes come from this file.** The GitHub release body is the
  section for the tag, built by `scripts/release-notes.sh`, instead of
  goreleaser's list of commit subjects.

## [0.3.0] — 2026-11-21

### Both services

- **Health, metrics and the NATS connection come from
  [stone-age-io/kit](https://github.com/stone-age-io/kit) v0.1.0.** The
  repository's own `internal/health` and `internal/natsconn` are gone. What
  changes on the wire:
  - Each check result in `GET /api/ready`, and the report, carry a `took`
    duration.
  - `checked` keeps sub-second precision.
  - `METRICS_TOKEN`, when set, protects `GET /metrics` with a Bearer token or a
    Basic password. Unset, `/metrics` stays open as before.

  Each command has a test that runs the kit's `healthtest.Conform` against its
  real handler.

### Repository

- **`contract/`** is a `stone` template: the `watchdog` Thing Type and NATS
  role. Onboarding an organization is `stone apply --template contract/`.

## [0.2.0] — 2026-11-12

### creds-keeper (new)

- A small companion program (ADR 0007). It signs in to the platform as the
  service's Thing, renews the session, and rewrites the service's creds file
  when the credential changes. The service that reads the file is not changed.
  Released as a binary archive and as `ghcr.io/stone-age-io/creds-keeper`.
- The image's service user has a fixed uid and gid (10001), and `/creds` and
  `/session` belong to it, so a keeper and its service can share a volume.

### watchdog

- **A stalled JetStream no longer blocks the watchdog.** A tick stops writing
  state at the first failed write. The unwritten targets stay dirty and are
  retried on the next tick, so `/api/ready` and the KV watchers no longer wait
  minutes behind it.
- **The client keeps reconnecting after a revoke** until a new creds file is
  in place, rather than giving up on the first authorization error.

### Repository

- ADRs 0005–0007 live in `docs/decisions/`, beside the code they decide.

## [0.1.0] — 2026-11-07

### watchdog (new)

- Reports when targets stop sending messages (ADR 0005). Checks live in the
  `watchdog_checks` KV bucket, and state changes are written to
  `watchdog_state`. It serves `GET /api/ready` and `GET /metrics`, and is a
  NATS micro service with read-only `status`, `checks` and `validate`
  endpoints.
- A NATS outage longer than a check's timeout marks nothing down.
- Released as a binary archive per platform and as
  `ghcr.io/stone-age-io/watchdog`.
