# AGENTS.md — freehold

Open-source appliance: one-command install, AI-agent-operated. Lands a Proxmox VE / VPS +
Kubernetes stack with Buzz Relay as the control plane and a skill framework that installs and
configures self-hosted OSS. Narrative: "reclaim the future we were promised."

The current version is the top entry in `CHANGELOG.md` — this file deliberately never
restates a version number, so it can't go stale. Chunk 1 (engine room) and Chunk 2 (relay
scope), including the durable volume plane (Phase 0.12), are implemented and live-verified
against real infrastructure (a real PVE host, a real relay/CP pair under a real domain).
Chunk 3 (the Rust→Go refactor) is complete. Chunk 4 (a real, reasoning CPA that lives in
Buzz) is live: the CPA holds conversations, survives a full rebuild, and creates new agents
itself when asked; its remaining Phase-F resource baseline is the current focus — see
`roadmap/POC.md`. For how we got here, see
`CHANGELOG.md`; this file describes the current state and the rules for working in this
repo, not the history.

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
  branch has commits; the Bot Review (`ai-pr-review.yml`) posts its review on the PR, so
  the PR body is where review findings get worked.
- **Wait for the review to settle BEFORE working its comments.** After pushing,
  poll until both checks finish — `gh pr checks <n>` (CI `check` and the
  `bot-review` check) — and only then open the findings:
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
- `install/` — the `freehold-install` bootstrap CLI (top-level Go module): get a control
  plane up in an environment (Proxmox today; Vultr/Hetzner providers come later) and a door
  to it; the shared provisioning engine lives in `platform/provisioning/box`. World bring-up
  after bootstrap is `freehold build` from any box via the CP. `install` and `bootstrap`
  **require `--name`**: the profile name scopes config + state to
  `profiles/<name>/` and prefixes the guest LXCs `<name>-<role>`; a name that already has a
  profile is refused (reconcile with `freehold build`). A world with no recorded name keeps
  the domain-derived LXC names, and durable-plane names stay domain-keyed.
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
  purpose from `agents/freehold/prompt.md` (embedded by the `freehold/agents` Go
  package and shipped by the control plane,
  mounted into the pod as the `<pod>-prompt` ConfigMap, re-read fresh on every spawn at
  `/srv/freehold/SYSTEM_PROMPT.md`), and delegates to the agents it spawns rather than
  doing expert-level work itself. The deterministic runner/CP layer underneath (grants,
  secrets, teardown/rebuild) is unchanged by this — reasoning decides what to do, that layer
  still does it auditably.
- **The agent org is two tiers: the CPA and five departments.** The CPA is the sole user
  touchpoint; **Gatekeeper** (access/security), **Vault** (data plane), **Provisioner**
  (compute), **Agent Ops** (models/providers/agents), and **Services** (installed OSS — the
  aggregate registry/monitor; manages one directly when no dedicated per-service agent does)
  are its direct reports, each a distinct identity scoped to one domain. Talk is unrestricted —
  the operator and any agent may converse with any department or agent directly; what is
  bounded is *capability execution*. A capability a department owns (external proxy, backup,
  compute/LXC, model registration) is executed by that department's identity, and the raw
  grant for it attaches to department identities, never to a custom agent that would then
  self-serve a second, ungoverned path to the exact capability the department exists to own
  and audit. Deployment shape is not locked (departments are not necessarily five
  permanently-running processes); only the identity/grant separation is. A custom agent that
  self-serves a department-owned capability is a containment failure even if a grant would
  technically allow it — the department's prompt is the first line of defense, the grant the
  second.
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
- **The five departments are defined but not yet deployed.** Each department's prompt lives
  at `agents/<department>/prompt.md` and is embedded by the `freehold/agents` package, and a
  create naming a department resolves it (`agents.SystemPrompt` — the names
  `gatekeeper`/`vault`/`provisioner`/`agent-ops`/`services` are reserved). But nothing spawns
  a department pod on its own yet: deployment is lazy/on-demand, and the capability tooling
  they broker (external proxy, backup, compute, model registration) is not in this phase.
  Enforcement of "capability work goes through the owning department" is therefore the
  existing grant model, not new code — raw capability grants sit with department identities
  once Chunk 5 begins issuing grants, and custom agents never receive them.
- **The Vault/Gatekeeper "check in on a new service" question has no trigger yet.** The hook
  fires when an agent requests a service/compute through the CPA's provision path; that path
  is Chunk 5/6. Until then there is no provisioning request to raise the question on.
- **Agents are told to read the repo on boot and re-check periodically, but the mechanism is
  not wired.** Every non-custom prompt (CPA + departments) carries a shared orientation block
  naming the repo and the read-on-boot/periodic-recheck discipline, and is honest that access
  is not available yet. The git/GitHub grant + the read/schedule path land with Chunk 5's
  workspace/git work.
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
  unzeroized body bytes (loopback, TLS-free — same exposure class as the CLI's stdin path).
- **The freehold CP toolset (create-agent / grant-agent / manage-agent) is a real MCP
  surface on the CP (`freehold-agent-tools`), not chat.** The Go methods
  (`control-plane/api/agent/tools.go`) are served by a dedicated CP-side binary
  (`control-plane/api/cmd/freehold-agent-tools`) whose handlers call them in-process, authenticated with the
  shared signed-header scheme and authorized against the server's own relay roster (its
  NIP-29 channel + 39002 membership, read fresh per call, fail-closed).   Seeded at bootstrap;
  the build dogfoods `create_agent` to bring the CPA up and reconcile re-creates any agent
  the CP registry holds. The CPA pod's harness attaches this toolset as callable MCP tools
  via a stdio bridge (`freehold-agent-tools mcp`, fetched into the pod at boot): it
  aggregates buzz-dev-mcp's message tools with create/manage, signed as the agent and
  authorized by the server's roster. **`grant_agent` is wired through the absorbed
  console-owner credential and is OPERATOR-scoped** (not reachable by the CPA's
  conversation+create-only harness): the server loads the console's own identity from the
  console's state dir (`/srv/data/cp/control-plane/console`, 0600 durable plane) and
  publishes the kind-9000 put-user to the runner's channel in-process — the runner
  re-reads its signed 39002 roster per call, so the grant lands without a restart. A
  missing console credential fails closed ("no relay/console-owner wiring") rather than
  silently succeeding. An agent granting onto an arbitrary runner would hand direct exec
  access to that runner's MCP surface, so grants are the operator's call (server-enforced,
  `-32003` for agents).
- **Every agent pod holds the litellm gateway's admin master key today.** `stageLitellm` seeds
  the `<pod>-litellm-key` Secret with the gateway's master (litellm's `/key/generate` needs a
  bootstrap *virtual* `sk-` key before scoped per-agent keys can be minted), so the CPA — and
  any Phase E-created agent reusing `AgentLiteLLMKeyScript` — can register/remove any model and
  mint keys until scoped keys are wired. Minting a bootstrap virtual key and switching agent
  pods to scoped per-agent keys is the named follow-up.
- **Thin-box `freehold teardown` is compute-only (whole-world).** A login-only box (no local
  `[runner]`) drives teardown through the CP's co-located runner (`/api/world-teardown`), so
  `--tenant` and `--data` are refused there: the cp dataset stays mounted by the still-running
  cp LXC until the final detached step, and the thin box has no local runner/plane coords.
  Those need the build box until the data path is sequenced into the detached last step.
  It also requires a CP running a current console (the route postdates the worlds that
  predate it).

## Build / test

- Rust (`control-plane/core/`, `control-plane/runner/`, `control-plane/testkit/`,
  `control-plane/core/harness/oracle/`) — the runner + core, plus their hermetic test fixtures:
  `mise exec rust@1.98.0 -- cargo build --workspace` + `cargo test --workspace` (`Cargo.toml`
  declares `rust-version = "1.94"`). Rust is used for the privileged exec endpoint, the
  byte-exact contract oracle, and the runner's own fixtures — nothing else.
- Go — five modules. Run Go through mise (`mise exec go@1.25.0 -- go …`; each `go.mod` pins
  `go 1.25.0`):
  - `agents/` (`freehold/agents` — the top-level home for agent definitions: `freehold/`
    the CPA prompt + skills, `custom/` the template for agents the CPA creates,
    `common/orientation.md` the shared system-orientation block, and the five
    department definitions (`gatekeeper/`, `vault/`, `provisioner/`, `agent-ops/`,
    `services/`); embeds its Markdown as Go values):
    `cd agents && go build ./... && go vet ./... && go test ./...`
  - `contract/` (`freehold/contract` — the shared wire/trust leaf: crypto/wire/client/config/
    console/relay/state/coords): `cd contract && go build ./... && go vet ./... && go test ./...`
  - `platform/` (`freehold/platform` — the evolving world: services/provisioning/
    migrations/terraform): `cd platform && go build ./... && go vet ./... && go test ./...`;
    `provisioning/box` holds the SHARED provisioning engine (LXC boot, storage plane,
    deploy-cp, the CP bootstrap `Engine`) — imported by BOTH the install CLI and the
    operator CLI, so it stays control-plane-free.
  - `install/` (`freehold/install` — the CP bootstrap CLI): `cd install && go build ./... &&
    go vet ./... && go test ./...`
  - `control-plane/` (`freehold/control-plane` — the mechanism: api/cli/secret-management;
    the `freehold` binary, the TUI, and the `harness/` release
    gate): `cd control-plane && go build ./... && go vet ./... && go test ./...`;
    `go test ./core/harness/` drives `target/debug/freehold-harness-oracle` and gates every
    crypto primitive against the Rust `core` byte-for-byte.
- **`freehold-agent-tools` must be built statically** (`CGO_ENABLED=0 go build -C control-plane
  -o target/release/freehold-agent-tools ./api/cmd/freehold-agent-tools`): the CP server ships
  its own binary to agent pods, which run Alpine/musl — a glibc-dynamic build "silently not
  found"s inside the pod (`interpreter /lib64/ld-linux-x86-64.so.2` is absent).
- **The full binary set a `rebuild`/`teardown`/`install` box needs** (`box.ResolveBins` fails
  the pipeline until every sibling is present, and prints the exact build one-liner):
  - `target/debug/freehold` (the CLI+TUI) — `go build -C control-plane -o target/debug/freehold ./cli/cmd/freehold`
  - `target/debug/freehold-install` (the CP bootstrap CLI) — `go build -C install -o target/debug/freehold-install ./cmd/freehold-install`
  - `target/debug/freehold-console` **and** `target/release/freehold-console` (the Go CP CLI
    the box-side provision/grant/adopt/add-secret/revoke stages call, and what `deploy-cp`
    ships) — `go build -C control-plane -o target/{debug,release}/freehold-console ./api/cmd/freehold-console`
  - `target/{debug,release}/runner` (Rust) — `cargo build --bin runner && cargo build --release --bin runner`
  - `target/release/freehold-agent-tools` (static, above)
  This is the same set `freehold build`/`freehold teardown`/`freehold-install bootstrap`
  resolve as siblings of the running
  binary — a box doing world bring-up needs all five present.
- No formatter/linter config beyond rustfmt + clippy defaults.
- `roadmap/POC_CHUNK3.md` (done), `roadmap/POC_CHUNK4.md` (current), and
  `roadmap/POC_CHUNK5.md` carry the live acceptance checkboxes; tick them as work lands.
  The Chunk-1/2 acceptance gate is Go now (`control-plane/acceptance/`, run by
  `go test ./...`): the CP provisioner lifecycle, the console HTTP surface, and the
  relay-channel fold against a hermetic fake relay — the connector/relay behaviors the
  runner owns stay in its Rust tests. It drives the real `runner` binary (a subprocess),
  so the box's `cargo build --bin runner` must have run first.

### Testing the TUI (`freehold`, `control-plane/cli/cmd/freehold` → bubbletea dashboard)

`go test` under `control-plane/cli/tui/` verifies form logic, but it does NOT prove the running TUI.
**Always test the BUILT binary** — never reason from `go test` + a stale `~/.cargo/bin/freehold`.
The test step below rebuilds it FIRST, so there is nothing to remember: if you change a TUI flow
(forms, keybindings, dispatch, pre-flow chaining like the rebuild→DNS ask), rebuild + test the
installed binary in one go:

```bash
# 0. rebuild + place the binary FIRST (a passing go test does not re-place it):
cd control-plane && go build -o target/debug/freehold ./cli/cmd/freehold && cp target/debug/freehold ~/.cargo/bin/freehold

# 1. isolate state so the flow is deterministic (e.g. no DNS cred already stored):
cat > /tmp/fh-tui-config.toml <<'EOF'
relay_url = 'https://relay.verify.example'
relay_ws_url = 'wss://relay.verify.example'
cp_url = 'https://cp.verify.example'
operator_pubkey = '<64-hex>'
managed = ['relay','cp']
EOF
mkdir -p /tmp/fh-tui-home

# 2. a sibling pane, then launch THE BUILT BINARY there:
herdr pane split --current --direction right --cwd "$PWD" --no-focus
herdr pane run <pane> "cd $PWD && FREEHOLD_HOME=/tmp/fh-tui-home ~/.cargo/bin/freehold --config /tmp/fh-tui-config.toml"

# 3. watch + drive it (herdr captures the alt-screen viewport):
herdr pane read <pane> --source visible --lines 40     # see the boot check / mode
herdr pane send-keys <pane> B                          # open a form
herdr pane send-keys <pane> Tab                        # advance a field
herdr pane send-text <pane> some-value                 # type into a text field
# then re-read to assert the expected next screen (e.g. chained into the DNS ask)
```

The same approach works in a plain **tmux** session (`tmux new-session -d`, `tmux send-keys/…`,
`tmux capture-pane -p`), or any pty you can feed and screenshot (`script -qec … /dev/null`).
Isolate state via `FREEHOLD_HOME` (DNS provider creds, ops identity live there) and a temp
`--config` so you aren't exercising/mutating the operator's real world; clean both up after.

Because step 0 rebuilds the binary before testing it, the installed `~/.cargo/bin/freehold` is
always current — a TUI change is never "tested" against a stale build.

## Code style

- Rust: follow rustfmt; small crates; keep the runner↔CP contract at the crate boundary and
  language-agnostic (MCP over HTTP). Go: `gofmt` + `go vet` clean across `contract/`,
  `control-plane/`, and `platform/`.
- **Never** put secrets in code, config, tests, logs, or committed files. Private keys arrive
  via env var / mounted secret. No secret dumps in output or agent context.
- Keep `exec` generic — do not add semantic tools to work around a connector's API.
- No UI/chat surface rebuilds: Buzz provides the chat; the CP console is admin/ops only.
