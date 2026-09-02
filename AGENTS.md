# AGENTS.md — freehold

Open-source appliance: one-command install, AI-agent-operated. Lands a Proxmox VE / VPS +
Kubernetes stack with Buzz Relay as the control plane and a skill framework that installs and
configures self-hosted OSS. Narrative: "reclaim the future we were promised."

**Current version: 0.3.0.** Chunk 1 (engine room) and Chunk 2 (relay scope), including the
durable volume plane (Phase 0.12), are implemented and live-verified against real
infrastructure (a real PVE host, a real relay/CP pair under a real domain). Chunk 3 (the
Rust→Go refactor) is complete; Chunk 4 (a real, reasoning CPA that lives in Buzz) is the
current focus — see `roadmap/POC.md`. For how we got here, see `CHANGELOG.md`; this file
describes the current state and the rules for working in this repo, not the history.

## Documentation hygiene (locked) — a primary job of this file

**Docs describe current-world state only.** `ROADMAP.md`, `POC.md`, `ARCHITECTURE.md`,
`BUZZ_SURFACE.md`, `README.md`, and this file say what's true *now* — never "formerly X,"
"SUPERSEDED," "as of 2026-08-20," or other change-narration inline. When a decision changes:

1.  Edit the affected doc(s) to state the new reality plainly, as if it had always been true.
2.  Add an entry to `CHANGELOG.md` explaining what changed and why (see "Pull
    requests": each phase bumps `0.x.y` and adds a changelog entry; a phase merged
    to `main` is tagged `v0.x.y`).

## Pull requests (locked) — every unit of work ships through a PR

- Every chunk/phase lands as a branch → PR → `main`. Create the PR as soon as the
  branch has commits; the `claude.yml` workflow posts its review on the PR, so the
  PR body is where review findings get worked.
- **Wait for the review to settle BEFORE working its comments.** After pushing,
  poll until both checks finish — `gh pr checks <n>` (CI `check` and the
  `claude.yml` `review` workflow) — and only then open the findings:
  `gh pr view <n> --json comments` (or `gh api repos/<owner>/<repo>/pulls/<n>/comments`)
  with a timestamp cursor, and fix what's real. Addressing comments mid-review wastes
  a round and risks editing files the review hasn't seen yet; `DEFER` items belong
  in the PR body or the `Known gaps` section below, not in re-review rounds.
- **Commit and push; never merge.** Merging is the operator's call — do it only
  when the operator explicitly says "merge when complete" (or equivalent). Until
  then the PR sits in review, even at `MERGE-READY`.
- **One `0.x.y` per phase.** Each phase that comes online gets its own
  `CHANGELOG.md` entry and version bump (`0.4.0`, `0.4.1`, …): the minor moves
  when a chunk's work lands, the patch when a phase inside it does. When a phase
  merges to `main`, that release tags the tree at `v0.4.1` etc. — the tag and
  the changelog entry are both part of landing the phase.

## Navigation

- `VISION.md` — narrative, single source of truth for the "why".
- `ARCHITECTURE.md` — system design, locked decisions, build plan.
- `CHANGELOG.md` — history of decisions, reversals, and version-by-version progress.
- `roadmap/ROADMAP.md` — chunked roadmap: POC chunks 1–7, MVP definition, North Star.
- `roadmap/POC.md` — POC scope, goal, chunk-by-chunk plan, acceptance, test/promote flow.
- `roadmap/POC_CHUNK<n>.md` — the plan that actually exists: Chunk 3 (Rust→Go refactor,
  done), Chunk 4 (CPA in Buzz, current), and Chunk 5 (agent workspaces + git/GitHub).
- `.agents/skills/release/SKILL.md` — the `release` skill: cut a `v0.x.y` annotated
  tag on `main` and publish a short, high-level GitHub Release (distilled from
  `CHANGELOG.md`, never the full changelog) — one per phase, per the PR rules above.
  This is the canonical, agent-agnostic location (auto-loaded by opencode and any
  other agent that reads `~/.agents/skills/`-style external skills).
- `roadmap/BUZZ_SURFACE.md` — the Buzz relay's actual surfaces and per-capability port
  decisions (native kinds vs. custom kinds).

## Locked model — do not change without an explicit user decision

- **One control plane = exactly ONE relay scope** (relay-as-scope). The CP lives on its own
  target (a dedicated LXC/box) and attaches to the relay — co-location is convenience, never
  assumed. A user's existing relay is onboarded as a service, not a nested scope. No
  "control plane of control planes."
- **Agent = brain; runner = dumb privileged hands.** One generic primitive:
  `exec(cmd, target, stream?)`. No semantic tools. Streaming is a property of `exec`. The
  runner executes the agent's command verbatim on the connection it owns.
- **Runner identity** = Nostr keypair (membership/signing) + a separate encryption keypair
  (env-injected / mounted secret, never committed).
- **CP = secret PROVISIONER, not a vault.** Encrypt-to-runner-key → ship ciphertext → inject
  runner private key → rotate. No master key. Runner holds only ciphertext + its own key;
  decrypts locally, uses in memory, forgets. Plaintext never on disk, never in agent context;
  agents reference secrets by name only.
- **Grants are coarse**: agent ↔ runner (whitelist of Nostr pubkeys). Dedicated runner per
  service by default; sharing via grants allowed. Readiness = the runner's own self-check:
  🟢 green / 🟡 yellow / 🔴 red.
- **The runner lifecycle rides native Nostr kinds, not custom ones.** A runner is a private
  NIP-29 channel; grant/revoke is channel membership (9000/9001); the runner's live
  whitelist is the relay's own signed roster (39002), read fresh per call, fail-closed on
  relay outage. No relay fork or patch.
- **The CPA is a real, LLM-backed reasoning agent — the system's main user touchpoint.**
  It runs on the same buzz-acp/goose-class harness as the expert agents it creates, gets its
  purpose from `CPA_SYSTEM_PROMPT.md`, and delegates to the agents it spawns rather than
  doing expert-level work itself. The deterministic runner/CP layer underneath (grants,
  secrets, teardown/rebuild) is unchanged by this — reasoning decides what to do, that layer
  still does it auditably.
- **Host-flexible — not locked to Proxmox.** Proxmox is the lead/default; VPS/cloud are
  first-class (the business path). The k8s layer and everything above the host driver run
  identically regardless of substrate. Installer/runner must target a VPS as easily as
  Proxmox — no Proxmox-only shortcuts.
- **Durable-plane guest paths follow the `/srv/data` convention** (see ARCHITECTURE.md's
  "Filesystem layout convention"). Every `--mpN` is born at `pct create` with an explicit
  `backup=` flag: the relay's docker-root stays at `/var/lib/docker` with `backup=1` (its
  Postgres/Redis/MinIO/git live as named volumes under the daemon root — relocating it would
  silently exclude the relay DBs from backup); relay deploy data lands at `/srv/data/relay`,
  CP at `/srv/data/cp`, k3s volumes at `/srv/data/k8s-volumes`, all `backup=1`; a
  `/srv/nobackup` mount gets `backup=0`. The converge pipeline's plane stage is never
  skipped — `ensure` is idempotent and runs every converge, because a skipped ensure after a
  compute-only teardown/rebuild would boot against stale recorded mounts.

## Known gaps (current, maintained here — not in CHANGELOG)

These are open limitations in the shipped code today, not history. Update this list as gaps
close or new ones surface; it's current-state, so it belongs here rather than in the
changelog.

- **No remote revocation of a capability already in a runner's hands.** The CP can stop
  issuing (revoke blocks provision/rotate) and erase its own copies, but a ciphertext blob
  someone else already holds still opens; re-keying after a leaked runner private key is out
  of scope. Epoch/staleness rejection is a named follow-up.
- **Backups can outlive "rotation = erase your copies."** `/srv/data` sits in the PBS +
  TrueNAS + Backblaze backup set, so a revoke that deletes the shipped `secrets.json` can
  still leave the old ciphertext in an off-site snapshot; backup retention is a named
  follow-up.
- **Rotate/re-grant don't reach an already-running runner.** A runner holds its package in
  memory from boot; only grants are re-read from disk per call. A rotate re-ships ciphertext
  a *restarted* runner will decrypt, but a live runner keeps serving the old in-memory
  credential until restart.
- **Abandoned streaming sessions are never reaped** — decrypted values stay in the session
  map for the process lifetime; a TTL reaper is sized but not built.
- **`timeout_s` kills the shell, not its descendants** (no setsid/killpg) — a timed-out
  command can leave orphans running.
- **Replay window:** a signed call can be replayed against the *same* runner within its 60s
  validity window; audience + runner binding closes cross-runner replay, but a per-runner
  replay cache is still open.
- **SSH connector:** capped at 2 pooled connections per target (the rationale predates a
  rollback and is now stale); a wedged connection stays pooled past a timeout; ssh timeouts
  return empty output where local execs return partial; no IPv6 in target parsing; pooled
  connections aren't re-authenticated after a rotate; no secret env injection over the ssh
  channel yet (extra requested secrets are rejected explicitly rather than silently ignored).
- **API connectors:** streamed exec on an API target redacts the injected secret value but
  not the (non-secret) base-URL env var — cosmetic, fix is a separate redaction list.
- **State store is single-process** — not cross-process atomic; planned Postgres swap at MVP
  addresses this.
- **Orchestrator `onboard` has no rollback** — a hard-fail at the readiness gate leaves CP
  state + the shipped package on disk; a re-run hits `RunnerExists`/`PackageDirInUse` and
  needs manual cleanup.
- **Console:** a secret posted to `/api/provision` or `/api/rotate` exists briefly as
  unzeroized body bytes before a `Zeroizing` wrapper takes ownership (loopback, TLS-free —
  same exposure class as the CLI's stdin path); the console's signing key is re-derived on
  every readiness probe rather than cached once.

## Build / test

- Rust (`core/`, `runner/`, `control-plane/`, `console-client/`, `testkit/`, `acceptance/`):
  `cargo build --workspace` + `cargo test --workspace`.
- Go (`orchestrator/` — the `freehold` and `freehold-orchestrator` binaries, the TUI, and the
  `harness/` release gate): `cd orchestrator && go build ./... && go vet ./... &&
  go test ./...`; `go test ./harness/` drives `target/debug/freehold-harness-oracle` and
  gates every crypto primitive against the Rust `core` byte-for-byte.
- No formatter/linter config beyond rustfmt + clippy defaults.
- `roadmap/POC_CHUNK3.md` (done), `roadmap/POC_CHUNK4.md` (current), and
  `roadmap/POC_CHUNK5.md` carry the live acceptance checkboxes; tick them as work lands.
  `freehold-acceptance` reproduces Chunk 1's acceptance criteria hermetically on loopback.

## Code style

- Rust: follow rustfmt; small crates; keep the runner↔CP contract at the crate boundary and
  language-agnostic (MCP over HTTP). Go: `gofmt` + `go vet` clean across `orchestrator/`.
- **Never** put secrets in code, config, tests, logs, or committed files. Private keys arrive
  via env var / mounted secret. No secret dumps in output or agent context.
- Keep `exec` generic — do not add semantic tools to work around a connector's API.
- No UI/chat surface rebuilds: Buzz provides the chat; the CP console is admin/ops only.
