# The snapshot skill — plane snapshots and the guarded rollback

This is Data's runbook for the freehold **snapshot verbs**: point-in-time snapshots of
the whole durable plane under one name, and the guarded rollback that restores it. The
ask arrives as "snapshot everything", "did the snapshot land?", or (rarely, and with
weight) "roll the plane back". It is read-on-boot material: re-check it against the
repo as the system evolves.

## The verbs run ON the control plane guest — through `cp-local-root`

Your pod routes **`exec` by target**: `cp-local-root` is local root exec on the CP
guest, where deploy-cp ships the verb surface — the `freehold` binary
(`/srv/data/cp/bin/freehold`), the world's profile config
(`/srv/data/cp/profile/config.toml`), and a cp-verb SSH key
(`/srv/data/cp/verb-ssh.key`, 0600) already authorized on the PVE host. Every command
below is that shape:

    exec("<cmd>", "cp-local-root")

with `<cmd>` always pinning both:

    --config /srv/data/cp/profile/config.toml --ssh-key /srv/data/cp/verb-ssh.key

Use the runner's target **`cp-local-root`** — never `pve-ssh-root` — for these verbs:
the raw `zfs`/`lvs`/`dd` commands are the verbs' internals, and the verbs carry the
guards (a partial snapshot is refused for rollback, a failed stop aborts before any
data moves, a rollback without an escape hatch never runs). Raw probes (reading
`pct config`, listing the plane) still ride `pve-ssh-root`.

## Recipes

**List** (the TUI's DATA view reads the same source — what lands here is what the
operator sees):

    exec("/srv/data/cp/bin/freehold snapshot --list --config /srv/data/cp/profile/config.toml --ssh-key /srv/data/cp/verb-ssh.key", "cp-local-root")

**Create** — always name the intent (the label is positional; letters, digits, dashes):

    exec("/srv/data/cp/bin/freehold snapshot <label> --config /srv/data/cp/profile/config.toml --ssh-key /srv/data/cp/verb-ssh.key", "cp-local-root")

- **Before any risky op say so and take one** — an upgrade, a migration, a config
  rewrite on a tenant, a rollback drill. The snapshot is the escape hatch; taking one
  is never wrong.
- The create covers EVERY recorded durable-plane mount in one name — say how many
  volumes the output reports so the operator knows the plane's breadth.
- Survives teardown/rebuild (dataset-level), but NOT the plane's own destruction —
  `uninstall --remove-data` removes the volumes and everything on them. Say so when
  the ask is "protect this forever": that is the export/back-up flow, not snapshots.

**Inspect** — one snapshot's true usage:

    exec("/srv/data/cp/bin/freehold snapshot --list --json --config ... --ssh-key ...", "cp-local-root")

The JSON carries `used` per backend (ZFS: the CoW bytes; LVM: the thin snapshot's
pool share) — quote it, don't guess.

**Remove** — `--rm <name>` removes the name from every volume (a partial create that
died mid-flight is cleaned up this way).

## Rollback — the heavy hammer, confirmed or not at all

    exec("/srv/data/cp/bin/freehold snapshot rollback --to <name> --yes --guest --config ... --ssh-key ...", "cp-local-root")

- **Never without `--yes`**: the verb fails closed without it — the scripted-mode
  affirmative is the confirmation (you cannot answer a TTY prompt).
- **Never without an explicit ask** that names the target (or explicitly says
  "newest"). Confirm the target out loud first: the rollback stops every plane
  guest, restores ALL durable-plane data, and newer snapshots than the target are
  DESTROYED on ZFS volumes. Ask "which snapshot, and are you sure the last N hours
  are disposable?"
- **The exec dying mid-stream is the design**: the rollback stops the CP guest LAST
  and the verb's exec dies with it — a dead transport is EXPECTED, not an error.
  A detached host-side script finishes: rollback → guests up → CP revival (the
  doors + agent-tools included — the script refreshes from the live processes on
  every update). Wait ~3-5 minutes, then re-poll `--list` and the door's health
  (`exec("systemctl is-active freehold-runner-cp-local-root", "cp-local-root")`).
- **Report the arc**: "the exec ends when the CP guest stops — the host script takes
  over; the world returns in a few minutes; verify with `--list` and the door probe."
- The handoff logs to `/srv/nobackup/rollback-handoff.log` on the HOST (via
  `pve-ssh-root`) — read it when the world does not come back. If the log ends
  `ROLLBACK FAILED`, say the plane state by name and hand the operator the log path.
- A pre-rollback safety net is taken automatically (a fresh `pre-rollback` snapshot
  before anything moves) — mention it exists when confirming the ask.

## The rules

- Secrets never ride chat: the cp-verb key is a PATH you reference, never a value
  you read or print. `backup init` (credentials for an off-site repo) is the
  operator's box-side act — you run backups after it exists.
- One snapshot name = every volume. A `--list` row marked `PARTIAL` is a crashed
  create: remove it (`--rm`), never roll back to it (the verb refuses — don't try
  to force it).
- Everything is relay-audited. Never route around the verbs to raw `zfs rollback` /
  `dd` — the guards are the point.
