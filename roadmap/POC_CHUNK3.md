# Chunk 3 — Detailed Build Plan (draft v5)

Status: DRAFT — for review (v5, folds the implementor review of v4); NOT yet locked.
Consolidates v3 (with its Phase-0 resolutions) + v5's changes: two structural supersessions
(onboarding inversion, k8s pull-forward) plus the sequencing/mechanics corrections from the
review. Everything below marked v3-unchanged stands as locked-in-v3; supersessions are
recorded explicitly, not silently.

## Pre-C0 progress (2026-08-27) — what landed before C0, after the world rebuild

The 2026-08-27 teardown/rebuild destroyed the pre-C0 live instances (the landing-strip LXC
harness and the LXC-105 LiteLLM reference; see the stale-claim amends below); the code and
harness for both paths survive (flavors #72/#73, terraform plans #76/#78). What landed in
the rebuild era and directly advances C0/C1:

- **k3s is now a deterministic CONFIGURE STAGE (#120)** — `freehold configure` boots the
  k3s LXC (auto vmid, coords recorded to `lxc.k3s` + `managed += k3s`) and installs k3s
  inside with the spike-verified unprivileged posture (KubeletInUserNamespace after the
  subcommand, unit override, node-ready wait, `/srv/data/k8s-volumes` carve-out). This is
  the C0 substrate's bring-up, landing as a runner-exec stage (C7's "runner-exec only"
  discipline honored; the Terraform wrapper for `--kind k3s` remains the C7 consolidation,
  not a prerequisite for C0). It applies Phase 0.08's install half LIVE on librem; the
  other half (nginx-through-NodePort from the PVE host) is still open — see 0.08.
- **The Services view is ready for litellm (#115/#120)** — the running dashboard lists
  `managed` pieces (relay/cp/k3s today); litellm appears automatically the moment its
  coords land in the config (the same machinery k3s used).
- **Agents registry + live availability (#118)** — the CP records named AI agents
  (delegate-peer registers itself at start) and reports ●/○ availability from relay kind-9
  presence; the buzz-acp agents register through the same path.
- **The console now has a WORKING relay scope** (deploy-cp wires `--relay-url`/
  `--relay-pubkey`/`--relay-host`/`--relay-host-ip`; a co-located console talks to the
  relay LXC over LAN with the community `Host` header + NIP-98 signed at the public URL +
  relay community membership) — a precondition for the litellm-kube reads and the agent
  channel views.
- **Teardown destroys the k3s LXC (#121)** — the box-lifecycle discipline now covers the
  substrate the rebuilds kept stranding.

C0 itself (litellm-kube apply + Postgres + master-key re-mint + the Services row) has NOT
started; see Phase C below.

## Phase 0.12 — Durable Volume Plane (pre-C0)

Named follow-ups (not yet live): the LVM-thin rung is IMPLEMENTED +
hermetic-tested — a stock PVE LVM host (VG `pve`, no ZFS) resolves
`Reuse(LvmThin)` and `ensure` builds thin-LV mounts: the VG's EXISTING thin
pool is REUSED (stock PVE `pve/data`; a fresh `freehold-thin` carve-out only
when the VG truly has none), one thin LV per tenant (relay keeps TWO,
docker-root + deploy), mkfs gated on blkid (a partial failure recovers on
rerun), mount at `/freehold/<domain-dash>/<tenant|child>` + chown to the
shifted guest uid + an idempotent /etc/fstab entry (a host reboot restores
the plane — a bare mount would not). The backend KIND is recorded in
`plane.backend_kind` and threaded back into every `storage ensure|destroy`
as `--kind`, so a host with BOTH a zpool and a VG never drives an
LVM-backed tenant through the ZFS arm; destroy unmounts + strips the fstab
line before `lvremove` (which refuses a mounted LV even with `-f`). The
advertised `ZFS → LVM-thin → bail` order is real on both rungs now. What
remains pre-C0 is the LIVE acceptance: running the LVM ensure/destroy +
relay two-child + k3s-reattach gates against the actual PVE host.

### Why this exists

The 2026-08-27 teardown destroyed the LXC harness and the LXC-105 LiteLLM reference because
their state lived only on the compute being torn down — acceptable for throwaway staging,
not once relay and CP are load-bearing. This phase builds the durable plane those two
services need, and makes acquiring that plane a **first-class precondition** on any target
(Proxmox-lxc AND VPS — Host-FLEXIBLE requires both), not a librem-specific fix. It is a
Phase-0 item (hardware/data substrate, like 0.08/0.09/0.11), deliberately sequenced before
C0: the plane is C0's precondition, not parallel work.

### Locked decisions

* **Storage-backend resolution is host-agnostic and runs as a STAGE IN THE CONVERGE
  PIPELINE** (where the k3s stage and the relay/CP deploys already run — not a separate
  "bootstrap sequence" naming that no longer matches the actual pipeline), inserted BEFORE
  the relay/CP boots. Both branches:
  - **Proxmox-lxc:** prefer ZFS (existing zpool, or create one if the target has
    unallocated space) → fall back to LVM-thin → bail if neither is viable.
  - **VPS:** prefer a provider block-storage volume (Vultr Block Storage / Hetzner Volumes),
    mounted as the tenant-volume root → if no block-storage API/budget is configured, a
    plain data directory on the instance disk with durability EXPLICITLY downgraded and
    stated as such ("survives compute-only teardown only if the instance disk itself
    survives it" — no snapshot capability, never presented as parity) → bail. **This is NEW
    connector code, called out as its own deliverable:** the vultr/hetzner connectors today
    are instance-lifecycle only (create/destroy) — no volume create/attach/format/mount
    surface exists. The branch rides the same runner-exec discipline, but "through the
    existing connectors" understates the work.
  - **Bail, either branch, is actionable:** names the fix (free space / attach a disk /
    attach a block volume / a NAS manually). This is a first pass, not full VPS storage
    hardening (deferred) — the downgraded fallback is honestly labeled rather than
    pretending parity.
  - **Idempotent on re-runs:** create a zpool/volume only if absent; never re-carve or
    resize an existing pool/volume on a re-converge; a re-run against an already-resolved
    target confirms the existing backend, creates nothing new.
* **The universal rule, unchanged:** all compute is disposable; all data durable and
  separately addressable, no exception for relay or CP; `~/.freehold` explicitly out.
* **Per-tenant datasets, not one shared dataset — for correctness, not accident
  prevention.** Each durable tenant (relay, CP, and k3s-volumes FROM THE START — C0 and
  locked 0.09 pin Postgres/LiteLLM to `/srv/data/k8s-volumes`, which today is carved INSIDE
  the disposable k3s LXC (`k3s-bringup.sh:64`) and dies with it — a live exception to the
  universal rule until it is a tenant) gets its own dataset under a common parent:
  (1) teardown granularity — data+compute teardown of one tenant must leave others
  untouched, by construction; (2) unprivileged LXC guests share a host uid range, so a
  shared dataset is cross-readable across tenants while both run, independent of teardown.
* **Snapshot-capable ≠ snapshot-consistent** — named (WAL-aware / quiesce-before-snapshot
  for any tenant running a database), implementation deferred.
* **ARCHITECTURE's "durable PVCs never on the daemon root" applies to the k3s/rancher root,
  not the docker data-root.** Mounting the tenant dataset AS `/var/lib/docker` puts durable
  data under a daemon root by construction — the opposite of that rule's wording. The
  accepted trade for DOCKER specifically: the images there are disposable-
  because-durable (harmless), and the mount is the only no-patch way to reach the named
  volumes. Recorded as the docker-root exception; the k3s rule stands unchanged
  (`/srv/data/k8s-volumes`).
* **Attachment is by reference, not by copy** — bind-mount (LXC), provider volume mount
  (VPS), or the k3s `local-path` root pointed at the tenant dataset (kube). **And the
  compute is BORN with the reference mount** — the mount is baked into the LXC
  create / VM provision / pod volume at first creation, never attached post-hoc. "Born on
  the plane from creation" is the deliverable; a post-hoc `pct set` is a different, weaker
  claim.
* **The relay's docker named volumes are under the daemon data-root — the mount mechanism
  must say so.** The compose bundle ships named volumes (`buzz-postgres-data` etc.), which
  physically live under `/var/lib/docker/volumes` INSIDE the guest — an `mp` at
  `/srv/data/relay` never reaches them. The no-patch route (the buzz bundle is NOT patched —
  locked decision) commits to ONE variant: the tenant dataset IS the docker data root — the
  guest's `/var/lib/docker` is the dataset's mount point, never a `daemon.json` `data-root`
  key (the bootstrap's fuse fallback TRUNCATES `/etc/docker/daemon.json` to
  `{"storage-driver":"fuse-overlayfs"}` at `bootstrap.rs:576-585` — a later converge would
  silently revert any data-root there to the ephemeral rootfs and the relay would boot
  healthy on an empty Postgres; the mount-point variant is immune by construction). Images
  become disposable-because-durable, harmless; the compose file stays as-shipped.
* **Two CHILD datasets for relay, not one at two mount points.** The `.env` holds
  `BUZZ_RELAY_PRIVATE_KEY` + every DB/S3 credential — it must NOT live inside the docker
  data-root (the routine fix for a wedged docker-in-LXC is wiping that root). Under the
  relay tenant parent: one child = the daemon data root, one child = the compose deploy dir
  (where the `.env` lands). Separate blast radii; the root wipe can never take the identity
  with it.
* **Unprivileged ownership is a landmine, stated as a requirement.** Guests write as
  host-uid 100000: the dataset root must be chowned to the shifted uid (or an idmap applied)
  BEFORE the guest can write the mount — verified at first mount, not assumed. The LVM-thin
  backend additionally needs mkfs + the same ownership handling. (Not "zoned" — that is a
  ZFS block-device property, unrelated to container mounts; the mechanism is idmap +
  dataset-root ownership.)
  **Consent is a FRONT-END concern — the converge pipeline itself is non-interactive**
  (the locked `lib.rs` discipline: "prompts and waiting belong to the front-ends"). The
  NORMATIVE expression of consent in the pipeline is the `--confirm-storage` flag — a
  headless converge must be able to grant or withhold consent and BAIL (not stall on a
  prompt) when a backend must be created and the flag is absent. The interactive confirm
  prompt exists only in the front-ends (`freehold-install`, the TUI), which translate the
  operator's answer into the flag for the stage. A configure-stage that would prompt
  directly is a contract violation.
* **Tenant→dataset mapping lives outside compute, two-place recoverable:** the workstation
  config (survives compute teardown by design) plus independently derivable from the
  host/provider's own volume listing. **Teardown reads the mapping from the config BEFORE it
  deletes the config** (today's teardown removes it) — the two-place rule is the recovery
  net, not the read order.
* **First tenants: relay and CP — migration means destroy-and-recreate-fresh, not a live
  cutover.** Neither running instance holds data worth preserving, so "migrate onto the
  plane" = tear down the existing relay/CP, resolve the backend, stand up FRESH instances
  with state born directly on the resolved tenant dataset. No copy-verification or
  stack-bounce deliverable under this resolution — if something worth keeping appears first,
  the decision is revisited explicitly, never silently reinterpreted as a live cutover.
* **A tenant's durable set is the WHOLE compute-born state that must outlive it, not just
  the obvious volumes — recorded per tenant:**
  - **Relay:** the Postgres/Redis/MinIO + git data volumes AND `deploy/compose/.env` — the
    compose bundle GENERATES the relay signing key + `POSTGRES_PASSWORD`/`REDIS_PASSWORD`/
    S3 keys onto the rootfs at deploy (`relay.rs:163-230`). Reattach the volumes but not the
    `.env` and: Postgres ignores `POSTGRES_PASSWORD` on a non-empty PGDATA (the state
    doesn't open), and a regenerated relay signing key invalidates every 39002 roster the
    fail-closed runners verify against their pinned `--relay-pubkey`. The `.env` is state.
  - **CP:** the whole state DIR (`/srv/freehold/control-plane`: `state.json` + the console
    identity + the shipped runner packages), not `state.json` alone — a compute-only
    teardown otherwise recreates a fresh console identity + loses the packages on EVERY
    cycle, not just the first. With the dir durable, the identity + packages survive and
    there is NO recurring re-member/re-adopt — the "fresh console identity" note above
    applies only to the one-time pre-plane cutover (where nothing was worth keeping).
* **`rebuild`'s role changes** once CP's state is plane-backed: volume-loss DR fallback,
  not the ordinary post-teardown recovery path.
* **CP's distributed/replicated form** — named, not built, gated on CP's state moving to
  the `control_plane` Postgres database first.
* **Relay-as-kube** — named direction, not built here.
* **Teardown gains THREE scopes — the config must survive per-tenant teardown.** Today's
  `teardown.rs` deletes `~/.freehold` + the config unconditionally (whole-world), which is
  incompatible with reattach (the config holds the coords + tenant→dataset mapping). The
  scopes:
  - **Whole-world (default for `freehold teardown`):** compute + config + local home go;
    optional data+compute on top with typed confirmation — the existing behavior, unchanged
    in intent.
  - **Per-tenant compute-only:** destroys exactly ONE tenant's LXC/pod; the config SURVIVES
    (it holds the coords + mapping for reattach — at most the tenant's coords are refreshed);
    the dataset is untouched; next compute reattaches by reference.
  **The destroy unit of data+compute is the tenant PARENT subtree, never a single child.**
  Relay is TWO child datasets (docker data-root + compose deploy dir) under its tenant
  parent; destroying "the relay's dataset" destroys BOTH children with the parent — a
  data+compute relay teardown must not leave the deploy-dir `.env` (or the data-root)
  orphaned behind while pretending a full intended loss. Same for CP/k3s (single dataset
  under their parent, for now). The acceptance covers the two-child relay destroy path,
  not just CP's single dataset.
* **Interplay with D4's blue/green (recorded, no decision forced):** the shared tenant
  dataset means both colors mount the SAME volume by reference — DB-style tenants handle
  shared storage natively; single-writer services need the flip at the mount/service
  selector, not in-place on one volume.

### Deliverables

1. Converge pipeline gains a storage-backend resolution stage (Proxmox-lxc: ZFS →
   LVM-thin → bail; VPS: provider block volume → explicitly-downgraded local directory →
   bail), idempotent on re-runs, inserting before the relay/CP boots.
2. Per-tenant dataset/volume for relay, CP, AND k3s-volumes under a common parent, each
   independently destroyable — the k3s bringup's `/srv/data/k8s-volumes` carve-out becomes
   a MOUNT of the k3s-volumes tenant dataset (the `local-path` provisioner root points at
   it), satisfying the universal rule + C0/0.09's assumption at once.
3. Fresh relay stood up with the Postgres/Redis/MinIO/git volumes AND the generated
   `deploy/compose/.env` BORN on relay's tenant datasets at creation: one child dataset IS
   the guest's `/var/lib/docker` (the named volumes land on it — NO `daemon.json` touch, so
   the bootstrap's truncating write can never revert it), a second child = the compose
   deploy dir (the `.env` lives there, outside the wipeable docker root). The mounts are
   baked into the LXC create; neither the volumes nor the `.env` are migrated from the
   running instance. A compute-only teardown then reattaches BOTH, so the reattached relay
   opens its state and keeps its signing identity (rosters stay valid). The dataset roots
   are chowned to the guest's shifted uid at first mount (guest-writable verified, not
   assumed).
4. Fresh CP stood up with the whole STATE DIR (state.json + console identity + runner
   packages) born on CP's tenant dataset at creation; atomic-write discipline verified
   across the mount boundary (and the rename's parent-dir fsync fixed there — futil's
   `write_0600_atomic` fsyncs the file, not the directory, so the rename itself can be lost
   on power-cut; that gap's natural home is this verification); the identity + packages
   survive compute-only teardown.
5. Tenant→dataset mapping in the workstation config, independently re-derivable from the
   host/provider's volume listing — under a NAMING CONVENTION so the derivation is
   checkable, PER BRANCH (provider volume labels are flat with a restricted charset; a ZFS
   dataset path is not):
   - **Proxmox (ZFS/LVM-thin):** `<pool>/freehold/<domain>/<tenant>` — the same
     world+tenant pattern the guests already follow with `<domain>-<role>`.
   - **VPS (block volume):** the FLATTENED label `fh-<domain-with-dashes>-<tenant>` (the
     `.` → `-` normalization the guest names already use); no slashes, derivable from the
     provider's volume list alone. Whole-world teardown on VPS REMOVES the tenant block
     volumes (or confirms leaving them) — otherwise the config dies and the volumes survive
     as orphaned billed resources with no mapping back (the two-place rule has only one
     place on VPS).
6. Teardown tooling updated with the THREE scopes (whole-world / per-tenant compute-only
   / per-tenant data+compute); per-tenant scopes KEEP the config (coords + mapping) —
   whole-world alone deletes it, and on VPS whole-world removes the tenant block volumes
   (or confirms leaving them) so they are never orphaned-and-billed with no mapping;
   data+compute carries a clear warning + typed confirmation. VPS connector surface:
   block-volume create/attach/format/mount (new code).

### Acceptance

* **Hermetic gates (testkit/mock, run before the live ones — the repo's hermetic-first
  discipline):** stage idempotency on CLEAN re-runs AND crash-mid-stage → re-converge (two
  different properties, both gated); the bail-message shape (actionable, names the fix);
  per-tenant teardown scoping (a data+compute teardown of one tenant provably leaves the
  other's dataset + mapping alone) — all against a fake `pct`/mock provider, mirroring the
  fake-relay/mock-Vultr pattern.
* Converge against a Proxmox-lxc target with no existing backend → resolution detects
  absence and, WITH operator consent (the confirm gate — covering zpool AND LVM-thin-pool
  creation alike), creates a zpool or falls back to LVM-thin; WITH CONSENT WITHHELD and no
  existing viable backend → bails with the actionable message (the resolution order is
  `ZFS → LVM-thin → bail`, unchanged — the acceptance asserts the bail, not a fourth
  tier); against a target that can support neither → bails with an actionable message.
* Converge against a VPS target → resolution attaches a provider block volume if available,
  or falls back to the explicitly-downgraded local directory, stating the durability
  difference; bails only if both are unavailable.
* Re-run resolution against an already-resolved target → confirms the existing backend,
  creates nothing new.
* **Durable backends (Proxmox / block-volume):** destroy the relay LXC (compute-only) →
  fresh relay LXC → reattach by reference → full prior state + the compose `.env` (same
  signing identity, same DB/Redis/MinIO secrets) intact, no rebuild needed. Same for CP
  with the full state dir (runners/grants/secrets ciphertext + the console identity +
  packages) intact and the existing relay memberships/rosters still valid.
* **Downgraded VPS fallback:** rebuild-fresh semantics, stated plainly — the instance disk
  dies with compute-only teardown, so this branch's teardown = fresh install (the durability
  downgrade in action, never a silent surprise).
* Invoke data+compute teardown on the RELAY tenant (two children: docker data-root + the
  compose deploy dir) → both child datasets destroyed with the parent subtree (no `.env`
  or data-root orphaned behind), and CP's + k3s-volumes' datasets provably untouched.
* Destroy the k3s LXC (compute-only) → fresh k3s LXC → reattach by reference →
  `/srv/data/k8s-volumes` contents intact (the k3s-volumes tenant survives, so C0/0.09's
  Postgres/LiteLLM durable assumption holds across a substrate teardown).
* Tenant→dataset mapping recoverable from the host/provider's volume listing alone, via
  the `<pool>/freehold/<domain>/<tenant>` naming convention.
* Tenant→dataset mapping recoverable from the host/provider's volume listing alone, via
  the `<pool>/freehold/<domain>/<tenant>` naming convention.

### Explicitly deferred

* Automated snapshot scheduling and retention policy.
* Snapshot consistency mechanics — named as a requirement, implementation deferred.
* Backblaze off-site backup.
* NFS/iSCSI-off-a-NAS substitution for the local Proxmox-lxc backend.
* Full VPS storage hardening beyond the first-pass block-volume/downgraded-fallback branch.
* Relay-as-kube, CP-as-distributed-kube (gated on named preconditions).
* LiteLLM and Postgres-for-k3s as plane tenants — new builds directly on the plane as part
  of C0's scope (C0 assumes them born on the plane).

## Locked decisions — SUPERSEDED or NEW this revision

* **v3's "one dedicated agent-harness LXC" placement is RETIRED in the target
  architecture.** Agents are pods, one identity per pod, on the k8s substrate pulled forward
  into Chunk 3 (ROADMAP.md:63 "**NO Kubernetes** in POC." is SUPERSEDED for Chunk 3 forward —
  that line is amended at lock; it remains true for Chunks 1–2). Deterministic pods,
  `envFrom` Secret, config pulled fresh per pod-start (ARCHITECTURE's already-specified
  model). **Landing-strip exception (execution, not architecture):** the FIRST expert
  (`@pihole` / `@tailscale`) runs on the already-staged LXC harness (goose 1.47.0 +
  buzz-acp + a minted LiteLLM key — live on-box as of 2026-08-24; that instance was
  destroyed by the downstream teardown/rebuild (2026-08-27), so the landing strip now
  re-stages from the flavors + harness, not a live box) while C0 stands up the
  substrate in parallel. Pod migration is a separate, C5-gated cutover — NOT a precondition
  for G1/G2.
* **Onboarding order INVERTED (supersedes ARCHITECTURE's + POC.md's "CPA provisions target →
  creates runner → creates agent" order),** for Chunk 3 forward: CPA creates the expert
  (identity + prompt: "you are the expert for `<service>`, install it") → the expert reasons
  about its own target (the skill's `target: lxc | pod | either` is a HINT it may accept or
  override, not a directive CPA acted on) → the expert asks **CPA** (never a peer directly)
  for what it needs → CPA resolves which named peer fulfills it and whether an
  approval/cost gate applies → delegates → the peer provisions + grants access → CPA/peer
  tells the expert via Buzz → expert installs. The expert never learns which peer it landed
  on — it receives only a slot descriptor (Phase 0.10). Default channel membership stays
  operator + CPA (the expert only ever talks to CPA — no widening needed).
* **"The provisioner" is not a generic role.** Every hardware-hosting box gets its own
  NAMED peer identity (`@vultr`, `@hetzner`, `@macmini`, `@<operator-named>`) — the direct
  relay duplicate of POC_CHUNK2's local provisioning expert, second application of the locked
  duplicate-not-promote rule (new identity, re-encrypted credentials, never promoted in
  place). A named peer's job = capacity fulfillment ("give me an LXC"/"give me a kube slot")
  via the runner bootstrap already granted it. **Box lifecycle (create, reachability,
  teardown, rebuild) stays with the deterministic bootstrap tool, always, with no exception,
  forever** — a box-hosted agent cannot report or recover from its own box's unreachability.
  Peers are exempt from disposability/rebuild treatment but still get a C5-style
  postcondition check on their service.
* **LiteLLM target flips from LXC to kube** (deterministic, C0). The LXC deployment
  (LXC 105, model registered, key minting proven) was the REFERENCE implementation C0 was to
  re-target — it died with the 2026-08-27 rebuild, so C0 now deploys GREENFIELD to the
  already-staged k3s and its own C5 postcondition stands alone (no live-reference soak, no
  blue/green twin).
* **Terraform is the expert's write-down artifact, not an infra gate (C6, softened).** The
  goal: the expert captures what it learned so it doesn't re-derive it — location
  `/srv/data/agents/<expert>/` unchanged, "skills may ship a starter" unchanged. For Chunk
  3's actual LXC/kube service targets the artifact is declarative capture in whatever form
  fits (script, manifest, .tf with remote-exec if the expert finds it natural).
* **Terraform becomes the BOOTSTRAP substrate driver — SUPERSEDES C6's "no Terraform
  consumer in Chunk 3" line (the substrate IS the consumer).** Each `bootstrap --kind`
  (proxmox-lxc, vultr-vps, hetzner-vps, k3s, litellm-kube) = one Terraform plan;
  `freehold bootstrap` stays the operator surface and executes the plans THROUGH the
  provisioning runner's exec (the runner's injected key remains the only door). Disciplines,
  all locked: (1) runner-exec only — never a workstation-side terraform with its own
  credentials; (2) operator-supplied secrets enter ONLY as runner-injected env
  (`TF_VAR_*`), never tfvars/provider-block literals; TF-GENERATED secrets (random_password,
  kube tokens, provider-returned keys) are acknowledged to live in state, so state is
  sensitive-by-default — encrypted backend or `/srv/data` at 0600; (3) host-sysadmin steps
  (kernel modules, sysctls, LXC features, cluster DNS pins) ride remote-exec inside the
  plans (C6's in-scope remote-exec decision); (4) the no-master-key acceptance gains a
  guard: after every apply, TF state contains none of the operator-supplied variable
  values; (5) service installs stay agent-improvised + write-down (unchanged).
* **Blue/green narrows unilateral destroy.** Once a service has a live, currently-serving
  instance, the expert's normal mode is never destroy-in-place: new version alongside →
  verify (C5) → flip a traffic pointer the expert owns → soak → remove old. Destroy-in-place
  stays available (D2: offer, don't perform) only for never-successfully-brought-up services.
  LXC = in-place rolling (second instance, same `/srv/data` mount, own reverse-proxy/upstream
  flip — proportionate for lightweight services; heavier services = a named scale boundary).
  k8s = single-cluster, two Deployments + Service-selector flip — a single-node k3s hosting
  both colors during a flip adds NO node. VM-targeted deploys are explicitly deferred (the
  provisioner simply doesn't offer VM; an expert asking for one gets "not available, pick
  LXC or kube").
* **Postgres comes into scope now** (deterministic pod config depends on prompt-as-DB-row;
  LiteLLM kube needs it): ONE instance, multiple logical databases (control_plane, litellm,
  per-client) per ROADMAP's "single cluster, many DBs" model. Runs as a kube Deployment with
  its durable volume PINNED to `/srv/data/k8s-volumes` (never the default local-path root —
  ARCHITECTURE's "durable PVCs never on the daemon root" rule, first application).
  Migrations via the CP-owned tooling pattern — no new admin surface.
* **Kube-slot mechanics:** on a single-node k3s, "give me a kube slot" = a dedicated
  namespace + ResourceQuota, created by the named peer via its own runner grant — the
  pod-world equivalent of "an LXC": bounded, revocable, handed to the expert.
* **Cost-gate criteria (Phase 0.11):** an operator approval gate applies iff the request is
  cost-bearing (paid cloud spend) or host-irreversible (box teardown). Local LXC creation /
  kube namespace+quota on already-owned hardware flows WITHOUT approval.
* **Slot descriptor (Phase 0.10):** what an expert actually receives, independent of which
  peer fulfilled it — architecture (amd64/arm), rough resource bounds (cores/RAM), compute
  kind (LXC with ssh access details / kube namespace + kubeconfig context), NOT peer identity.
  Until C0's cutover completes, LXC is the only offered kind; "pod" answers "not yet
  available, pick LXC" (same shape as the VM deferral).
* **G6's convergence is concrete, re-tasked onto the existing (shelved) chunk2-closeout
  live-verify harness** — extended, not reinvented: relay + CP + the named hardware peer +
  its grants + its memory + one running service (pihole) all present and correct after
  re-bootstrap. Un-shelving chunk2-closeout is the implementation vehicle for G6.
* **G7 explicitly un-shelves the workspace/git runner + a minimal image-build/push path**
  (the forcing function from the blue/green discussion): registry default = a local
  `registry:2` on the k3s node LXC; retention = bounded (last 3 images/state versions, named
  requirement, mechanics at G7 execution). No general CI/CD.
* **Wall-clock budget:** once agents are pods, natively enforceable as a pod
  deadline/TTL; for the LXC landing-strip experts the spawner-side timeout applies. A7's
  setsid/killpg fix stays REQUIRED for LXC-targeted installs (pod teardown contains pod
  targets — A7's container analog is free).

## Locked decisions carried over unchanged from v3

* Outbound default-deny via capability runners; safe/risky classification with the
  risky-install/risky-host sub-classes and risky-host CPA-only grants.
* Policy-level enforcement honesty (no network-level egress gate implied): harness-native
  web/browse/search AND shell/exec/file-write tools disabled at spawn (A5/A5b); MCP runner
  tools are the only sanctioned outbound; codex EXCLUDED (no tool-deny mechanism — SURFACE §4).
* Inbound default-deny via per-service private channels (9007/9000/9001, live-proven);
  channel membership IS the gate (403 at ingest); `--respond-to allowlist` is spawn-time
  state (STARTUP-ONLY — SURFACE §2) → CPA derives it from the roster at every (re)spawn,
  restarts the agent unit on membership change, and reconciles at CPA start (idempotent fold).
* Prompt-injection bounded by membership (operator + CPA only; buzz-acp DM hardening).
* Skills stay agent-improvised for the software layer (explicit supersession of POC.md's
  "skills instead of free-form shell"); readiness is postcondition-gated (C5), never
  agent-reported.
* A6 per-service install runners (generalized to "the expert's granted compute, LXC or
  pod"); A7 process-group-kill for runner timeouts (LXC case); risk_level metadata
  end-to-end (implemented, merged #72).
* Harness verdict: goose + claude-code comply; codex excluded (SURFACE §4).
* First skills: tailscale, pihole — deliberately NOT LiteLLM (C0 proves LiteLLM's
  deterministic path separately; it must not be the first case of an expert's free
  target-reasoning).

## Goal (one sentence)

Prove that real reasoning agents, under capability-brokering and inbound-gating, install and
operate services end to end — the first expert via the LXC landing strip (immediate, human-
facing), the architecture via deterministic pods — with every side effect brokered through an
audited, classed runner or contained to disposable compute.

---

## Phase 0 — Capability & risk research

01–06 RESOLVED (2026-08-23, deliverable `roadmap/POC_CHUNK3_SURFACE.md`) — capability runner
list + classes, --respond-to STARTUP-ONLY restart-sync, LiteLLM key path, harness matrix,
audit 48001+spool, prompt-injection record. New this revision:

* [ ] 07. **Base image variants.** Single generic agent base image (buzz-acp + harness)
      carries every needed harness: buzz-acp BUILDS (workstation, 52s, release binary
      verified 2026-08-24), goose 1.47.0 runs (harness LXC verified). Confirm no
      codex-only need exists (codex excluded) → lock the single image.
* [ ] 08. **k3s-on-librem re-verification.** HALF-DONE by the Pre-C0 configure stage
      (#120, live 2026-08-27): the unprivileged k3s LXC (102) with exactly this posture —
      `INSTALL_K3S_EXEC="server --kubelet-arg feature-gates=KubeletInUserNamespace=true"` —
      now boots + installs deterministically through `freehold configure`. REMAINING: the
      nginx pod through NodePort reachable from the PVE host (the 2.5 spike proved that
      half on the cloud hosts, 10.10.0.7; librem's NodePort path is unproven).
      PREREQUISITE for C0's apply.
* [ ] 09. **Postgres placement — LOCKED** (new locked decision above): single instance,
      multiple logical DBs, kube Deployment, durable volume pinned to `/srv/data/k8s-volumes`
      (never the default local-path root), CP-owned migrations.
* [ ] 10. **Slot descriptor surface — LOCKED** (new locked decision above): architecture,
      resource bounds, compute kind + kind-specific access, NOT peer identity; LXC-only until
      C0 cutover.
* [ ] 11. **Cost-gate criteria — LOCKED** (new locked decision above): approval iff
      cost-bearing or host-irreversible; local flows un-gated.

## Phase A — Capability runners (v3-unchanged)

A1–A7 as v3 (github/websearch/litellm flavors IMPLEMENTED + merged #72/#73 with risk_level
end-to-end). A6's "service LXC" language generalizes to "the expert's granted compute, LXC or
pod" — no runner-design change (targets are kind strings; pods change nothing in the
dispatch).

## Phase B — Inbound gating (v3-unchanged)

B1–B4 as v3 (RESOLVED for SURFACE §2). Default membership stays operator + CPA — confirmed
NOT widened by the onboarding inversion.

## Phase C — Skill framework v1

* [ ] C0. **k3s → LiteLLM, deterministic, operator/CPA-driven.** Sequenced AFTER
      Phase 0.12 (the durable plane — LiteLLM + Postgres are new builds born ON the plane,
      not migrations). The k3s substrate is
      staged (configure stage, #120 — install done live; 0.08's NodePort reachability
      proof is the remaining pre-apply gate); C0 = the litellm-kube
      apply (the #76/#78 plan) + Postgres (`/srv/data/k8s-volumes` pinned) + the master-key
      re-mint via the litellm runner + surfacing the Services row. Sequenced AFTER Phase
      0.08's re-verification; NOT blocking Phase E's first expert (landing strip). No live
      LXC reference remains — C0's own C5 postcondition is the gate.
* [ ] C1. Skill schema: `target: lxc | pod | either` documented as a default HINT the expert
      may override (the inversion, recorded in schema docs).
* [ ] C2. Disposability per compute type: LXC installs `/srv/nobackup` + `/srv/data/<service>`
      (unchanged); pod installs use the deterministic-pod model (recreate pod, config
      re-pulled from Postgres, no local state by design).
* [ ] C3. First skills: tailscale, pihole (unchanged).
* [ ] C4. Readiness 🟢/🟡/🔴, postcondition-gated (unchanged).
* [ ] C5. Independent postcondition, unchanged from v3 (implemented as `verify:` — per-skill
      external check run by CPA via a runner after the agent reports done).
* [ ] C6. Declarative-capture artifact, softened per the new locked decision (service layer:
      script/manifest capture; Terraform-proper reserved for cloud-resource graphs).
* [ ] C7. **Terraform bootstrap substrate** (per the new locked decision): `terraform/` plans
      per kind (proxmox-lxc, k3s, litellm-kube, later vultr-vps/hetzner-vps), executed via
      the runner's exec with the four disciplines; G6's teardown/rebuild runs on
      `terraform destroy` / `terraform apply` + the verify harness.

## Phase D — Retry/escalation budget

D1–D3 unchanged from v3, plus the pod-deadline enforcement note (new locked decision) and:

* [ ] D4. Blue/green narrows destroy-in-place: once a service has a live instance,
      budget-exceeded escalation applies to GETTING THE NEW VERSION WORKING; the old,
      currently-serving instance is torn down only after a successful flip + soak window.

## Phase E — Named hardware peers + expert onboarding (rewritten)

* [ ] E1. Bootstrap deploys the first named hardware peer (`@<operator-named>` — the PVE
      host on the test world) — the relay duplicate of the Chunk-2 local provisioning expert
      (duplicate-not-promote, second application). Runs on the LXC harness for now (landing
      strip); migrates to a pod after C0's cutover. Its skill = capacity fulfillment for its
      own box; it does NOT own that box's lifecycle.
* [ ] E2. CPA creates the new expert identity with only a service-name + install prompt — no
      target pre-assigned.
* [ ] E3. The expert reasons about its own target (overriding the skill hint if warranted),
      asks CPA; CPA resolves the named peer (+ approval gate per Phase 0.11) and delegates.
* [ ] E4. The named peer provisions (LXC or kube namespace+ResourceQuota) via its runner
      grant, hands access back through CPA to the expert (slot descriptor, Phase 0.10).
* [ ] E5. Expert installs via its skill (Phase C), verified via C5, capturing what it learns
      per C6.
* [ ] E6. Secondary-relay onboarding remains explicitly out of this chunk (unchanged).

## Phase F — LiteLLM

F1 SUPERSEDED by C0 (kube-deployed, deterministic) — this phase confirms F2/F3 against the
kube deployment, nothing fresh. F2 operator-config unchanged. F3 per-agent keys minted by
the CP via the litellm runner (proven live) and surfaced via pod `envFrom` Secret (replaces
the retired colocated-runner-on-shared-LXC mechanism).

## Phase G — Acceptance / real dogfood UAT

* [ ] G1. The @tailscale/@pihole experts install + configure via skills end-to-end, with
      capability runners + channel gating enforced — run against the LXC-harness-hosted
      first expert, NOT gated behind C0.
* [ ] G2. Unscripted UAT: an operator asks `@pihole` in Buzz and gets a correct result — PVE
      test world first; home dogfood recorded, not an acceptance blocker.
* [ ] G3. Non-granted capability refused + non-member channel write gets the 403.
* [ ] G4. Budget-exceeded escalation: agent stops, reports, offers (never performs) a
      rebuild for non-ephemeral state.
* [ ] G5. Confidently-wrong acceptance: agent reports success on a broken install →
      readiness stays 🔴 (C5 postcondition fails).
* [ ] G6. **Full-cycle repeatability** via the EXTENDED chunk2-closeout harness: bootstrap
      (relay + CP + named peer live) → operator manually requests an LXC from the peer and
      sets up pihole by hand (fulfillment path isolated from expert target-reasoning) → full
      teardown → re-bootstrap → CONVERGENCE = relay + CP + peer + grants + memory + pihole
      all present and correct.
* [ ] G7. **Blue/green proof:** new LiteLLM version via the kube two-Deployment selector flip
      with zero dropped requests across the flip, old Deployment retained through soak.
      UN-SHELVES the workspace/git runner + minimal image-build/push (registry:2 local
      default, bounded retention).

## Open items carried forward from Chunk 1–2.6.1

Unchanged from v3 (B2 live-account leg, Vultr relay-scoped runner identity, relay-terminated
TLS, consolidated live-acceptance harness [now the G6 vehicle], CP-restart memory
persistence, audit-as-channel-messages, secondary-relay onboarding).

## Explicitly not in Chunk 3

Unchanged from v3, plus: multi-cluster k8s (single cluster, occasionally two nodes hosting
both colors during a flip, is the ceiling); VM-targeted provisioning or blue/green (deferred);
image-build/registry automation beyond the workspace/git-runner minimal path (no CI/CD
system).
