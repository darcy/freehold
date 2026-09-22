# Roadmap — The AI-operated Appliance

Current *released* version: the latest GitHub Release (equivalently the latest `v*` tag;
never restated here). See GitHub Releases for how this plan arrived here; this document
describes the current plan only. Deferred work is tracked in `docs/followups.md`.

Product: open-source appliance — Proxmox VE + k8s, Buzz Relay control plane, and an  
agent that installs/configures self-hosted OSS via a **skill framework**. One app, three  
modes (bootstrap / operate / connected). Vision: "reclaim the future we were promised."  
**Organizing model: one relay per control plane (relay-as-scope)** — agents, secrets,  
runners, and memory are scoped to a single relay.

## Core model (locked)

*   **A control plane exists for exactly ONE relay.** Its only job: manage that relay's  
    agents, runners, and secrets. No nested/peer relay machinery — no "control plane of  
    control planes" to solve.
    
*   **A user's existing relay is just a service** — onboarded like any external service via  
    a relay runner (same as algolia). Not a nested scope.
    
*   **Bootstrap is self-scoping:** install creates the Buzz relay → creates the CP → creates  
    a runner that reaches/manages both → CP adds itself as a member. The operator names the  
    CPA during this step; the name becomes its Buzz handle.
    
*   **Agent = brain, runner = dumb privileged hands.** Runner provides connection + executes  
    the command the agent writes verbatim. NO semantic tools — one generic `exec(cmd, target)`.  
    Runner streams output for long-running/live commands and signs a Nostr audit event per command.
    
*   **The master/control agent (CPA) is a real, LLM-backed reasoning agent — the system's**  
    **main user touchpoint.** It runs on a real-agent harness (buzz-acp/goose-class), gets  
    its purpose from a versioned system-prompt file in the repo, and delegates to the  
    per-service expert agents it creates rather than doing expert-level work itself. The  
    deterministic runner/CP layer underneath (provisioning, grants, secrets, teardown/  
    rebuild) stays as-is — reasoning decides what to do, the same auditable machinery does it.
    
*   **The agent org is two tiers: the CPA and four departments.** The CPA is the sole user  
    touchpoint; **Network** (the network surface — access/exposure), **Data** (data plane), **Compute** (the box  
    itself — CPU/RAM/disk, LXC/kube and remote provisioning, plus its monitoring tooling),  
    and **AI** (models/providers/agents, plus AI hardware) are  
    its direct reports, each a distinct identity scoped to one domain. Talk is unrestricted —  
    the operator and any agent may converse with any department directly; what is bounded is  
    capability execution: a department-owned capability (proxy, backup, compute, model  
    registration, AI hardware) is executed by that department's identity, and its raw grant  
    attaches there, never to a custom agent that would self-serve a second, ungoverned path.  
    Service lifecycle is not a department — whichever agent created a service owns it, ad hoc  
    and unvetted. The four are installed as part of the core build — each in the private `#freehold` plus  
    its own private `#freehold-<department>` channel, with the CPA a member of all; only the  
    identity/grant separation is locked (capability tooling arrives per department later).
    
*   **Runner identity = Nostr membership + separate encryption keypair (env-injected).**  
    CP is a **secret PROVISIONER** (encrypt-to-runner-key + ship + rotate + membership), not a  
    vault — no master key. Runner holds only ciphertext + its own key; decrypts locally, uses in  
    memory, forgets. Per-runner containment. On-demand decryption = opt-in for external runners.
    
*   **Grants (agent ↔ runner) are coarse, not 1:1.** Dedicated runner per service = default;  
    sharing via grants allowed (whitelisting Nostr pubkeys of who may call a runner/agent).
    

## POC — proof of concept (pre-MVP)

The smallest thing that proves the core idea works: an agent can manage services on your  
behalf via privileged runners, with a management relay as the scope.

**Shipped:** the first four chunks — the engine room (control plane web UI, runners as MCP  
tool servers, the secret provisioner, SSH/Vultr/Backblaze connectors, readiness, coarse  
grants), the management relay scope (identity on real Nostr membership, relay-persisted  
encrypted memory, delegation mode, the durable volume plane), the Rust→Go refactor, and  
Chunk 4's real, reasoning CPA that lives in Buzz and creates agents itself. Chunks 1–4 were  
live-verified against real infrastructure (a real PVE host, a real relay/CP pair under a  
real domain).

**Remaining — Chunk-by-chunk plan: see `docs/POC.md`.**

*   **Chunk 5 — agent workspaces + git/GitHub:** a named peer provisions the agent's LXC  
    workspace, the agent commits a durable change to Buzz's git and/or pushes to GitHub.
    
*   **Chunk 6 — agents deploy via Kubernetes:** the same loop targets a kube namespace; the  
    skill schema, postcondition-gated readiness, budgets-as-escalation, Terraform-per-kind,  
    and the department check-in hook land here.
    
*   **Chunk 7 — remaining connectors + North Star:** Vultr and Backblaze exercised for real  
    onboarding, then portable backup & hardware migration (see below).

*   **Long-standing POC acceptance:** the CPA manages an SSH machine / Vultr / Backblaze via  
    runner + installs skills (tailscale, pihole) with a readiness view; the management relay  
    is the scope. The human-facing Buzz leg is live (Chunk 4); skills + kube deploys complete  
    it in Chunks 5–6.
    
*   **Environments:** VPS dev/smoke + PVE host test + home dogfood.
    

## MVP — public release (definition)

The public release builds on the POC and adds the Kubernetes substrate. Core promise:  
**a user runs one app, points it at Proxmox (or a VPS), and gets an agent that manages**  
**services via skills on a Kubernetes fabric — with the control plane, console, and a**  
**management relay.**

### MVP scope (in)

*   Everything from the POC (control plane app, runners, secret provisioner, skills, console,  
    management relay, connectors).
    
*   **Kubernetes substrate** — the k8s layer IS part of MVP/public release:
    *   deterministic agent pods (ephemeral, repo+skills mounted)
        
    *   **LiteLLM as a k8s Deployment** (config mounted, state in Postgres)
        
    *   **Postgres** (single cluster, many DBs: control_plane, litellm, per-client)
        
    *   control plane as a k8s deployment
        
*   **Host driver abstraction** — at least Proxmox (lead) + VPS/cloud (advanced/business).
    
*   **Runner connector model** — reach self-hosted AND external/SaaS services; secrets via  
    provisioner (runner holds ciphertext + injected key; agent uses but never reads).
    
*   **Skill framework v1** (tailscale, pihole) — works identically on LXC and k8s.
    
*   **Minimal console** — service readiness dashboard (green/yellow/red) + agent chat +  
    services/resources view + skill install.
    
*   **Management relay** as the scope for agents + secrets + runners.
    
*   **Environments:** VPS dev/smoke + PVE host test + home dogfood.
    
*   **Bootstrap mode:** install onto existing Proxmox + install onto a VPS (advanced).
    

### MVP scope (out / explicitly not public-release v1)

*   **Pre-installed/shipped box** (hardware) — phase 2/3.
    
*   **Local control plane provisioning other boxes** — future.
    
*   **Full unified monitoring console** (Grafana/Prometheus deep views) — post-MVP.
    
*   **Mobile app** — future (control plane is a web service, natural later).
    
*   **Multi-tenant / multi-box scaling** — future (relay-as-scope enables it).
    
*   **Guide-Proxmox-ISO install** — stretch; likely deferred to keep scope tight.
    

### MVP acceptance criteria (public release "done")

*   Fresh install: run the app → create a management relay → point at Proxmox or a VPS →  
    control plane live in k8s.
    
*   Agent manages services via runner (SSH machine / Vultr / Backblaze / LXCs / pods) with  
    readiness (green/yellow/red) in one place.
    
*   Agent installs + configures tailscale + pihole via skills, on both LXC and k8s.
    
*   Secrets via provisioner: runner holds only ciphertext + injected key, decrypts locally,  
    uses in memory, forgets; agent uses but never reads; revoke membership (cut off) + rotate  
    (erase).
    
*   LiteLLM routes agent models; Postgres holds control-plane + service state.
    
*   Runs safely on PVE host test box AND home box (dogfooded daily).
    

## North Star — portable backup & hardware migration

**Target: as soon as Chunk 7** (see `docs/POC.md`) — the earliest point a real
workflow exists worth migrating.

Run freehold locally, back it up reliably, and stand up a fresh freehold on **different
hardware or a different provider** (e.g. a home Proxmox box → Vultr) restored from that
backup — same identity, memory, grants, and running services. This is hardware-loss
disaster recovery, distinct from same-host compute-only teardown/rebuild: it requires
off-site backup, cross-storage-backend restore, and a bootstrap path that restores state
from backup rather than assuming the original dataset is still reachable.

*   **Why it's the North Star:** it's the single test that proves the whole model — durable
    plane, relay-as-scope, deterministic rebuild — holds under the failure mode that
    actually matters (losing the hardware, not just the compute).
    
*   **Why it's worth dogfooding, not just a checkbox:** once it works, it's a standing
    workflow — clone a running production freehold onto disposable hardware, test a risky
    change against the clone, discard it, without ever touching the real system.
    
*   **What it needs that doesn't exist yet:** off-site backup shipping (Backblaze), a
    portable snapshot/restore format independent of the source storage backend (ZFS ↔
    LVM-thin ↔ VPS block volume), and a restore-from-backup bootstrap path distinct from
    live reattach.
    

## Future items (prioritize later)

*   **Pre-installed box (shipping)** — phase 2/3, hardware + support + self-boot onboarding.
    
*   **Local control plane provisions other boxes** — "add a box = extend the system" (software).
    
*   **Full console** — unified resource/activity view (Proxmox + k8s + services), skill  
    management, deep monitoring (Grafana/Prometheus), AI box telemetry (DCGM).
    
*   **Local AI hosting** — separate GPU box as LiteLLM upstream + monitored in console.
    
*   **Base directions** — personal / family / business starter presets.
    
*   **Skill ecosystem / marketplace** — community skills, host-agnostic.
    
*   **Mobile app** — phone as a client to the control plane.
    
*   **Multi-box scaling + Ceph replication** — PVE hosts join the cluster.
    
*   **Security hardening** — finer-grained target-scoped grants, privilege escalation, audit,  
    approval gates on the runner; on-demand decryption opt-in for external/less-trusted  
    runners; Data for dynamic secrets.
    
*   **Multi-user / multi-tenant** — relay-as-scope enables per-scope agents + secrets.
    
*   **Open-core business model** — cloud offering funds OSS build-out.
    
*   **Guide-Proxmox-ISO install** — flash-drive walkthrough (if deferred from MVP).
    

## Notes / open questions

*   Buzz is REQUIRED; the install creates a new management relay (one relay per control plane).
    
*   MVP assumes Proxmox already running for the happy path; VPS = advanced option.
    
*   Security: control-plane LXC/pod is privileged by design (holds Proxmox token + SSH keys);  
    secrets via provisioner — runner holds ciphertext + injected key, agent uses but never reads.
    
*   Grants coarse for POC (agent ↔ runner); finer target-scoped permissions = later.
