# Buzz Surface — Phase 0 deliverable (Chunk 2)

Facts from the primary source (`github.com/block/buzz`, Apache 2.0, Block Inc.), read
2026-08-17 against `main`. This note is the input D1's event-kind schema and E2's delegation
design must match: **no kind is designed against an assumption.**

## 1. What Buzz is (and what it means for relay-as-scope)

Buzz is a self-hosted Nostr-format workspace where humans and agents share rooms. It is a
Rust monorepo: relay (Axum, WS + REST), Postgres (events + FTS), Redis (pub/sub, presence),
S3/MinIO (Blossom media), plus CLI/ACP/desktop clients.

- A **community** is the workspace selected by the request host. In the default single-relay
  setup, **one relay URL = exactly one community** — which is precisely relay-as-scope: our
  management relay is a single-community Buzz deployment, and the CP's scope IS that community.
- The relay is the **single source of truth**: no P2P/gossip; all reads/writes through it.
- Every action is a signed Nostr event; the `kind` integer is the only dispatch switch and
  adding a new kind is a sanctioned, zero-breaking-change extension ("existing clients see
  nothing and break nothing").

## 2. Install & operations (deploy/compose)

- Production bundle: `deploy/compose` — Postgres, Redis, MinIO, git volume, optional
  Caddy/TLS. `./run.sh start`; TLS via `BUZZ_COMPOSE_TLS=true` (Let's Encrypt); liveness at
  `/_liveness`. `./run.sh backup-hint` ships a backup checklist.
- Secrets that must be stable across restarts: `BUZZ_RELAY_PRIVATE_KEY` (the relay's signing
  key — **required for member administration**), `BUZZ_GIT_HOOK_HMAC_SECRET`, DB/Redis/S3.
- `BUZZ_AUTO_MIGRATE=true` (or `buzz-admin migrate`) to bootstrap a fresh DB; `RELAY_OWNER_PUBKEY`
  enables **closed-relay mode** (auth restricted to the owner's community membership).
- `BUZZ_IMAGE` defaults to `ghcr.io/block/buzz:main`; pin a sha/semver for production.

## 3. Identity & membership

- Identity = Nostr keypair (`nsec`, hex pubkey). Mint per-agent with `buzz-admin generate-key`
  (secret printed once, never stored — matches our "env-injected, never committed" discipline).
- Membership is **relay-administered, protocol-native**: `buzz-admin add-member --pubkey <hex>`
  publishes a **kind 13534 membership list event, signed by `BUZZ_RELAY_PRIVATE_KEY`** (NIP-43
  membership list). The relay enforces membership: EVENT/REQ require NIP-42 auth, and
  channel-scoped events run a `check_channel_membership` step in the ingest pipeline.
- **Consequence for "CP self-adds":** a membership write needs the RELAY signing key, so the CP
  cannot self-add with its own keypair. Bootstrap/onboarding membership writes happen THROUGH
  the relay-admin runner — CPA drives generic exec of `buzz-admin add-member <pubkey>` on the
  relay host (the architecture's "runner that can reach and manage both the relay and the CP").
  **The CP never holds `BUZZ_RELAY_PRIVATE_KEY`** — same no-extra-trust discipline as the
  no-master-key model; the relay's signing key stays on the relay's own host.
- **Being a member is enforced at the protocol layer** (auth + channel ingest), regardless of
  who performs the add.
- **Decided live (Phase C, deploy-relay):** `RELAY_OWNER_PUBKEY` = the CP's console identity
  pubkey (the owner is auto-membered as `owner`); the CPA `@freehold` is added explicitly as a
  `member`. `deploy-relay --owner-pubkey` writes it into the compose `.env`.

## 4. Auth & wire surface

- WS: NIP-42 challenge→auth before EVENT/REQ (unauthenticated → rejected with `auth-required`).
- HTTP: NIP-98 Schnorr-signed auth on the HTTP bridge (`/events`, `/query`, `/count`,
  `/hooks/{id}`, `/media/*`, `/git/*`, `/info`); API tokens with scopes also supported.
- Wire limits: max frame 64 KB, 1024 subs/conn, 500 historical results per filter.
- Our runner/CP clients authenticate as their nsec (NIP-42 for WS, NIP-98 for REST) — same
  keypairs we already ship; no new identity scheme.

## 5. Event kinds — the registry is the contract

Kinds are u32; custom range 40000+ is sanctioned. Used kinds we must NOT collide with
(source of truth: `crates/buzz-core/src/kind.rs`):

| Kind(s) | Meaning |
|---|---|
| 9 / 40002–40008 / 40099 | Stream (channel) messages — NIP-29 group chat + v2/edit/pin/bookmark/schedule |
| 13534 (NIP-43) | **Membership list** (relay-signed) |
| 30174 | **`KIND_AGENT_ENGRAM`** — agent memory unit (native agent-memory kind) |
| 10100 / 30175–30179 | Agent profile / persona / team / managed-agent kinds |
| 41001 / 41010–41012 | DM created / open / add-member / hide |
| 42000 | Product feedback |
| 43001–43006 | **`KIND_JOB_REQUEST / ACCEPTED / PROGRESS / RESULT / CANCEL / ERROR`** — a NATIVE job/delegation protocol |
| 44100–44101 | Member added / removed notifications |
| 45001–45003 | Forum post / vote / comment |
| 46001–46012, 46020, 46030–31 | Workflow execution + approval grant/deny |
| 48001 | **`KIND_AUDIT_ENTRY`** — relay-level audit events |
| 48100–48106, 49001 | Huddles, media upload |
| 20001 | Presence (ephemeral) |

Free for us (D1 picks from, e.g.): 40500–40899, 41100–41999, 42100–42999, 43100–44099,
44300–44999, 45100–45999, 46100–47999, 48200–48999, 49100–49999. **All ≥40000 are
append-only (NIP-16: replaceable = 10000–19999, addressable = 30000–39999).** Anything that
must be a CURRENT state (grants; later, scope bookkeeping) uses the addressable range with a
`d`-tag — NOT a 40000+ kind, or revocations would append history instead of replace.

## 6. Agent surface (buzz-acp / buzz-cli)

- **buzz-acp** is an ACP↔MCP harness that connects an **LLM agent** (goose, codex via
  codex-acp, claude code via claude-agent-acp) to Buzz: listens for @mentions (kind 9 with the
  agent pubkey in a `#p` tag), prompts the agent, agent replies via buzz-cli. Env: `BUZZ_PRIVATE_KEY`,
  `BUZZ_RELAY_URL`, agent command, optional MCP server binary, idle/turn timeouts, N parallel
  agents (1–32, same identity), heartbeat prompts, and an **Inbound Author Gate**
  (`--respond-to owner-only|allowlist|anyone|nobody` + hex allowlist) — a per-agent coarse
  inbound grant.
- **buzz-cli**: JSON-in/JSON-out CLI (send_message, get_messages, create_channel, …) for
  scripted/agent use.
- **buzz-acp runs every real (LLM) agent in the fabric, including the CPA.** see ARCHITECTURE.md's 
  "Agent fabric" section for its current design (real reasoning agent, own system prompt, 
  create/grant/manage-agent toolset).

## 7. Memory

- Buzz has a **native agent-memory kind** (`KIND_AGENT_ENGRAM` 30174) plus the relay event log
  with Postgres FTS as the searchable history ("agents search six months of history").
- Plan decision stands: memory event payloads are **encrypted** (a relay operator is not a
  reader of agent memory). D3: adopt/native-knowledge engram semantics where they fit, or our
  own encrypted memory kind — decided in D1 against the engram schema.

## 8. Audit

- The relay runs its own **hash-chain tamper-evident audit log** (`buzz-audit`) for every
  stored event — so anything we publish is relay-audited by construction.
- Runner exec-audit publishes as **kind 48001 (AUDIT_ENTRY)** — verify schema in D1; keep the
  Chunk-1 local spool as the pre-relay/offline log (D5 stands).

## 9. Per-capability port decision (the Phase 0 deliverable)

| Capability | Decision | Native surface | Custom needed? |
|---|---|---|---|
| **Membership** | NATIVE | kind 13534 (relay-signed); `buzz-admin add-member`; relay enforces at auth + channel ingest | No |
| **Grants** | CUSTOM | none for agent↔runner; (buzz-acp author gate is per-AGENT inbound, not runner grants) | **Yes — a freehold grant-list kind in the ADDRESSABLE range (30000–39999, e.g. 30180), `d`-tag = runner pubkey: one current list per runner; a new event with the same d-tag REPLACES it, so revocation never appends (fail-stale is the wrong direction). Runner re-reads live per call (REQ with the d-tag filter) → closes the running-runner gap** |
| **Memory** | NATIVE + encrypted payloads | kind 30174 engram + event-log FTS | Payload encryption is ours (D3) |
| **Audit** | NATIVE | kind 48001 audit entry + relay hash-chain; local spool stays (D5) | No |
| **Delegation** | NATIVE | kinds 43001–43006 job request/result/error (or @mention+reply as simplest path) | No |
| **Surface (F)** | NATIVE | stream channels (kind 9) + DMs (41001); scripted @freehold CPA via its own NIP-42 client | No |

## 9.5 Ingest surface correction (found live, Phase D)

The §9 "custom kinds are sanctioned" claim is about the KIND REGISTRY (adding a kind breaks
nothing) — NOT the ingest gate: `crates/buzz-relay/src/handlers/ingest.rs` `scopes()` has a
HARDCODED match of accepted kinds, and any kind outside it is refused with `restricted:
unknown event kind` (verified live: publishing our 30180 got exactly that). There is no
config allowlist. DECISION (post-Phase-D review): freehold does NOT patch buzz. The
custom grant kind (30180) stays a DORMANT, hermetic-tested capability, usable only if a
relay implementation ever accepts it; the OPERATIONAL grant flow is the shipped-package
one (web console + `control-plane grant`/`revoke-grant`, re-read by the runner per call).

**SUPERSEDED (Chunk 2.6.1, roadmap §Chunk 2.6.1):** the custom grant/profile kinds are
WITHDRAWN in favor of NATIVE NIP-29 channels — runner = private channel (9007 create),
grants = membership (9000 put-user / 9001 remove-user, owner-gated), whitelist = the
runner's own RELAY-SIGNED 39002 roster, profile/status = 39000 group metadata. No custom
kind, no ingest-patch gate; the remaining live question is exactly which of these NIP-29
kinds the stock `scopes()` accepts (a config-agnostic allowlist read — G-A/G-C in the
roadmap), not whether any custom kind can pass at all.

## 9.6 Engram (30174) ingest rules (found live, Phase D3)

The stock ingest ACCEPTS kind-30174 (no patch gate — D3 was live-verifiable), but with
hard validation (all observed live):
- content MUST be a valid **NIP-44 v2 payload** (base64; other base64 or JSON wrappers
  get \`agent-engram content is not valid base64 (length)\` / \`too short for NIP-44 v2\`);
- exactly one \`p\` tag (the owner counterparty, 64-hex); a composite-d-tag (\`pk#key\`)
  is refused (\`agent-engram d tag must be 64 lowercase hex chars\`);
- reads require \`authors=[self]\` or \`#p=[self]\` (\`restricted: agent-engram reads...\`).

Freehold outcome: memory = NIP-44 v2 SELF-encryption (conversation key from the agent's
own nostr keypair — sender == receiver == agent; the relay only ever stores ciphertext),
d-tag = sha256(\`<agent-pk>#<key>\`) (64-hex, deterministic per agent+key, replaceable),
content = the NIP-44 payload, reads filter \`authors=[self]+#d\` with local sig verify.

## 9.7 Delegation wire (found live, Phase E)

The NATIVE job kinds (43001-43006) are NOT in the ingest scope match (like our 30180 — an
ingest-patch gate). Delegation therefore rides the accepted paths: NIP-29 channel messages
(kind 9) in a CPA-created OPEN channel (kind 9007 create, `h`+`name`+`visibility=open`
— "public" is REJECTED, verified live). Request/result correlate by an id echoed in
content envelopes. Route findings: a **#p-FILTERED kind-9 query hung** on the live relay for
the requester identity (the p-filtered path worked for the executor); the requester polls
unfiltered and filters by author/id client-side. Result: CPA -> relay -> peer -> runner-direct
-> relay -> CPA — the phase's mode transition — proven live.

## 10. Known gaps / verify-before-design

- **Private-channel member management has no REST/event API yet** (Buzz's own listed gap) —
  channel membership currently via `create_channel` (creator auto-member). Concrete F risk: a
  runner or agent onboarded AFTER a private channel exists cannot be added to it — F must
  create its channels deliberately at bootstrap/onboarding time, or accept public channels for
  the POC. The §9 "Surface: No custom needed" row is scoped to this constraint.
- Engram (30174) schema and job-kind (43001/43004) payload contract: read `buzz-core` in D1
  before adopting — adoption assumes the schema fits; a custom kind stays the fallback.
- Closed-relay mode (`RELAY_OWNER_PUBKEY`) may already give us "non-member cannot even
  authenticate" — verify against G6(a) rather than assuming the runner-side check alone.

## Sources

- `github.com/block/buzz` — README, `ARCHITECTURE.md`, `crates/buzz-core/src/kind.rs`,
  `crates/buzz-acp/README.md`, `deploy/compose/README.md` (all `main`, 2026-08-17).


## 9.8 Domain is identity — tenant host binding (found in source, Phase B)

The community is resolved from the REQUEST HOST, not from a config knob alone:

- `buzz-relay/src/tenant.rs` ("Row-zero host binding"): `req.community = resolve_host(connection.host)`
  through a DB mapping (`Db::resolve_host`); `normalize_host` canonicalizes; an UNMAPPED host
  is a non-success (restricted/refused), never a silent accept.
- `CommunityLabel` is a UUID; the tenant context carries `.host()`; community-provisioning and
  host binding land in `handlers/community_provisioning.rs`.
- Media URLs are built from `config.relay_url + tenant.host()` — the canonical URL config
  anchors the domain; the request host must match.

Consequence (POC): an IP-hosted community IS IP-identity. Bootstrap must force a domain from
event zero (A4 gate) and clients must connect by the domain — the strict host map then
REFUSES IP connects, which is the desired enforcement. The first live deploy was IP-anchored
(ws://<lan-test-ip>:3000) and is treated as disposable: killed + re-provisioned under a
domain at the fresh-run re-test.
