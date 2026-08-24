# Chunk 3 Surface Note — Capability & Risk (Phase 0 deliverable)

Facts from primary sources, read 2026-08-23: `github.com/block/buzz` (`crates/buzz-acp`,
`main`), `BerriAI/litellm` (`main`, f005afa), Block Goose, OpenAI Codex CLI, Anthropic
Claude Code docs, and the freehold workspace. This note is the input Phase A–F of
`roadmap/POC_CHUNK3.md` must match. Everything here is resolved; where a doc line
contradicts it, THIS note wins.

## 1. Capability runners (outbound default-deny, policy-level)

The enforcement is POLICY-level (locked in POC_CHUNK3): harness native shell/web tools are
disabled (see §4 — codex CANNOT comply, therefore excluded); MCP runner tools are the only
sanctioned outbound. The runner list for Chunk 3:

| Capability | Runner | Class | Credential → env | Reach |
|---|---|---|---|---|
| GitHub (read/repo state, releases, actions) | NEW api flavor `github` | safe | PAT/app token `github-token` → `GITHUB_TOKEN` | `https://api.github.com` |
| Web search (research during installs) | NEW api flavor `websearch` | safe | `websearch-key` → `WEBSEARCH_KEY` | a self-hosted **SearXNG** LXC (default: `http://127.0.0.1:8888` on the harness LXC, JSON API) — no external key; the `verify` backend is a freehold-managed service like any other |
| Service installs | `ssh` per service LXC (`<service>-install`, A6) | risky-install | ssh key → injected | the disposable service LXC |
| PVE host provisioning | existing `ssh` runner (proxmox-box) | **risky-host** | ssh key → injected | the PVE host — CPA-only grants, no disposability backstop |
| LiteLLM admin (mint per-agent keys) | NEW api flavor `litellm` (Phase F) | safe | master key `litellm-admin-key` → `LITELLM_ADMIN_KEY` | `http://<litellm-lxc>:4000` |

**Safe/risky metadata placement (Phase 0.02 resolved):** `RunnerRecord.risk_level:
Option<String>` in `control-plane/src/state.rs` — values `"safe" | "risky-install" |
"risky-host"` (None = unknown), set at provision time from a kind → class map in
`control-plane/src/provisioner.rs`, rendered in the console overview. Risk attaches to the
TARGET the runner reaches, not to the secret, so the runner record is the right home (not
SecretRecord). `runner/src/registry.rs` already enumerates new kinds automatically from the
provisioned package — no dispatch change there.

**Also verified (worker report, ApiFlavorMap):** adding an api flavor = three dispatch
points (`runner/src/mcp.rs` `api_status()` :481 and `handle_status()` :556 match, plus the
new kind string at provision), a mock router in `testkit/src/mock.rs`, an acceptance leg in
`acceptance/src/lib.rs`. `env_name()` + the `{CRED_NAME}_URL` injection
(`meta.address` → env) are already generic — new flavors copy the vultr/b2 pattern exactly.

## 2. Inbound gate (private channels + buzz-acp `--respond-to`) — RESOLVED

Source: `crates/buzz-acp` (block/buzz `main`), read this session — **STARTUP-ONLY**.

- `--respond-to` / env `BUZZ_ACP_RESPOND_TO`: `owner-only` (default) | `allowlist` | `anyone`
  | `nobody`. `--respond-to-allowlist` / `BUZZ_ACP_RESPOND_TO_ALLOWLIST`: comma-separated
  64-hex pubkeys, OWNER ALWAYS IMPLICITLY INCLUDED; required for `allowlist` mode
  (config.rs:460-481, 1034-1070). `--allowed-respond-to` restricts which modes may run.
- **NO hot-reload**: config is read once at startup (`Config::from_cli`, lib.rs:1945). No
  signal handler, no file watch, no API. **Changing the allowlist requires a buzz-acp
  process restart.**
- Mention filter: kind 9 with a `#p` tag whose value == the agent pubkey (filter.rs:390-397).
- Channel-scoped reads: buzz-acp queries with `#h` = channel id (relay.rs:3209); non-member
  reads are refused by the relay (403) transparently — the harness just never sees them.
- **DM hardening (bonus finding):** in DMs, ONLY the owner (or a cryptographically verified
  same-owner sibling) may fire a turn, regardless of `allowlist`/`anyone` (lib.rs:251-273).
  So the allowlist gate governs CHANNEL mentions; DMs are owner-only by buzz-acp's own
  hardening — consistent with "operator is the only human who talks to experts".

**Consequence for POC_CHUNK3 Phase B (branch resolved):** the `--respond-to allowlist` is
spawn-time state → the CPA "sync" is (1) re-derive the allowlist from the channel roster at
every (re)spawn, and (2) restart the agent's buzz-acp unit on membership change (bounded
cost; relay-side 9000/9001 membership lands live instantly — the reply-gate lags one process
lifetime, documented). CPA start reconciles unit params ← current roster idempotently (the
`control-plane rebuild` shape) — no drift window survives a crash. B4's "restart-free"
assertion applies to the RELAY side only.

## 3. LiteLLM key path (Phase 0.04) — RESOLVED

Source: `BerriAI/litellm` (key_management_endpoints.py, _types.py, schema.prisma,
docker-compose.yml), read this session.

- **Mint per-agent keys:** `POST /key/generate` with `Authorization: Bearer <master key>`,
  body fields `agent_id`, `key_alias`, `max_budget`, `budget_duration`, `models` (optional —
  EMPTY = all models; keys mintable BEFORE provider models are configured). Response:
  `{ "key": "sk-...", "token_id", "expires", ... }` (GenerateKeyResponse).
- **Auth:** the master key is the SOLE credential for key management (no scoped admin keys;
  master key never echoed/logged — stable alias substituted).
- **Lifecycle:** `PATCH /key/update` (by key or key_alias), `POST /key/delete`
  ({keys:[...]} or {key_aliases:[...]} → deleted_keys). Spend tracked automatically on the
  token row.
- **Deploy:** docker-compose (litellm :4000 + postgres:16); **`DATABASE_URL` REQUIRED** at
  startup (virtual-key persistence + migrations auto-run); `STORE_MODEL_IN_DB=True`;
  healthcheck `/health/liveliness`.
- **Freehold wiring (Phase F3 concrete):** a `litellm` api-runner (kind `litellm`, safe
  class) holds the master key as ciphertext (`litellm-admin-key` → `LITELLM_ADMIN_KEY`,
  address = `http://<litellm-lxc>:4000`); the CPA spawn flow calls it
  (`litellm /key/generate agent_id=<agent> key_alias=<agent> max_budget=<b>`) instead of
  resolving the key into any agent's context. F2 stays operator-managed: the operator
  supplies the master key + provider keys once at LiteLLM bring-up (the provider keys live in
  LiteLLM's config/db, never in the CP store).

## 4. Harness tool-disable (Phase A5/A5b) — RESOLVED, with a constraint

Sources: Block Goose config-files + tool-permissions docs; OpenAI Codex CLI config docs;
Anthropic Claude Code permissions docs (2026-08).

| Harness | Disable shell/exec/file-write | Disable web/search | MCP registration | Verdict for Chunk 3 |
|---|---|---|---|---|
| **goose** | `~/.config/goose/config.yaml`: `extensions[<name>].enabled: false` (the built-in `developer` extension carries bash/file tools; disabling it removes them wholesale — no per-tool granularity for built-ins) + `available_tools: [...]` filter | same mechanism | `extensions[<name>]` with `type: stdio` (command/args) or `streamable_http` (uri/headers) | ✅ compliant |
| **claude-code** | `~/.claude.json` `permissions.deny: ["Bash", "Write", ...]` — bare NAMES REMOVE the tools from context entirely (plus `allow`/`ask` tiers) | `deny: ["WebSearch", ...]` | `mcpServers: {<name>: {command/args}}` — CAUTION: bare glob `deny: ["mcp__*"]` kills ALL MCP tools too; deny by server, not glob | ✅ compliant |
| **codex** | **NO tool-deny mechanism.** Only `approval_policy` (untrusted/on-request/never) + `sandbox_mode` (:read-only/:workspace/:danger-full-access) gate the built-in shell; it can NEVER be fully disabled | no deny mechanism | `~/.codex/config.toml [mcp_servers.<name>]` command/args or url | ❌ **EXCLUDED** from Chunk 3 unless run in an OS-level container sandbox (out of POC) |

**Decision (A5b gate):** Chunk-3 experts run **goose or claude-code** — both fully support
the "MCP runner tools are the only sanctioned surface" posture. Codex is excluded; if a
future need demands codex, it runs inside a nested container that denies access to the
harness LXC's runner/credential surface (documented future work, not POC). A5b's verification
(enumeration asserts no shell/exec/file-write tool present) is run against whichever harness
a spawn config uses, with the deny configs above as the documented baseline.

## 5. Audit surface (Phase 0.05) — RESOLVED

**Operational audit stays kind-48001 + the local spool** (native kind, relay hash-chain per
`BUZZ_SURFACE.md` §8, already live — POC_CHUNK2's D5). Kind-9 redacted channel receipts
(G-B) stay deferred: a second audit path duplicates maintenance for no Chunk-3 consumer.
Phase A4 therefore implements capability-runner audit via the EXISTING 48001 + spool path
(no new kind, no channel posting).

## 6. Prompt injection standing answer — RECORDED

Locked in POC_CHUNK3: private channels default to operator + CPA membership, so the
injection surface IS operator prompts; buzz-acp's DM hardening (§2) extends it to DMs.
Multi-member channels later re-open content-level defense as future work. Nothing to build.

## Sources

- `github.com/block/buzz` `crates/buzz-acp` (`config.rs`, `lib.rs`, `filter.rs`, `relay.rs`)
- `github.com/BerriAI/litellm` (`key_management_endpoints.py`, `_types.py`,
  `schema.prisma`, `docker-compose.yml`)
- Block Goose docs (config-files.md, tool-permissions.md); OpenAI Codex CLI docs (config.md);
  Anthropic Claude Code docs (permissions.md)
- freehold workspace read-only map: `runner/src/mcp.rs`, `control-plane/src/state.rs`,
  `testkit/src/mock.rs`, `acceptance/src/lib.rs` (deltas via agent report ApiFlavorMap)
