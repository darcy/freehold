# Chunk 2 — Detailed Build Plan

Status: locked decisions from discussion; ready to structure into steps.  
Scope: POC, brings in Buzz relay + real control plane deployment. NO Kubernetes. NO general
provisioner-picker (that's Chunk 6). NO `@buzz-relay` agent (deferred/unnecessary — see below).

## Locked decisions

*   **The master/control agent (CPA) is handled as `@freehold`** in the relay — not
    `@control-plane` or similar. The agent is the product's voice; users talk to `@freehold`.

*   **CP and relay share ONE target.** Not split across separate services for the POC — a
    `@buzz-relay` agent would have almost nothing to do besides "add an agent," which doesn't
    justify its own identity/grants/readiness surface yet. Extractable later if a real reason
    shows up (substrate swap, multi-relay, etc.).

*   **Two operating modes for CPA, not two capabilities:**
    *   **Runner-direct mode** — CPA calls runners straight, no delegation, no relay dependency.
        This is what bootstrap *is* (nothing to delegate to yet) and it's also what "emergency
        fix Buzz" is (if Buzz is down, delegation isn't available either). Same capability, two
        triggers.
    *   **Delegation mode** — relay is up, real peer agents exist, CPA orchestrates by asking
        agents to do things instead of calling runners itself. Steady-state, and what Chunk 3's
        onboarding pattern assumes.

*   **Chunk 2's actual job is proving the transition from runner-direct → delegation mode**,
    not just "relay is up."

*   **The provisioning agent is the vehicle for that proof**, not a synthetic one. CPA uses the
    provisioning runner (e.g. `@proxmox` or `@vultr`, whichever target) runner-direct for
    bootstrap, then — once the relay is live — talks to that same capability as a real
    relay-addressable peer agent. One concrete thing proves both modes.

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
    existing promote flow (VPS smoke → old-laptop Proxmox test → home).

## Goal (one sentence)

Prove the mode transition: CPA (`@freehold`) provisions a target runner-direct, stands up Buzz
+ CP on it, ports Chunk 1's identity model onto real relay membership, and then delegates its
first real task to a now-relay-addressable peer agent — instead of calling a runner directly.

## Demo that defines done

CPA (runner-direct) provisions an LXC/VPS → deploys Buzz relay onto it → deploys CP onto the
same target (OPERATE mode) → CP joins the relay as a member → Chunk 1's runners (SSH/Vultr/B2)
get re-registered under real Nostr identities on the relay instead of the local stand-in
registry → a user talks to `@freehold` in Buzz (room/DM) and asks it to do something involving
the provisioning capability → CPA delegates that ask to the provisioning agent (now a relay
peer) instead of calling the runner itself → result comes back through Buzz.

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
iteration speed, matching the existing promote flow, then validated against old-laptop Proxmox.

- [x] [x]

A2. CPA (still scripted/orchestrator, no real reasoning yet) calls the relevant provisioning
runner **directly** — no agent fabric exists yet — to stand up the target. Capabilities:
`vultr create/destroy` via the EXISTING vultr runner (VPS); for Proxmox, the EXISTING ssh
runner drives `pvesh`/`pct` on the laptop host (generic exec — the agent writes the commands;
no new Proxmox connector is built).

- [x] [x]

A3. Verify target reachable (SSH/API) before proceeding — the same self-check pattern as
Chunk 1's runner readiness, applied to the freshly provisioned box.

### Phase B — Deploy the Buzz relay

- [x] [x]

B1. Install the self-hosted Nostr relay stack onto the target (Postgres, Redis, S3/MinIO
backend per the Architecture doc), driven by the provisioning runner's exec.

- [x] [x]

B2. Confirm relay is reachable and healthy (its own self-check, distinct from CP readiness).

- [x] [x]

B3. This relay becomes the control plane's ONE scope going forward (relay-as-scope).

### Phase C — Deploy the control plane onto the same target

- [ ] [ ]

C1. Deploy CP app onto the same target, now running in **OPERATE mode** instead of localhost
(Chunk 1 was effectively local/BOOTSTRAP-adjacent). **The console STAYS bound to loopback on
the deployed target** — OPERATE mode means the process + its data live on the box, NOT that
the UI is network-exposed. The console has no authentication (loopback-only by design,
Chunk 1); operator access from elsewhere is an SSH tunnel
(`ssh -L 8080:127.0.0.1:8080 target`).

- [ ] [ ]

C2. CP self-adds as a member of the relay it just helped create ("bootstrap is self-scoping:
the CP adds itself as a member").

- [ ] [ ]

C3. Verify the console is served from the deployed target and reachable ONLY via the
loopback tunnel: `curl` on the box's own 127.0.0.1 works; a remote attempt at the box's LAN
address is refused. **The CP refuses to bind a non-loopback address without an authn/TLS
story** — the guard is part of this item. Console authentication + TLS for real non-loopback
exposure is a named security-hardening follow-up (ARCHITECTURE Future items), NOT in this
chunk.

- [ ] [ ]

C4. Decide the console's data source post-port: the RELAY is authoritative for
membership/grants; local state becomes a cache mirror (readable offline, write-through). State
this in the console code, not implicitly.

### Phase D — Port identity onto real relay membership

- [ ] [ ]

D1. Define the relay event kinds for the port: membership (who is in the scope), grants
(agent↔runner, membership-derived), memory (agent state that persists across runs), and
audit. Schema is part of this item — the event kinds are the new contract — and must match
the Phase 0 surface (native Buzz concept where one exists, custom kind where we define it;
never a kind designed against an assumption).

- [ ] [ ]

D2. Re-register Chunk 1's three runners (SSH/Vultr/B2) on the relay, replacing the
local-registry stand-in. **The identity material is UNCHANGED** — the existing Nostr keypairs
and encryption pubkeys stay exactly as shipped (sealed blobs are pinned to the recipient enc
pubkey + secret name; new keys would silently kill every shipped `secrets.json`). What moves
is the RECORD: membership + grants now live on the relay instead of local state.json.

- [ ] [ ]

D3. Master agent (`@freehold`) gets a real identity in the relay; memory becomes relay-persisted
(relay event store) instead of local/ephemeral. **Memory event payloads are encrypted** — a
relay operator is not a reader of agent memory; exact kind/scheme decided in D1 against the
Phase 0 surface.

- [ ] [ ]

D4. Grants keep the Chunk-1 model: coarse agent↔runner whitelists of real Nostr pubkeys.
Relay **membership is necessary but NOT sufficient** — a member must still be explicitly
granted to a runner; grants do not collapse into "in the scope." What changes: the whitelist
lives on the relay instead of local state, and its validity derives from relay membership (a
grant references a member). **Decided here:** grants are re-read LIVE from relay events
(subscribe/poll, same per-call freshness as today) — this CLOSES the Chunk-1 gap
"rotate/re-grant don't reach a running runner" (boot-read was rejected for exactly that
reason). The runner-side mechanic lands in D1's grant event design.

- [ ] [ ]

D5. **Audit becomes additive, not a replacement:** the same BIP-340-signed event is spooled
locally (Chunk 1's `audit.log` stays) AND published to the relay once live (the locked model:
the runner signs a Nostr event for every executed command into the relay). Phases A/B run
PRE-relay and are the chunk's most privileged execs — they must be audited before any sink
exists. Fail-closed rules: local append can never fail silently; relay publish failure
degrades to local-spool-only and is surfaced, never silently dropped. The acceptance script's
G3.1 check adapts to read relay events while still asserting the local spool.

### Phase E — Prove delegation mode

- [ ] [ ]

E1. Promote the provisioning capability used in Phase A (e.g. `@proxmox` or `@vultr`) from
"runner CPA calls directly" to a real relay-addressable peer agent — a relay identity with its
own NIP-42 client (scripted; `buzz-acp`/LLM harness is Chunk-3+), NOT a reasoning agent.

- [ ] [ ]

E2. CPA (`@freehold`), now relay-connected, delegates a provisioning-flavored ask to that agent
over relay events (request event → reply event, correlated) instead of calling the runner
itself — the actual mode-transition proof, not a new capability.

- [ ] [ ]

E3. Confirm the result flows back through the agent, not a direct runner response — this is
what distinguishes delegation mode from Phase A's runner-direct call.

### Phase F — Buzz as the interaction surface

- [ ] [ ]

F1. User can open a room/DM with `@freehold` in Buzz (not the local script from Chunk 1).

- [ ] [ ]

F2. User asks `@freehold` (via Buzz) to do the Phase E task; verify it triggers delegation to
the peer agent rather than a local script call.

- [ ] [ ]

F3. Confirm memory persists across a restart of the CP process (proving relay-persisted
memory, not in-process state).

### Phase G — Acceptance script

CI runs the checks against hermetic fixtures — a mock relay in `testkit` (same pattern as
the mock Vultr/B2/sshd) — with the REAL relay on the promote path (VPS → laptop). The
G1–G6 script is parameterized the same way `freehold-acceptance` already is; no local
dev loop is introduced by adding fixtures.

- [ ] [ ]

G1. Fresh run: provision target → relay up → CP up on same target → CP is a relay member.

- [ ] [ ]

G2. Chunk 1's three connectors (SSH/Vultr/B2) still work, now under real relay identities.

- [ ] [ ]

G3. A real delegation happens at least once (CPA → peer agent, not CPA → runner directly) and
is demonstrably distinct from bootstrap's runner-direct calls.

- [ ] [ ]

G4. User can talk to `@freehold` via Buzz room/DM and get the delegated result back.

- [ ] [ ]

G5. Memory persists across a CP restart.

- [ ] [ ]

G6. RE-ADAPT Chunk 1's acceptance invariants (G3.1–G3.3: secrets never in agent context,
ciphertext-only + injected key, no master key) via the existing `freehold-acceptance`
harness under the relay regime, testing the port's DELTA only:
(a) a NON-MEMBER pubkey is denied;
(b) a MEMBER-but-ungranted pubkey is denied — the case that actually catches a
grants-collapse regression (D4);
(c) the console is unreachable off-loopback without the tunnel (C1/C3 bind guard).
Everything else must still hold unchanged.

### Phase H — Test / promote

- [ ] [ ]

H1. Build/iterate against VPS first (fast, disposable, matches dev/smoke pattern).

- [ ] [ ]

H2. Validate the same flow against old-laptop Proxmox — LXC provisioning via the ssh runner
driving `pvesh`/`pct` (no new connector) — proving bootstrap isn't VPS-only, without yet
building the general Chunk-6 provisioner picker.

- [ ] [ ]

H3. Do **not** promote to home dogfood yet — Chunk 3 (skill framework + real expert agents) is
the more meaningful dogfood milestone; Chunk 2 is infrastructure-proving.

---

## Open items carried forward (not blocking, but worth tracking)

*   Whether "emergency fix Buzz" as a CPA capability gets exercised/tested in Chunk 2, or just
    architecturally reserved for later — Chunk 2 doesn't need to break Buzz on purpose to prove
    runner-direct fallback still works, but it's worth a mental note that the capability should
    still be *possible* post-Chunk-2.
*   Chunk 6 will need to generalize Phase A's single-target bootstrap into the real "pick a
    provisioner" picker (VPS vs Proxmox vs Incus) — Chunk 2 deliberately hardcodes one path per
    target type, same way Chunk 1 hardcoded a scripted orchestrator instead of real reasoning.
*   **Existing relay onboarding** (a user's own Buzz relay becomes a service via a relay
    runner, per the architecture's locked model) is Roadmap-Chunk-3 work — consciously deferred
    here, not omitted.
*   **Agent naming in the relay** — `@freehold` is the CPA; peer experts are `@<service>`.
    Naming/mention conventions beyond that land with Chunk 3's fabric work.
