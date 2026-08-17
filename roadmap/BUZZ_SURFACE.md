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
- So "CP self-adds as a member" = the CP becomes a community member via the same membership
  mechanism (13534), and **being a member is already enforced at the protocol layer**.

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
44300–44999, 45100–45999, 46100–47999, 48200–48999, 49100–49999.

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
- **Corrects one plan assumption:** buzz-acp exists to run REAL (LLM) agents. The POC's
  scripted `@freehold` CPA should join the relay with ITS OWN NIP-42 client (it IS our code)
  rather than running under buzz-acp — preserving "no real reasoning agent in POC". buzz-acp +
  the harness is the Chunk-3+ path for real expert agents. (E1 wording in POC_CHUNK2.md
  amended accordingly.)

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
| **Grants** | CUSTOM | none for agent↔runner; (buzz-acp author gate is per-AGENT inbound, not runner grants) | **Yes — freehold grant-list kind (e.g. 47001, replaceable), per runner; runner re-reads live per call (closes the running-runner gap)** |
| **Memory** | NATIVE + encrypted payloads | kind 30174 engram + event-log FTS | Payload encryption is ours (D3) |
| **Audit** | NATIVE | kind 48001 audit entry + relay hash-chain; local spool stays (D5) | No |
| **Delegation** | NATIVE | kinds 43001–43006 job request/result/error (or @mention+reply as simplest path) | No |
| **Surface (F)** | NATIVE | stream channels (kind 9) + DMs (41001); scripted @freehold CPA via its own NIP-42 client | No |

## 10. Known gaps / verify-before-design

- **Private-channel member management has no REST/event API yet** (Buzz's own listed gap) —
  channel membership currently via `create_channel` (creator auto-member). F's room/DM setup
  must work within this.
- Engram (30174) schema and job-kind (43001/43004) payload contract: read `buzz-core` in D1
  before adopting — adoption assumes the schema fits; a custom kind stays the fallback.
- Closed-relay mode (`RELAY_OWNER_PUBKEY`) may already give us "non-member cannot even
  authenticate" — verify against G6(a) rather than assuming the runner-side check alone.

## Sources

- `github.com/block/buzz` — README, `ARCHITECTURE.md`, `crates/buzz-core/src/kind.rs`,
  `crates/buzz-acp/README.md`, `deploy/compose/README.md` (all `main`, 2026-08-17).
