# AGENTS.md — freehold

Open-source appliance: one-command install, AI-agent-operated. Lands a Proxmox VE / VPS +
Kubernetes stack with Buzz Relay as the control plane and a skill framework that installs and
configures self-hosted OSS. Narrative: "reclaim the future we were promised."

**Status:** Chunk 2 complete + Chunk 2.6 runner-lifecycle slice IMPLEMENTED
and SUPERSEDED by Chunk 2.6.1 — runners-as-NIP-29-channels, IMPLEMENTED hermetic
(no custom kinds: 9007 create / 9000-9001 membership / 39000 meta / 39002 relay-signed
roster = the whitelist; G-1 resolved = native-kinds-only; live relay gates G-A/G-C open).
Legacy: (RUNNER_PROFILE kind 30181 published at every lifecycle mutation;
`control-plane rebuild` = the disposable-CP fold, deterministic+idempotent;
G-2 resolved query-per-call fail-closed; G-1 fork-vs-contribution deferred).
Docs + locked Chunk 1 plan; Phases A (identity, MCP skeleton, exec/readiness/
audit), B (provisioner), C (SSH + vultr + b2 connectors), D (coarse grants), E
(orchestrator: the scripted CPA stand-in — onboard/readiness/exec/demo), F (local
admin/ops web UI: services-at-a-glance with LIVE readiness via a console agent, and
runner/secret/grant management), and G (the acceptance script: `cargo run -p
freehold-acceptance` reproduces every Chunk-1 acceptance criterion hermetic on
H1 (the PVE host as a real SSH target)
is DONE — the PVE host is onboarded as a runner and execs green.
Phase 0 (Buzz surface research — `roadmap/BUZZ_SURFACE.md`) and Phase A (bootstrap
provisioning: `freehold bootstrap` with proxmox-lxc + vultr-vps + hetzner-vps drivers,
hermetic-tested, dry-run verified against the PVE host) are DONE;
the domain gate (A4) requires `--domain` and blocks until it RESOLVES
(directly, or at an operator-managed proxy that forwards to the target)
is in `roadmap/POC_CHUNK2.md`. Phase B (deploy-relay driver: docker gate,
curl+tar bundle fetch, compose start, /_liveness verify, scope claim) is
implemented and hermetic-tested. The proxmox-lxc bootstrap is idempotent
(template ensure by host arch + docker+compose in the guest); the CLI is
`freehold`. Phase B and the LIVE relay deploy are DONE: LXC 100 `relay-box`
(deb-13 amd64, unprivileged, 16G) runs the Buzz compose stack; Phase C is
DONE: the control
is deployed in OPERATE mode in ITS OWN LXC (`cp-box` 102, `/srv/freehold`,
binary shipped as base64 through the runner's exec-only primitive — every
remote command routes through `pct exec`, per the CP-off-the-host decision);
the console has NIP-98 operator auth (admin whitelist seeded by
`--operator-pubkey`, bind guard authn-conditional, UI login panel,
`--public-origin cp-<relay-domain>` behind the operator's proxy), and the
operator logs in with their OWN nsec (`console-login --nsec nsec1...`) —
the key never leaves their machine. Phase D (identity
port) slice 1 is DONE: the grant-list kind (30180, addressable, d-tag =
runner pubkey, replaceable) is defined + implemented end-to-end — CP
publishes via NIP-98 POST /events, the runner reads the CURRENT list live
per call via NIP-98 GET /query (fail-closed on relay outage; revoke lands
WITHOUT a runner restart — hermetic proof included; NIP-98 signatures
cross-verified byte-for-byte with rust-nostr, the crate the relay uses).
The relay's ingest restrict-list refuses custom kinds (BUZZ_SURFACE §9.5).
DECISION (post-Phase-D review): freehold does NOT patch buzz — relay-grants
(kind-30180) stays a DORMANT, hermetic-tested optional mode; the OPERATIONAL
grant flow remains the shipped-package one (web console UI +
`control-plane grant` / `revoke-grant` / `revoke`, re-shipped and re-read by
the runner per call — revoke lands without a restart). D2 (membership records on the relay) is partially
live under the DOMAIN community (`freehold-test.darcydev.net`, operator
nginx on the tailnet forwards the hostname to the relay LXC — the relay's
strict host map makes the domain the only door; deploy-relay writes
BUZZ_DOMAIN/RELAY_URL from --relay-url / --domain; TLS rides the proxy, or
the local-CA posture when --domain is used without it). The IP-anchored
first live relay was KILLED and the full stack re-provisioned fresh under
the domain (A4 gate verified both the direct-DNS and proxy cases). D5 (audit publishing) is DONE. D3 (encrypted memory) is DONE:
D3 (encrypted memory) is DONE: the CPA's memory lives on the relay as
kind-30174 engrams, NIP-44 v2 SELF-encrypted (only the agent's own key can
decrypt; the relay stores ciphertext only), d-tag = sha256(agentpk#key),
live-verified set/get through the real relay (the ONE D item with no ingest
patch gate — the engram wire rules are recorded in BUZZ_SURFACE §9.6).
`freehold memory set|get`. E (delegation) is DONE: the CPA delegates a
task over the relay to a peer agent that execs it runner-direct —
CPA -> relay (kind-9 channel in a created OPEN channel) -> peer -> runner ->
relay -> CPA — proven LIVE; requests/results correlate by id (BUZZ_SURFACE
§9.7; the peer's exec uses the target's own credential, the CPA polls
unfiltered because a #p-filtered kind-9 query hung for its identity). D5 (audit publishing) is DONE: the runner's audit row is now a kind-48001
NIP-01 event (id + BIP-340 over the id) — the SAME bytes spooled locally
(0600, fail-closed read, never silent) AND published to the relay when
--relay-url is set (detached so a wedged relay never delays the agent's
exec; publish failure degrades to spool-only and is surfaced). D3 (encrypted memory) and E (delegation) landed after this — both live.
NO buzz ingest patch is planned: the relay-grants mode stays dormant/
hermetic, and grants operate via the shipped-package flow (web UI + CP
CLI). A real Vultr token is still needed for the VPS leg.
Work ships via branches, pending a GitHub outage before the PR/review cycle.
Post-review corrections (locked): the CP deploys to its OWN LXC and ATTACHES to the
relay (Buzz = substrate, co-location never assumed; naming: the PVE host vs the local
workstation). Bootstrap REQUIRES a domain (identity) with a blocking resolution gate
(A4); TLS = local CA on the domain by default, LE DNS-01 with a provider key; console
auth = NIP-98 operator login, admin seeded by --operator-pubkey; the bind guard becomes
authn-conditional. The live IP-anchored relay community WAS killed and the full stack
RE-PROVISIONED fresh under `freehold-test.darcydev.net` (relay LXC 100 @
192.168.30.238:3000 behind the operator's tailnet nginx; CP in its own LXC 102 @
192.168.30.254:8080; console admin = the operator's new key b2ab89...; memory +
delegation re-proven live under the domain; the old PVE-host CP stopped).


## Next (implemented)
- **Chunk 2.6.1 — Runners-as-Channels, Grants-as-Membership** (roadmap
  §Chunk 2.6.1): IMPLEMENTED hermetic — supersedes 2.6's custom-kind wire
  format (30181 + 30180 surfaces DELETED; the relay's hardcoded INGEST
  allowlist in `ingest.rs::scopes()` refused them — NOT the ALL_KINDS
  registry). G-1 resolved = **native-kinds-only** (fork + upstream
  rejected; zero Buzz changes). Mapping: runner = private NIP-29 channel
  (9007), grant/revoke = 9000 put-user / 9001 remove-user (owner-gated),
  the runner's whitelist = its own relay-SIGNED 39002 roster read per call
  (fail-closed; trust anchor = `--relay-pubkey`, the relay's key — not the
  console's), profile/status/rotation = 39000 group metadata, `rebuild`
  folds 39000 (same guarantees). CP cannot self-author membership writes
  (§3.2 — it COMMANDS, the relay mints). Web UI: console relay scope
  persisted (`serve --relay-url/--relay-pubkey`), lifecycle actions sync
  channels, `GET /api/runner/<name>/channel` shows profile + verified
  roster + messages without operator membership. OPEN live gates (need
  relay access, not code): G-A headless drive path, G-C 39000/39002 ingest
  allowlist read, audit-receipts-as-channel-messages (D5 48001 stays
  operational; G-B redaction is its release gate). Testkit fake relay now
  models the NIP-29 contract incl. 403 membership gating.

## Navigation

- `VISION.md` — narrative, single source of truth for the "why".
- `ARCHITECTURE.md` — system design, locked decisions, build plan (including host feasibility and
  the two axes: skills × host).
- `roadmap/ROADMAP.md` — chunked roadmap: POC chunks 1–3, MVP chunks 4–6.
- `roadmap/POC.md` — POC scope, goal, acceptance, test/promote flow.
- `roadmap/POC_CHUNK1.md` — detailed Chunk 1 build plan. **Locked, ready to execute.**
- `roadmap/POC_CHUNK2.md` — detailed Chunk 2 build plan (relay/CP deployment + identity port).
- `roadmap/BUZZ_SURFACE.md` — Phase 0 deliverable: the Buzz relay's actual surfaces and the
  per-capability port decision (native kinds vs our own custom kinds).

## Locked model — do not change without an explicit user decision

- **One control plane = exactly ONE relay scope** (relay-as-scope). The CP lives on its OWN target
  (a dedicated LXC/box) and ATTACHES to the relay — co-location with the relay is convenience,
  never assumed; the CP does not depend on managing the relay. A user's existing relay is
  onboarded as a service, not a nested scope. No "control plane of control planes".
- **Agent = brain; runner = dumb privileged hands.** ONE generic primitive: `exec(cmd, target,
  stream?)`. NO semantic tools (`tail_log`, `create_server`, … don't exist). Streaming is a
  property of exec. Runner executes the agent's command verbatim on the connection it owns.
- **Runner identity** = Nostr keypair (membership/signing) + a separate encryption keypair
  (env-injected / mounted secret, never committed).
- **CP = secret PROVISIONER, not a vault.** Encrypt-to-runner-key → ship ciphertext → inject
  runner private key → rotate. **NO master key.** Runner holds only ciphertext + its own key;
  decrypts locally, uses in memory, forgets. Plaintext never on disk, never in agent context;
  agents reference secrets BY NAME only.
- **Grants are coarse**: agent ↔ runner (whitelist of Nostr pubkeys). Dedicated runner per
  service = default; sharing via grants allowed. Readiness = the runner's OWN self-check:
  🟢 green / 🟡 yellow / 🔴 red.
- **POC is pre-MVP**: NO Kubernetes, NO real reasoning agent (Chunk 1 = scripted orchestrator,
  CPA stand-in), Buzz required only from Chunk 2. Deterministic k8s pods + LiteLLM = public
  release.
- **Host-FLEXIBLE — not locked to Proxmox.** Proxmox is the lead/default, but VPS/cloud are
  first-class supported options (the business path). K8s layer and everything above the host
  driver run identically regardless of substrate. Installer/runner must target a VPS as easily
  as Proxmox from day one — no Proxmox-only shortcuts.
- **K8s fixed; hosting substrate pluggable.** Two orthogonal axes: SKILLS (what to install) ×
  HOST (where the appliance lives). Anything × anything composes.

## Chunk 1 (current work)

Prove the engine room standalone: local control plane (web UI) + runners as MCP tool servers +
secret provisioner + coarse grants + readiness. Connectors: SSH, Vultr, Backblaze B2. No Buzz,
no k8s.

- Rust workspace: `core` (identity, auth — the shared signed-call protocol, sealed-box
  crypto, secret packaging, atomic-0600 fs), `runner` (+ ssh connector via russh,
  in-memory keys), `control-plane` (provisioner + local admin/ops web console), and
  `orchestrator` (scripted CPA stand-in: signed MCP client + onboarding/demo flows),
  `testkit` (hermetic fixtures: mock Vultr/B2 APIs + in-process sshd), and `acceptance`
  (the Chunk 1 acceptance script) crates.
- MCP over HTTP for agent↔runner even though co-located — proves the real shape.
VPS (dev/smoke) → PVE host (test/staging, SSH target only) → home
  dogfood. Chunk 1 touches Proxmox only as an SSH target; a VPS or any SSH-able box stands in.

## Chunk 1 known gaps (honest scope)

- **No remote revocation of a capability already in a runner's hands.** The CP can stop
  issuing (revoke blocks provision/rotate), erase its own copies (rotate re-seals, revoke
  deletes the shipped `secrets.json`), and blobs are pinned to recipient + secret name — but
  a blob someone else kept still opens, and re-keying (a leaked runner private key) is out
  of scope. Epoch/staleness rejection is a named follow-up (tracked post-A4; wire-format
  addition, nothing deployed yet). "Rotation = erase" refers to YOUR copies, not copies
  others held.
- **Loopback exposure narrowed, not gone (D done):** every tools/call now requires a
  signature from a GRANTED agent pubkey (fail closed) — a random local process can no
  longer exec. Remaining: session state isn't tied to the grant (a signed request is
  verified fresh each call), and relay membership (Chunk 2) is the real cut.
- **Backups outlive "rotation = erase your copies"** (review-surfaced): `/srv/data` is part
  of the PBS + TrueNAS + Backblaze set and can hold runner secrets ciphertext, so a revoke
  that deletes the shipped `secrets.json` still leaves the old blob in every off-site
  snapshot — same class as "a blob someone else kept still opens"; backup retention is a
  named copy-holder follow-up.
- **Rotate/re-grant do not reach a RUNNING runner** (G-surfaced): the runner holds its
  package in memory from boot (only GRANTS are re-read from disk per call). A rotate
  re-ships new ciphertext that a RESTARTED runner decrypts, but the live runner keeps
  serving the old in-memory credential until restart — same class as the revocation
  gap; the acceptance script proves rotation by restarting the runner.
- **Abandoned streaming sessions are never reaped** — decrypted values stay in the session
  map for the process lifetime. A TTL reaper is Phase C-sized.
- **`timeout_s` kills the shell, not its descendants** (no setsid/killpg yet) — a timed-out
  command can leave orphans running.
- **Grants (D) accepted gap**: a signed call can be replayed against the SAME
  runner within the 60s window (audience + runner binding closes cross-runner
  replay; a per-runner replay cache is a Chunk-2/security item).
- **API connectors (C2/C3) accepted gaps**: streamed exec on an api target
  redacts the injected `<SECRET>_URL` env (the base URL) from output too —
  non-secret, cosmetic; the fix is a redaction list separate from the child
  env list.
- **SSH connector (C1) accepted gaps**: one command at a time per pooled connection (a
  concurrent exec waits unboundedly — per-command channels are the real fix); a wedged
  connection stays pooled after a timeout; ssh timeouts return empty output where local
  returns partial; no IPv6 in `SshTarget::parse`; pooled connections aren't
  re-authenticated after a rotate; half-open connections surface as an error rather than a
  transparent reconnect; ssh injects NO secret env over the channel (extra requested
  secrets are rejected explicitly — the honest contract while that's unimplemented).
- **State store is single-process** (`StateStore` open→mutate→save is not cross-process
  atomic; TODO for the Postgres swap at MVP).
- **Orchestrator (E) accepted gaps**: `onboard` has no rollback — a hard-fail at the
  readiness gate leaves CP state + the shipped package on disk and a re-run hits
  `RunnerExists`/`PackageDirInUse` (operator cleans up by hand); the in-process runner
  task is only aborted on the success path (harmless in the CLI, matters if `onboard`
  is ever called twice in one process).
- **Web console (F) accepted gaps**: a secret posted to `/api/provision` or
  `/api/rotate` exists unzeroized as axum body bytes + a serde `String` before
  `Zeroizing` takes ownership (loopback, TLS-free — same exposure as the CLI's stdin
  path); the console's signing key is re-derived (hex-decode) on every readiness probe
  rather than held once (bounded, but a zeroize-fast path would re-derive it once).

## Build / test

- Workspace: `cargo build`, `cargo test` (once scaffolded in Phase A).
- No formatter/linter config yet — rustfmt + clippy defaults.
- Each phase in `roadmap/POC_CHUNK1.md` has acceptance checkboxes; tick them as work lands.

## Code style

- Rust: follow rustfmt; small crates; keep the runner↔CP contract at the crate boundary and
  language-agnostic (MCP over HTTP).
- **Never** put secrets in code, config, tests, logs, or committed files. Private keys arrive via
  env var / mounted secret. No secret dumps in output or agent context.
- Keep `exec` generic — do not add semantic tools to work around a connector's API.
- No UI/chat surface rebuilds: Buzz provides the chat; the CP console is admin/ops only.
