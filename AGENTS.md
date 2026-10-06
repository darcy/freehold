# AGENTS.md — freehold

Open-source appliance: one-command install, AI-agent-operated. Lands a Proxmox VE / VPS +
Kubernetes stack with Buzz Relay as the control plane and a skill framework that installs and
configures self-hosted OSS. Narrative: "reclaim the future we were promised."

The current *released* version is the latest GitHub Release; the newest `v*` tag may
still be a pre-release awaiting e2e validation and promotion (see "Releases") — this file
deliberately never restates a version number, so it can't go stale. The engine room, the relay
scope, the durable volume plane, the Rust→Go refactor, and the real, reasoning CPA that lives in
Buzz are implemented and live-verified against real infrastructure (a real PVE host, a real
relay/CP pair under a real domain): the CPA holds conversations, survives a full rebuild, and
creates new agents itself when asked. The department capability runners are live, the CPA can
provision capability on the fly (`provision_runner`), and the freehold-subnet gateway and the data
plane (snapshot, export, restic backup) ship. Agent workspaces + git/GitHub are next; see
`docs/ROADMAP.md`. Open gaps and plans live in the domain docs (see "Navigation" and "Known
gaps"). For how we got here, see the repository's GitHub Releases; this file describes the
current state and the rules for working in this repo, not the history.

## Todo tracking (always)

**Every multi-step task runs under a todo list** (`todowrite`): one item per unit of work
or evidence gate, in the order it runs. Exactly one item is `in_progress` at a time;
items become `completed` only on evidence (a command's output, a check's pass), never on
intent; new work discovered mid-task becomes a new item instead of silently expanding an
old one. The list is the operator's live progress bar — keep it current, surface it when
reporting state, and let blocked items carry their blocker in the item text so a paused
task is resumable without re-deriving where it stopped.

## Documentation hygiene (locked) — a primary job of this file

**Docs describe current-world state only.** `docs/ROADMAP.md`, `docs/ARCHITECTURE.md`,
`docs/FREEHOLD.md`, `docs/AI.md`, `docs/NETWORK.md`, `docs/DATA.md`, `docs/COMPUTE.md`,
`docs/BUZZ_SURFACE.md`, `README.md`, and this file say what's true *now* — never "formerly X,"
"SUPERSEDED," "as of 2026-08-20," or other change-narration inline. When a decision changes:

1.  Edit the affected doc(s) to state the new reality plainly, as if it had always been true.
2.  Record the change for the next release: the release notes are written **at release
    time** from git history (see "Releases"), so no per-merge entry or version bump happens
    here.

## Pull requests (locked) — every unit of work ships through a PR

- Every chunk/phase lands as a branch → PR → `main`. Create the PR as soon as the
  branch has commits; the Bot Review (`ai-pr-review.yml`) posts its review on the PR, so
  the PR body is where review findings get worked.
- **Wait for the review to settle BEFORE working its comments.** After pushing,
  poll until both checks finish — `gh pr checks <n>` (CI `check` and the
  `bot-review` check) — and only then open the findings:
  `gh pr view <n> --json comments` (or `gh api repos/<owner>/<repo>/pulls/<n>/comments`)
  with a timestamp cursor, and fix what's real. Addressing comments mid-review wastes
  a round and risks editing files the review hasn't seen yet; 💡 SUGGESTION items belong
  in the PR body or the `Known gaps` section below, not in re-review rounds.
- **Commit and push; never merge.** Merging is the operator's call — do it only
  when the operator explicitly says "merge when complete" (or equivalent). Until
  then the PR sits in review, even at `MERGE-READY`.
- **No version numbers in commit or PR titles.** The version is assigned only at
  release time; a merge to `main` is not a release.

## Releases (locked) — a version exists only when it is released

- **Versions are tied to releases, never to merges.** There is no version bump per
  phase, per chunk, or per merge to `main`. Work lands with no version attached.
- **Every release is two things together:** an annotated tag `vX.Y.Z` on `main` and a
  GitHub Release (notes + assets). A tag without its Release, or a Release without the tag
  it names, is not a release.
- **The release notes are written at release time.** They compare the codebase at the
  previous release to the current one and record the **net** difference — what is true now
  that wasn't then — not a chronological log of every merge. Superseded or refactored-away
  work is omitted; the final state wins. The `release-prepare` skill owns this flow;
  `release-publish` promotes the result.
- **A release is cut as a pre-release, then tested, then promoted.** `release-prepare`
  produces the candidate: annotated tag + a GitHub **pre-release** (marked `prerelease`,
  assets attached, short notes) whose body ends in a **test-status table** (one row per
  provider × flow, seeded `⚪ Unverified`). `release-test` orchestrates the testing — a dev
  deploy first (`test-dev`: update dev to main, manually verify the changes with the
  operator), then the per-provider e2e (`release-test-proxmox` runs the live lifecycle on
  the pre-release's downloaded assets and fills the Proxmox rows `✅`/`❌`; `⚪` when it can't
  test). `release-publish` then promotes it to a full release only when **every** row is `✅`,
  with the operator's go-ahead — same tag, same commit, same assets.
- **Release flow (trunk-first, current).** `main` is the trunk; all work lands there and
  every release is the tip of `main`, so the tag goes there — there is **no release
  branch**. The skill generates the notes from git history since the previous release and
  gets the operator's approval on the draft **before tagging**, then tags the `main` commit
  and publishes it as a pre-release with those short, high-level notes. There is no release
  PR and no changelog file — the tag and its Release are the release. An rc (`vX.Y.Z-rc.N`)
  is tagged the same way and is likewise a pre-release; iterating re-tags the next rc on
  `main`.
- **Release branches (future, when development continues past a release).** Then the model
  becomes trunk-first: `main` stays the trunk where all work lands, and a long-lived
  `release/vX.Y.Z` branch is cut for a release; fixes which should ship in it are
  **backported** (cherry-picked) onto that branch, and the tag goes on the **release branch
  head**, not `main`. The branch is not merged back (the trunk already has the originals).
  `release/` is the prefix. Not needed yet — everything is currently about the current
  release.
- **Numbering:** `0.x.y` stays semver-ish pre-MVP (minor for a chunk's work, patch
  for a phase); `1.0.0` is reserved for the MVP / public release.

## Environments — dev, test, prod

`.envs.yml` at the repo root (gitignored — it maps this operator box's profiles) is the
source of truth for what each world is. Role keys — `dev`, `test-<provider>-<env>`
(`test-proxmox-rebuild`, `test-proxmox-live`; Vultr slots in when its e2e lands), `prod` — each hold a
list of profile names (`freehold profiles`). A world is reached through the agent's own
shells (tmux/herdr panes included) via the `freehold` CLI / SSH / doors — the file governs
*which* worlds, not *how*.

- **Listed = accessible and expected to work there. Unlisted = do NOT access it.** If a
  task touches a world that isn't in the file, get its mode from the operator — never
  guess — then add the listing yourself and proceed. The file is agent-extensible by
  design; the operator-stated mode is the only gate. `fresh` is transient (gone on
  uninstall) and is never listed.
- **dev — build, test, sandbox.** Provision new services for agents, verify they work,
  write the skills prod agents will use; refactor and exercise directly on the box. **The
  update flow is the only way changes reach any world — dev included:** PR → `main` →
  `freehold update` (or a world's `freehold build`). Doing a thing by hand on a box is
  for TESTING only — proving a fix works before it is code — never the way a change lands.
  When dev work exposes a fix: fix the world to unblock testing, then reproduce it in the
  codebase — a fix that lives only on the box isn't done until the PR merges and the update
  flow delivers it.
- **test — release e2e only**, via `release-test-proxmox` (Fresh/Rebuild/Live; Proxmox
  now; the Vultr e2e is next), reached only after the dev deploy (`test-dev`) is green. Never a dev
  sandbox. Fresh is disposable; Rebuild/Live persist
  between releases.
- **prod — review/debug only.** Observe, diagnose, report. No changes without the
  operator's explicit go-ahead; the fix path is always recreate on dev → PR → release.
  A direct prod fix happens only when explicitly allowed (hotfix).

## Navigation

- `docs/VISION.md` — narrative, single source of truth for the "why".
- `docs/ARCHITECTURE.md` — the core platform's current architecture: trust model, control
  plane, operator surface, the agent org, grants on the fly, locked decisions.
- **One doc per domain — the same buckets as `agents/`.** Each states the domain's current
  architecture, its known gaps, and its future work; this is where a gap or a plan is
  recorded. The retired plan docs (`docs/POC.md`, `docs/POC_CHUNK5.md`, `docs/POC_GRANTS.md`,
  `docs/followups.md`) were absorbed into them — no checkboxes live outside the domain docs,
  and the retired docs' history is git's. The five:
  - `docs/AI.md` — the LiteLLM gateway, the agent runtime (pods, identity, prompts, memory),
    AI hardware.
  - `docs/NETWORK.md` — the freehold-subnet, the gateway guest, the Caddy/cert/DNS edge,
    Pangolin as the public-path north star.
  - `docs/DATA.md` — the durable plane, `freehold snapshot`/`export`/`backup`, Data's
    `cp-local-root` door, the North Star.
  - `docs/COMPUTE.md` — the provider seam, the world's guests, storage, k3s + the terraform
    module, teardown/rebuild.
  - `docs/FREEHOLD.md` — everything that isn't a department: the core platform's known gaps
    (runners/grants/secrets, migrations, the relay, the console/CLI/TUI, CI), plans, UI/UX,
    and product direction.
- `freehold-cli/` — the local operator surface (top-level Go module): the `freehold` CLI
  + TUI, `login`/profiles, and the `install` surface. It gets a control plane up in an
  environment (Proxmox or a Vultr VPS today; Hetzner next) and a door to it; the
  shared provisioning engine lives in `platform/provisioning/box`. It drives the server
  only through the CP API or sibling binaries — it never links `control-plane/`. World
  bring-up after install is `freehold build` from any box via the CP. `install` **requires
  `--name`** (the profile name scopes config + state to `profiles/<name>/` and prefixes
  the guest LXCs `<name>-<role>`), and `--host` **unless the mint creates the host** —
  a `--provider vultr` install derives it from the instance it creates (the profile
  records it, so a later `uninstall --name` resolves it without the flag). A fresh
  plane also needs the relay/CP
  domains + the proxy IP (the guided flow prompts), plus the operator's Buzz display name
  (`--display-name`) — the kind:0 profile the build publishes from it is what makes the
  desktop app skip its stock first-run onboarding. Re-running an existing name whose CP is
  **absent re-adopts** the plane's runner (identity preserved — the door rotates, never the
  Nostr/enc key), while a **live** CP is refused (reconcile with `freehold build`, drop it
  with `teardown`/`uninstall`, or join it with `freehold login`). On an api-vultr world a
  default uninstall leaves the instance RUNNING AND BILLING — the profile is kept as its
  handle; `VULTR_API_KEY=... freehold uninstall --destroy-host` stops the bill. There is no
  `bootstrap`
  alias — `install --non-interactive` is the headless surface. A world with no recorded name
  keeps the domain-derived LXC names, and durable-plane names stay domain-keyed.
- **GitHub Releases** (not a repo file) — the released versions, their notes, and assets; the
  version history lives there, not in the tree.
- `docs/ROADMAP.md` — the ordered view across domains: Now / Next / Later, the cross-domain
  milestones, the MVP definition, the North Star. It points at the domain docs for detail.
- `.agents/skills/release-prepare/SKILL.md` — the `release-prepare` skill: cut a versioned
  candidate by generating the release notes from git history since the previous release,
  getting the operator's approval, then tagging the `main` commit and publishing a GitHub
  **pre-release** with the built assets, short, high-level notes, and the test-status
  table — see "Releases" above. The canonical, agent-agnostic location (auto-loaded by
  opencode and any other agent that reads `~/.agents/skills/`-style external skills).
- `.agents/skills/release-test/SKILL.md` — the `release-test` skill: the testing
  orchestrator, in LOCKED order — the dev deploy + manual verification first (`test-dev`),
  then the per-provider e2e; hands off to `release-publish`.
- `.agents/skills/test-dev/SKILL.md` — the `test-dev` skill: deploy main to the dev world
  (the profile listed under `dev:` in `.envs.yml`), diffing first and confirming with the
  operator what to manually test, then deploying via the update flow and running those
  checks. A NEW defect stops the release e2e.
- `.agents/skills/release-test-proxmox/SKILL.md` — the `release-test-proxmox` skill:
  exercise a pre-release's downloaded assets through the full lifecycle (install →
  agent replies in the relay → teardown → all down → rebuild → agent replies → uninstall →
  all gone) on a real PVE host, and record the Proxmox rows of the release's test-status
  table.
- `.agents/skills/release-publish/SKILL.md` — the `release-publish` skill: promote a
  pre-release to a full release only when every row of its test-status table is `✅ Passed`
  (metadata-only flip; the tag never moves).
- `docs/BUZZ_SURFACE.md` — the Buzz relay's actual surfaces and per-capability port
  decisions (native kinds vs. custom kinds).
- `docs/DOOR_SPEC.md` — the login-authorized door's security spec.

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
  agents reference secrets by name only. Two deliberate CP-held secrets, both sealed to the
  console identity: the DNS/litellm creds, and the operator's Nostr SIGNING key
  (`world-secrets/operator.json`, shipped by the box build). The operator key is the
  world's root credential — it attests agent memory (`contract/nipoa`) AND signs
  arbitrary operator events (console logins, relay owner-role actions), so a CP
  compromise yields operator impersonation; the CP holds it because the attestation is
  minted at every agent create/re-apply. Nothing under the CP state dir opens a runner's
  blobs.
- **Grants are coarse**: agent ↔ runner (whitelist of Nostr pubkeys). Dedicated runner per
  service by default; sharing via grants allowed. Readiness = the runner's own self-check:
  🟢 green / 🟡 yellow / 🔴 red. The unit of grant is the runner: one runner per capability,
  never widened to admit a second use — a new capability is a new runner. Runners are named
  `<target>-<protocol>-<identity>` (`pve-ssh-root`, `kube-api-caddysa`) — never for the
  consumer; identical capabilities share one runner's roster. The granting rules + guardrails
  live in the CPA's first skill (`agents/freehold/skills/granting.md`).
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
  department or agent directly; what is bounded is *capability execution*. On the wire the
  talk surface is buzz-acp's inbound author gate: the CPA responds to **anyone** (relay
  membership is the bound), each core department to an explicit **allowlist** — the operator
  plus the core agents — and a custom agent to its asker plus the CPA. A capability a
  department owns (external proxy, backup, compute/LXC, model registration, AI hardware) is
  executed by that department's identity, and the raw grant for it attaches to department
  identities, never to a custom agent that would then self-serve a second, ungoverned path to
  the exact capability the department exists to own and audit. Service lifecycle is **not** a
  department: whichever agent created a service — a freehold-delegate or a custom agent — owns
  its install/config/operation, ad hoc and unvetted as before. The four departments are
  **installed as part of the core build** (each a pod on the same harness as the CPA): all in
  the shared private `#freehold` channel — there are no per-department channels; conversations
  happen where they already are, with #freehold the fallback every core agent belongs to. The
  **first-run surface** the operator meets is freehold's, not the Buzz desktop app's: the build
  publishes the operator's kind:0 profile (name asked at install, `--display-name`), which makes
  the desktop app SKIP its stock onboarding (no starter channels, no private Welcome, no
  built-in welcome-team agents); the build instead stands up the open `#general` channel
  (CPA-owned, operator + CPA) and posts a one-time welcome in `#freehold` (guarded by any
  prior message there — a world with history is not a first run). Only
  the identity/grant separation is locked; capability tooling/secrets arrive per department
  later. A custom agent that self-serves a department-owned capability is a
  containment failure even if a grant would technically allow it — the department's prompt is
  the first line of defense, the grant the second.
- **Every agent reasons through the LiteLLM gateway by alias.** The build ensures the default
  alias set on the gateway before pods apply (`stageLitellmAliases`, registered straight
  from the CP's litellm store — the operator's first-build provider choice; terraform
  deploys the gateway but registers no model): `Code` (coding agents), `General` (the default for custom
  agents), `Freehold` (the core agents — the CPA + departments, pinned), `ExtraThinking`
  (complex architecture / deep thinking). A created agent's choice rides its registry row, so
  a rebuild re-applies the same alias.
- **Host-flexible — not locked to Proxmox.** Proxmox is the lead/default; VPS/cloud are
  first-class (the business path). The k8s layer and everything above the host driver run
  identically regardless of substrate. Installer/runner must target a VPS as easily as
  Proxmox — no Proxmox-only shortcuts.
- **Durable-plane guest paths follow the `/srv/data` convention** (see docs/ARCHITECTURE.md's
  "Filesystem layout convention"). Every `--mpN` is born at `pct create` with an explicit
  `backup=` flag: the relay's docker-root stays at `/var/lib/docker` with `backup=1` (its
  Postgres/Redis/MinIO/git live as named volumes under the daemon root — relocating it would
  silently exclude the relay DBs from backup); relay deploy data lands at `/srv/data/relay`,
  CP at `/srv/data/cp`, k3s volumes at `/srv/data/k8s-volumes`, all `backup=1`; a
  `/srv/nobackup` mount gets `backup=0`. The converge pipeline's plane stage is never
  skipped — `ensure` is idempotent and runs every converge, because a skipped ensure after a
  compute-only teardown/rebuild would boot against stale recorded mounts.

## Known gaps — recorded in the domain docs

Open limitations in the shipped code are current-state, not history, and each lives in the
domain doc that owns it — update it there as gaps close or new ones surface:

- **AI** (`docs/AI.md`): every pod holds the gateway master key; the sprig image is a moving
  tag; the memory attestation is unbounded in time; the respond-to allowlist is fixed at
  deploy; agents read the repo but cannot write it; AI hardware has no tooling.
- **Network** (`docs/NETWORK.md`): the gateway host route is not persisted; no per-guest
  vhosts; no tailscale/pihole skills; flat-LAN guests are DHCP.
- **Data** (`docs/DATA.md`): snapshot is Proxmox-only; backup has no scheduling or restore
  verb; backups can outlive rotation.
- **Compute** (`docs/COMPUTE.md`): no VPS provider; core guests carry no runner-client;
  world-config degradation on update; terraform destroy can reach other worlds.
- **Core** (`docs/FREEHOLD.md`): runners/grants/secrets (no remote revocation, rotate
  doesn't reach a running runner, replay window), migrations/update (no reverse migrations,
  the console-executor window), the relay (post-redeploy 403s), the console/CLI/TUI, and CI.

Anything a doc must restate about a gap (e.g. an agent prompt) points at that section rather
than copying it.

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
    config/console/litellm/relay/delegate/identity/worldfacts): `cd contract && go build ./... && go vet ./... && go test ./...`
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
    `status/`, `update/`, `exec/`, `profiles/`, `door/`, `dns-cred/`, `add-relay-member/`,
    `backup/`)
    plus `login/` + `tui/` and `internal/common/` + `internal/certcred/` +
    `internal/stages/`; the `install/` surface holds `internal/cpdeploy/`; the `freehold`
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
  - `target/{debug,release}/runner` (Rust) — `cargo build --bin runner && cargo build --release --bin runner`.
    **On a box whose glibc is newer than the guests' Debian, build against the guest's glibc in a
    container** — a runner linked against a newer glibc crash-loops the co-located unit on the CP
    guest (`version 'GLIBC_2.39' not found`, systemd stuck in `activating`):
    `docker run --rm --dns 1.1.1.1 -u $(id -u):$(id -g) -e HOME=/tmp/c -e CARGO_HOME=/tmp/c -v <repo>:/src -w /src rust:1.98-bookworm cargo build --bin runner` (and `--release`); deploy-cp fails
    the deploy with the same message up front when the just-shipped binary cannot load (the
    preflight), so the mismatch surfaces in the step log, not the guest's journal.
  - `target/release/freehold-agent-tools` (static, above)
  This is the same set `freehold build`/`freehold teardown`/`freehold install`
  resolve as siblings of the running binary — a box doing world bring-up needs all
  five present.
- No formatter/linter config beyond rustfmt + clippy defaults.
- Open verification legs and acceptance for in-flight work are tracked in the domain doc
  that owns the work (`docs/FREEHOLD.md` for grants-on-the-fly); tick them as work lands.
  The acceptance gate is Go
  (`control-plane/acceptance/`, run by
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
