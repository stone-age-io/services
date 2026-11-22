# The services contract

What an organization needs on the platform to run watchdog: its Thing Type
and NATS role. It is a `stone` template, so onboarding an organization is one
command, run as an owner or admin of it:

```sh
stone org switch <org>
stone apply --template contract/ --dry-run
stone apply --template contract/
```

Applying it again updates an organization to this version and changes
nothing that already matches. See `stone apply --help` (TEMPLATES).

| Thing Type | Process | NATS role |
|---|---|---|
| `watchdog` | watchdog | `watchdog` |

creds-keeper has no row: it never connects to NATS, and it signs in as the
Thing of the process it serves.

The `watchdog` role subscribes to every heartbeat channel (`*.*.heartbeat`
and `*.*.heartbeat.>`). A check on any other subject needs that subject added
to the role. If you change `CHECKS_BUCKET` or `STATE_BUCKET`, change the
bucket names here too.
