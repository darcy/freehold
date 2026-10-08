# Compute — the box, its guests, and the substrate

**Owner:** the Compute department (`agents/compute/`). **Scope:** the box itself — CPU,
RAM, disk, guest and kube provisioning, remote hosts, and the build/teardown engines.
What runs *on top* of compute belongs to whichever agent created it; AI hardware is AI's.

## The idea

A world is a small set of guests on one host, converged by `freehold build` and removable
and rebuildable without touching durable data. All of that orchestration is
**substrate-independent**: it says "create a guest with these mounts and this network,"
never "run `pct create`." Everything substrate-specific sits behind a **provider seam**
(`providers/`), so a new substrate — a VPS, a cloud, another hypervisor — is a new provider,
not a rewrite. Proxmox is the lead/default and Vultr is the second (the created-host
shape: a cloud instance running PVE); the seam is deliberate and the rules below hold
regardless of it.

```
   freehold CLI · CP build engine       ← composition roots: own the sequence
                │ inject a provider
                ▼
   platform/  (provider-independent)    ← converge, plane math, stages, deploy helpers
                │  Provider interface   ← never imports providers/, never names a substrate command
                ▼
   providers/<substrate>/               ← the only place substrate commands live
                │
                ▼
   host  →  guests: gateway · cp · relay · k3s     (everything above the guests —
                                                    k3s, services, agents — is identical
                                                    on every substrate)
```

## The abstraction

*   **Two seams, one rule.** The formal `Provider` interface is the GUEST
    engine's substrate (guest exec/list/address/mounts/destroy, the VM-aware
    next-vmid pick, the stop command the erase path needs); the
    `HostProvider` interface is the HOST-provisioning seam — how a world's
    host comes to exist and what the operator must supply for it. Both live
    in `platform/provisioning`; the create, storage, snapshot, and teardown
    engines remain in the provider package, reached by the composition roots
    directly (folding them behind the interface, and shedding the two
    Proxmox-flavored methods it still carries — `local-lvm` status/repoint —
    is part of landing the third substrate).
*   **The host provider owns its needs.** `Needs()` declares EVERYTHING the
    operator must supply — the credential (secret: prompted no-echo in the
    guided flow, env-var fallback headless, never stored), the plain answers
    (region/plan/os), and which of the installer's core inputs it consumes
    (proxmox: the host address, the edge's LAN address, the storage-create
    consent; vultr: none of those — it derives the address and the edge IP
    from the instance it creates). The installer asks exactly `Needs()`
    against the registry (`providers/registry`) — no substrate names, asks,
    or defaults live in the installer. `Defaults()` supplies the substrate
    defaults the flags may override (storage/bridge/relay-gw).
*   **The provider owns the door and the lifecycle of ITS host.**
    `Prepare` mints (vultr: the instance born with the door key authorized +
    the PVE-on-Debian install) or re-adopts (verify the recorded instance;
    only a definitive 404 re-creates — anything else fails loudly rather
    than minting a second billed instance beside the plane).
    `InstallDoorKey` gets the substrate key onto the host (vultr: appended
    over the key the instance was born with; proxmox: the paste gate).
    `Destroy` removes a created host (vultr: the API destroy behind
    `uninstall --destroy-host`; proxmox: a no-op — the host is the
    operator's).
*   **What a provider owns** — the classes of command that differ per substrate:
    *   **Guest lifecycle:** create (with its durable mounts and network attached at birth),
        list, destroy, and allocate identity (ids, names).
    *   **Guest access:** run a command inside a guest or on the host, read a guest's
        address and mounts. The provider owns the command wrapping; the caller owns the
        transport (the runner's MCP client, or direct SSH before any runner exists).
    *   **Storage:** inventory what's available, classify it for safety, create and mount
        the durable volumes, snapshot and roll back, export.
    *   **Networking:** a guest's interfaces, tags, and addressing — what lets the gateway
        and internal subnet exist; and (host provider) whether the HOST itself is the
        gateway (`HostsGateway` — a created cloud host is, a reached LAN host runs the
        gateway guest).
    *   **World removal:** the teardown engine, ordered and verified.
*   **What a provider never owns:** sequencing. There is no `provider.Install()`; install,
    build, teardown, and uninstall are orchestrators that decide the order and inject the
    provider. This keeps every substrate on the same lifecycle.
*   **The seam is enforced.** `platform/` never imports `providers/` and contains no substrate
    command strings; a guard test fails the build if either happens. Providers import
    `platform/` and `contract/`, never the reverse.
*   **Capability is explicit, not assumed.** Substrates differ in what they can do — a cheap
    snapshot primitive exists on ZFS/LVM-thin but not on a plain VPS disk — so verbs declare
    what they need, and fall back (restic) where a substrate can't (`docs/DATA.md`).

## How a world is built

*   **Guests** — gateway, cp, relay, k3s — are unprivileged guests named `<profile>-<role>`,
    created by the provider with their durable mounts attached at birth. Install creates the
    gateway and CP; the CP's build creates relay and k3s. Terraform only adopts them.
*   **Storage** is chosen by a safety classifier that never erases a dirty disk; the plane stage
    is idempotent and runs every converge (what lives there: `docs/DATA.md`).
*   **k3s + Terraform:** k3s comes up via an idempotent script; services (Postgres, LiteLLM,
    Caddy, the kube doors) are real `kubernetes`-provider Terraform; the substrate itself stays
    exec-first. Kube-door tokens are re-sealed every build.
*   **Teardown / rebuild:** `teardown` is CP-preserving — it removes relay and k3s and keeps
    the CP, gateway, plane, config, and data. `uninstall` removes the CP and gateway too, and
    `--remove-data` the datasets. Both work from a thin box or against a dead CP over a
    transient root-SSH door — except `--remove-data`, which needs the build box.
*   **Compute's grants:** raw host access (`pve-ssh-root` today) and cluster-admin kube access.
    Its `create-lxc` skill creates a guest, installs a runner-client on it, and hands the door
    to the CPA to provision.
*   **Kube slots** — a custom agent's kube access — ride the same machinery: one
    namespace holding an SA bound to a full-access-within-the-namespace Role
    (never cluster scope), optionally quota'd. Compute carves the slot through
    `kube-api-root` (the carve is the department's audited leg, so a kube
    cluster on a remote box needs no redesign); the CP verifies the slot,
    reads its SA token and seals it to a dynamic `kube-api-<slot>` door (live
    immediately, no console fill); the record is the spec the build re-creates
    the slot from — and re-seals the token from — on every rebuild, exactly as
    `doors.tf` does for the static doors. Retiring the door leaves the
    namespace (its workloads are the grantee's; deletion is Compute's, and
    cascades).

## Known gaps

*   Hetzner is unwired (the third substrate); the created-host seam has one
    implementation so far. The VPS world's storage is the dir backend (no
    ZFS/VG to detect), so the formal guest `Provider` interface still carries
    the two Proxmox-flavored methods (`local-lvm` status/repoint), and the
    create, snapshot, and teardown engines remain reached by the composition
    roots directly.
*   Core guests (relay, k3s, gateway) have no runner-client.
*   World-config can lose create params after an update; a later teardown → rebuild may fail
    at guest creation.
*   Pre-terraform kube workloads can't be adopted; a re-adopted plane's `terraform destroy`
    can reach another world's cluster (needs a cluster-identity guard).
*   `uninstall` leaves the cp-verb key's host line behind: the key cleanup collects the
    substrate + co-located runner keys, never the data-verbs `cp-verb` key
    (`freehold-<world>-cp-verb` in the host's `authorized_keys`). The private half lives in
    TWO places — the box's `profiles/<name>/cp-verb-key` (wiped with the profile) and the CP
    guest's durable plane (`/srv/data/cp/verb-ssh.key`, shipped at deploy), which a default
    uninstall KEEPS (only `--remove-data` drops it) and the off-site backup set covers. So on
    a default uninstall the host line + its private key both survive in the world's own
    records — a working credential pair, not an orphan; the exposure is a leaked backup
    yielding host root SSH. Only after `uninstall --remove-data` is the leftover line a true
    orphan. Adding the cp-verb key to the uninstall's key refs is the cleanup —
    security-relevant, not hygiene.
*   **The re-adopt door: uninstall removes the host's `authorized_keys` line and the
    provision-REUSE path never re-prints it.** An uninstall strips the box's substrate line
    from the host; a re-install then reuses the runner package ("already exists" — no fresh
    key printed, no door gate) and fails the transient door check with an EMPTY message (that
    path lacks the served-runner path's recover/print affordance). Live-proven three times on
    the gateway reinstall round. Fix: the reuse path re-derives + prints the door line, and
    the transient verify carries the same recover/print affordance.
*   **The fresh-install CP package is a wholesale clone of the box's runner package** — so a
    later re-adopt's substrate rotation drops the BOX's own line as "the old one" (the same
    key body in both packages; the box's door dies mid-reinstall, the adopt fails on the dead
    transport). Happened on every first gateway re-adopt. Fix: ship a FRESH sealed substrate
    credential to the CP at first deploy (or exempt the box's own line from the rotation's
    deauthorize).
*   The transient uninstall can't destroy running guests (no stop-first).
*   No per-guest or per-pod resource bounds (no `--cores`, no requests/limits, no quota).
*   Create-device storage is deferred.
*   A kube slot's token read is substrate-local (`pct exec` into the k3s
    guest); a kube cluster on a remote box needs a credential-source seam
    (the carve already rides Compute's door unchanged).
*   The doors are intent-and-audit boundaries, not hard containment on a shared host.

## Future

*   **Hetzner, the third substrate** — the same driver shape as Vultr (an
    `api-hetzner` access mode); the seam already held once, this is the
    second proof.
*   **Runner-client in every guest**, kept fresh by `freehold update`.
*   **Terraform per kind**, the substrate moved off exec-first shell onto a real terraform provider (or a clone-based template), the relay stack
    as Terraform, and one-time infra migrations through the runner.
*   Monitoring and a resource baseline; sleep/wake for idle agent pods; blue/green with a
    local registry; multi-box + Ceph.
*   Guest names keyed on the Buzz domain; a freehold-namespaced path for adopted relays.

## Where the code is

`platform/provisioning/` (both seams, plane math, shared box engine),
`providers/proxmox/` (create, storage, `drive/`, `teardown/`, the reached-host
provider), `providers/vultr/` (the created-host provider: instance lifecycle +
the PVE-on-Debian install), `providers/registry/` (name → provider),
`freehold-cli/{install,uninstall,teardown}/`, `control-plane/api/cpbuild/`
(build stages + `terraform/`), `agents/compute/`.
