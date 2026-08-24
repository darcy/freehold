# POC Steps — The AI-operated Appliance

Scope: proof of concept (pre-MVP). NO Kubernetes in the POC. Agents run via Buzz  
buzz-acp (local) in the POC; deterministic k8s pods are the public-release target.

## Core model (locked)

*   **One relay per control plane.** A control plane exists for exactly ONE relay and manages  
    that relay's agents, runners, and secrets. No nested/peer relay machinery.
    
*   **Agent = brain, runner = dumb privileged hands.** Runner provides the connection + executes  
    the command the agent writes verbatim. NO semantic tools — one generic `exec(cmd, target)`.  
    Streaming is a property of exec (long-running/live commands return output over time).
    
*   **Runner identity = Nostr membership + separate encryption keypair (env-injected).** CP is a  
    **secret PROVISIONER** (encrypt-to-runner-key + ship + rotate + membership), not a vault.  
    Runner holds only ciphertext + its own key; decrypts locally, uses in memory, forgets.
    
*   **Readiness = the runner's own self-check** (green/yellow/red = runner↔service connection).
    
*   **Grants coarse, not 1:1** — agent ↔ runner; dedicated runner per service = default;  
    sharing via grants (whitelist Nostr pubkeys).
    

## POC goal

Prove the core value: **an agent (via privileged runners) can manage services on your**  
**behalf — a local machine over SSH, Vultr, and Backblaze — with a management relay as**  
**the scope, and a readiness view showing what the agent can reach.** Buzz is required;  
the install creates a new management relay (one relay per control plane).

## Chunk 1 — Local control plane + runners + secrets + connectors

Goal: prove the engine room works standalone (before Buzz is in the picture).

*   **Local web UI** (`localhost`) — admin/ops view: services-at-a-glance + readiness.
    
*   **Runner as MCP tool server** — the privileged connector bridge, separate from Buzz, with  
    the generic `exec(cmd, target)` primitive (agent writes commands; runner owns connection +  
    streams + audits). Runner API: `list, exec, config, status, snapshot`.
    
*   **Secret PROVISIONER embedded in the control plane** — encrypt-to-runner-key + ship +  
    rotate + membership; NO master key; runner is a Nostr identity + separate encryption keypair,  
    holds only ciphertext, decrypts locally, uses in memory, forgets.
    
*   **Connectors:**
    *   SSH to a local machine (generic remote exec)
        
    *   Vultr (cloud infra — create/destroy/status a server)
        
    *   Backblaze B2 (S3-compatible storage — read/write round-trip)
        
*   **Readiness model:** green/yellow/red = health of runner↔service connection, from the  
    runner's own self-check.
    
*   **Coarse grants:** agent ↔ runner (whitelist Nostr pubkeys); dedicated runner per service  
    = default.
    
*   **No Buzz yet.** Master agent works directly in the control plane.
    
*   Test against the PVE host (safe target), then promote to home.
    

### Chunk 1 acceptance

*   Control plane web UI lists the 3 connectors with readiness states.
    
*   Agent can exec on the SSH machine, create/destroy a Vultr server, and do a B2  
    read/write round-trip — via the runner, with secrets resolved by the runner from ciphertext  
    provisioned by the CP.
    
*   Secrets never appear in agent context; runner holds only ciphertext + its own injected key,  
    no master key anywhere; revoking a runner's membership at the CP cuts it off; rotating a  
    secret re-encrypts it.
    

## Chunk 2 — Create the management relay (Buzz)

Goal: bring in the interaction surface + memory backbone; establish the scope.

*   Install creates a **new management relay** → becomes the control plane's ONE scope.
    
*   Master agent + fabric get Nostr identity; memory is relay-persisted.
    
*   The control plane manages Buzz as a child service (peer model).
    
*   If the user has an existing Buzz relay, it's onboarded as a service (relay runner), not a  
    nested scope.
    

### Chunk 2 acceptance

*   Management relay is up; master agent has identity in it; memory persists across runs.
    
*   Users can talk to the master agent in Buzz (rooms/DMs).
    

## Chunk 3 — Skill framework v1 + relay-scoped service agents

NOTE (2026-08-24): the detailed plan is `roadmap/POC_CHUNK3.md` (draft v5). It SUPERSEDES
the onboarding order below (expert created first, reasons about its own target, asks CPA —
CPA resolves the named hardware peer; the old CPA-provisions-target-first order is replaced
for Chunk 3 forward) and adds k8s-as-pods to Chunk 3 (superseding ROADMAP's NO-Kubernetes-
in-POC for this chunk). This section stays the short goal/acceptance summary.

Goal: agents can install/configure services, with per-service experts in the relay scope.

*   **Skill schema + runner** — declarative playbooks (target: lxc | pod | either).
    
*   **First skills:** tailscale, pihole.
    
*   **Spawn per-service expert agents** IN the management relay (`@tailscale`, `@pihole`,  
    and later `@vultr`, `@b2`) — built with relay-as-scope (agents + secrets + runners scoped  
    to relay).
    
*   **Onboarding pattern (happy path):** user tells CPA to set up a service → CPA provisions/  
    points at target → creates runner (holds credential) → validates runner connects (readiness  
    🟢) → creates buzz agent → grants it the runner → writes AGENTS.md → adds to a channel →  
    tells it (via Buzz) to install/verify and report back.
    
*   Agent executes skills instead of free-form shell.
    

### Chunk 3 acceptance

*   Master agent installs + configures tailscale and pihole via skills.
    
*   Per-service expert agents exist in the management relay and can be talked to directly.
    
*   Secrets for services scoped to the management relay.
    

## POC done

Master agent manages SSH machine / Vultr / Backblaze via runner + installs skills  
(tailscale, pihole), with a readiness view; management relay is the scope; Buzz  
optional-but-working as a managed child service.

## Test / promote flow (dogfood)

1.  **VPS (dev/smoke):** fast iteration on runner, agent, skills, plumbing.
    
2.  **PVE host (test/staging):** real Proxmox API + LXC lifecycle, safe.
    
3.  **Home Proxmox (prod):** daily driver; dogfooded daily; never the first test.  
Promotion: code → VPS smoke → PVE host test → home. Installer/runner must install to
    VPS as easily as Proxmox from day one (no Proxmox-only shortcuts).
    

## Out of scope for POC (later)

*   Kubernetes / deterministic pods (public release).
    
*   Full console with deep monitoring (Grafana/Prometheus).
    
*   LiteLLM as a k8s deployment.
    
*   DM mirroring/sync between control plane and relay when relay is down.
    
*   Finer-grained (target-scoped) grants; on-demand decryption for external runners.
    
*   Pre-installed box, mobile, multi-tenant, multi-box scaling.
