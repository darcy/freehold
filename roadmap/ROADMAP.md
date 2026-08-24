# Roadmap — The AI-operated Appliance

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
    a runner that reaches/manages both → CP adds itself as a member.
    
*   **Agent = brain, runner = dumb privileged hands.** Runner provides connection + executes  
    the command the agent writes verbatim. NO semantic tools — one generic `exec(cmd, target)`.  
    Runner streams output for long-running/live commands and signs a Nostr audit event per command.
    
*   **Runner identity = Nostr membership + separate encryption keypair (env-injected).**  
    CP is a **secret PROVISIONER** (encrypt-to-runner-key + ship + rotate + membership), not a  
    vault — no master key. Runner holds only ciphertext + its own key; decrypts locally, uses in  
    memory, forgets. Per-runner containment. On-demand decryption = opt-in for external runners.
    
*   **Grants (agent ↔ runner) are coarse, not 1:1.** Dedicated runner per service = default;  
    sharing via grants allowed (whitelisting Nostr pubkeys of who may call a runner/agent).
    

## POC — proof of concept (pre-MVP)

The smallest thing that proves the core idea works: an agent can manage services on your  
behalf via privileged runners, with a management relay as the scope.

*   **Local control plane web UI** (localhost) — the admin/ops view (services-at-a-glance,  
    readiness, master agent access). NOT the chat surface (that's Buzz).
    
*   **Runners as MCP tool servers** — privileged connectors, separate from Buzz, generic  
    `exec` primitive (agent writes commands; runner owns connection + streams + audits).
    
*   **Secret PROVISIONER embedded in the control plane** — encrypt-to-runner-key + ship +  
    rotate + membership; NO master key; runner is a Nostr identity + separate encryption  
    keypair; holds only ciphertext, decrypts locally, uses in memory, forgets.
    
*   **Connectors (POC): SSH to a local machine + Vultr + Backblaze (S3-compatible).**
    
*   **Readiness model** (green/yellow/red = health of runner↔service connection, reported by  
    the runner's own self-check).
    
*   **Coarse grants** (agent ↔ runner; whitelist Nostr pubkeys). Dedicated runner per service  
    = default.
    
*   **Management relay (Buzz required):** the install creates a new relay → becomes the  
    control plane's scope (agents + secrets scoped to it). User's existing relay is onboarded  
    as a service (relay runner), not a nested scope.
    
*   **Agent placement:** POC = Buzz agents via buzz-acp (local); k8s pods later (public release).
    
*   **NO Kubernetes** in POC — SUPERSEDED FOR CHUNK 3 FORWARD (2026-08-24, see
    roadmap/POC_CHUNK3.md v5: agents deploy as deterministic pods on a pulled-forward
    k3s substrate; still true for Chunks 1–2).
    
*   Environments: VPS dev/smoke + PVE host test + home dogfood.
    
*   POC acceptance: master agent manages SSH machine / Vultr / Backblaze via runner + installs  
    skills (tailscale, pihole) with readiness view; management relay is the scope.
    

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
    runners; Vault for dynamic secrets.
    
*   **Multi-user / multi-tenant** — relay-as-scope enables per-scope agents + secrets.
    
*   **Open-core business model** — cloud offering funds OSS build-out.
    
*   **Guide-Proxmox-ISO install** — flash-drive walkthrough (if deferred from MVP).
    

## Notes / open questions

*   Buzz is REQUIRED; the install creates a new management relay (one relay per control plane).
    
*   MVP assumes Proxmox already running for the happy path; VPS = advanced option.
    
*   Security: control-plane LXC/pod is privileged by design (holds Proxmox token + SSH keys);  
    secrets via provisioner — runner holds ciphertext + injected key, agent uses but never reads.
    
*   Grants coarse for POC (agent ↔ runner); finer target-scoped permissions = later.
