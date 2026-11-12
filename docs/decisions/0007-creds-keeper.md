# ADR 0007: creds-keeper, Platform-Managed Credentials for Account Services

**Status:** Accepted — implemented in [`stone-age-io/services`](https://github.com/stone-age-io/services), with small deviations. See [As implemented](#as-implemented).
**Date:** 2026-11-07 (implemented 2026-11-06)

> This ADR decides how the processes that run inside an organization's account get their NATS credentials from the platform, instead of by copying `.creds` files around. That covers our own services, such as the [Watchdog](./0005-watchdog-service.md), and the third-party ones beside them (Telegraf, rule-router, eKuiper, Benthos). It adds a second program to [`stone-age-io/services`](https://github.com/stone-age-io/services).

---

## Context

Every process in a tenant's account connects to NATS with a user creds file. Today an operator generates that file on the platform, copies it to the host, and copies it again after every Regenerate or Revoke. The platform's own controls stop at the copy:

- **Regenerate** doesn't reach a running service.
- **Revoke** locks a service out until someone delivers a new file by hand.
- **Deactivating** has nothing to stop that is visible in inventory.

The [Agent](https://github.com/stone-age-io/platform-docs/blob/main/docs/agent.md) already solved this for devices. A device is a **Thing**:

1. **Bootstrap.** It signs in once with the Thing's email and password and reads its credential from the Thing's linked NATS identity (`nats_user` → `creds_file`).
2. **Keep signed in.** It saves the PocketBase session token and renews it with `auth-refresh`, so the password can be removed after the first boot.
3. **Sync.** It checks the credential's `updated` time on every sync, and downloads the key only when that time has changed.

The platform's access rules let an authenticated Thing see only its own record and its own NATS identity, so no new server route is needed.

Services need the same lifecycle. Two facts shape how it's delivered:

- **Many services aren't ours.** Telegraf and rule-router can't link a platform client into their code.
- **Almost every NATS client already re-reads its creds file.** In nats.go, `nats.UserCredentials(path)` reads the file inside its connect callbacks, so every reconnect uses whatever is on disk. rule-router passes its `creds_file` to exactly this option, and Telegraf's NATS plugins use the same client.

---

## Decision

**A small companion program, `creds-keeper`, keeps one creds file current for one Thing.** It talks only HTTPS to the platform, and never NATS. The process that uses the file is not changed: it keeps reading its creds path as it does today.

```
platform (HTTPS)                              host or pod
  things auth ──► nats_user.creds_file ──► creds-keeper ──writes──► /creds/user.creds
                                                                       │ read on every connect
                                                          watchdog / telegraf / rule-router
```

### 1. The Agent's credential lifecycle, unchanged

creds-keeper follows the Agent's platform conversation step for step. It uses the standard PocketBase auth pattern: the password once, then the session token renewed with `auth-refresh`.

| Step | When | Platform calls |
|---|---|---|
| **Bootstrap** | The creds file is missing | With a usable saved session: `auth-refresh`, then fetch the credential. Otherwise: `auth-with-password?expand=nats_user`. Write the creds file and the session file. |
| **Sync** | At start if the creds file exists, then every `SYNC_INTERVAL` (default `5m`) | `auth-refresh` with the saved token, then `GET /api/collections/nats_users/records/{id}?fields=updated`. Only if the revision changed: `GET` the full record and rewrite the creds file. Save the renewed token and the revision. |

- **The key crosses the network only when it changed.** PocketBase doesn't apply `?fields=` to auth responses, so an auth call with `expand=nats_user` returns the whole private key. The routine sync therefore refreshes without `expand` and probes only `updated`. This is the Agent's rule, and keeping it means both implementations behave the same.
- **There is no separate refresh token.** `auth-refresh` swaps a still-valid session token for a new one with a fresh expiry, 7 days for a Thing. A 5-minute sync renews it long before then.
- **The password is a fallback.** When `auth-refresh` fails and `PLATFORM_PASSWORD` is set, creds-keeper signs in with the password and carries on. When it isn't set, the sync fails and readiness says why (§5).

### 2. Removing the password after the first run

After the first successful run, the session file is enough, and `PLATFORM_PASSWORD` can be deleted from the environment. That is the reason the token is saved to disk rather than kept in memory: a token that dies at every restart would force the password to stay configured forever.

What removing it buys:
- **No long-lived secret on the host.** A container's environment is easy to read: `docker inspect`, compose files, CI logs. A `0600` session file is narrower.
- **The token expires on its own** if it isn't renewed for 7 days.
- **Deactivating the Thing kills the token at once,** because deactivation changes the Thing's `tokenKey`.

What it costs, exactly as for the Agent:
- **Offline for more than 7 days** (stopped, or unable to reach the platform): the token expires, and the keeper can't sign in until the password is set once more.
- **After deactivation and reactivation:** the old session is gone, so an Owner or Admin sets a new password on the Thing and puts it in the environment for one start.
- **Deleting the creds file alone is always safe.** The keeper restores it from the session without the password.

Keep the password configured where a service may be stopped for long periods.

### 3. Files

| File | Variable | Mode | Contents |
|---|---|---|---|
| Creds file | `NATS_CREDS` | `CREDS_FILE_MODE`, default `0600` | The `.creds` the service connects with |
| Session file | `SESSION_FILE`, default `platform-session.json` beside the creds file | always `0600` | The session token, the NATS identity id and the credential revision. Never the credential. |

- **Both are written atomically,** to a temporary file in the same directory followed by a rename, so no reader ever sees half a file.
- **`CREDS_FILE_MODE` exists for containers.** The keeper and the service usually run as different users, so set `0640` with a shared group.
- **Keep the session file away from the service where you can.** It grants platform access as the Thing, which is more than the service needs. In containers, put it on a volume only the keeper mounts.

### 4. How a change reaches the process that uses the file

| Platform action | Effect on the file | How the client picks it up |
|---|---|---|
| **Regenerate** | New JWT, same key pair | On its next reconnect. The old JWT stays valid, so nothing forces one. |
| **Revoke** | New key pair. The old public key goes on the account's revocation list. | The server closes the revoked connection at once. The client reconnects and fails until the next sync writes the new file (at most one `SYNC_INTERVAL`), then connects. |
| **Deactivate** | Nothing. The identity is suspended, and the token stops working. | The client stays locked out. That is the point of deactivating. |

**The client must keep retrying after an auth error.** By default nats.go stops reconnecting when the server returns the same auth error twice in a row. That would let a revoked client give up before its replacement arrives. So:

- **Our services:** `internal/natsconn` sets `nats.IgnoreAuthErrorAbort()` and reconnects forever.
- **Third-party programs** may exit instead. Their restart policy brings them back, and they connect once the new file is in place.

### 5. Health and metrics

The same contract as every service in the repository: `GET /api/ready` and `GET /metrics`, with no login. There is no NATS connection and no micro registration (§7).

| Readiness check | State |
|---|---|
| `credentials` | **fail** until the creds file has been written or confirmed once, so a process gated on it never starts without a file. ok afterwards. |
| `platform` | **warn** when the last sync failed or no sync has succeeded for 3× `SYNC_INTERVAL`. When the session has lapsed and no password is configured, the detail says so and `fix` says to set `PLATFORM_PASSWORD` for one start. Never fail: the file on disk usually still works, and a platform outage must not take services out of rotation. |

| Metric | Type |
|---|---|
| `creds_keeper_last_sync_timestamp_seconds` | gauge |
| `creds_keeper_sync_errors_total` | counter |
| `creds_keeper_credential_changes_total` | counter |

### 6. Starting order: readiness and restart policy

A process must not connect before its creds file exists. Two things the deployment already has cover it, so creds-keeper adds no start-order mode of its own:

- **Health gating.** `/api/ready` fails until the first bootstrap. Docker Compose's `depends_on: condition: service_healthy` and Kubernetes readiness wait on it.
- **Restart policy.** Every consuming process needs one anyway, to recover from a Revoke. Without health gating, a process that starts too early fails, restarts, and connects once the file exists.

### 7. No NATS micro service

creds-keeper is the one program in the repository that is **not** a NATS micro service. Its job is to keep working when NATS refuses the credential, so it never touches NATS:

- **Discovery would cost every consuming role.** Registering would need `$SRV.>` and a per-Thing status subject in the role of every service it serves: subjects those services don't use, against least privilege.
- **`/api/ready` and `/metrics` already report** everything a status endpoint would.

### 8. One keeper per Thing, one Thing per service instance

- **Each service instance is its own Thing**, with its own email, password and NATS identity. Each gets a NATS role with exactly the permissions it needs, such as the list `TestLeastPrivilegePermissions` verifies for the Watchdog.
- **One creds-keeper runs per Thing,** as a sidecar container or a second systemd unit.
- **There is no multi-identity mode.** Handling several Things in one process would need a config file and a loop over identities. A second small process is simpler.

On the platform, a service instance needs:
- a Thing Type for its kind, such as `watchdog`, `telegraf` or `rule-router`;
- a NATS role with the permissions that kind needs;
- a Thing of that type with a password and a NATS identity in that role.

As far as we know, all of this exists in the platform today. Building this confirms it.

### 9. Configuration

| Variable | Default | |
|---|---|---|
| `PLATFORM_URL` | none | Required. Must be `https://`. |
| `PLATFORM_IDENTITY` | none | Required. The Thing's login email. |
| `PLATFORM_PASSWORD` | none | Required for the first run, and whenever the session has lapsed. Optional otherwise (§2). |
| `PLATFORM_ALLOW_INSECURE` | `false` | Allows an `http://` URL. Development only: bootstrap sends a password and receives a private key. |
| `NATS_CREDS` | none | Required. The file to write. It is the same path the service reads. |
| `SESSION_FILE` | `platform-session.json` beside `NATS_CREDS` | |
| `CREDS_FILE_MODE` | `0600` | |
| `SYNC_INTERVAL` | `5m` | |
| `HTTP_ADDR` | `:8080` | |

### 10. The Agent keeps its own implementation

creds-keeper and the Agent run the same lifecycle against the same platform routes. They are deliberately separate code:

| | Agent | creds-keeper |
|---|---|---|
| Built for | Thousands of devices, often offline, some on Windows | A few services per account, on servers and containers |
| Sync | `24h` by default | `5m` by default |
| Password | Named by `password_env` | `PLATFORM_PASSWORD` |
| Also handles | Code check, Nebula config, leaf node config, `cmd.rotate_creds` | Nothing else |

Sharing a module would tie a device agent's release cycle to a server sidecar's, for a few hundred lines against a stable API. If the platform's credential routes change, both must be updated, and this ADR is where that is written down.

---

## Alternatives Considered

**Sign in with the password on every sync.** One code path, but the password must stay on the host for good. The saved session lets it go after the first run, and that is the standard PocketBase pattern.

**Keep the token in memory only.** Two sign-in paths without the benefit: a restart loses the token, so the password still has to stay configured.

**Build a platform client into each service.** A `PLATFORM_URL` mode in `internal/` beside `NATS_CREDS`. We rejected it for two reasons:
- It covers only our own services, not Telegraf or rule-router, which is half of what an account runs.
- Every service would gain an HTTP client, a sync loop and credential swapping.

The companion program needs no change to any service.

**Register creds-keeper as a NATS micro service.** Rejected in §7.

**Let the Agent manage the creds of other processes on its host.** We rejected it because:
- The Agent's identity is the device's Thing, and managing other Things' credentials would mean holding their secrets too.
- It would tie services to hosts that run an Agent, which a container platform doesn't have.

**The platform pushes credentials,** for example over NATS. We rejected it because:
- The process that needs a credential can't use NATS until it has one.
- Revocation recovery must work while NATS refuses the old credential, so the pull over HTTPS is the part that can't be avoided.

**A shared Go module for the Agent and creds-keeper.** Rejected in §10.

---

## Consequences

- **Platform controls reach every process in the account:**
  - Regenerate reaches it within `SYNC_INTERVAL`.
  - Revoke locks out the old key at once, and the replacement arrives within `SYNC_INTERVAL`.
  - Deactivate removes the process from the bus.
- **Services become inventory.** Each instance is a Thing with a type, an owner and an audit trail.
- **Permissions move into platform roles,** instead of YAML copied into a server config.
- **No service changes,** apart from `IgnoreAuthErrorAbort` in our own `natsconn`.
- **No long-lived secret needs to stay on the host** after the first run. The session token takes its place, and it expires and dies with deactivation.
- **The platform becomes a dependency at first start.** A service can't bootstrap while the platform is down. Once its file exists, a platform outage only delays rotation.
- **One more small process per service instance,** with one state file beside the creds file.
- **Two implementations of the same platform conversation,** the Agent's and creds-keeper's, kept in step by hand.

## Out of Scope

- A rotate trigger, over NATS or otherwise. Regenerate and Revoke in the console reach the file within `SYNC_INTERVAL`.
- One keeper managing several Things.
- Nebula certificates and leaf node config for services.
- Packaging creds-keeper with the services it serves, which waits for the appliance work.

---

## As implemented

creds-keeper shipped in [`stone-age-io/services`](https://github.com/stone-age-io/services) at commit `fca77da`, built on nats.go v1.55.0 and tested against an embedded nats-server 2.15.2 (`credskeeper/`, `cmd/creds-keeper/`, [docs](../creds-keeper.md)). It follows the decision above, with these deviations:

1. **One sync path, also for the first start.** §1 gives bootstrap its own call, `auth-with-password?expand=nats_user`. The implementation has no bootstrap path. A first start is the routine sync with no saved session: sign in without `expand`, probe `updated`, then fetch. That costs two more calls on a first start. The key still crosses the network one time.
2. **The session file holds the token and the revision only.** §3 also puts the NATS identity id in it. Every auth response already gives the id, so a saved copy could only go stale.
3. **The password is used only when the platform rejects the token** with `401` or `403`. §1 says "when `auth-refresh` fails". When the platform cannot be reached, the keeper does not try the password, and readiness does not say that the session lapsed. A platform outage then does not tell the operator to set a password.
4. **A failed sync is tried again after 30 seconds,** not after `SYNC_INTERVAL`. A first start during a short platform outage then does not wait 5 minutes.
5. **The `credentials` check is "the creds file exists",** read on each request. §5 says it fails until the file has been "written or confirmed once". With the file check, a keeper that restarts during a platform outage does not hold back its service.
6. **Additions:**
   - `CREDS_FILE_MODE` refuses a mode that gives other users access.
   - The keeper does not make directories, because a directory it made could have the wrong owner.
   - The shared `Dockerfile` now fixes the `service` user's gid at `10001`, and makes `/creds` and `/session` owned by that user. An empty named volume mounted there is then writable by the keeper and readable by the service.

**Verified by tests:**

- §4: nats.go reads the creds file again on every reconnect, and `IgnoreAuthErrorAbort` is necessary. `TestReconnectsWithNewCredsAfterRevoke` revokes a key on an operator-mode server, waits through more than two auth errors, writes a new file, and checks that the client connects as the new user. Without the option, the test fails.
- §1 and §2: the call sequence, against a fake platform. The routine sync does not fetch the key. A deleted creds file is restored without the password. A lapsed session without a password gives the `fix` text.

**Not yet verified:** a run against a live platform. The calls are the Agent's, which run against the platform today. §8 (the Thing Type, role and Thing that each service instance needs) is still to be confirmed on a live platform.
