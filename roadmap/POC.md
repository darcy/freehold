# POC Steps — The AI-operated Appliance

Current *released* version: the top entry in `CHANGELOG.md` (equivalently the latest `v*`
tag; never restated here). For the history of how this plan changed (superseded decisions,
reordering, reversed calls) see `CHANGELOG.md` — this document describes the current plan
only.

Scope: proof of concept (pre-MVP). Chunks 1–2 avoid Kubernetes entirely; Chunk 5 introduces
it once there's a real agent workflow worth generalizing to it (see Chunk 5 below).

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
    are installed as part of the core build (`freehold build`): each in `#freehold` plus its  
    own private `#<department>` channel, with the CPA a member of all. Only the  
    identity/grant separation is locked; capability tooling/secrets arrive per department  
    later (Chunk 5/6).
    

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

Goal: bring in the interaction surface + memory backbone; establish the scope. The actual
job is proving the transition from runner-direct bootstrap to **delegation mode** — the
control plane deploys onto its OWN target (the `freehold` service), the CP joins a relay
as a member, and `@freehold` delegates its first real task to a relay-addressable peer
agent instead of calling a runner itself.

*   **Phase 0: Buzz surface research first.** Buzz is a real product, not a blank event
    store — no kind is designed against an assumption. Deliverable:
    `roadmap/BUZZ_SURFACE.md` (membership 13534, memory 30174, audit 48001, jobs
    43001–43006, DMs 41001; GRANTS was the only capability needing a custom kind).

*   **Phase A: bootstrap provisioning (pre-relay, runner-direct).** `freehold install`
    stands up the target via the existing API runners (`vultr create/destroy`,
    `hetzner create/destroy`) or the ssh runner driving `pvesh`/`pct` — no new
    Proxmox connector. A **blocking domain gate** requires `--domain` and holds until it
    resolves: the domain, not the IP, is the identity from event zero.

*   **Phase B: create-new vs attach-existing.** No operator relay → create it (Postgres,
    Redis, S3/MinIO per Architecture); one pre-existing at bootstrap → skip creation,
    verify liveness + membership feasibility, and attach. Idempotent: re-runs resume,
    never re-create. TLS rides whatever the operator's proxy terminates; the relay serves
    plain HTTP behind the strict host map.

*   **Phase C: the `freehold` service, not a co-located pair.** The CP deploys onto its
    OWN dedicated LXC/box (relay on `relay-box`, CP on `cp-box`, attached over the
    network), and joins the relay via its own member management (`buzz-admin add-member`
    — the CP cannot self-add). The console starts loopback-only (SSH-tunnel access) and
    gains **NIP-98 operator login** (`--operator-pubkey` seeds the admin whitelist);
    with authn configured it may bind the LAN.

*   **Phase D: Chunk 1's identity model ports onto relay membership.** The keys were
    always real Nostr keypairs; what moves is where membership/grants/memory/audit live.
    Memory rides native 30174 (self-encrypted — a relay operator is not a reader of agent
    memory); grants stay in the shipped package (the custom-kind 30180/30181 surface was
    built, then withdrawn: stock Buzz's hardcoded `ingest.rs::scopes()` refuses
    out-of-scope kinds — we don't patch Buzz, so 2.6.1 remaps runners onto NIP-29
    channels and membership commands, with the runner's whitelist as its own
    relay-signed 39002 roster, read fresh per call). Audit is additive: 48001 spools
    locally AND publishes to the relay when `--relay-url` is set; publish failure
    degrades to spool-only, never silently dropped.

*   **Phase E: delegation mode, live.** The provisioning capability is **duplicated** into
    the relay, not promoted: a NEW runner identity + a NEW relay-addressable peer agent
    with their own keypair and re-encrypted credentials (key-material separation is the
    assertion — a same-key rename would be a failure). `@freehold`, connected to the
    relay, delegates a provisioning-flavored ask over relay events (request event → reply
    event, correlated by id); the local provisioning expert stays local and dormant.

### Chunk 2 acceptance

*   A fresh run: provision the target → relay up (TLS on the domain, `wss://`, non-domain
    hosts refused) → CP up on its own LXC → CP is a relay member. Re-runs and
    attach-existing (point the CP at a pre-existing relay) resume rather than re-create.

*   `@freehold` delegates to a relay peer; the result comes back through the relay, not
    as a direct runner reply — with the peer's keypair and credential provably distinct
    from the local expert's.

*   Chunk 1's three connectors still work under relay identities (SSH live; Vultr live as
    a HOST path — the real account sat in the Chunk 2.5 spike; B2 hermetic-only). A
    non-member pubkey is denied; a member-but-ungranted pubkey is denied; the console is
    unreachable without an operator session.

*   Memory persists across a CP restart (30174's store is relay-side; set/get round-trips
    live; the restart-persistence run is still owed).

### Chunk 2.5 + k3s (the same appliance on cloud compute)

*   `bootstrap --kind vultr-vps | hetzner-vps` collapses into "provision a PVE host on
    <provider>," then the existing LXC flows run verbatim. Live on **both** providers —
    Vultr 45.76.255.185 and Hetzner 178.156.179.204 (Debian 13, apt-route PVE).
    Cloud PVE has a single public NIC: vmbr0 over eth0, a private vmbr1 for LXC-to-LXC,
    DNAT off the public IP; guests need static IPs (cloud DHCP won't lease to veths) and
    dnsmasq on vmbr1 for DNS; pve-firewall's nftables persist past `systemctl stop`.

*   k3s proven READY inside an **unprivileged** LXC (10.10.0.7) with
    `INSTALL_K3S_EXEC="server --kubelet-arg feature-gates=KubeletInUserNamespace=true"`
    (the kubelet dies without `/dev/kmsg` otherwise); an nginx pod serves 200 on pod /
    ClusterIP / NodePort, and 31500 from the PVE host. No nested virt needed — the
    whole appliance is LXC/pod-shaped. (Recorded here for completeness: k8s itself is
    Chunk 5's substrate, and the skill schema is Chunk 3's.)

## Chunk 3 — Capability runners, skill framework, and named peers

Goal: real reasoning agents, under capability-brokering and inbound-gating, install and
operate services end to end — the first expert via the LXC landing strip, the architecture
via deterministic pods, every side effect brokered through an audited, classed runner or
contained to disposable compute. The detailed plan (v5) is `roadmap/POC_CHUNK3.md`; the
deliverables actually shipped are the pre-C0 items:
*   **k3s is a deterministic configure stage (#120).** `freehold configure` boots the k3s
    LXC (auto-vmid, coords recorded to `lxc.k3s`, `managed += k3s`) and installs k3s with
    the spike-verified unprivileged posture
    (`INSTALL_K3S_EXEC="server --kubelet-arg feature-gates=KubeletInUserNamespace=true"`
    — without it the kubelet dies for lack of `/dev/kmsg`); pods ride it (nginx serving
    200 on pod / ClusterIP / NodePort, 31500 from the PVE host).

*   **The Services view is ready for litellm (#115/#120)** — the running dashboard lists
    `managed` pieces (relay/cp/k3s today); litellm appears automatically the moment its
    coords land in the config (the same machinery k3s used).

*   **Agents registry + live availability (#118)** — the CP records named AI agents
    (delegate-peer registers itself at start) and reports ●/○ availability from relay
    kind-9 presence; the buzz-acp agents register through the same path.

*   **The console has a WORKING relay scope** — deploy-cp wires
    `--relay-url`/`--relay-pubkey`/`--relay-host`/`--relay-host-ip`; a co-located console
    talks to the relay LXC over LAN with the community `Host` header + NIP-98 signed at
    the public URL + relay community membership — the precondition for the litellm-kube
    reads and the agent channel views.

*   **Teardown destroys the k3s LXC (#121)** — the box-lifecycle discipline covers the
    substrate; a per-tenant teardown keeps the workstation config (coords + tenant→dataset
    mapping) so reattach-by-reference works.

*   **Nothing lives only on disposable compute.** The durable volume plane (Phase 0.12)
    runs as a stage in the converge pipeline before relay/CP boot: Proxmox-LXC takes a
    read-only INVENTORY of every zpool/VG/thin-pool and whole disk (classified by the
    data each carries — freehold's own, safe-to-share, or ruled out), recommends the
    safest backend, and lets the operator choose; freehold reuses an existing backend
    and never erases a device that carries data. VPS resolves `block volume →
    downgraded local dir → bail`; per-tenant datasets sit under
    `<pool>/freehold/<domain>/<tenant>` (VPS labels flatten to
    `fh-<domain-dashes>-<tenant>`), the relay keeps TWO children (docker-root plus the
    compose deploy dir holding `BUZZ_RELAY_PRIVATE_KEY`), and compute-only teardown
    reattaches by reference — a new `pct create` born with its `mp=` mounts.

### Chunk 3 acceptance

*   The three connectors still work under relay identities (SSH live; Vultr live as a
    HOST path — the real accounts sat in the Chunk 2.5 spike; B2 hermetic-only). A
    non-member pubkey is denied; a member-but-ungranted pubkey is denied; the console is
    unreachable without an operator session.

*   Memory persists across a CP restart (30174's store is relay-side; set/get
    round-trips live; the restart-persistence run is still owed).

## Chunk 4 — A resilient CPA that creates agents and lives in Buzz

Goal: `@freehold` is a real, LLM-backed reasoning agent running in a Kube-slot — the system's main user
touchpoint. It is responsive in Buzz, durable and rebuildable with no data loss, gets its
purpose from a versioned system prompt, and can create a new agent on request.

*   **CPA runs on a real-agent harness in kube** (buzz-acp/goose-class) with a create/grant/
    manage-agent toolset, not as a scripted command-matcher.
    
*   **Bootstrap names the CPA.** Install asks the operator what to call their agent (default
    offered, e.g. `freehold`); the name becomes its Buzz handle/profile identity. Personal,
    not a fixed brand name baked into the product.
    
*   **Durable and rebuildable, live-proven:** a real Buzz conversation with the CPA survives
    a restart of its process and a full compute-only teardown/rebuild of its Kube, with
    identity and relay-persisted memory intact both times.
    
*   **Agent-creates-agent, stripped to the relationship, not the capability:** CPA can spin
    up a second agent from just a name + purpose — no service, no skill, no target — that
    gets its own durable relay-scoped identity/memory and is directly reachable in Buzz.
    
*   **Resource stress-test:** with at least CPA + one created agent running, get a real
    read on what an ad-hoc agent process actually costs (CPU/RAM/idle footprint), to inform
    whether/when agents need to sleep when idle and wake on @mention. Sleep/wake mechanics
    themselves are Chunk 5's deliverable (built against the pod substrate); this chunk
    collects the numbers.
    
*   **CPA system-prompt:** The CPA's purpose, tone, and toolset boundaries live in a single Markdown file
    (`agents/freehold/prompt.md`, embedded by the `freehold/agents` package and shipped by the control plane
    and mounted into each agent pod as its `<pod>-prompt` ConfigMap) — not generated at runtime,
    not improvised per spawn. Bootstrap loads it into the harness config at first spawn; every
    restart reloads the current file, so editing the prompt and redeploying is how CPA's purpose
    changes — versioned and reviewable like any other repo change, same as a per-expert
    `AGENTS.md` but for the one agent that isn't spawned by anything else.

### Chunk 4 acceptance

*   A human opens a room/DM with the named CPA in Buzz and gets real, reasoned responses.
    
*   Memory survives both a process restart and a full LXC teardown/rebuild, demonstrated
    live in Buzz.
    
*   CPA, asked in Buzz, creates a second agent (name + purpose only) that gets its own
    durable identity and is directly talkable — also surviving a rebuild.
    
*   CPA's purpose is defined by `agents/freehold/prompt.md`; changing the file and redeploying
    changes CPA's behavior.
    
*   Baseline resource numbers recorded for one CPA + one created agent, idle and active.
    

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
    need it.
    
*   The workspace/git credential surface (credential-helper vs. generic `exec()`) is decided
    explicitly here, not assumed.
    
*   Proof point: an agent deploys a service to an LXC using what it committed.
    
*   **The first department capability gets teeth: Compute.** Compute requests routed
    through the CPA land on the Compute identity, which holds the raw compute grant;
    custom agents never receive it. This is where "capability work goes through the owning
    department" stops being documentation and becomes the grant layout.
    
*   **Agents can read the repo.** The shared orientation block tells every non-custom agent
    to read the source repo on first boot, keep a memory of it, and re-check periodically
    because it is active; this chunk wires the git/GitHub grant and the read/schedule path
    that makes that instruction real.

### Chunk 5 acceptance

*   CPA-routed request → named peer provisions an LXC workspace → agent commits a real
    change → change is verified durable in Buzz's git and/or pushed to a real GitHub repo.
    
*   The workspace/git-runner credential surface is either adopted (with a written carve-out
    from the generic `exec()` model) or explicitly rejected in favor of it.
    

## Chunk 6 — Agents deploy via Kubernetes

Goal: the same agent-does-real-work loop from Chunk 4 generalizes from an LXC target to a
kube target.

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
    later vultr-vps/hetzner-vps) running runner-exec under the four locked disciplines (see
    `AGENTS.md`'s "Locked model"); G6's teardown/rebuild rides `terraform destroy` /
    `terraform apply` plus the verify harness.
    
*   Sleep/wake for ad-hoc agents is built here (idle auto-reap on pods), using the resource
    numbers gathered in Chunk 3.
    
*   **The department check-in hook lands here.** When an agent requests a service/compute
    through the CPA's provision path, Data asks "back this up?" and Network asks
    "reachable outside your network?" — a visible choice, with "no" as a valid final answer.
    Department status reuses the same postcondition-gated 🟢/🟡/🔴 language as runner/service
    readiness.
    

### Chunk 6 acceptance

*   Same proof point as Chunk 4 (agent deploys a service), targeting a kube namespace
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
    
*   VM-targeted deploys are deferred; G7's blue/green flip (Chunk 6) keeps the North Star's
    cross-provider restore reproducible on the same discipline.
    
*   This is also a standing dogfood tool once built: clone a running production freehold
    onto disposable hardware to test a risky change, without touching the real system, then
    discard the clone.
    

### Chunk 7 acceptance

*   Backblaze and Vultr each carry at least one real onboarded service/workload through the
    CPA loop.
    
*   A freehold instance running locally is backed up off-site, torn down entirely (hardware
    gone, not just compute), and restored onto different hardware/provider — same identity,
    memory, grants, and services present and correct.
    

## Done bar (current)

A real, durable, talkable, agent-creating CPA (Chunk 4) is the baseline "done" this roadmap
builds from; Chunks 5–6 extend it toward agents that do real work on LXC and kube targets,
and toward full hardware-portability.

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
