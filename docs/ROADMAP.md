# Roadmap — The AI-operated Appliance

Product: an open-source appliance — Proxmox VE (or a VPS) + Kubernetes, a Buzz relay
control plane, and an agent that installs/configures self-hosted OSS via a skill
framework. Vision: "reclaim the future we were promised" (`docs/VISION.md`). The locked
model (relay-as-scope, agent = brain / runner = hands, secrets as a provisioner, the
two-tier agent org) is in `docs/ARCHITECTURE.md`.

This is the ordered view across domains. It carries order, the MVP definition, and the
cross-domain milestones; every item points at the section of the domain doc that holds
its detail. The released version is the latest GitHub Release (never restated here).

| Domain | Doc |
| --- | --- |
| Core platform (CP, runner, grants, relay, console/CLI/TUI, skills, product) | `docs/FREEHOLD.md` (+ `docs/ARCHITECTURE.md`) |
| AI — LiteLLM, the agent runtime, AI hardware | `docs/AI.md` |
| Network — gateway, edge, public path | `docs/NETWORK.md` |
| Data — durable plane, snapshot/export/backup | `docs/DATA.md` |
| Compute — providers, guests, storage, k3s | `docs/COMPUTE.md` |

## What is live

The engine room (runners, the secret provisioner, connectors, readiness, grants), the relay
scope (real Nostr membership, relay-persisted memory, the durable plane), a reasoning CPA
that creates agents, the four departments and their runners, grants on the fly, durable
agent workspaces, the freehold-subnet gateway, and the data plane (snapshot, export, backup).
A world converges on one `freehold build`.

## Now

*   **Agents that ship code** — workspace LXC, durable commits in Buzz's git, a GitHub grant
    (`docs/AI.md`, `docs/COMPUTE.md`).
*   **Harden the base** — console as a systemd unit, runner crash restart, relay roster
    reconcile after redeploy, the live-grant roster break (`docs/FREEHOLD.md`).
*   **Scoped per-agent LiteLLM keys** and a resource baseline (`docs/AI.md`); **runner-client
    in every guest** (`docs/COMPUTE.md`).

## Next

*   **Agents deploy via Kubernetes** — kube slots, skill schema, readiness postconditions,
    the department check-in hook (`docs/COMPUTE.md`, `docs/FREEHOLD.md`).
*   **A VPS provider** and the gateway on a cloud host (`docs/COMPUTE.md`, `docs/NETWORK.md`).
*   **Backup scheduling** (`docs/DATA.md`); **Network's first skills** (`docs/NETWORK.md`);
    a **guest inventory** (`docs/FREEHOLD.md`).

## Later

*   **The North Star** (below); **Pangolin** and agent-operated exposure; **local AI hosting**;
    the full console, mobile, security hardening, multi-box + Ceph; product direction
    (`docs/FREEHOLD.md`).

## Cross-domain milestones

Each is a proof point no single domain owns; the items it needs are named in the domain
docs.

1.  **Agents ship code (LXC).** A CPA-routed request → a named peer provisions an LXC
    workspace → the agent commits a real change, verified durable in Buzz's git and/or pushed
    to GitHub → it deploys a service using what it committed. The credential surface
    (generic `exec()` vs. a carve-out) is decided explicitly.
2.  **Agents deploy via Kubernetes.** The same loop against a kube namespace; a skill's
    `target:` never widens authority; blue/green flips drop no requests; a failed `verify:`
    keeps readiness 🔴.
3.  **Anywhere.** The full lifecycle passes identically on Proxmox and a VPS, same gateway role.
4.  **The North Star: portable backup & hardware migration.** Back up off-site, lose the
    hardware, restore onto different hardware or a different provider — same identity,
    memory, grants, services. It proves the whole model (durable plane, relay-as-scope,
    deterministic rebuild) under the failure that matters, and doubles as a dogfood clone of
    production. Needs a restore verb and a VPS provider (`docs/DATA.md`, `docs/COMPUTE.md`).

## MVP — the public release

The public release builds on everything live and adds the Kubernetes substrate as a
first-class citizen. Core promise: **a user runs one app, points it at Proxmox (or a
VPS), and gets an agent that manages services via skills on a Kubernetes fabric — with the
control plane, console, and a management relay.**

**In:** everything live; the k8s substrate (agent pods, LiteLLM, Postgres, CP services as
workloads); Proxmox plus a VPS/cloud host driver; the runner connector model (self-hosted and
external); skill framework v1 (tailscale, pihole) on LXC and k8s; a minimal console; bootstrap
onto Proxmox or a VPS. **Out:** a shipped box, local CP provisioning other boxes, the full
monitoring console, mobile, multi-tenant/multi-box, the ISO guide.

**Done when:** a fresh install yields a control plane live in k8s; the agent manages services
across SSH / Vultr / Backblaze / LXCs / pods with 🟢/🟡/🔴 readiness; it installs tailscale and
pihole on both LXC and k8s; secrets follow the provisioner model; LiteLLM routes models; and it
runs safely on a PVE test host and the home box (dogfooded daily).

Numbering: `0.x.y` stays semver-ish pre-MVP; `1.0.0` is reserved for the public release.
Environments and the release flow (dev → test → prod, pre-release → test → promote) are in
`AGENTS.md`.
