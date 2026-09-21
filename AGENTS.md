# AGENTS.md — freehold

Open-source appliance: one-command install, AI-agent-operated. Lands a Proxmox VE / VPS +
Kubernetes stack with Buzz Relay as the control plane and a skill framework that installs and
configures self-hosted OSS. Narrative: "reclaim the future we were promised."

The current *released* version is the top entry in `CHANGELOG.md`; the newest `v*` tag may
still be a pre-release awaiting e2e validation and promotion (see "Releases") — this file
deliberately never restates a version number, so it can't go stale. Chunk 1 (engine room)
and Chunk 2 (relay scope), including the durable volume plane (Phase 0.12), are implemented
and live-verified against real infrastructure (a real PVE host, a real relay/CP pair under a
real domain).
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
2.  Record the change for the next release: `CHANGELOG.md` is written **at release
    time** from git history (see "Releases"), so no per-merge changelog entry or
    version bump happens here.

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
- **No version numbers in commit or PR titles.** The version is assigned only at
  release time; a merge to `main` is not a release.

## Releases (locked) — a version exists only when it is released

- **Versions are tied to releases, never to merges.** There is no version bump per
  phase, per chunk, or per merge to `main`. Work lands with no version attached.
- **Every release is three things together:** a `CHANGELOG.md` entry, an annotated
  tag `vX.Y.Z` on `main`, and a GitHub Release. A tag or a Release without the
  changelog entry is not a release; a changelog-only edit is not one either.
- **The changelog entry is written at release time.** It compares the codebase at the
  previous release to the current one and records the **net** difference — what is true now
  that wasn't then — not a chronological log of every merge. Superseded or refactored-away
  work is omitted; the final state wins. The `pre-release` skill owns this flow;
  `publish-release` promotes the result.
- **A release is cut as a pre-release, then promoted.** The `pre-release` skill produces the
  candidate: `CHANGELOG.md` entry + annotated tag + a GitHub **pre-release** (marked
  `prerelease`, assets attached, short notes). The `publish-release` skill then e2e-tests the
  pre-release's own downloaded assets against real infrastructure and, only on green with the
  operator's go-ahead, flips it to a full release — same tag, same commit, same assets.
- **Release flow (trunk-first, current).** `main` is the trunk; all work lands there and
  every release is the tip of `main`, so the tag goes there — there is **no release
  branch**. The skill generates the entry and gets the operator's approval on the draft
  **before committing**, commits it on a normal PR branch (e.g. `docs/changelog-vX.Y.Z`) →
  PR → waits for `check` + `bot-review` to settle → **the operator merges** (the skill never
  merges) → the skill tags the merged `main` commit and publishes it as a pre-release with
  short, high-level notes distilled from the entry. An rc (`vX.Y.Z-rc.N`) is tagged the same
  way and is likewise a pre-release; iterating re-tags the next rc on `main`.
- **Release branches (future, when development continues past a release).** Then the model
  becomes trunk-first: `main` stays the trunk where all work lands, and a long-lived
  `release/vX.Y.Z` branch is cut for a release; fixes which should ship in it are
  **backported** (cherry-picked) onto that branch, and the tag goes on the **release branch
  head**, not `main`. The branch is not merged back (the trunk already has the originals).
  `release/` is the prefix. Not needed yet — everything is currently about the current
  release.
- **Numbering:** `0.x.y` stays semver-ish pre-MVP (minor for a chunk's work, patch
  for a phase); `1.0.0` is reserved for the MVP / public release.

## Navigation

- `VISION.md` — narrative, single source of truth for the "why".
- `ARCHITECTURE.md` — system design, locked decisions, build plan.
- `freehold-cli/` — the local operator surface (top-level Go module): the `freehold` CLI
  + TUI, `login`/profiles, and the `install` surface. It gets a control plane up in an
  environment (Proxmox today; Vultr/Hetzner providers come later) and a door to it; the
  shared provisioning engine lives in `platform/provisioning/box`. It drives the server
  only through the CP API or sibling binaries — it never links `control-plane/`. World
  bring-up after install is `freehold build` from any box via the CP. `install` **requires `--name`
  + `--host`**: the profile name scopes config + state to `profiles/<name>/` and prefixes
  the guest LXCs `<name>-<role>`; the host is recorded in the profile (so a later
  `uninstall --name` resolves it without the flag). A fresh plane also needs the relay/CP
  domains + the proxy IP (the guided flow prompts). Re-running an existing name whose CP is
  **absent re-adopts** the plane's runner (identity preserved — the door rotates, never the
  Nostr/enc key), while a **live** CP is refused (reconcile with `freehold build`, drop it
  with `teardown`/`uninstall`, or join it with `freehold login`). There is no `bootstrap`
  alias — `install --yes` is the non-interactive surface. A world with no recorded name
  keeps the domain-derived LXC names, and durable-plane names stay domain-keyed.
- `CHANGELOG.md` — history of decisions, reversals, and releases.
- `roadmap/ROADMAP.md` — chunked roadmap: POC chunks 1–7, MVP definition, North Star.
- `roadmap/POC.md` — POC scope, goal, chunk-by-chunk plan, acceptance, test/promote flow.
- `roadmap/POC_CHUNK<n>.md` — the plan that actually exists: Chunk 3 (Rust→Go refactor,
  done), Chunk 4 (CPA in Buzz, current), and Chunk 5 (agent workspaces + git/GitHub).
- `.agents/skills/pre-release/SKILL.md` — the `pre-release` skill: cut a versioned
  candidate by generating the `CHANGELOG.md` entry from git history since the previous
  release, landing it via a normal PR branch (e.g. `docs/changelog-vX.Y.Z`), then tagging
  the merged commit and publishing a GitHub **pre-release** with the built assets and
  short, high-level notes (distilled from the entry, never the full changelog) — see
  "Releases" above. The canonical, agent-agnostic location (auto-loaded by opencode and
  any other agent that reads `~/.agents/skills/`-style external skills).
- `.agents/skills/publish-release/SKILL.md` — the `publish-release` skill: promote a
  pre-release to a full release only after an end-to-end run on its downloaded assets
  against real infrastructure (metadata-only flip; the tag never moves).
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
- **The agent org is two tiers: the CPA and four departments.** The CPA is the sole user
  touchpoint; **Network** (the network surface — access/exposure), **Data** (data plane), **Compute** (the box
  itself — CPU/RAM/disk, Proxmox LXC/kube and remote provisioning, plus the monitoring tooling
  it needs), and **AI** (models/providers/agents, plus AI hardware — local-AI
  accelerators like an RTX 3090 or DGX Spark are provisioned and tuned by AI, separate
  from Compute's general resources) are its direct reports, each a distinct identity scoped to
  one domain. Talk is unrestricted — the operator and any agent may converse with any
  department or agent directly; what is bounded is *capability execution*. A capability a
  department owns (external proxy, backup, compute/LXC, model registration, AI hardware) is
  executed by that department's identity, and the raw grant for it attaches to department
  identities, never to a custom agent that would then self-serve a second, ungoverned path to
  the exact capability the department exists to own and audit. Service lifecycle is **not** a
  department: whichever agent created a service — a freehold-delegate or a custom agent — owns
  its install/config/operation, ad hoc and unvetted as before. The four departments are
  **installed as part of the core build** (each a pod on the same harness as the CPA): in
  `#freehold` plus its own private `#<department>` channel, with the CPA a member of all. Only
  the identity/grant separation is locked; capability tooling/secrets arrive per department
  later (Chunk 5/6). A custom agent that self-serves a department-owned capability is a
  containment failure even if a grant would technically allow it — the department's prompt is
  the first line of defense, the grant the second.
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

- **The co-located runner starts with package grants, not the relay roster.**
  `deploy-cp` starts the CP's runner with only `--state-dir` (install and
  update alike), so it reads its whitelist from the shipped package grants — the
  console's self-grant — rather than the relay-signed 39002 roster. That is
  fine while the only caller is the console (the agent↔runner exec surface is
  still unwired, below); moving the co-located runner to the relay roster must
  land together with publishing the console's grant to the relay, as one change.
- **No reverse migrations.** Migrations are one-way `<epoch>.sh` scripts and
  completion markers are never un-marked, so re-deploying an older version runs
  old code against config a newer migration may have rewritten and cannot undo
  it. There is no downgrade verb; rolling back below the highest applied
  migration needs the snapshot escape hatch (out of scope); re-running `update`
  retries only *pending* work.
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
- **The four departments are installed, but have no capability tools yet.** `freehold build`
  creates each reserved department (`network`/`data`/`compute`/`ai`)
  through the same audited `create_agent`: its embedded prompt, `#freehold` plus its own
  private `#<department>` channel, and the CPA added to each channel — four pods, reconciled
  across a rebuild like any registry agent. The capability tooling they broker (external
  proxy, backup, compute, model registration, AI hardware) and the runner grants/secrets that
  back it are **not** in this phase: no agent can reach a runner's exec yet (the pod's MCP
  bridge exposes only messages + create/manage), so enforcement of "capability work goes
  through the owning department" is still just the grant model — raw capability grants sit with
  department identities once Chunk 5 wires the agent↔runner exec surface, and custom agents
  never receive them.
- **A rebuild of a world built before a department rename leaves stale agents.** The
  retired reserved names `gatekeeper`/`provisioner`/`services` (and, after the latest rename,
  `security`/`vault`/`agent-ops`) are no longer reserved, so on a
  rebuild `reconcileCreatedAgents` re-creates any surviving registry rows as custom-template
  agents in their old `#gatekeeper`/`#provisioner`/`#services` (or `#security`/`#vault`/
  `#agent-ops`) channels; nothing removes them.
  Fresh builds are clean. A retired-name cleanup on reconcile is a named follow-up.
- **The Data/Network "check in on a new service" question has no trigger yet.** The hook
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
- **`uninstall --remove-data` needs a box with a local provisioning runner.** The
  whole-world `teardown`, and `uninstall`'s world + door + substrate-key removal, work from
  a thin login-only box or against a dead CP through the transient root-SSH path (the box's
  DOOR_SPEC key). `--remove-data` still reaches the durable plane's storage through a LOCAL
  runner, so it needs the build box; `teardown --tenant` likewise needs the build box.
- **Re-adopt rotates the runner's substrate SSH key (same-host only).** deploy-cp
  regenerates it, authorizes it on the host, re-seals it into the plane's package (runner
  identity + grants preserved), restarts the runner, and drops the old `authorized_keys`
  line. Repointing a target to a **new host** still needs a target-repoint step on top of
  this (`install --restore`, out of scope).
- **Whole-world `teardown --data` is refused** — data removal is `uninstall --remove-data`.
  Per-tenant `teardown --tenant --data` still works (needs the build box).
- **`install`'s live-CP refusal is profile-based *and* host-side.** It probes a profile's
  recorded `cp_url` (`/healthz`) and, when the box holds an authorized DOOR_SPEC key (a prior
  `login`), also lists the host's guests for the `<name>-cp` guest and refuses. A box with no
  ops identity or an unreachable host falls back to the profile probe; re-adopt is already
  identity-preserving, so the exposure is only the un-requested re-deploy, not orphaned
  grants.

## Build / test

- Rust (`control-plane/core/`, `control-plane/runner/`, `control-plane/testkit/`,
  `control-plane/core/harness/oracle/`) — the runner + core, plus their hermetic test fixtures:
  `mise exec rust@1.98.0 -- cargo build --workspace` + `cargo test --workspace` (`Cargo.toml`
  declares `rust-version = "1.94"`). Rust is used for the privileged exec endpoint, the
  byte-exact contract oracle, and the runner's own fixtures — nothing else.
- Go — six modules. Run Go through mise (`mise exec go@1.25.0 -- go …`; each `go.mod` pins
  `go 1.25.0`):
  - `agents/` (`freehold/agents` — the top-level home for agent definitions: `freehold/`
    the CPA prompt + skills, `custom/` the template for agents the CPA creates,
    `common/orientation.md` the shared system-orientation block, and the four
    department definitions (`network/`, `data/`, `compute/`, `ai/`);
    embeds its Markdown as Go values):
    `cd agents && go build ./... && go vet ./... && go test ./...`
  - `contract/` (`freehold/contract` — the shared wire/trust/protocol leaf: crypto/wire/client/
    config/console/relay/delegate/identity/worldfacts): `cd contract && go build ./... && go vet ./... && go test ./...`
  - `platform/` (`freehold/platform` — the evolving world: services/provisioning/
    migrations/terraform): `cd platform && go build ./... && go vet ./... && go test ./...`;
    `provisioning/box` holds the SHARED provisioning engine (LXC boot, storage plane,
    deploy-cp, the CP bootstrap `Engine`) — imported by BOTH the install CLI and the
    operator CLI, so it stays control-plane-free. **`platform/` is provider-independent**:
    substrate commands live behind the `Provider` seam in `platform/provisioning` and are
    injected by the composition roots; nothing here imports `providers/` or names a `pct`
    command (a guard test enforces both).
  - `providers/` (`freehold/providers` — the substrate providers; `providers/proxmox/`
    holds guest create/exec/list, LVM/ZFS/thin-pool storage, the pct stage/DNS builders,
    and the `proxmox/teardown` world-destroy engine): `cd providers && go build ./... &&
    go vet ./... && go test ./...`. Imports `platform/` + `contract/`; never the reverse.
  - `freehold-cli/` (`freehold/freehold-cli` — the local operator surface: the `freehold`
    CLI + TUI, one dir-per-verb (`install/`, `uninstall/`, `build/`, `teardown/`,
    `status/`, `update/`, `exec/`, `profiles/`, `door/`, `dns-cred/`, `add-relay-member/`)
    plus `login/` + `tui/` and `internal/common/` + `internal/certcred/` +
    `internal/stages/`; the `install/` surface holds `install/cpdeploy/`; the `freehold`
    binary's main is `cmd/freehold`):
    `cd freehold-cli && go build ./... && go vet ./... && go test ./...`. **It never
    imports `control-plane/`** (an import-graph guard enforces it).
  - `control-plane/` (`freehold/control-plane` — the server + engines: api/cpbuild (build),
    api/console, api/agenttools, secret-management, state, core; the `freehold-console` +
    `freehold-agent-tools` binaries, and the `harness/` release gate):
    `cd control-plane && go build ./... && go vet ./... && go test ./...`;
    `go test ./core/harness/` drives `target/debug/freehold-harness-oracle` and gates every
    crypto primitive against the Rust `core` byte-for-byte. **It never imports
    `freehold-cli/`** (an import-graph guard enforces it).
- **`freehold-agent-tools` must be built statically** (`CGO_ENABLED=0 go build -C control-plane
  -o target/release/freehold-agent-tools ./api/cmd/freehold-agent-tools`): the CP server ships
  its own binary to agent pods, which run Alpine/musl — a glibc-dynamic build "silently not
  found"s inside the pod (`interpreter /lib64/ld-linux-x86-64.so.2` is absent).
- **The full binary set a `rebuild`/`teardown`/`install` box needs** (`box.ResolveBins` fails
  the pipeline until every sibling is present, and prints the exact build one-liner):
  - `target/debug/freehold` (the CLI+TUI+install) — `go build -C freehold-cli -o target/debug/freehold ./cmd/freehold`
  - `target/debug/freehold-console` **and** `target/release/freehold-console` (the Go CP CLI
    the box-side provision/grant/adopt/add-secret/revoke stages call, and what `deploy-cp`
    ships) — `go build -C control-plane -o target/{debug,release}/freehold-console ./api/cmd/freehold-console`
  - `target/{debug,release}/runner` (Rust) — `cargo build --bin runner && cargo build --release --bin runner`
  - `target/release/freehold-agent-tools` (static, above)
  This is the same set `freehold build`/`freehold teardown`/`freehold install`
  resolve as siblings of the running binary — a box doing world bring-up needs all
  five present.
- No formatter/linter config beyond rustfmt + clippy defaults.
- `roadmap/POC_CHUNK3.md` (done), `roadmap/POC_CHUNK4.md` (current), and
  `roadmap/POC_CHUNK5.md` carry the live acceptance checkboxes; tick them as work lands.
  The Chunk-1/2 acceptance gate is Go now (`control-plane/acceptance/`, run by
  `go test ./...`): the CP provisioner lifecycle, the console HTTP surface, and the
  relay-channel fold against a hermetic fake relay — the connector/relay behaviors the
  runner owns stay in its Rust tests. It drives the real `runner` binary (a subprocess),
  so the box's `cargo build --bin runner` must have run first.

### Testing the TUI (`freehold`, `freehold-cli/cmd/freehold` → bubbletea dashboard)

`go test` under `freehold-cli/tui/` verifies form logic, but it does NOT prove the running TUI.
**Always test the BUILT binary** — never reason from `go test` + a stale `~/.cargo/bin/freehold`.
The test step below rebuilds it FIRST, so there is nothing to remember: if you change a TUI flow
(forms, keybindings, dispatch, pre-flow chaining like the rebuild→DNS ask), rebuild + test the
installed binary in one go:

```bash
# 0. rebuild + place the binary FIRST (a passing go test does not re-place it):
cd freehold-cli && go build -o target/debug/freehold ./cmd/freehold && cp target/debug/freehold ~/.cargo/bin/freehold

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
