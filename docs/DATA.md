# The Data Plane — snapshots, export, and restic off-site

The data-surface build plan: local point-in-time snapshots of the durable
plane, a portable export bundle, and restitutive off-site shipping — without a
PBS VM. The Data department's home ground ("backup/off-site, DR planning,
scheduling, restore verification") starts here: these CLI verbs are the
operator surface, and Data's later capability tooling (Chunk 6+) drives the
same primitives through `exec` — no second mechanism. It is a named build
plan, not a numbered POC chunk — it runs alongside the Chunk 5/6/7 work and
owns no version of its own.

Backblaze B2 off-site is part of this plan (as restic's backend), closing the
"off-site backup shipping" need the portable-backup north star names
(`docs/ROADMAP.md`). PBS is not: its value-add (dedup, verify, incremental
guest archives) is traded for a VM the appliance would have to run, manage,
and rebuild — while the durable plane is already isolated by the `backup=`
mount-flag rule (`docs/ARCHITECTURE.md`), which is all the backup-able surface
the model needs.

## The substrate rule (locked)

**The durable plane is always host-side paths; every verb reads them from the
recorded config, never from host discovery.** `cfg.Plane.Mounts` (HOST source
+ guest path per tenant) plus `BackendKind` is the complete inventory: ZFS
datasets and LVM-thin LVs on Proxmox, plain directories on a VPS. The verbs
differ only in what the substrate can do:

| Verb | Proxmox (ZFS / LVM-thin) | VPS (plain dirs) |
| --- | --- | --- |
| `snapshot` (create/list/rm/rollback) | ✅ native snapshot primitives | ❌ no cheap primitive — restic is the snapshot story |
| `export` (vzdump bundle) | ✅ vzdump per guest | ❌ vzdump is a PVE tool |
| `backup` (restic) | ✅ runs on the PVE host | ✅ runs on the VPS — identical code, different host |

restic is the universal leg; snapshot/export are Proxmox strengths. A
non-Proxmox world that wants point-in-time safety gets restic to a remote
repo — which is the backend-independent format the north star wants anyway.

## Phase 1 — `freehold snapshot` (create / list / rm / rollback)

1.  **One snapshot spans the whole plane.** `freehold snapshot [label]`
    snapshots every recorded durable-plane mount under one name,
    `fh-<timestamp>[-<label>]` — all datasets/LVs, seconds apart. Per-backend:
    `zfs snapshot <ds>@<name>` / `lvcreate -s <vg>/<lv> -n <name>`.
2.  **Snapshots are dataset-level, so they survive compute-only
    teardown/rebuild** — "snapshot before a risky rebuild" is a standing
    workflow, and rebuild never touches them (the plane's ensure stage is
    idempotent; snapshots ride along).
3.  **`--list` / `--rm <name>`** — name, timestamp, label, per-dataset
    footprint. `--rm` removes that name from every dataset.
4.  **Rollback is deliberate, guarded, and CP-down tolerant.**
    `freehold snapshot rollback` (or `--to <name> --yes`) rides the same
    transient SSH path teardown uses, so it works with the CP down — you may
    be rolling back *because* the CP is broken. The flow:
    1.  Pick the snapshot (TUI selector — a table like the existing
        agent/runner rows, arrow-select + enter — or `--to <name>`).
    2.  Risk confirmation, naming the affected guests: they stop for the
        rollback and restart after; **ZFS `rollback -r` destroys newer
        snapshots** on every dataset; **LVM-thin merge (`lvconvert
        --mergethin`) consumes the chosen snapshot**.
    3.  If no snapshot was taken today, offer to take a fresh one first
        (the pre-rollback safety net).
    4.  Stop the affected guests → roll back every dataset → start them →
        report per-dataset status.
5.  **All-or-nothing granularity:** one name applies to every dataset and
    rollback restores all of them together — the world model, not per-tenant
    surgery. Per-tenant rollback is a later flag if it's ever wanted.

**Verification gate before calling this done:** create → list → rollback
exercised on a real world (ZFS and LVM-thin branches both), including the
rollback of a live relay with data written after the snapshot.

**Acceptance:**

*   [ ] `freehold snapshot smoke` snapshots every recorded mount on ZFS and on
    LVM-thin; `--list` shows one name spanning all datasets.
*   [ ] Write data to the relay after a snapshot, roll back, data is gone,
    relay answers again.
*   [ ] Rollback refuses without confirmation; confirms with the risk text;
    offers a fresh snapshot when none exists today.
*   [ ] Rollback works with the CP down (transient SSH path).
*   [ ] `--rm` removes the name everywhere; snapshots survive a compute-only
    teardown/rebuild.

## Phase 2 — `freehold export` (the portable bundle)

1.  **Estimate, confirm, then act.** For each recorded guest
    (`cfg.Lxc.*.Vmid` — no host discovery): used bytes of the rootfs volume +
    every `backup=1` mount, printed per-guest with the total; the operator
    confirms before anything runs.
2.  **vzdump per guest** (`--dumpdir` under `/srv/nobackup` on the host,
    `--mode snapshot`, `--compress zstd`; `suspend` fallback on
    non-snapshot-capable storage) — the archives carry rootfs + every
    `backup=1` mount, so the durable plane rides inside them (the CP guest's
    `/srv/data/cp` is `backup=1`).
3.  **Bundle:** the `.tar.zst` archives are pulled to the box and wrapped,
    with the profile's `config.toml`, into one `tar.gz` — the portable,
    backend-independent artifact. Host tmp is cleaned; nothing is left on the
    guest hosts.
4.  vzdump is Proxmox-only and the call sits in a provider-level package, so
    the limitation is structural, not an oversight. A VPS world's export story
    is its restic repo (Phase 3) — the same portable format, incremental.

**Acceptance:**

*   [ ] `freehold export` prints a per-guest size estimate and waits for
    confirmation before running vzdump.
*   [ ] The bundle contains one `.tar.zst` per guest + the profile config;
    host `/srv/nobackup` tmp is empty afterward.
*   [ ] A bundle restores by hand on a fresh PVE host (vzdump's own restore
    path) — proving "portable" without building a restore verb yet.

## Phase 3 — `freehold backup init/run` (restic, arbitrary backend)

1.  **We implement zero backends.** restic's `-r` URI *is* the backend:
    `sftp:` → TrueNAS, `b2:` / `s3:` → Backblaze B2, `local:`, `rest:`, anything
    restic speaks. freehold resolves the `restic` binary on the substrate host
    (PATH; a clear error names the missing binary), and runs `init`/`backup`
    over the same SSH exec path — on the PVE host today, on a VPS box
    identically when that substrate lands (the host paths are the recorded
    mount sources either way).
2.  **Scope = the durable plane + the profile config**, tagged per-profile so
    one repo can hold several worlds. Dedup makes the first run the expensive
    one; increments after that are small.
3.  **Password:** a 0600 `restic-password` file in the profile dir on the box,
    passed as `RESTIC_PASSWORD_FILE`. Whoever holds the profile can restore —
    the CP secret provisioner path (env injection over the runner) is a later
    tightening, blocked on secret-env over the ssh channel.
4.  **Test matrix = two backends, one code path:** TrueNAS (sftp) and
    Backblaze B2. Both are URI + credentials; nothing in freehold differs.
5.  **Scheduling is deliberately absent** — CLI-invoked first; a host-side
    timer lands when the Data department owns scheduling (Chunk 6+), not
    before.

**Acceptance:**

*   [ ] `freehold backup init -r sftp://…` on TrueNAS and `-r b2:…` on B2 both
    succeed; credentials never appear in freehold's output or config.
*   [ ] `freehold backup run` completes against both backends; a second run
    is near-no-op (dedup visible in restic's stats).
*   [ ] A file deleted from the plane is restored from each backend by hand
    (`restic restore`) — proving the round trip without building a restore
    verb yet.
*   [ ] Works with the CP down (SSH exec path).

## Not building

*   A PBS-equivalent service (daemon, scheduler, verify/QC web UI) — the verbs
    are the product; Data's department tooling drives them via exec later.
*   Per-tenant snapshots/rollback — one name spans the plane; granularity
    splits only if a real workflow demands it.
*   Kube-based backup (a restic pod) — a pod can only see
    `/srv/data/k8s-volumes`; the relay/CP datasets are PVE-host mounts
    attached to LXCs and unreachable from a pod without hostPath chains.
    restic on the host sees everything.
*   `freehold restore` (world bootstrap from backup) — that is the
    portable-backup north star's own path (`docs/ROADMAP.md`), built when the
    bundle format has settled.

## Code-touch map (implementing-agent blast radius)

*   `providers/proxmox/drive/` — snapshot primitives (create/list/rm/rollback)
    for both backends, fake-exec tested like `lvm_test.go`.
*   `providers/proxmox/export/` (new) — the estimate + vzdump + bundle flow.
*   `freehold-cli/snapshot/`, `freehold-cli/export/`, `freehold-cli/backup/`
    (new verbs) — cobra commands in the one-dir-per-verb pattern; transport
    mirrors teardown's (transient SSH / door probe).
*   `freehold-cli/tui/` — the snapshot table + rollback confirm (Phase 1).
*   `docs/ARCHITECTURE.md` — the backup-chain line and the "most teams won't
    have a PBS server" paragraph say the new reality as phases land.
