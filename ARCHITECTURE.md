# The AI-operated Appliance — Architecture

Product: an open-source "box + install script" (Omarchy-style) that lands a  
Proxmox VE + Kubernetes appliance, with Buzz Relay as the control plane and an  
agent that installs/configures self-hosted OSS via a **skill framework**.  
Covers personal, family, and business — from a home box to the cloud.

## Relay-as-scope — one relay, one control plane (the core organizing model)

**A control plane exists for exactly ONE relay.** Its only job is to manage that relay's  
**agents, runners, and secrets** for that relay. The relay is the scope for runners,  
secrets, and memory.

*   **No nested / peer relay machinery.** There is no "control plane of control planes"  
    problem to solve — a control plane is intrinsically single-relay, so nothing nests.
    
*   **The appliance install REQUIRES Buzz and creates a NEW management relay** — the  
    control plane's scope. That relay holds the service-management agents.
    
*   **A user's EXISTING relay is just a service.** If the user already has a Buzz relay,  
    onboarding it is identical to onboarding any external service (same as algolia):  
    create a relay runner (connector to that relay's API) → validate → create an  
    `@myrelay` expert agent → grant → add to a channel. No nested-scope machinery.
    
*   **Bootstrap is self-scoping:** first install BRANCHES — CREATE a new Buzz relay on the
    target, or **attach-existing** (attach to the operator's existing relay — its PRIMARY/management relay
    pre-existing at bootstrap; this is NOT the "existing relay = service" bullet below,
    which is Chunk-3 SECONDARY-relay onboarding) → create/deploy the control plane onto that
    relay's scope → create a runner that can reach and manage both the relay and the CP →
    the CP adds itself as a member. Create-new: the CP literally creates its own scope and
    the runner it needs to operate it. Attach-existing reaches the same end state via the
    relay's own member management; relay-creation is a skippable, idempotent step. The
    

## Buzz — the user-facing surface AND required substrate for the fabric

**Buzz is required** (peer model: the control plane manages Buzz as a child service, but the  
agent fabric lives in the relay). It is the "living room" where users interact + the memory  
backbone for agents.

*   Self-hosted Nostr relay workspace; humans + agents as members (own keys, identity-scoped,  
    audit trail). Backend: Postgres, Redis, S3/MinIO. Rust workspace.
    
*   Agent surface: buzz-cli (JSON in/out), buzz-acp (ACP↔MCP harness), buzz-agent. Model-agnostic.
    
*   Deterministic k8s agent pods (bare Pods, digest-pinned sprig, per-attempt envFrom Secret,  
    no mgmt channel by design, idle auto-reap, emptyDir, no PVC v1) — PUBLIC RELEASE target.
    
*   **Agent placement:** POC = scripted agents joining the relay via their own NIP-42 client
    (the `buzz-acp` harness targets LLM agents and is the Chunk-3+ path — see
    `roadmap/BUZZ_SURFACE.md`); public release = k8s pods. Runners are separate — see the
    Runners section below.
    
*   **We do NOT build a chat UI / agent-management surface** — Buzz provides it.
    

## Agent fabric — master agent + per-service expert agents

Single fabric, memory anchored to the relay, agents scoped to the relay:

*   **Master / control agent (CPA)** — appliance-level: setup, orchestration, fixing Buzz,  
    coordinating experts, and **onboarding** (creating runners, granting agents, validating).  
    Lives in the management relay; also drives the control plane (same memory via relay).  
    Emergency tool to fix Buzz + primary setup tool.
    
*   **Per-service expert agents** (`@litellm`, `@algolia`, `@pihole`, `@vultr`, `@b2`…) — each  
    expert in one service, spawned on onboarding, scoped to the relay. Master delegates; users  
    talk to them directly in Buzz.
    
*   **Onboarding pattern (happy path):**
    1.  User tells CPA to set up a service (e.g. "set up LiteLLM on an LXC" or "manage my  
        existing algolia at ip X with key Y").
        
    2.  CPA provisions/points at the target → creates a **runner** holding the credential  
        (e.g. ssh key to the LiteLLM LXC, or the algolia API key).
        
    3.  CPA validates the runner connects → readiness goes 🟢.
        
    4.  CPA creates the **buzz agent** (e.g. `@litellm`, `@algolia`), grants it permission to  
        use that runner, writes an **AGENTS.md** (the agent's job: manage this process, be an  
        expert in it, how to use the runner).
        
    5.  CPA connects it to Buzz, adds it to a channel (`#litellm`), and tells it (via Buzz) to  
        install / verify and report back.
        
*   **Mirroring (conceptual target):** talking to master agent in control plane mirrors as DMs in  
    the relay; if relay down, sync later. (Resolve later — not a POC blocker.)
    

## Runners — privileged generic executors (the connector bridge)

**Runners are SEPARATE from Buzz** (the control plane installs them; plumbing works independent  
of the fabric). Runner = an MCP tool server on the target side, privileged, reaching  
ssh-machine | vultr | backblaze | proxmox | lxc | external/saas.

### Generic exec — the agent reasons, the runner executes

*   **Agent = the brain.** It figures out intent and composes the actual command.
    
*   **Runner = dumb, privileged hands.** It owns the connection (ssh session, key, reach) and  
    executes the command the agent hands it **verbatim**.
    
*   **NO semantic tools.** There is no `tail_log()`, no `list_directory()`, no `create_server()`.  
    One generic primitive: `exec(cmd, target)` — "run this exact command on that target, return  
    output." `ls`, `tail`, `docker ps` — the agent writes them.
    
*   **Streaming is a property of exec, not a separate tool.** Long-running/live commands  
    (`tail -f`, installs, builds) return output **over time** via a stream (pull-style chunk  
    buffer in the runner; push-style SSE possible later). The agent can watch progress.
    
*   **Transport:** MCP over HTTP/SSE for discrete calls. The runner keeps persistent ssh  
    connections (ControlMaster/ControlPersist pool) so repeated commands are cheap — connection  
    lifetime is a runner-internal concern, never the agent's. WebSocket only if a real  
    bidirectional-interactive case appears (rare for agents).
    

### Runner API (minimal contract)

`list` (what targets can I reach), `exec(cmd, target, stream?)`, `config`, `status`, `snapshot`.

*   **Readiness = the runner's OWN self-check**, not the agent's. Green/yellow/red = "runner can  
    reach its service with its creds." Like every MCP call, the readiness probe is signed and  
    GRANTED — the runner fails closed (Phase D). The CP's console holds its own agent identity,  
    auto-granted to runners it provisions; readiness is never an unauthenticated side door.
    

### Runner identity & secrets (provisioner model)

*   Runner = **a Nostr identity** (auth/membership; can be revoked at the relay) + a **separate**  
    **encryption keypair** (injected as an env var / mounted secret by the CP at provision time).  
    The Nostr nsec is for _signing/membership_; a distinct key handles _secrets_ (keeps blast  
    radius small; nsec stays pure identity).
    
*   **CP = secret PROVISIONER, not a vault.** At onboarding the CP generates the runner  
    identity, encrypts each secret TO the runner's encryption pubkey, ships the ciphertext to  
    the runner's config, and injects the runner's private key. No decrypt-on-demand, no master  
    key stored centrally.
    
*   **Runner holds only ciphertext + its own private key.** It decrypts locally, uses in memory,  
    forgets. Plaintext never on disk, never in agent context.
    
*   **Per-runner containment, no master key.** A compromised runner leaks only its own secrets  
    (not the relay's, not other services'). A compromised CP reveals nothing readable — it holds  
    only ciphertext. This trades the old "runner holds zero secrets at rest" for per-runner  
    containment; the better trade for personal/family-first on owned hardware.
    
*   **On-demand decryption is a FUTURE opt-in**, NOT part of the shipped no-master-key model.  
    If adopted for runners on external/less-trusted targets (a cloud VPS we don't control), the  
    store would hold each runner's OWN encryption key — per-runner containment only, never a  
    master key; the CP still holds nothing but ciphertext. Secret-resolution is entirely  
    internal to the runner either way, so a runner can be swapped between "decrypt locally" and  
    "ask the store" without touching the agent or the tool contract. Tracked in Future items  
    ("Vault for dynamic secrets").
    

### Revocation & audit

*   **Revocation = two levers:** (1) revoke the runner's Nostr membership at the relay → cuts off  
    its ability to _act_ (be called as a valid member) — the "cut-off" lever; (2) **rotate** the  
    secret (re-encrypt to a fresh key / re-issue to remaining runners) → the "erase" lever.  
    Membership revocation does not un-decrypt ciphertext already on disk; rotation does.
    
*   **Audit = the runner's signature job.** Because exec is raw, the runner **signs a Nostr**  
    **event for every executed command** (agent pubkey, target, command, result) into the relay —  
    an identity-scoped, queryable audit trail. Fits relay-as-scope + runner-as-Nostr-identity.
    

## Secrets — provisioner model (root of trust = the user; scoped to relay)

**The user is the root of trust.** Two distinct secret paths:

*   **Agent pod** ← its OWN Nostr identity (nsec; needs it to sign events / be a member)
    
*   **Runner** ← SERVICE credentials (agent can trigger use, NEVER read; out of model context)
    

Flow: CP provisions runner (generate identity, encrypt secret to runner pubkey, ship ciphertext,  
inject private key) → runner decrypts locally on use → uses in memory → forgets. Plaintext never  
on disk, never in agent context. Revoke membership (cut off) + rotate (erase). Crypto tool  
(age/libsodium) is orthogonal; a separate encryption keypair (NOT the user nsec — nsec stays pure  
identity/auth).

## Deterministic agent pods + injected secrets (mirroring Buzz kubes)

Buzz's deterministic pods are the model for disposable agent fleets (public release). "Use  
without seeing": agent references a credential BY NAME; runner resolves it. Plaintext never  
in context window → no exfiltration even if agent compromised/confused.

## Agent placement — two categories, not one

Public release doesn't put every agent in the same kind of pod. Two distinct placement categories:

* **Ephemeral service-expert pods** — for managing external/remote things (Vultr, B2, LiteLLM, Buzz itself). Classic bare-pod model: `emptyDir`, no PVC, idle auto-reap. Stateless by design — nothing on the pod matters if it dies, because everything that matters lives behind a **runner**, not on the pod. Agent restarts freely to pick up a prompt change.

* **Long-lived team/project environment LXCs** — for coding-agent work: dev and QA agents with real repo access, running `git`/`docker compose` directly against checked-out code. These are **not** k8s pods. `buzz-acp` + the harness (Goose/Claude Code/opencode/etc.) run natively on the LXC's own filesystem — this matches Buzz's own supported "Relay Bridge" pattern for headless server-side agents. Multiple agent identities (dev, QA) can be granted onto the same LXC via coarse grants, sharing one filesystem/DB/compose stack intentionally — but different teams/projects get separate LXCs so DB migrations and compute never collide across them.

**Prompt sourcing is the same in both cases and is intentionally NOT volume-paired to the agent.** AGENTS.md / system prompt lives as a row in the `control_plane` Postgres database (see Database model below), authored/updated by CPA. On every pod start or LXC agent restart, the current prompt is pulled fresh from that row — via ConfigMap mount (pods) or direct read at process start (LXC) — and handed to the harness at session creation. This means a prompt tweak is just "CP updates its row, restart the agent process" — no volume to keep in sync, no drift risk, and the source of truth is something already in the storage-reliability plan.

**The runner boundary looks different in each category, and that's intentional:**

* In the ephemeral case, the runner may genuinely be a separate process/target — it's brokering to a remote API the pod has no other way to reach.
* In the team-environment case, **the runner is colocated on the same LXC as the agent.** It is not a network hop. It's a local daemon (Unix socket) that holds decrypted git-deploy-key / registry-token secrets in memory only, exposed to the agent's own `git`/`docker` invocations via their native credential-helper protocols:

```
git config --system credential.helper '/usr/local/bin/team-runner-cred'
# helper calls out to /run/runner.sock — local only, plaintext never on disk
```

This preserves "secrets never in agent context" without forcing coding work through a remote `exec()` hop — the agent runs commands locally like a developer would; only the runner's own outbound call (fetching from the actual git remote / registry) ever leaves the box. Provisioning, rotation, and revocation follow the identical CP-provisioner model as every other runner.

*Harness working state (session history, tool caches — whatever a given harness accumulates beyond the checked-out repo itself) is NOT assumed durable via the Buzz relay.* Buzz's relay-native memory covers channel history and its own agent memory; it does not absorb the local state of externally-run harnesses. Anything that needs to survive a restart belongs on the LXC's own volume, covered by the storage tiers below — not assumed to be relay-backed.

---

## Storage & backup placement (per-service, driven by the skill's `needs:`)

Not every service's data belongs in the same place. Four placement tiers, each mapping onto the existing Proxmox / PBS / TrueNAS / Backblaze layering used for the appliance's own backup strategy:

| Tier | What lives here | Backed up via | Notes |
|---|---|---|---|
| **K8s host LXC/VM** | k8s binaries, OS | PBS (machine-level) | Standard rootfs backup, same as any LXC |
| **control_plane Postgres (PVC on k8s host)** | prompts, grants, service registry, audit | PBS + TrueNAS (scheduled) | Same bucket as any app database — important, not reproducible |
| **Ephemeral service pods** | agent runtime, scratch | *Not backed up* | `emptyDir`, fully reproducible from AGENTS.md + skill — same logic as excluding `/var/lib/docker` |
| **Team/project environment LXCs** | rootfs / `/srv/data` (repo, compose, runner secrets ciphertext) / `/srv/nobackup` (relocated container stores, caches, PVCs excluded by root pinning) | rootfs+`/srv/data` → PBS+TrueNAS; `/srv/nobackup` → excluded (as its own mount point, `backup=0` on the `mpN` entry) | Mirrors the Docker-host LXC pattern — the `/srv` split IS the backup config, implemented as two mount points |
| **Data-heavy services** (Nextcloud, Immich, media) | live user datasets under `/srv/data/<service>` | TrueNAS (mounted directly, not local NVMe) + ZFS snapshots + Backblaze off-site | Proxmox stays fast/small; service LXC mounts TrueNAS via NFS/iSCSI rather than storing data locally |

A skill declares which tier it needs, and CPA/the provisioning flow places it accordingly:

```yaml
name: install-nextcloud
target: lxc
needs: {database: true, volume: 20Gi, mount: truenas-nfs}
---
name: install-litellm
target: pod
needs: {database: true}   # no mount — ephemeral, no PVC
---
name: provision-team-env
target: lxc
needs: {database: true, volume: 40Gi, workspace_runner: true}   # colocated git/registry runner
```

Governing principle, carried over from the appliance's own backup design: **back up what matters, not what's easily recreated.** Anything derivable from a skill install (images, layers, model downloads, build cache) is excluded regardless of tier; anything that represents real work or real state (databases, checked-out repos with uncommitted changes, user data) gets the full PBS-plus-off-site treatment.

### Filesystem layout convention — `/srv/data` vs `/srv/nobackup`

The tiers above encode onto every LXC/VM as a single top-level split, so the backup config is one rule instead of a per-path carve-out list:

```
/srv/
├── data/                 # durable application data — THE backup set
│   ├── nextcloud/        # live user datasets (TrueNAS-backed via NFS/iSCSI)
│   ├── agents/           # durable agent/harness state that must survive restarts
│   ├── <service>/        # anything with needs.volume; repos, compose, runner
│   │                     #   secrets ciphertext
│   └── ...
│
└── nobackup/             # reproducible / disposable — its MOUNT POINT carries
                          #   backup=0 (the flag lives on the mpN entry, not
                          #   on a directory — see below)
    ├── docker/           # container stores: daemon roots RELOCATED here (or
    ├── containerd/       #   bind-mounted), so the exclude is structural, not
    ├── rancher/          #   a fragile path list — but ONLY the daemon root:
    │                     #   k3s/rancher state is reconstructible from
    │                     #   control_plane Postgres (deterministic pods +
    │                     #   config-as-data); durable PVCs are pinned under
    │                     #   /srv/data, never the daemon root
    ├── caches/
    └── scratch/
```

*   **The backup rule is the split, implemented as two MOUNT POINTS.** `/srv/data` and `/srv/nobackup` are separate `mpN:` volume mounts, not plain directories inside the rootfs — and the flag must be set on BOTH entries, because vzdump's default excludes volume mount points: `mp0: …,mp=/srv/data,backup=1` (rootfs + `/srv/data` = the PBS job) and `mp1: …,mp=/srv/nobackup,backup=0` (`/srv/nobackup` never in it). On a box that can't add the second mount point, `vzdump --exclude-path /srv/nobackup` is the rootfs fallback. Nothing under `/srv/nobackup` is individually "important" — if it needs a carve-out, it was filed in the wrong half.
*   **Container stores relocate under `/srv/nobackup`** (`docker`/`containerd`/`rancher` daemon roots), mirroring the existing "exclude `/var/lib/docker`" Docker-host LXC pattern — but by layout, not by config list.
*   **Durable PVCs are pinned under `/srv/data` — never the daemon root.** k3s's default `local-path` provisioner stores PVCs *under the rancher root* (`/var/lib/rancher/k3s/storage/…`); relocating that whole root would drag the control_plane Postgres PVC — the thing every "reconstructible from" claim depends on — into the excluded half, silently. Configure provisioner roots explicitly: disposable volumes on a `nobackup`-rooted storage class, durable ones pinned to `/srv/data/k8s-volumes`.
*   **Data-heavy services mount TrueNAS under `/srv/data/<service>`** — backed by TrueNAS snapshots + Backblaze off-site, not by PBS rootfs copies.
*   **Skills map onto it:** `needs: {volume: …}` → `/srv/data/<service>`; a scratch-only service (nothing durable) → `/srv/nobackup/<service>` or a pod `emptyDir`. `needs.volume` is the declaration that something is durable — absence means disposable by default.

---

## Fourth connector flavor: workspace/git runner

Alongside SSH / Vultr / Backblaze: a **workspace runner** for team/project environment LXCs — holds git deploy keys and container-registry tokens as ciphertext, decrypts locally, and serves them through native credential-helper protocols (`git credential.helper`, `docker credHelpers`) rather than the generic `exec()` primitive. Same identity, provisioning, rotation, and audit model as every other runner; the difference is purely in how the secret is surfaced to the caller (local credential-helper socket vs. remote exec). Not required for Chunk 1's three connectors, but the same core (Phase A–B) covers it — worth flagging as the connector to add once team environments are in scope. **Status: proposed, not locked.** AGENTS.md's locked model defines ONE generic `exec` primitive; adopting the credential-helper surface needs an explicit locked-model carve-out before team environments enter scope.

## Grants — agent ↔ runner (not 1:1)

*   **A runner is scoped to a credential/target**, not an agent. **An agent is granted runners.**  
    The grant is the real concept (many-to-many via grants).
    
*   **Dedicated runner per service = the happy-path default** (buy isolation + surgical  
    revocation; clean audit: revoke the LiteLLM runner, cut off only LiteLLM).
        
*   **Sharing via grants allowed:** master agent needs many runners (create LXCs, create agents);  
    shared infra (one Proxmox runner) is granted to multiple experts. Following Buzz's approach of  
    **whitelisting Nostr pubkeys** of who may call a given agent/runner — revisit finer-grained  
    (target-scoped) permissions later; keep grants COARSE for Chunk 1 (agent ↔ runner, maybe  
    per-tool).

*   **Grants authority (Phase D):** once a runner serves with `--relay-url`, the relay's
    **kind-30180 grant list (d-tag = runner pubkey) is AUTHORITATIVE** and is read LIVE per call
    (a revoke lands without a restart — closes the Chunk-1 running-runner gap); the shipped
    package becomes the offline mirror. **Trust anchor:** only kind-30180 events authored by
    the console/owner pubkey (`--grant-author`, NIP-98-signed publish) whose Schnorr signature
    verifies locally are accepted — never any member's word. Without `--relay-url`, the
    package list is the source (loopback/local runners).
    

## Control plane — the management layer / engine room (our build)

A **management layer over agent-manageable services**, for exactly ONE relay. Also the  
appliance Buzz lives in (setup tool + emergency fix) + shows all services with access at a glance.

*   **Register services** manually OR **auto-discover** on Proxmox; **opt-in** per service.
    
*   Applies to LXCs created, EXISTING services, EXTERNAL/SaaS (SSH machines, Vultr, Backblaze,  
    user's existing relay).
    
*   **Web UI (admin/ops view):** services-at-a-glance + readiness + setup + master agent access  
    (mirrors to relay). NOT the chat surface (that's Buzz).
    
*   **The product's job: give the agent connection tools so services aren't in a red state.**
    

### Service health / readiness states (the console's core signal)

*   🟢 **Green** — runner self-check passes; runner has access → service manageable.
    
*   🟡 **Yellow** — can connect + manage, but needs updates/remediation to "fix."
    
*   🔴 **Red** — can't connect / can't manage (missing creds, no access, unreachable).  
    Readiness = health of the runner↔service connection (reported by the runner's own self-check).
    

## Control plane app — ONE application, three modes

The control plane is a single app (web UI + master agent + skill framework + runner client).  
Installer is NOT a separate script — it is the app's first-run/BOOTSTRAP mode.

```
Modes:
  BOOTSTRAP  — local first-run: guide Proxmox ISO, provision VPS, create management relay,
               install CP onto a target, create self-scoping runner
  OPERATE    — deployed on the target (LXC / VPS): the always-on control plane (a web UI)
  CONNECTED  — local app connects to + surfaces a deployed control plane
```

*   Local = web UI at localhost; deployed = web UI at [https://box](https://box). Same app, different mode.
    (Chunk 2: deployed keeps loopback + SSH tunnel until console authn/TLS lands — see Future items.)
    
*   Only true split = the RUNNER boundary. Everything above the runner can run anywhere.
    
*   Mobile is natural later (CP is a web service).
    
*   Single-relay by construction — no distributed "control plane of control planes."
    

## Two orthogonal axes (the core product architecture)

```
AXIS 1 — SKILLS  :  WHAT to install / how to configure (install-plane, tailscale…)
AXIS 2 — HOST    :  WHERE the appliance lives (proxmox, vps, cloud, incus…)
```

Anything × anything composes. K8s layer runs identically regardless of host.

## Environments (dev / test / prod / dogfood)

*   **VPS (dev/smoke):** fast, cheap, disposable. Tests runner abstraction, agent logic, skill  
    plumbing. Also IS the VPS/cloud product driver dev env (same path).
    
*   **PVE host (test/staging):** real Proxmox API + LXC lifecycle BEFORE production.
    Low-end hardware = proof point for "appliance on modest hardware." Primary Proxmox dev target.
    
*   **Home Proxmox (prod):** real daily driver; dogfooded daily. Never the first test.
    
*   **Later:** PVE hosts join as cluster nodes for multi-box scaling + Ceph.
    
*   **Discipline:** installer/runner must install to a VPS as easily as Proxmox from day one.
    

## MVP scope (installer / provisioning)

**Happy + supported path = Proxmox route** (assumes Proxmox ALREADY running; install-only).  
**VPS = advanced option** (provision + install) for business path + substrate isolation.

*   Provisioning ≠ installing. VPS provisions; Proxmox is install-only.
    
*   Install creates a new management relay OR attaches to an existing one (Buzz required; relay-creation is a skippable idempotent step).
*   **"VPS" = Proxmox-on-Cloud-Compute (LXC-only).** Vultr Cloud Compute and
    Hetzner Cloud instances have no nested hardware virtualization — Proxmox
    on them manages LXC containers, not KVM VMs. The whole stack is
    container/pod-shaped, so the VPS host collapses into the Proxmox host
    driver: provision an amd64 instance, custom-ISO PVE install, then the
    same LXC flows (relay + cp LXCs + services as more LXCs). A real-VM
    requirement routes to Bare Metal (Vultr BM / Hetzner dedicated), never
    the VPS host. (Chunk 2.5.)
    
*   Unified control plane app; bootstrap is a MODE, not a script.
    
*   Co-locate the control plane + its runner with the CP's OWN target LXC/box for MVP — the
    relay is an ATTACH (own LXC, different infra, or unmanaged), never a co-location requirement.
    "Local CP + remote k8s" = not MVP.
    
*   Pre-installed box = phase 2/3. Future: local CP can provision another Proxmox box (software).
    
*   Installer state machine (bootstrap): (1) provision VPS + install, (2) install on this  
    machine, (3) install on existing Proxmox (create LXC + CP), (4) guide Proxmox ISO install,  
    (5) tear down.
    
*   Security: control-plane LXC/pod is privileged by design (holds Proxmox token + SSH keys).
    

## Design decisions (locked)

*   **No Supabase.** Plain Postgres as single shared system-of-record (control-plane state;  
    Buzz relay has its own Postgres).
    
*   **Flexible deploy target.** Skills _declare_ pod vs LXC; agent falls back to heuristics.
    
*   **Config volume-mounted / ConfigMaps**; images stay thin.
    
*   **Deterministic agent pods** (public release); connections as env vars; secrets via  
    provisioner model (runner holds ciphertext + injected key; agent uses, never reads).
    
*   **Agent placement:** POC = Buzz agents via buzz-acp; public release = k8s pods.
    
*   **Buzz required; management relay created by install; one relay per control plane.**
    
*   **LiteLLM** as a k8s Deployment (replicas), config mounted, state in Postgres.
    
*   **Storage**: ZFS now → Ceph on 2nd box. Layered reliability.
    
*   **Buzz Relay headless**; first-party **console** is the admin/ops surface.
    
*   **K8s fixed; hosting substrate pluggable** (proxmox lead, vps/cloud, incus).
    
*   **K8s is part of the public release (MVP), not the POC.** POC proves the connector world.
    
*   **Generic exec runner** (agent writes commands, runner owns connection + streams + audits).
    

## Base directions (opinionated starter presets)

*   **Personal** — individual power-user: Pihole, Tailscale, Plane, notes, local AI.
    
*   **Family** — shared use, safety/guardrails: Pihole, Tailscale, media, shared calendar,  
    kid-safe defaults (parental controls as a skill).
    
*   **Business** — reliability/ops/team: Plane, Jira/Linear optional, SSO/auth, backups-first,  
    cloud host option, compliance-friendly.  
    Agent seeded with the direction's conventions; can deviate on request.
    

## Host provider interface (each substrate = setup skill + management skill)

A host driver implements the runner contract: provision host, run k8s, storage, network,  
snapshots/backups. Keep interface MINIMAL; do not flatten away substrate superpowers.

## Host feasibility (honest)

*   **Proxmox** — lead/default. Best for the homelab box. MVP happy path.
    
*   **VPS / cloud** — cleanest; single VPS simpler than Proxmox, managed k8s = zero control-plane  
    ops. THE business path. MVP advanced option. Also the dev/smoke env.
    
*   **incus** — feasible; system containers, same family as LXC.
    
*   **Mac "containers"** — dev-only (Docker Desktop/Colima/OrbStack/Lima). Not production.
    
*   **Pre-installed box (shipping)** — phase 2/3. Needs self-boot onboarding.
    

## Business model (open-core)

*   Homelab/self-hosted (Proxmox) — free, enthusiast path = moat + community.
    
*   Cloud (managed k8s) — paid, business path; same OSS core + skills, different host driver;  
    funds OSS build-out.
    
*   Skill ecosystem — community/marketplace, host-agnostic, works everywhere.
    

## The stack / layers

```
USER
  ▼  Buzz clients (desktop/mobile/web)
BUZZ — self-hosted Nostr relay + agent management   ← INTERACTION SURFACE (Buzz provides)
  ├─ MANAGEMENT RELAY (created by install) — the control plane's ONE scope
  │    ├─ master agent (CPA) + service agents (@litellm, @algolia, @pihole, @vultr, @b2…)
  │    └─ memory + audit events (relay-persisted)
  ├─ user's EXISTING relays — onboarded as services (relay runner connector), NOT nested scopes
  ├─ deterministic k8s pods (public release) | local buzz-acp (POC) | LXC
  │ agents call tools ↓
RUNNERS — privileged generic MCP tool servers (the bridge, separate from Buzz)   ← OUR BUILD (engine)
  ├─ Nostr identity (auth/membership) + separate encryption keypair (env-injected)
  ├─ generic exec: agent writes commands, runner owns connection + streams + signs audit
  └─ reach ssh-machine | vultr | backblaze | proxmox | lxc | external/saas | user's relay
  ▼
CONTROL PLANE (management layer for ONE relay + appliance Buzz lives in)   ← OUR BUILD
  ├─ SECRET PROVISIONER (encrypt-to-runner-key + ship + rotate + membership; no master key)
  ├─ register/discover services, opt-in per service
  ├─ grants (agent ↔ runner, coarse)
  ├─ readiness green/yellow/red (from runner self-check)
  ├─ skill framework (install/configure)
  └─ web UI = admin/ops view + setup + emergency Buzz fix + master agent access
  ▼
SERVICES — ssh-machine | vultr | backblaze | proxmox | lxc | external/saas | user's relay
```

## Console (control surface, not metrics store)

*   Web UI: services-at-a-glance + readiness (green/yellow/red) + setup + master agent access  
    (mirrors to relay) + skill/service management.
    
*   Reads Proxmox API, k8s API, Prometheus, LiteLLM, AI box (node_exporter+DCGM).
    
*   Compose telemetry backbone from existing tools; build aggregating shell. Control shell +  
    thin backend first, deep views second.
    

## Database model — one Postgres cluster, many databases (control plane)

```
control_plane | litellm | plane | per_client_<n>
```

Logical isolation, single ops surface, single storage volume. (Buzz relay has its own Postgres.)

## Storage reliability (layered onion)

1.  Physical replication — ZFS (1) → Ceph (2+). 2. Postgres WAL + logical backup.
    
2.  Off-box pg_dump. (Per-substrate storage implementations.)
    

## Skill framework

```yaml
name: install-plane
target: lxc | pod | either
runtime: community-scripts | hand-rolled
inputs: [domain, admin_email]
needs: {database: true, volume: 20Gi, network: host}
steps: [fetch script, provision, wire config, register]
```

Skills DECLARE their target; agent falls back to heuristics when absent.

## Provisioning flow

User → master agent (Buzz or CP) resolves intent → pick skill (or improvise) → fill inputs →  
skill runner checks needs (db, volume, network) → deploy to pod or LXC → wire config →  
register → spawn per-service expert agent (@service, in management relay) → agent verifies  
health → report to user.

## Build plan (chunked)

**POC (pre-MVP, NO k8s):**

1.  **Chunk 1 — Local control plane + runners + secrets + connectors:** local web UI  
    (admin/ops console — NOT chat; Buzz owns conversation); runner as MCP tool server with  
    generic `exec`; secret PROVISIONER in the control plane (encrypt-to-runner-key + ship +  
    rotate + membership; no master key); connect to SSH local machine + Vultr + Backblaze;  
    readiness model (runner self-check); coarse grants. Runners separate from Buzz. Runs  
locally, connects to remote services. TEST against the PVE host.
    
2.  **Chunk 2 — Create the management relay (Buzz):** install creates a new relay → becomes the  
    control plane's scope; agents get Nostr identity; fabric + shared memory light up.
    Detailed phase plan: `roadmap/POC_CHUNK2.md`.
    
3.  **Chunk 3 — Skill framework v1 + relay-scoped service agents:** skill schema + runner; first  
    skills (tailscale, pihole); spawn per-service expert agents IN the management relay; build  
    with relay-as-scope (agents + secrets scoped to relay; user's existing relay onboarded as a  
    service via a relay runner).  
    POC done = master agent manages LXCs + external services (SSH/Vultr/Backblaze) via runner +  
    installs via skills, with readiness view; management relay as the scope.
    

**MVP (public release — ADDS k8s):**  
4. **Chunk 4 — Kubernetes substrate:** deterministic agent pods, LiteLLM as deployment, Postgres  
cluster, control plane as deployment, host driver abstraction (Proxmox + VPS).  
5. **Chunk 5 — Full console:** service readiness dashboard, unified resource/activity view,  
skill/service management, deep monitoring (Grafana/Prometheus).  
6. **Chunk 6 — Installer/bootstrap:** the app's BOOTSTRAP mode producing a box in MVP state —  
verifiable against the working system. Must install to VPS and Proxmox equally.  
MVP done = public release (k8s + control plane + console + skills, Proxmox + VPS).

## Future items (prioritize later)

*   Pre-installed box (shipping), phase 2/3
    
*   Local CP provisions other boxes ("add a box = extend the system")
    
*   Full console deep monitoring + AI box telemetry (DCGM)
    
*   Local AI hosting (separate GPU box as LiteLLM upstream, monitored in console)
    
*   DM mirroring / sync between control plane master agent and relay (when relay down)
    
*   Base directions (personal/family/business presets)
    
*   Skill ecosystem / marketplace
    
*   Mobile app
    
*   **Runner name addressing (POC follow-up):** agents/users address runners by NAME — the
    client resolves `name → runner pubkey + MCP addr` from the control-plane registry. A
    client-side lookup only: no network router, no shared trust anchor, the MCP transport
    stays per-runner and audience-bound. Today the orchestrator hand-takes
    `--runner-pubkey`/`--addr`; this removes that (e.g. `exec --runner proxmox-box`).
    
*   Multi-box scaling + Ceph replication
    
*   Security hardening (privilege escalation, audit, approval gates); on-demand decryption opt-in  
    for external/less-trusted runners; Vault for dynamic secrets
    
*   **Console authentication + TLS:** the console gains NIP-98 operator login IN Chunk 2
    (admin whitelist seeded by --operator-pubkey at bootstrap), making the bind guard
    authn-conditional (loopback-only refusal until authn + an admin are configured; LAN
    bind once they are). TLS = the domain cert: local CA by default (LAN-only), Let's
    Encrypt DNS-01 when a DNS provider key is given. A real public posture (the
    "deployed = web UI at https://box" line elsewhere in this doc) is then optional,
    not gated.
    
*   Multi-user / multi-tenant (relay-as-scope enables this)
    
*   Open-core business model (cloud offering funds OSS)
    
*   Guide-Proxmox-ISO install (if deferred from MVP)
