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
    detached host-side script).
*   **No PBS VM.** Its value isn't worth a VM the appliance would have to run and rebuild.

## Known gaps

*   Snapshot is Proxmox-only; a VPS world's point-in-time story is restic.
*   `backup` has no scheduling and there is no restore verb.
*   Backups can outlive a "rotate = erase your copies" revoke; no retention policy.
*   Restic credentials sit in the profile dir and a host-side env file (no secret-env
    injection yet).
*   LVM rollback needs thin-pool headroom ≥ the origin set and a manual `fstrim` after.
*   Data's skill covers `snapshot` only — nothing for `export` or `backup run`.
*   The "back this up?" check-in has no trigger yet (`docs/FREEHOLD.md`).
*   The live Backblaze leg is unverified (hermetic tests only).

## Future

*   **Scheduled backups** owned by Data (a host-side timer, with retention).
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
