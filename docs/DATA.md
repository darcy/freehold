# Data — protecting what can't be rebuilt

**Owner:** the Data department (`agents/data/`). **Scope:** the durable plane and the
verbs that snapshot, export, and back it up.

## The idea

A freehold world has two halves. The **reconstructible half** — guest rootfs, container
images, the k3s cluster — is rebuilt by `freehold build`. The **durable half** — relay
data, CP state (identity, sealed secrets), agent workspaces and memory-adjacent files —
cannot be re-derived. Data's job is to keep the durable half safe, and the whole design
rests on keeping the two halves on separate volumes.

```
  guests (rebuildable)            durable plane (host volumes, backup=1)
  ┌────────────┐                  ┌───────────────────────────────────┐
  │ relay      │── /srv/data/relay, /var/lib/docker ─►               │
  │ cp         │── /srv/data/cp ───────────────────────►  ZFS / LVM-thin
  │ k3s        │── /srv/data/k8s-volumes ──────────────►  (or plain dirs on a VPS)
  └────────────┘                  └───────────────────────────────────┘
                                      │ snapshot   │ export     │ backup
                                      ▼            ▼            ▼
                              point-in-time    portable     off-site restic
                              + rollback       tar bundle   (sftp / B2 / any URI)
```

## How it works

*   **The plane is the recorded config.** Every verb reads the list of durable mounts from
    the profile config, never from host discovery. Each mount is born with an explicit
    `backup=` flag; the relay's docker root is deliberately included (its databases live
    there), minus the re-pullable image layers.
*   **Durable PVCs pin under the plane.** k3s's default `local-path` provisioner stores
    PVCs under the rancher daemon root — relocating that root would drag the control-plane
    Postgres PVC (the thing every "reconstructible from" claim depends on) into the
    excluded half. The provisioner's default path is configured explicitly to
    `/srv/data/k8s-volumes`; disposable classes ride a `/srv/nobackup`-rooted storage class.
*   **Three verbs, all run from the box over root SSH, so they work with the CP down:**
    *   `freehold snapshot` — one name spans every mount; list / rm / guarded rollback.
        Proxmox-only (ZFS or LVM-thin primitives). Rollback takes a safety net first,
        stops the guests, rolls back, and restarts them.
    *   `freehold export` — the plane's data + the profile config as one 0600 gzip bundle.
        No guest rootfs. Restore is "untar onto a fresh host, then `freehold build`".
    *   `freehold backup` — restic on the host to any URI (sftp NAS, Backblaze B2, …).
        freehold implements no backends. Works on a VPS identically.
*   **Data runs the same verbs itself.** The `cp-local-root` runner is the CP guest; the
    verbs and a dedicated SSH key ship there at install/update, so Data's pod executes the
    same guarded commands as the operator (a rollback survives its own guest's stop via a
    detached host-side script). All three backup-family verbs take `--ssh-key` for that
    path (default from the box: the DOOR_SPEC key).
*   **The off-site leg keeps a host-side timer, rendered from the verb.**
    `freehold backup install-timer` writes `/srv/nobackup/freehold-backup{,-verify}.sh`
    plus four systemd units to the PVE host, embedding the exact restic line `backup run`
    builds (BackupArgs) — the timer is a render of the verb, never a fork; re-run the verb
    after the plane changes and the host's scope follows. Nightly 04:00 host-local +
    a weekly repo round-trip check, both `Persistent=true`, so a missed run catches up
    after downtime. It runs on the host on purpose: the 04:00 push needs neither the
    operator's box nor the CP. The scripts carry no credentials — they reference the
    0600 files `backup init` pushed and fail loudly with named reasons until those exist.
    Data re-renders it through `cp-local-root`.
*   **The credentials live where restic runs — on the host — and that is sound.** restic
    needs the password + backend keys at run time on the machine that runs it; the 04:00
    run must work with everything else down. Root on the PVE host can already read and
    destroy every plaintext byte of the plane — a backup credential there is no new
    tier. The files sit under `/srv/nobackup` (the `backup=0` half), so they can never
    recursively end up inside a backup, and they exist in exactly two places (the box's
    profile dir + the host, both 0600) so either can die without losing the repo. The
    later tightening is secret-env injection over the runner — deliberately not yet,
    because it would break the host-runs-alone property. Mitigation today: scope the B2
    application key to the bucket so a leaked host copy cannot delete the off-site
    history.
*   **No PBS VM.** Its value isn't worth a VM the appliance would have to run and rebuild.

## Known gaps

*   Snapshot is Proxmox-only; a VPS world's point-in-time story is restic.
*   `backup` has no restore verb — restore is a host-side restic call (Data's drill:
    restore latest to a scratch dir, check, clean up).
*   Backups can outlive a "rotate = erase your copies" revoke; retention/`forget` is
    unscheduled and deliberately the operator's call (deleting off-site history is
    never agent-initiated).
*   Restic credentials sit in the profile dir and a host-side env file (no secret-env
    injection yet — see above for why that's deferred).
*   LVM rollback needs thin-pool headroom ≥ the origin set and a manual `fstrim` after.
*   Data's skill covers `snapshot` and the backup leg (`backblaze.md`) — no `export`
    runbook yet.
*   The "back this up?" check-in has no trigger yet (`docs/FREEHOLD.md`).
*   A host rebuild wipes `/srv/nobackup` — the timers and credential files go with it.
    The box's profile copy survives, so `backup init` (adopt) or `install-timer`
    re-lands them, but nothing automates that yet.

## Future

*   **`freehold restore`** and **restore verification** — periodically restore to scratch.
*   **The North Star:** back up off-site, lose the hardware, stand up a fresh freehold on
    different hardware or a different provider — same identity, memory, grants, services.
    Needs a restore verb, a backend-independent format, and a restore-from-backup bootstrap.
    Also a dogfood tool: clone production onto disposable hardware, test, discard.

## Not building

A PBS-equivalent service; guest rootfs in exports; per-tenant snapshots (until a workflow
needs them); a kube-based backup pod (can't see the host mounts).

## Where the code is

`providers/proxmox/drive/` (snapshots), `providers/proxmox/export/`,
`freehold-cli/{snapshot,export,backup}/`, `freehold-cli/internal/cpdeploy/` (verb surface
for the CP guest), `agents/data/`. How the volumes are created: `docs/COMPUTE.md`.
