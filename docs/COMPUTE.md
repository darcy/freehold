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
not a rewrite. Proxmox is the leading provider today and the only one shipped; the seam is
deliberate and the rules below hold regardless of it.

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

*   **What a provider owns** — the classes of command that differ per substrate:
    *   **Guest lifecycle:** create (with its durable mounts and network attached at birth),
        list, destroy, and allocate identity (ids, names).
    *   **Guest access:** run a command inside a guest or on the host, read a guest's
        address and mounts. The provider owns the command wrapping; the caller owns the
        transport (the runner's MCP client, or direct SSH before any runner exists).
    *   **Storage:** inventory what's available, classify it for safety, create and mount
        the durable volumes, snapshot and roll back, export.
    *   **Networking:** a guest's interfaces, tags, and addressing — what lets the gateway
        and internal subnet exist.
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
*   **Where it is narrow today:** the formal `Provider` interface covers guest exec, list,
    address, mounts, and destroy; the create, storage, snapshot, and teardown engines live in
    the provider package and are reached by the composition roots directly. Folding them behind
    the interface, and shedding the two Proxmox-flavored methods it still carries
    (`local-lvm` status/repoint), is part of landing the second provider.

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
    transient root-SSH door.
*   **Compute's grants:** raw host access (`pve-ssh-root` today) and cluster-admin kube access.
    Its `create-lxc` skill creates a guest, installs a runner-client on it, and hands the door
    to the CPA to provision.

## Known gaps

*   Only one provider ships; the Vultr/Hetzner drivers exist but are unwired, so the seam is
    untested against a second substrate.
*   Core guests (relay, k3s, gateway) have no runner-client.
*   World-config can lose create params after an update; a later teardown → rebuild may fail
    at guest creation.
*   Pre-terraform kube workloads can't be adopted; a re-adopted plane's `terraform destroy`
    can reach another world's cluster (needs a cluster-identity guard).
*   The transient uninstall can't destroy running guests (no stop-first).
*   No per-guest or per-pod resource bounds (no `--cores`, no requests/limits, no quota).
*   Create-device storage is deferred; "kube slot" provisioning is a prompt claim with no code.
*   The doors are intent-and-audit boundaries, not hard containment on a shared host.

## Future

*   **A second provider** — a VPS (Vultr first, then Hetzner) — the proof that the seam holds:
    same orchestration, new `providers/<substrate>/`, and the formal interface widened to cover
    create, storage, snapshot, and teardown.
*   **Runner-client in every guest**, kept fresh by `freehold update`.
*   **Kube slots** (namespace + ResourceQuota) via the CPA's peer-fulfillment pattern, with
    postcondition-gated readiness and budget-exceeded-as-escalation.
*   **Terraform per kind**, the substrate moved off exec-first shell onto a real terraform provider (or a clone-based template), the relay stack
    as Terraform, and one-time infra migrations through the runner.
*   Monitoring and a resource baseline; sleep/wake for idle agent pods; blue/green with a
    local registry; multi-box + Ceph.
*   Guest names keyed on the Buzz domain; a freehold-namespaced path for adopted relays.

## Where the code is

`platform/provisioning/` (seam, plane math, shared box engine), `providers/proxmox/`
(create, storage, `drive/`, `teardown/`), `control-plane/api/cpbuild/` (build stages +
`terraform/`), `freehold-cli/{install,uninstall,teardown}/`, `agents/compute/`.
