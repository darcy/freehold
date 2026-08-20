# Chunk 2 — Detailed Build Plan

Status: locked decisions from discussion; ready to structure into steps.  
Scope: POC, brings in Buzz relay + real control plane deployment. NO Kubernetes. NO general
provisioner-picker (that's Chunk 6). NO `@buzz-relay` agent (deferred/unnecessary — see below).

## Locked decisions

*   **The master/control agent (CPA) is handled as `@freehold`** in the relay — not
    `@control-plane` or similar. The agent is the product's voice; users talk to `@freehold`.

*   **CP and relay share a SCOPE, not a machine.** One CP = exactly one relay scope
    (relay-as-scope). The CP deploys to its OWN target (a dedicated LXC/box the operator
    chooses) and ATTACHES to the relay — same LXC only if co-location is chosen, never
    assumed, and the CP does not depend on managing the relay. A `@buzz-relay` management
    agent is not part of the POC (it would have almost nothing to do besides "add an
    agent") — extractable later if a real reason shows up (substrate swap, multi-relay).

*   **One CPA mode: relay-native.** The master/control agent (`@freehold`) exists ONLY as a
    relay-addressable peer inside the relay scope. There is no "runner-direct CPA" — the CPA
    never calls a runner directly, in any phase. Bootstrap + emergency repair are NOT CPA
    capabilities; they belong to the local provisioning expert.
*   **A separate, narrow local tool — the "local provisioning expert" — does bootstrap +
    repair, NOT the CPA.** It is the orchestrator's bootstrap/repair mode (`freehold
    bootstrap` — a role of the existing binary, not a new component): runner-shaped, in that
    it drives the connector runners through the same grant/exec primitives as any agent. It
    stands up the target + relay pre-relay, and it is re-invoked (dormant, on demand) for
    emergency repair when the relay is down (Phase F). Same tool, two triggers; no CPA
    involvement in either.
*   **Delegation mode is the CPA's only mode and the steady state:** relay is up, real peer
    agents exist; `@freehold` orchestrates by asking agents to do things instead of touching
    runners itself. What Chunk 3's onboarding pattern assumes.

*   **Chunk 2's actual job is proving the transition from local runner-direct bootstrap to
    delegation mode**, not just "relay is up."
*   **`freehold` is the SERVICE: the control plane** — agent (`@freehold`) +
    grant-management console + provisioner — deployed as its own unit on its OWN target
    (a dedicated LXC/box). Buzz is the SUBSTRATE it attaches to: same host, different
    infrastructure, or a relay the operator does not manage at all. The relay is NOT part
    of the service unit; co-location is convenience, and the CP never assumes it owns the
    relay. `@freehold` is the AGENT IDENTITY that lives inside the service once it is up
    (Phase D).
*   **Bootstrap treats "create the relay" and "deploy the CP onto a relay" as DECOUPLED,
    idempotent steps:** create-new (stand up a fresh relay on the target, the original path)
    or **attach-existing** (operator already runs a relay — its PRIMARY/management relay
    pre-existing at bootstrap; skip creation, point the CP at it). This is NOT the
    Architecture doc's "a user's existing relay is just a service" case (that is Chunk 3,
    onboarding a SEPARATE/secondary relay as a service) — the primary-attach case is decided
    here. Once attached, membership + port proceed IDENTICALLY to create-new: the CP becomes
    a member via the relay's own member management (buzz-admin add-member; the CP cannot
    self-add — locked, live-verified); identity porting (Phase D) is per-identity and
    relay-agnostic. The only new surface in attach mode is a liveness +
    membership-feasibility check before proceeding.

*   **The DOMAIN is the identity — never the IP.** Buzz resolves the community from the
    REQUEST HOST (row-zero host binding; an unmapped host is REFUSED — BUZZ_SURFACE §9.8).
    Bootstrap therefore REQUIRES `--domain` with a BLOCKING gate (Phase A4): create the
    target → IP known → print the resolver hint (LAN DNS, or `/etc/hosts` for the POC) →
    poll until the domain RESOLVES → only then write `BUZZ_DOMAIN`/`relay_url`
    = `<domain>` and continue. The resolution may point DIRECTLY at the target IP (the
    strict case) OR at an OPERATOR-MANAGED PROXY that forwards to it (e.g. nginx on a
    tailnet) — the gate proceeds on ANY resolution, warning loudly when the target
    differs so a silently-wrong resolver can't strand clients. The domain is permanent;
    the resolver is swappable (real DNS later). The current IP-anchored community is
    DISPOSABLE and is re-provisioned under the domain at the fresh-run re-test.
*   **VPS = Proxmox-on-Cloud-Compute (LXC-only) — Vultr AND Hetzner Cloud.**
    Both providers' cloud instances are KVM-virtualized with NO nested
    hardware virtualization, so Proxmox on them manages LXC containers but
    canNOT run KVM/QEMU VMs. Our stack is fully container/pod-shaped (CP,
    relay, service agents, LiteLLM, k3s-in-a-nested-LXC all LXC/pod), so the
    constraint is known-and-fine: an amd64 instance, custom-ISO PVE install,
    two LXCs (relay + cp) on the one host, more LXCs as services grow. The
    "vps" host collapses into the Proxmox host driver — the driver becomes
    "provision a PVE host on <provider>", then the SAME LXC flows run
    verbatim. ARM cloud instances (e.g. Hetzner cax*) are a separate
    architecture and are NOT the VPS path. A service that genuinely needs a
    real KVM VM routes to BARE METAL (Vultr BM or Hetzner dedicated), never
    the VPS host — a separate box, managed independently.

*   **TLS = the domain cert.** Default: a LOCAL CA cert issued for the domain (a LAN-only
    box has no Let's Encrypt path). When the operator provides a DNS provider API key: LE
    via DNS-01 (works behind NAT). `wss://` (and `https://` for the console) everywhere
    once the domain is live.

*   **One operator pubkey at bootstrap.** `--operator-pubkey` (supersedes
    `--installer-pubkey`): relay invite on create-new, membership/auth anchor on
    attach-existing, and the console's INITIAL admin whitelist entry (C3.5). Fail-closed:
    nobody is an operator or console admin until it is provided.

*   **The provisioning capability is the vehicle for that proof — DUPLICATED, not promoted.**
    The local provisioning expert boots the target + relay runner-direct. Once the relay is
    live, a NEW, separately-provisioned runner identity is minted INSIDE the relay for a NEW
    relay-addressable peer agent (e.g. `@proxmox` / `@vultr`) — credentials re-encrypted to
    the new runner key, independently granted, never the same identity promoted in place.
    The CPA delegates to that peer; the local expert stays local + dormant (repair reuse).

*   **Provisioning logic is owned by the expert (agent/runner), not CPA.** CPA doesn't know
    Proxmox/Vultr internals; it asks.

*   **Identity porting happens here.** Chunk 1's standalone keypairs + local registry get
    ported onto real Nostr relay membership (per Chunk 1's own locked decision: "Ports onto
    relay membership in Chunk 2"). Precise scope of the port: the KEYS are already real Nostr
    keypairs — the stand-in is the LOCAL REGISTRY (grants + runners in local state.json). The
    port moves membership/grants/memory/audit onto relay events.

*   **Delegation wire contract is relay events in the POC:** agents exchange rooms/DMs on the
    relay; a request and its result are correlated by event references (reply/quote). Pull-style
    like exec streaming; push later. No second HTTP surface for agent↔agent in Chunk 2.

*   **POC agents run scripted, joining the relay with their own NIP-42 client** — NOT a real
    reasoning agent (POC remains pre-reasoning; the proven wire + delegation shape is the
    point). Phase-0 research (BUZZ_SURFACE.md) corrected an assumption: `buzz-acp` targets LLM
    agents (goose/codex/claude) and is the Chunk-3+ harness path; E1 carries the same wording.

*   **No local-machine dev loop.** VPS and Proxmox are the two real targets, matching the
existing promote flow (VPS smoke → PVE host test → home).

## Goal (one sentence)

Prove the mode transition: the local provisioning expert boots a target + relay (create-new
the control plane deploys onto its own dedicated target (the `freehold` service,
OPERATE mode), Chunk 1's identity model ports onto real relay membership, and the CPA
(`@freehold`) delegates its first real task to a NEW relay-addressable peer agent — instead
of any runner-direct call.

## Demo that defines done

Local provisioning expert provisions an LXC/VPS → deploys the Buzz relay onto it (create-new;
→ deploys the CP onto its OWN dedicated LXC = the `freehold` service
(OPERATE mode; console loopback-only until authn lands) → CP joins the relay as a member
(SSH/Vultr/B2) get re-registered under real Nostr identities on the relay instead of the
local stand-in registry → a NEW provisioning runner identity is minted inside the relay,
re-encrypted credentials, granted to a relay-addressable peer agent (a DUPLICATE, not a
promotion) → a user talks to `@freehold` in Buzz (room/DM) and asks it to do something
involving the provisioning capability → CPA delegates that ask to the relay peer instead of
calling a runner itself → result comes back through Buzz.

---

## Ordered steps

### Phase 0 — Buzz surface research (the external-system gate)

Buzz is a real product with opinions about workspaces, membership, agents, and memory — it
is NOT a blank event store. Every port design decision in D–F keys off what it actually
exposes, so the surface is resolved BEFORE the schema is written.

- [x] [x]

01. Read Buzz's actual integration surfaces we must consume: workspace + member model
(invite/join, or open?), agent identity (keys/roles), event kinds (arbitrary/custom, or
fixed?), rooms/DMs, and any native agent memory. Deliverable: a short written note naming
exactly what the port consumes per capability.

- [x] [x]

02. Decide per capability — membership, grants, memory, audit, delegation — whether it rides
Buzz's native concept or a custom kind we define ON the relay. This is the input D1's event
schema must match: no kind is designed against an assumption.

**Deliverable:** `roadmap/BUZZ_SURFACE.md` — the surface note naming exactly what the port
consumes per capability. Native kinds found for most of it (membership 13534, agent memory
30174, audit 48001, jobs 43001–43006, DMs 41001); only GRANTS needs a freehold custom kind.

### Phase A — Bootstrap provisioning (runner-direct, pre-relay)

- [x] [x]

A1. Confirm/choose the primary target type for this chunk's build+test pass — VPS first for
iteration speed, matching the existing promote flow, then validated against the PVE host.

- [x] [x]

A2. The **local provisioning expert** (the orchestrator's bootstrap mode — a narrow,
    runner-shaped local tool, NOT the CPA: the CPA has no local existence in any mode) calls
    the relevant provisioning runner **directly** — no agent fabric exists yet — to stand up
    the target. Capabilities: `vultr create/destroy` + `hetzner create/destroy` via the EXISTING API runners (VPS);
    for Proxmox, the EXISTING ssh runner drives `pvesh`/`pct` on the PVE host (generic
    exec — the tool writes the commands; no new Proxmox connector is built).

- [x] [x]

A3. Verify target reachable (SSH/API) before proceeding — the same self-check pattern as
Chunk 1's runner readiness, applied to the freshly provisioned box.

- [x] [ ]

A4. **Domain gate (blocking).** Require `--domain`. After the target is up with an IP,
    print the IP + the domain + the resolver hint ("map <domain> → <IP> in LAN DNS, or
    /etc/hosts for the POC") and POLL until the domain resolves to that IP (bounded
    retry). The install does NOT proceed until the resolver is tied to it — the domain,
    not the IP, becomes the community's identity from event zero.

### Phase B — Deploy the Buzz relay

- [x] [x]

- [x] [ ]

B0. **Branch: create-new vs attach-existing.** No operator relay → create it (B1–B2).
    Operator has one (its primary/management relay pre-existing at bootstrap) → SKIP
    creation; verify the relay is reachable + healthy and the CP can become a member (open,
    or member-invitable via the relay's own admin path — buzz-admin), then proceed straight
    to C. Re-runs are idempotent: same target → resume, never re-create.

- [ ] [ ]

B1. Install the self-hosted Nostr relay stack onto the target (Postgres, Redis, S3/MinIO
backend per the Architecture doc), driven by the provisioning runner's exec.

- [x] [x]

B2. Confirm relay is reachable and healthy (its own self-check, distinct from CP readiness).

- [ ] [ ]

B2b. **TLS on the domain.** Issue the domain cert (local CA by default; LE DNS-01 when a
     DNS provider key is given); write `BUZZ_DOMAIN`/`relay_url` = `https://<domain>`
     (`wss://`); the relay serves TLS with the domain cert, and refuses non-domain hosts
     (the strict host map is a feature).

- [x] [x]

B3. This relay becomes the control plane's ONE scope going forward (relay-as-scope) —
    create-new only; attach-existing already HAS its scope: the operator's relay.

### Phase C — Deploy the control plane onto its own target (the `freehold` service)

- [x] [ ]

C1. Deploy the CP app onto its OWN target (a dedicated LXC/box the operator chooses — a
    different LXC than the relay's by default; the CP attaches to whatever relay it is
    pointed at, managed or not), now running in **OPERATE mode** instead of localhost
    (Chunk 1 was effectively local/BOOTSTRAP-adjacent). **The console is loopback-bound on
    the deployed target** — OPERATE mode means the process + its data live on the target,
    NOT that the UI is network-exposed. TODAY the console has no authentication:
    loopback-only, and operator access from elsewhere is an SSH tunnel
(`ssh -L 8080:127.0.0.1:8080 target`).

- [x] [ ]

C2. CP becomes a member of the relay it is pointed at — the SAME path for create-new and
    attach-existing (the relay's own member management, buzz-admin add-member; the CP
    cannot self-add). "Bootstrap is self-scoping" applies to the ferry identity that
    arranges the join, not to the relay's membership record (live-verified in Phase C).

- [x] [ ]

C3. Verify the console is served from the deployed target and reachable ONLY via the
loopback tunnel: `curl` on the box's own 127.0.0.1 works; a remote attempt at the box's LAN
address is refused. **The CP refuses to bind a non-loopback address without an authn/TLS
story** — the guard is part of this item. Console authentication + TLS for real non-loopback
exposure is a named security-hardening follow-up (ARCHITECTURE Future items), NOT in this

- [ ] [ ]

C3.5. **Console authentication (NIP-98 operator login).** The bootstrap seeds the ADMIN
      whitelist with `--operator-pubkey`. Login: server issues a challenge `{nonce, ts}`
      (60s freshness) → the operator signs it with their nsec → the server verifies the
      signature, strips the pubkey, checks it is in the admin whitelist → issues a session
      cookie (httponly, SameSite=Strict, Secure once TLS is up). Every `/api/*` call
      requires the session; Origin-check + login rate-limit. The bind guard becomes
      AUTHN-CONDITIONAL: authn + admin whitelist configured → the console may bind the LAN
      (reachable over the network with operator auth); otherwise the loopback-only refusal
      (C3) stays byte-for-byte. TLS (the domain cert) removes the need for ip-binding and
      short-TTL session gymnastics.
chunk.

- [x] [ ]

C4. Decide the console's data source post-port: the RELAY is authoritative for MEMBERSHIP;
    local state stays authoritative for GRANTS (the operational shipped-package flow — the
    kind-30180 relay path is dormant, per D1/D4). Local state mirrors membership (readable
    offline, write-through). State this in the console code, not implicitly.

### Phase D — Port identity onto real relay membership

- [x] [ ]

D1. Define the relay event kinds for the port: membership (who is in the scope), grants
(agent↔runner, membership-derived), memory (agent state that persists across runs), and
audit. Schema is part of this item — the event kinds are the new contract — and must match
the Phase 0 surface (native Buzz concept where one exists, custom kind where we define it;
never a kind designed against an assumption). **Grant list (kind 30180):** addressable
30000–39999, `d`-tag = runner pubkey, content `{"grants":[<64-hex agent>...],"schema":1}`,
replaceable (a re-publish REPLACES — revocation never appends). Published by the console
identity (NIP-98 POST /events), read live by the runner (NIP-98 GET /query, newest
created_at wins incl. same-second).`** **Operational scope:**
grants run via the shipped-package flow (web console + CP CLI, re-read per call); the
kind-30180 relay path is DORMANT/optional — buzz's ingest restrict-list refuses custom
kinds (BUZZ_SURFACE §9.5) and freehold does NOT patch buzz.`**

- [ ] [ ]

D2. Re-register Chunk 1's three runners (SSH/Vultr/B2) on the relay, replacing the
    local-registry stand-in. **The identity material is UNCHANGED** — the existing Nostr
    keypairs and encryption pubkeys stay exactly as shipped (sealed blobs are pinned to the
    recipient enc pubkey + secret name; new keys would silently kill every shipped
    `secrets.json`). What moves is the MEMBERSHIP RECORD: membership now lives on the relay
    instead of local state.json. GRANTS stay in the shipped package (operational, D4) — the
    relay grant event (D1) is the dormant alternative.

- [x] [ ]

D3. Master agent (`@freehold`) gets a real identity in the relay; memory becomes relay-persisted
(relay event store) instead of local/ephemeral. **Memory event payloads are encrypted** — a
relay operator is not a reader of agent memory; exact kind/scheme decided in D1 against the
Phase 0 surface.

- [x] [ ]

D4. Grants keep the Chunk-1 model: coarse agent↔runner whitelists of real Nostr pubkeys.
    Relay **membership is necessary but NOT sufficient** — a member must still be explicitly
    granted to a runner; grants do not collapse into "in the scope." **Operational scope
    (decided):** the whitelist stays in the SHIPPED PACKAGE (web console + CP CLI), re-read
    by the runner PER CALL — which already delivers revoke-without-restart (the Chunk-1 gap
    closed in Chunk 1). The kind-30180 relay event path (D1) is the DORMANT alternative, not
    the deployed one.

- [x] [ ]

D5. **Audit becomes additive, not a replacement:** the same BIP-340-signed event is spooled
locally (Chunk 1's `audit.log` stays) AND published to the relay once live (the locked model:
the runner signs a Nostr event for every executed command into the relay). Phases A/B run
PRE-relay and are the chunk's most privileged execs — they must be audited before any sink
exists. Fail-closed rules: local append can never fail silently; relay publish failure
degrades to local-spool-only and is surfaced, never silently dropped. The acceptance script's
G3.1 check adapts to read relay events while still asserting the local spool.

### Phase E — Prove delegation mode

- [x] [ ]

E1. DUPLICATE the provisioning capability into the relay — do NOT promote the Phase-A
    identity in place. Mint a NEW runner identity + NEW relay-addressable peer agent (e.g.
    `@proxmox` / `@vultr`) with its own NIP-42 client (scripted; `buzz-acp`/LLM harness is
    Chunk-3+), grant it, and re-encrypt the target credentials to the NEW runner key.
    Acceptance asserts KEY-MATERIAL SEPARATION: the peer's keypair + credential must be
    provably distinct from the local expert's — a same-key rename is a failure. The local
    provisioning expert is not promoted — it stays local and dormant (Phase F repair reuse).
    Not a reasoning agent.

- [ ] [ ]

E2. CPA (`@freehold`), now relay-connected, delegates a provisioning-flavored ask to that agent
over relay events (request event → reply event, correlated) instead of calling the runner
itself — the actual mode-transition proof, not a new capability.

- [ ] [ ]

E3. Confirm the result flows back through the agent, not a direct runner response — this is
what distinguishes delegation mode from Phase A's runner-direct call.

### Phase F — Emergency repair (locked — NOT an open item)

The relay-down case re-invokes the SAME dormant local provisioning expert against the SAME
target — a second invocation of the Phase-A tool, NOT a fresh bootstrap and NOT a CPA
capability (the CPA exists only inside the relay; if the relay is down, the CPA is down
too). Locked decision: exercised here, not just reserved.

- [ ] [ ]

F1. Take the relay down (compose stop on the target) while the CP + local expert remain
    available on the box.

- [ ] [ ]

F2. Re-invoke the local provisioning expert against the target; confirm the call is a
    REPAIR (state-aware resume — target already provisioned, relay known) not a fresh
    bootstrap: no re-create, no new identity, same target.

- [ ] [ ]

F3. Restore the relay; confirm the CP rejoins its scope and the relay-addressable peer
    agents reconnect — delegation mode intact after repair, no CP reinstall.


### Phase G — Buzz as the interaction surface

- [ ] [ ]

G1. User can open a room/DM with `@freehold` in Buzz (not the local script from Chunk 1).

- [ ] [ ]

G2. User asks `@freehold` (via Buzz) to do the Phase E task; verify it triggers delegation to
the peer agent rather than a local script call.

- [ ] [ ]

G3. Confirm memory persists across a restart of the CP process (proving relay-persisted
memory, not in-process state).

### Phase H — Acceptance script

CI runs the checks against hermetic fixtures — a mock relay in `testkit` (same pattern as
the mock Vultr/B2/sshd) — with the REAL relay on the promote path (VPS → PVE host). The
H1–H6 script is parameterized the same way `freehold-acceptance` already is; no local
dev loop is introduced by adding fixtures.

- [ ] [ ]

H1. Fresh run (create-new): provision target → relay up (TLS on the DOMAIN, `wss://`,
    non-domain hosts refused) → CP up on ITS own LXC → CP is a relay member. The A4
    [x] create-new leg proven LIVE under freehold-test.darcydev.net (relay LXC +
    CP LXC + domain gate + operator login + memory + delegation). The A4
    domain gate is asserted (install never proceeds without the domain resolving to the
    target IP). PLUS attach-existing run: point the CP at a pre-existing relay (skip
    creation) → CP is a member, same acceptance.

- [ ] [ ]

H2. Chunk 1's three connectors (SSH/Vultr/B2) still work, now under real relay identities.

- [x] [ ]

H3. A real delegation happens at least once (CPA → relay peer, not any runner-direct call)
    and is demonstrably distinct from bootstrap's local-expert runner-direct calls.

- [ ] [ ]

H4. User can talk to `@freehold` via Buzz room/DM and get the delegated result back.

- [ ] [ ]

H5. Memory persists across a CP restart.

- [ ] [ ]

H6. RE-ADAPT Chunk 1's acceptance invariants (G3.1–G3.3: secrets never in agent context,
    ciphertext-only + injected key, no master key) via the existing `freehold-acceptance`
    harness under the relay regime, testing the port's DELTA only:
    (a) a NON-MEMBER pubkey is denied;
    (b) a MEMBER-but-ungranted pubkey is denied — the case that actually catches a
    grants-collapse regression (D4);
    (c) the console is unreachable off-loopback without the tunnel (C1/C3 bind guard).
Everything else must still hold unchanged.

### Phase I — Test / promote

- [ ] [ ]

I1. Build/iterate against VPS first (fast, disposable, matches dev/smoke pattern).

- [ ] [ ]

I2. Validate the same flow against the PVE host — LXC provisioning via the ssh runner
driving `pvesh`/`pct` (no new connector) — proving bootstrap isn't VPS-only, without yet
building the general Chunk-6 provisioner picker.

- [ ] [ ]

I3. Do **not** promote to home dogfood yet — Chunk 3 (skill framework + real expert agents) is
the more meaningful dogfood milestone; Chunk 2 is infrastructure-proving.

---

## Open items carried forward (not blocking, but worth tracking)

*   Chunk 6 will need to generalize Phase A's single-target bootstrap into the real "pick a
    provisioner" picker (VPS vs Proxmox vs Incus) — Chunk 2 deliberately hardcodes one path per
    target type, same way Chunk 1 hardcoded a scripted orchestrator instead of real reasoning.
*   **SECONDARY-relay onboarding** (a user's SEPARATE Buzz relay becomes a service via a
    relay runner, per Architecture's locked model) remains Roadmap-Chunk-3 work —
    consciously deferred here, not omitted. This is NOT the bootstrap attach-existing path
    (a pre-existing PRIMARY/management relay at bootstrap, decided in the locked decisions
    above); the two cases are deliberately distinct.
*   **Agent naming in the relay** — `@freehold` is the CPA; peer experts are `@<service>`.
    Naming/mention conventions beyond that land with Chunk 3's fabric work.

---

## Chunk 2.5 — VPS = Proxmox-on-Cloud-Compute (refactor + spike)

Scope: unify the VPS legs onto the Proxmox host driver and PROVE the whole
appliance on a cloud PVE host live.

- [ ] [ ]

V1. **Driver reshape.** `bootstrap --kind vultr-vps | hetzner-vps` becomes
    "provision a PVE-capable HOST on <provider>": create the amd64 instance,
    custom-ISO-boot the Proxmox VE installer (unattended answer file), wait
    for PVE to respond — then the existing `bootstrap proxmox-lxc`,
    `deploy-relay`, `deploy-cp` flows run IDENTICALLY. No VM-per-service
    host driver exists anymore.

- [ ] [ ]

V2. **Networking path (the one new subsystem).** Cloud PVE has a SINGLE
    public NIC: vmbr0 over eth0; a PRIVATE bridge (vmbr1) for LXC-to-LXC;
    DNAT on the public IP to the relay LXC's Caddy and the CP console.
    Client reachability is domain → proxy → host public IP → DNAT → LXC.
    The home-flow "LXC on the LAN with its own IP" pattern does not apply.

- [x] [x]

V3. **The spike (de-risking, live on BOTH providers).** COMPLETE — see the
    Chunk-2.5 LIVE VERIFICATION note below.

- [ ] [ ]

V4. **Skill-schema flag.** `target: lxc | pod | either` assumes no skill
    declares a hard KVM-VM requirement; if one ever does, it routes to Bare
    Metal, not the VPS host. Noted, not built.

Storage mapping (instance root disk + optional attached block volume as the
LVG) and the amd64-only note ride V2/V3 — decided in the spike, not in
advance. Bare-metal is a documented escape hatch, NOT an MVP path.

## Chunk 2.5 — LIVE VERIFICATION (both providers)

The full appliance ran on Proxmox-on-Cloud-Compute on Vultr (45.76.255.185)
AND Hetzner (178.156.179.204): PVE-on-Debian-13 (apt route), two static-IP
LXCs (relay + cp) on a private bridge, relay + CP deployed, console a relay
member, operator NIP-98 login — relay and console both reachable PUBLICLY
through host DNAT (`/_liveness` ok). No nested KVM exists on either cloud
(`cpuinfo` confirms) — the LXC/pod appliance needs none.

Findings (all became driver/docs hardening):
- Cloud DHCP will NOT lease to LXC veths -> guests need STATIC IPs on a
  private bridge + host NAT (driver: --lxc-ip/--lxc-gw, #58).
- pve-firewall's nftables PERSIST after `systemctl stop` (14 drop rules) —
  flush them (or never start it on a single-host spike) or every guest
  loses egress, silently.
- download.proxmox.com serves CN=enterprise.proxmox.com -> apt over http;
  the trixie release key exists ONLY on enterprise.proxmox.com (the docs
  URL 404s). The apt-route needs: /etc/hosts node -> non-loopback IP
  (pmxcfs refuses 127.0.1.1), the pve node lxc/qemu-server dirs, no vmbr0
  by default. Debian 13's compose v2 package is `docker-compose`
  (not docker-compose-v2). Guest DNS needs a relay (dnsmasq on vmbr1) —
  the host's resolver won't answer NAT'd guests. Hetzner: NIC = eth0,
  cloud-init `chpasswd: expire: false` avoids the forced root-password
  change, and the private-only + DNAT-off-the-NIC variant avoids the
  bridge-move lockout entirely (Vultr kept the public bridge move).

## k8s substrate verification (POST-2.5 — closes the "k3s unproven" flag)

The Chunk-3/MVP "deterministic k8s pods" substrate was verified INSIDE the
Proxmox-on-Cloud path (spike finding follow-up):

- k3s node READY in an UNPRIVILEGED LXC on the cloud PVE host (10.10.0.7,
  static private IP; v1.36.3+k3s1, containerd). The ONE required flag:
  `INSTALL_K3S_EXEC="server --kubelet-arg feature-gates=KubeletInUserNamespace=true"`
  — the kubelet otherwise dies without /dev/kmsg (absent in the unprivileged
  LXC; even a device-cgroup allow does NOT materialize it — userns).
- Real workload: nginx pod Running (image pulled through the LXC's NAT
  egress); pod-ip:200, cluster-ip:200, NodePort:200 — both inside the LXC
  and from the PVE host (10.10.0.7:31500 -> pod).
- Conclusion: the k8s layer rides the same static-net LXC + NAT/DNAT path
  the appliance already uses; no nested virt needed, matching the
  LXC/pod-shaped design. Chunk-3 deterministic-manifest work builds on a
  verified substrate.

## Chunk 2.6 — Relay-authoritative runner lifecycle (RUNNER_PROFILE + rebuild)

**Status:** IMPLEMENTED, hermetic-VERIFIED. Scope decision: the
CP-stays-writer slice — the low-risk 90%. Live relay PUBLISHING is
DORMANT against stock Buzz (the same gate as grants): the relay's ingest
allowlist is hardcoded (`buzz-relay/src/handlers/ingest.rs::scopes()`,
BUZZ_SURFACE §9.5 — "there is no config allowlist"), so kind 30181
(like 30180) is refused with `restricted: unknown event kind` until G-1
is resolved. `rebuild` + the author gate are LIVE-READY and the gate was
proven live (a fresh console folds nothing — 403 membership-required —
until re-admitted).

### What landed (additive; no deployed contract changed)

- [x] New addressable kind **30181 `RUNNER_PROFILE`** (core): a runner's
      lifecycle snapshot — identity pubkeys, connector kind/address, status,
      secret NAME only. Carries revoke as a status flip and rotate as a
      `rotated_at` flip: SAME d-tag (runner pubkey), REPLACE never append.
      Kind discipline: 30000–39999 addressable registry, next to 30180
      (BUZZ_SURFACE §5). Number collision-checked against the documented
      used set (30174–30179 taken; 30181 free) — free NUMBER, but note
      ingest acceptance is a SEPARATE gate (G-1; see Status).
- [x] CP publishes the profile at EVERY lifecycle mutation (`--relay-url`
      on provision/adopt/rotate/revoke): provision/adopt publish "active",
      rotate flips `rotated_at`, revoke flips `status` (+ the existing
      empty-grants cut-off). Author-gated to the console, Schnorr-verified
      locally — same trust anchor as grants (a rogue member cannot mint or
      clobber runner records).
- [x] `control-plane rebuild --relay-url` — the disposable-CP fold: query
      ALL profiles, reconstruct the store deterministically + idempotently
      (re-run converges to the same state). Restored records carry NO
      ciphertext/package path — the relay never holds secret material;
      `adopt` per runner re-arms the package (documented re-trust step: a
      rebuilt CP's new console pubkey reads nothing until re-admitted).
- [x] **G-2 (freshness) resolved for this slice:** grants stay
      query-per-call, fail-closed on relay-down — no cache, so no TTL
      needed and NO posture flip. Subscription+TTL only becomes relevant if
      a cache is introduced later; that is its own gate.
- [x] **G-1 (fork-vs-contribution) DEFERRED, not resolved:** no Buzz
      changes shipped. Runner self-reporting / relay-side handlers wait
      until a real multi-writer need (Chunk 3 self-discovering agents).
- [x] **Migration:** additive-only (new kind; 30180 contract untouched;
      old runners ignore the new kind). The live fleet needs no cutover.

### Verification (hermetic, all green)

- [x] Unit: parse/accept-reject, newest-wins-per-runner replace semantics,
      rogue-author ignored (merge fn is shared with the HTTP query, so the
      REAL path is tested).
- [x] Integration (fake relay with real NIP-98 + signature verify):
      publish→query roundtrip over HTTP; revoke REPLACES (never appends);
      rogue author can't mint/clobber; rebuild is deterministic +
      idempotent (two folds → identical records AND identical state.json),
      restored records carry no ciphertext/package path.
- [x] Workspace: full `cargo test` green, clippy 0, fmt clean.

### Outcome mapped to the Chunk 2.6 plan

Code-complete + hermetic-verified: 30180 (grants) and 30181 (lifecycle)
are both implementable as relay-published addressable records, and CP's
file store is foldable into a projection (`rebuild`). What stays true
TODAY against a stock relay: the operational grant flow is the
shipped-package per-call read (30180 relay publish is dormant, per
BUZZ_SURFACE §9.5); 30181 publishing is dormant for the same reason.
CP loss is rebuild + re-adopt ONLY once G-1 lets the profiles reach the
relay. Until then CP's file store remains the durable record. This keeps
Chunk 2.6 honest: it does NOT silently turn `--relay-url` into a
working live path — it makes the profile path EXIST, tested, and
gated exactly like grants.
