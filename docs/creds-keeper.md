# creds-keeper

creds-keeper keeps one NATS creds file current for one Thing on the platform. Without it, an operator copies a `.creds` file to each host, and copies it again after each Regenerate or Revoke. With it, the platform's controls reach the running process:

- **Regenerate** reaches the file within one `SYNC_INTERVAL`.
- **Revoke** locks out the old key at once. The new key reaches the file within one `SYNC_INTERVAL`.
- **Deactivate** removes the process from the bus.

The process that uses the file does not change. It reads its creds path as before.

**This binary is for programs that are not ours,** such as Telegraf, rule-router and eKuiper, which cannot link Go code. Our own programs, the [Watchdog](watchdog.md) among them, run the same package, [`kit/credskeeper`](https://github.com/stone-age-io/kit/tree/main/credskeeper), in their own process, and need no second container.

The design is in [ADR 0007](decisions/0007-creds-keeper.md). [ADR 0009](https://github.com/stone-age-io/platform-docs/blob/main/docs/decisions/0009-thing-credentials-in-kit.md) moved the package into the kit and added device-held keys.

---

## 1. How It Works

```
platform (HTTPS)                              host or pod
  things auth ──► nats_user.creds_file ──► creds-keeper ──writes──► /creds/user.creds
                                                                       │ read on every connect
                                                          watchdog / telegraf / rule-router
```

creds-keeper uses the same platform calls as the [Agent](https://github.com/stone-age-io/agent/blob/main/docs/credentials.md). It talks only HTTPS to the platform, and never NATS. It syncs at start, then every `SYNC_INTERVAL`. Each sync does three steps:

1. **Authenticate.** It renews the saved session token with `auth-refresh`. It signs in with the password only when the platform rejects the token.
2. **Probe.** It reads only the credential's `updated` time.
3. **Fetch.** It reads the credential and writes the creds file, but only when `updated` changed or the file is missing.

The first start is the same sync with no saved session. So the private key crosses the network only when it changes.

A failed sync is tried again after 30 seconds.

### How a change reaches the process

| Platform action | Effect on the file | How the client picks it up |
|---|---|---|
| **Regenerate** | New JWT, same key pair | On its next reconnect. The old JWT stays valid, so nothing forces a reconnect. |
| **Revoke** | New key pair. The account revokes the old public key. | The server closes the connection at once. The client fails to reconnect until the next sync writes the new file, then connects. |
| **Deactivate** | Nothing | The client stays locked out. |

**The client must keep trying after an auth error.** By default nats.go stops after the same auth error two times.

- **Our services** set `nats.IgnoreAuthErrorAbort()`, so they keep trying. An integration test (`TestReconnectsWithNewCredsAfterRevoke`) checks this.
- **Other programs** can exit. Give them a restart policy. They connect when the new file is in place.

---

## 2. Set Up the Platform

Each service instance is its own Thing. For each instance, make these on the platform, in the console or with `stone`:

1. **A Thing Type** for the kind of service, such as `watchdog`, `telegraf` or `rule-router`.
2. **A NATS role** with only the permissions that kind needs. For the Watchdog, use the list in [Permissions](watchdog.md#permissions).
3. **A Thing** of that type, with a login email, a password and a NATS identity in that role. Generate the credential.

One creds-keeper runs for each Thing. Do not share one Thing between two service instances.

---

## 3. Run It

### Docker Compose

This example runs Telegraf with a creds-keeper beside it.

```yaml
services:
  creds-keeper:
    image: ghcr.io/stone-age-io/creds-keeper:latest
    restart: unless-stopped
    environment:
      PLATFORM_URL: https://platform.example.com
      PLATFORM_IDENTITY: telegraf-01@things.example.com
      PLATFORM_PASSWORD: ${TELEGRAF_PASSWORD}   # remove after the first start
      NATS_CREDS: /creds/user.creds
      SESSION_FILE: /session/platform-session.json
      CREDS_FILE_MODE: "0640"   # Telegraf runs as another user
    volumes:
      - creds:/creds
      - session:/session        # only the keeper mounts this

  telegraf:
    image: telegraf:1.34
    restart: unless-stopped
    group_add: ["10001"]        # the keeper image's group
    depends_on:
      creds-keeper:
        condition: service_healthy
    volumes:
      - creds:/creds:ro
      - ./telegraf.conf:/etc/telegraf/telegraf.conf:ro   # nats output: credentials = "/creds/user.creds"

volumes:
  creds:
  session:
```

- **Start order.** `/api/ready` fails until the creds file exists. `condition: service_healthy` holds the service until then. The image's health check polls `/api/ready` every 30 seconds.
- **The session file is on its own volume.** It gives platform access as the Thing, which is more than the service needs.
- **The program runs as another user.** The keeper image runs as uid and gid `10001`. Set `CREDS_FILE_MODE: "0640"` on the keeper and add the program's container to group `10001`, as above.

### systemd

Run the keeper as a second unit beside the service. Make the directory first, owned by the user that runs the keeper:

```ini
# /etc/systemd/system/creds-keeper-telegraf.service
[Unit]
Description=creds-keeper for telegraf
After=network-online.target

[Service]
User=creds-keeper
Group=telegraf
Environment=PLATFORM_URL=https://platform.example.com
Environment=PLATFORM_IDENTITY=telegraf-01@things.example.com
Environment=NATS_CREDS=/etc/telegraf/creds/user.creds
Environment=SESSION_FILE=/var/lib/creds-keeper/telegraf/platform-session.json
Environment=CREDS_FILE_MODE=0640
Environment=HTTP_ADDR=127.0.0.1:8081
EnvironmentFile=-/etc/creds-keeper/telegraf.env
ExecStart=/usr/local/bin/creds-keeper
Restart=always

[Install]
WantedBy=multi-user.target
```

Put `PLATFORM_PASSWORD=...` in `/etc/creds-keeper/telegraf.env` (mode `0600`) for the first start, then delete the file.

The service unit needs `Restart=always`, so it recovers from a Revoke and from a start before the file exists.

### Configuration

| Variable | Default | |
|---|---|---|
| `PLATFORM_URL` | none | **Required.** Must be `https://`. |
| `PLATFORM_IDENTITY` | none | **Required.** The Thing's login email. |
| `PLATFORM_PASSWORD` | none | Required for the first start, and when the session has lapsed. Optional at other times. See [§4](#4-remove-the-password-after-the-first-start). |
| `PLATFORM_ALLOW_INSECURE` | `false` | Allows an `http://` URL. Use it for development only: the first sync sends a password and receives a private key. |
| `NATS_CREDS` | none | **Required.** The creds file to write. It is the same path the service reads. |
| `SESSION_FILE` | `platform-session.json` beside `NATS_CREDS` | |
| `CREDS_FILE_MODE` | `0600` | Octal. Use `0640` with a shared group. A mode that gives other users access is refused. |
| `SYNC_INTERVAL` | `5m` | |
| `HTTP_ADDR` | `:8080` | Serves `/api/ready` and `/metrics`. |
| `METRICS_TOKEN` | none | When set, `/metrics` requires it as a Bearer token or a Basic password. `/api/ready` never asks for one. |

### Files

| File | Mode | Contents |
|---|---|---|
| Creds file | `CREDS_FILE_MODE` | The `.creds` file the service connects with. |
| Session file | always `0600` | The session token and the credential's revision. It never holds the credential. |
| Seed file | always `0600` | Only for a [device-held key](#device-held-keys): the NATS seed, beside the creds file with the extension `.nk` (`user.creds` → `user.nk`). |

- **Both files are written atomically:** a temporary file in the same directory, then a rename. A reader never sees half a file.
- **Keep them out of backups** that leave the host, such as a PocketBase application's `pb_data/`. They hold the private key and a session that signs in as the Thing.
- **The directories must exist.** creds-keeper does not make them, because a directory it made could have the wrong owner. In our images, `/creds` and `/session` exist and belong to the service user.

### Device-held keys

A NATS identity made with **device key** on the platform holds no private key there. The platform has only the public key, and signs a JWT for it. The keeper reads which kind the identity is from the platform; there is nothing to configure.

| | Platform-held key (the default) | Device-held key |
|---|---|---|
| The platform has | the seed and the creds file | the public key and a signed JWT |
| The keeper | writes the creds file it downloads | builds the creds file from the JWT and its own seed |
| The seed lives | on the platform | in the seed file, on this host only |

- **The first sync makes the key.** The keeper makes a key pair, writes the seed, then enrolls the public key with `POST /api/me/keys`.
- **A Revoke empties the key, and the keeper enrolls a new one.** It never sends the old key again.
- **A key this host did not make stops the keeper.** An owner or admin pasted it, or the seed was lost. The `platform` check says so. The fix is to revoke the NATS key on the Thing; the next sync makes and enrolls a new one.
- **The seed exists only on this host.** A container recreated without its volume loses it, and recovery needs an owner or admin to revoke the key. Keep the volume. Choose a device-held key when the platform must never hold the key; otherwise keep the default.

---

## 4. Remove the Password After the First Start

After the first successful sync, the session file is enough. Remove `PLATFORM_PASSWORD` from the environment.

What this gives you:

- **No long-lived secret on the host.** A container's environment is easy to read. A `0600` session file is not.
- **The token expires** if it is not renewed for 7 days.
- **Deactivating the Thing stops the token at once.**

What it costs:

- **Offline for more than 7 days:** the token expires. Set the password again for one start.
- **After deactivation and reactivation:** the old session is gone. An Owner or Admin sets a new password on the Thing. Set it in the environment for one start.
- **Deleting only the creds file is always safe.** The keeper restores it from the session, without the password.

Keep the password set where a service can stop for long periods.

---

## 5. Health and Metrics

### `GET /api/ready`

| Check | State |
|---|---|
| `credentials` | **fail** when the creds file does not exist. **ok** when it exists. |
| `platform` | **warn** when the last sync failed, or no sync has completed in 3× `SYNC_INTERVAL`. When the session has lapsed and no password is set, `fix` tells you to set the password for one start. When the platform holds a device-held key this host did not make, `fix` tells you to revoke it. It never fails: the file on disk usually still works, and a platform outage must not stop the services. |

A platform outage is not reported as a lapsed session. creds-keeper uses the password only when the platform rejects the token with `401` or `403`.

### `GET /metrics`

| Metric | Type |
|---|---|
| `creds_keeper_last_sync_timestamp_seconds` | gauge |
| `creds_keeper_sync_errors_total` | counter |
| `creds_keeper_credential_changes_total` | counter |

```yaml
- alert: CredsKeeperStale
  expr: time() - creds_keeper_last_sync_timestamp_seconds > 3 * 300
  for: 5m
```

creds-keeper is not a NATS micro service. Its job is to work when NATS refuses the credential, so it never connects to NATS.

---

## 6. Troubleshooting

| Symptom | Likely cause |
|---|---|
| `platform` warns "session has lapsed" | The keeper was offline for more than 7 days, or the Thing was deactivated. Set `PLATFORM_PASSWORD` for one start. |
| `sign in as ...: platform returned 400` | The email or the password is wrong. |
| "has no NATS identity" | The Thing has no NATS identity. Assign one on the platform. |
| "has no credential" | The NATS identity has no credential. Generate one on the platform. |
| `write creds file: ... permission denied` or `no such file or directory` | The directory does not exist, or the keeper's user cannot write to it. |
| The service cannot read the creds file | Set `CREDS_FILE_MODE=0640` and give the service the file's group. |
| The service stays locked out after a Revoke | It gave up after auth errors. Give it a restart policy. |
| "a NATS key enrolled for this Thing that this host does not hold" | A device-held key that this host did not make, or whose seed was lost. Revoke the NATS key on the Thing. |
| "enrolled but the platform has issued no JWT" | The NATS identity is deactivated. Reactivate the Thing. |

---

## 7. A Compromised Host

**Revoke is for a leaked creds file. A compromised host needs the Thing deactivated.**

Whoever holds the session file, or the process that holds the session, can sign in as the Thing. A Revoke does not stop them:

- **Platform-held key:** the Revoke issues a working replacement, and the session downloads it at its next sync, as the keeper does.
- **Device-held key:** the Revoke empties the key, and the session can enroll a key of its own before the keeper does.

Deactivating the Thing ends every session at once and revokes the NATS identity without issuing anything back. To recover:

1. **Deactivate** the Thing.
2. An owner or admin **sets a new password** on it. The old one may have been in the host's environment.
3. **Reactivate** the Thing, then **Revoke** its NATS key, so the key pair changes as well as the JWT.
4. **Start the keeper once with the new password.**

The same holds for a program that runs the keeper in its own process, such as the Watchdog. There the session lives in the program itself.
