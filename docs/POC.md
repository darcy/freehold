# POC Steps — The AI-operated Appliance

Current *released* version: the latest GitHub Release (equivalently the latest `v*` tag;
never restated here). The release history lives in GitHub Releases — this document
describes the current plan only. Deferred work pulled from retired chunk plans lives in
`docs/followups.md`.

Scope: proof of concept (pre-MVP). Chunks 1–4 are shipped and live-verified; Chunks 5–7
remain (agent workspaces + git/GitHub, Kubernetes deploys, and hardware portability).
Kubernetes arrives with Chunks 6–7.

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
    
*   **The agent org is two tiers: the CPA and four departments.** The CPA is the sole user  
    touchpoint; **Network** (the network surface — access/exposure), **Data** (data plane), **Compute** (the box  
    itself — CPU/RAM/disk, LXC/kube and remote provisioning, plus its monitoring tooling),  
    and **AI** (models/providers/agents, plus AI hardware) are  
    its direct reports, each a distinct identity scoped to one domain. Talk is unrestricted  
    (the operator and any agent may converse with any department directly); capability  
    execution is bounded — a department-owned capability is executed by that department's  
    identity, and its raw grant attaches there, never to a custom agent. Service lifecycle is  
    not a department: whichever agent created a service owns it, ad hoc and unvetted. The four  
    are installed as part of the core build (`freehold build`): each in the private `#freehold` plus its  
    own private `#freehold-<department>` channel, with the CPA a member of all. Only the  
    identity/grant separation is locked; capability tooling/secrets arrive per department  
    later (Chunk 5/6).
    

## POC goal

Prove the core value: **an agent (via privileged runners) can manage services on your**  
**behalf — a local machine over SSH, Vultr, and Backblaze — with a management relay as**  
**the scope, and a readiness view showing what the agent can reach.** Buzz is required;  
the install creates a new management relay (one relay per control plane).

## Shipped (Chunks 1–4)

The engine room (runners, the secret provisioner, SSH/Vultr/B2 connectors, the readiness
view) and the relay scope (identity moved onto real Nostr membership, relay-persisted
encrypted memory, delegation mode, the durable volume plane) are live, and the Rust→Go
refactor is complete. Chunk 4's CPA is a real, LLM-backed reasoning agent on the
`buzz-acp`/goose-class harness: it holds conversations in Buzz, survives a process restart
and a full compute-only teardown/rebuild with identity and memory intact, and creates new
agents on request. Its Phase-F resource baseline and the other open deferrals are tracked in
`docs/followups.md`.

## Chunk 5 — Agents get a workspace and can commit code

Goal: an agent can get its own LXC workspace, provisioned by a named hardware peer, and use
it to make and ship a real change — to Buzz's own git and/or GitHub.

*   A named peer provisions the workspace LXC on request, routed through CPA (CPA resolves
    which peer, gates cost/irreversibility, delegates; the agent never learns which peer it
    landed on).
    
*   A commit made from the workspace is verified durable in Buzz's relay-hosted git — not
    just present on the disposable workspace LXC.
    
*   An agent pushes a commit to a real GitHub repo via a GitHub grant.
    
*   Minimal skills, scoped to only what this needs (clone/edit/commit/push reliably) — the
    fuller skill-schema/readiness/verify-harness design is pulled in only as later chunks
    need it. The first named skills are tailscale + pihole (deliberately not LiteLLM, which
    rides the k8s path).
    
*   The workspace/git credential surface (credential-helper vs. generic `exec()`) is decided
    explicitly here, not assumed.
    
*   Proof point: an agent deploys a service to an LXC using what it committed.
    
*   **The first department capability gets teeth: Compute.** Compute requests routed
    through the CPA land on the Compute identity, which holds the raw compute grant;
    custom agents never receive it.
    
*   **Data's capability runner (built).** Data holds the first live raw capability grant:
    a dedicated `data-pve` runner (`root@<host>` SSH) in its own channel, reached from
    Data's pod by a scoped `exec`/`list` signing as Data's own key against the runner's
    relay-signed roster. It verifies every LXC/kube volume lands on a backed-up mount.
    This is the pattern the remaining departments follow.
    
*   **Agents can read the repo.** The shared orientation block tells every non-custom agent
    to read the source repo on first boot, keep a memory of it, and re-check periodically;
    this chunk wires the git/GitHub grant and the read/schedule path that makes it real.

### Chunk 5 acceptance

*   CPA-routed request → named peer provisions an LXC workspace → agent commits a real
    change → change is verified durable in Buzz's git and/or pushed to a real GitHub repo.
    
*   The workspace/git-runner credential surface is either adopted (with a written carve-out
    from the generic `exec()` model) or explicitly rejected in favor of it.
    

## Chunk 6 — Agents deploy via Kubernetes

Goal: the same agent-does-real-work loop generalizes from an LXC target to a kube target.

*   **The skill schema and the first skills.** A `provision` skill declares `target: lxc | pod |
    either` — a *hint* the expert may override, not an authorization. The expert reasons
    about its own target and asks CPA — never a peer directly; CPA resolves which named
    peer fulfills the ask, gates cost/irreversibility, and delegates. The expert receives
    only a slot descriptor (architecture, resource bounds, compute kind — never peer
    identity). 

*   Kube-slot fulfillment: a named peer hands out a namespace + ResourceQuota instead of an
    LXC, same "give me compute" shape.
    
*   **Budget-exceeded is escalation, never action:** the agent stops, reports, and
    *offers* — never performs — a rebuild for non-ephemeral state.
    
*   **Readiness is postcondition-gated:** 🟢/🟡/🔴 reflects the `verify:` checks a runner
    runs *after* the expert reports done, so a confidently-wrong acceptance keeps the
    service 🔴 rather than flipping it green on the agent's word.
    
*   An agent deploys a service to that slot, verified live.
    
*   **Terraform per kind:** `terraform/` plans per kind (proxmox-lxc, k3s, litellm-kube,
    later vultr-vps/hetzner-vps) running runner-exec under the four locked disciplines;
    teardown/rebuild rides `terraform destroy` / `terraform apply` plus the verify harness.
    
*   Sleep/wake for ad-hoc agents is built here (idle auto-reap on pods), using the resource
    numbers gathered earlier.
    
*   **The department check-in hook lands here.** When an agent requests a service/compute
    through the CPA's provision path, Data asks "back this up?" and Network asks
    "reachable outside your network?" — a visible choice, with "no" as a valid final answer.
    Department status reuses the same postcondition-gated 🟢/🟡/🔴 language as runner/service
    readiness.
    

### Chunk 6 acceptance

*   Same proof point as Chunk 5 (agent deploys a service), targeting a kube namespace
    instead of an LXC, via the same CPA-routed peer-fulfillment pattern.
    
*   A `target:`-declared skill never widens authority: the expert acts only through the
    slot descriptor CPA hands it — architecture, resource bounds, compute kind, never peer
    identity.
    
*   Blue/green flips with zero dropped requests (new version alongside → verify → flip →
    soak → remove old); a budget blow-up stops at an offer, and a failed `verify:` keeps
    readiness 🔴. Images/state versions ride a local `registry:2` with bounded retention
    (the last 3).
    

## Chunk 7 — Remaining connectors exercised + North Star: portable backup & hardware migration

Goal: exercise Vultr and Backblaze for real onboarding work through the now-mature CPA/
expert/runner loop, and take a first real run at the **North Star**: run freehold locally,
back it up reliably, and stand up a fresh freehold on different hardware or a different
provider (e.g. local Proxmox → Vultr), restored from that backup — same identity, memory,
grants, and services.

*   Onboard a real external service via Vultr and via Backblaze B2 through a real agent
    conversation.
    
*   Off-site backup of the durable plane (Backblaze).
    
*   A portable snapshot/restore format independent of the source storage backend (e.g. a
    ZFS-backed local box's data restoring onto a VPS provider's block volume).
    
*   A bootstrap path that restores identity + memory + grants + running services from a
    backup — disaster recovery, distinct from the live compute-only reattach path.
    
*   VM-targeted deploys are deferred; the blue/green flip (Chunk 6) keeps the North Star's
    cross-provider restore reproducible on the same discipline.
    
*   This is also a standing dogfood tool once built: clone a running production freehold
    onto disposable hardware to test a risky change, without touching the real system.

### Chunk 7 acceptance

*   Backblaze and Vultr each carry at least one real onboarded service/workload through the
    CPA loop.
    
*   A freehold instance running locally is backed up off-site, torn down entirely (hardware
    gone, not just compute), and restored onto different hardware/provider — same identity,
    memory, grants, and services present and correct.
    

## Test / promote flow (dogfood)

1.  **VPS (dev/smoke):** fast iteration on runner, agent, skills, plumbing.
    
2.  **PVE host (test/staging):** real Proxmox API + LXC lifecycle, safe.
    
3.  **Home Proxmox (prod):** daily driver; dogfooded daily; never the first test.  
Promotion: code → VPS smoke → PVE host test → home. Installer/runner must install to
    VPS as easily as Proxmox from day one (no Proxmox-only shortcuts).
    

## Out of scope for POC (later)

*   Full console with deep monitoring (Grafana/Prometheus).
    
*   DM mirroring/sync between control plane and relay when relay is down.
    
*   Finer-grained (target-scoped) grants; on-demand decryption for external runners.
    
*   Pre-installed box, mobile, multi-tenant, multi-box scaling.
