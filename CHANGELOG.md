# Changelog

All notable decisions, reversals, and supersessions live here — the rest of `roadmap/`
(ROADMAP.md, POC.md, ARCHITECTURE.md, BUZZ_SURFACE.md, README.md) describes the **current**
plan only and should never carry inline "SUPERSEDED / formerly / previously" narration.
When something changes, update the docs to state the new reality plainly and add an entry
here explaining what changed and why. Most recent changes at the top.

## Versioning

This project follows [Semantic Versioning](https://semver.org) once there's a public
release to version. Before that (pre-1.0), we're using it loosely as a project-progress
marker:

*   **0.x.y** — pre-MVP chunks (see `roadmap/POC.md`); each patch bump roughly corresponds
    to "through Chunk N," e.g. 0.2.0 = first Chunk 2 step, 0.2.1 next chunk 2 step, 
    0.3.0 = first Chunk 3 step. Merging chunk steps (sub tasks, etc) to main will increment 
    the minor version, same for tweaks and fixes. These numbers aren't tied to chunk 
    deliverables but to merges to main. Note that this was introduced in version 0.3.0,
    previous versions have all merges and minor versions collapsed.
*   **1.0.0** — reserved for the MVP / public release definition in ROADMAP.md.

### Known gaps
See `AGENTS.md`'s "Known gaps" section for the current, maintained list of open
limitations (revocation/rotation reach, replay windows, connector edge cases, etc.) — that
list is current-state and kept there rather than duplicated here.

## [0.4.7] — Chunk 4 Phase G: operator-box ⇄ CP decoupling (login foundation)

Phase G wraps Chunk 4 with the CP-decoupling base: the operator box stops being
the single root of trust, so a fresh box can rejoin a world whose CP survives.
This entry covers the phase as it lands; remaining Phase-G steps (slim `build`
to CP-bring-up, CP build/teardown action, `teardown` reorder, durable
world-state, migrations, async cert) extend it as they land.

### Added

- **`freehold login`, root-free.** Prompts **CP address + CP pubkey + operator
  nsec**, authorizes this operator against that CP via NIP-98 (`internal/oplogin`
  reworked), then **ends** — afterwards the operator just runs `freehold`. It
  pulls the CP's `/api/world` summary and seeds a local connection/desire profile
  (relay + CP coords, operator pubkey derived from the nsec), so a fresh box
  recovers with nothing that lived only on a lost one. The old nsec-only
  `--login` is superseded.
- **The CP pubkey is an enforced trust anchor.** `config.CpPubkey` records the
  CP identity a box has never met; `resolveCPPubkey` cross-checks the
  operator-supplied value against the CP's own `/api/world` report — hard error
  on mismatch, blank falls back to the report, neither → no anchor (so a wrong/
  hijacked CP address never seeds a bogus anchor).
- **`freehold logout`.** Clears this box's login ledger only; CP/world untouched
  (idempotent).
- **`console.World()`** (`GET /api/world`) + `WorldSummary` client method, and the
  **CP console's `/api/world` endpoint behind it** (`control-plane/src/web.rs`): a
  logged-in operator pulls relay + CP coords themself, so the recovery source of
  truth is the CP — `login` seeds from real CP state, not a mock.
- **Login materializes the box's own provisioning identity.** After a successful
  `freehold login`, `internal/oplogin` mints (first-run-wins) the box's
  `control-plane/agent-ops` ops identity — the same identity `freehold build` /
  `teardown` sign with — on disk. A freshly-logged-in box is therefore a durable,
  self-owned actor whose grant to the CP can live locally; it does **not**
  fabricate a `[runner]` block (that's the deployed runner's own identity,
  authored by `build`). `logout` clears the operator nsec ledger only and never
  erases this box identity.

### Fixed

- All login prompts share **ONE buffered stdin reader** (the AGENTS.md
  discipline): a fresh reader per prompt read ahead past the first newline and
  broke back-to-back/piped auth on a fresh box.



0.4.5 shipped agent-creates-agent as **relay-message watching**: a `watch-agents` daemon on
the operator box polled #freehold for a natural-language `create-agent name: X purpose: Y`
post and turned it into a deploy. That built the control-plane toolset as a regex over a
chat channel, on the operator box, and taught the CPA to "say a creation request in the
relay." It contravened the locked design (BUZZ_SURFACE: grants are native NIP-29 channel
membership, and A4 calls for a dedicated create/grant/manage toolset) — so this phase
reverts it and reimplements it correctly.

### Changed

- **A real CP-side toolset replaces watch-agents.** `freehold-agent-tools` is now a dedicated
  Go MCP server **on the control plane** exposing `create_agent` / `grant_agent` /
  `manage_agent`; each handler calls `internal/agent/tools.go` in-process (direct, not a
  proxy). The `watch-agents` executor and the CPA prompt instructing it to post `create-agent`
  to #freehold are deleted. Agent creation is a control-plane action, not a chat post.
- **Grants are a roster, not a static list.** The server authenticates callers with the
  shared signed-header scheme and authorizes them against **its own relay roster**: a private
  NIP-29 channel (9007), grants as channel membership (9000 put-user / 9001 remove-user), and
  the live whitelist as the relay's signed 39002 roster, read fresh per call and fail-closed
  on relay outage — the exact model a runner's grants follow. Seeded at bootstrap with the
  build/operator identity; revocation is a roster change and needs no restart.
- **The build dogfoods the audited path.** `stageCpa` no longer deploys the CPA in-binary —
  it calls the CP's `create_agent` over MCP, signed as the build identity (the seeded grant),
  the same call shape agent-creates-agent will use. The CPA is now the *first product* of the
  one audited path.
- **The local `freehold create-agent` CLI is gone.** All agent creation flows through the CP's
  `freehold-agent-tools` server. Identity minting moved to the CP's durable plane
  (`/srv/data/cp/agent-tools/…`), so it survives an LXC teardown. The durable source of truth
  for "which agents exist" is the CP registry: a rebuild reconciles it, re-creating agents
  idempotently with the same durable pubkeys (E3).
- **The CPA's deploy runner is the CP's own co-located runner.** The build co-locates the
  runner inside the CP (the IDH server's privileged transport into the box), grants
  `freehold-agent-tools` on it, and it applies agent pods through it — so runtime agent
  creation does not depend on an operator-box process.

### Added

- **The TUI drives the remote CP.** `l` logs into the CP console with the
  **operator's nsec** (the console admin — so it grants every remote operation
  locally: overview, provision, grant, revoke, agents, the web portal), persists
  the key 0600 under `~/.freehold/control-plane/operator`, and every later TUI
  launch **auto-logs in** from it (`freehold --login` does the same from the
  shell). `w` opens the web console pre-authorized (single-use portal token).
  The **Runners** tab toggles between the CP console and the local loopback
  list with `s`, and the **Agents** tab now shows the CP's *live* agent roster
  (name/pubkey/presence) from the freehold-agent-tools registry instead of the
  stale local loopback registry — `l` fixes what was a broken agent-ops
  (non-admin) login before.
- **The CPA's harness can now actually call the CP toolset.** `freehold-agent-tools`
  gained an `mcp` stdio bridge that aggregates buzz-dev-mcp's message tools with
  `create_agent`/`grant_agent`/`manage_agent`; the CPA pod fetches it from the
  CP server at boot (a static Alpine-compatible build, into `/tmp` since the
  image runs non-root), points `BUZZ_ACP_MCP_COMMAND` at it (falling back to
  plain buzz-dev-mcp if the fetch fails), and signs calls as its own nsec — the
  CPA is membered into the agent-tools roster at create time so it is
  authorized. Verified: `manage_agent` from inside the pod returns the real
  registry. The system prompt now reflects that create/grant/manage are real,
  callable tools (closing the honesty note).

### Removed

- `freehold watch-agents`, its `--since`/`--poll-ms` flags, and the `#freehold` control-
  channel parsing (`parseCreateReq`/`cpaNostrSecret`/`createReqRe`).
- The CPA system prompt's write-a-structured-request-to-#freehold ritual.
- The operator-side `create-agent` command, its deploy engine, and the config's recorded
  `[[agents]]` list (the CP registry now owns agent durability).

### Fixed

- **0.4.5 mints created-agent identities on the operator box** (`FREEHOLD_HOME/control-plane/
  agent-<name>`), which would not survive the CP's LXC teardown. Creation now mints on the
  CP's durable plane, closing that hole — and the CPA's identity follows it in.
- The CPA prompt now reflects that its harness actually runs the `freehold-agent-tools` stdio
  MCP bridge (create/grant/manage are real, callable tools — verified live, a CPA asked in
  Buzz created an agent end-to-end), closing the original 0.4.1/0.4.3 tool-contract honesty
  gap for good.

### Docs

- **Docs updated to current reality (a locked hygiene obligation).** README, AGENTS.md,
  VISION.md, ARCHITECTURE.md and the roadmap now describe what 0.4.6 shipped: the CP-side
  `freehold-agent-tools` toolset, the CPA's live agent creation, and the remote-CP TUI
  access. ARCHITECTURE's stale "Pieces" (the old freehold-delegate / four-view TUI / Go
  control-plane fiction) was rewritten to match the code, and `roadmap/SSL_FIX.md` — a note
  about a temporary, already-resolved issue — is removed.

### Known gaps at this version

See `AGENTS.md`'s Known gaps. Most relevant to this phase: the CPA pod's harness has not yet
attached `freehold-agent-tools` as callable MCP tools (a stdio MCP facade the pod would spawn
is the follow-up); and `freehold-agent-tools` deploys pods through the CP's co-located runner,
so runtime agent-creation depends on that runner being reachable.

## [0.4.5] — Chunk 4 Phase E: the CPA can create agents

### Added

*   **Agent-creates-agent is live.** The CPA, asked in Buzz to create a new
    agent, posts `create-agent name: X purpose: Y` to #freehold; the
    `watch-agents` executor deploys it: `freehold create-agent` mints a durable
    identity (under the CP control-plane area), adds the pubkey as a relay
    member, seats it in #freehold + publishes its profile, applies its pod via
    the runner, and registers it. Created agents are conversational-only (no
    skill/target) with their own relay-persisted memory (E1/E2).
*   **Created agents survive a rebuild.** They're recorded in the config and the
    build reconciles them after the CPA, redeploying them with the same durable
    pubkey; `mergeFromAnswers` carries the created-agents list forward (E3).
*   **Durability proven live (D1–D3):** a real conversation, a process restart,
    and a full teardown+rebuild — the CPA's identity, memory (kind-30174
    engram), and thread all survive on the relay's durable store.

### Fixed

*   `create-agent` rejects a name that sanitizes to the CPA's pod name or an
    existing created agent's (no silent clobbering of the live control-plane
    agent), while still allowing idempotent re-deploy of the same agent during
    rebuild reconcile.
*   `watch-agents` no longer drops a second create-agent request that lands in
    the same Nostr second (it dedupes by event signature instead of a strict
    timestamp watermark).

## [0.4.4] — Chunk 4: the freehold agent is live and conversational in Buzz

### Added

*   `rebuild` on-boards the CPA to the community relay: its pubkey is added as a
    relay member (`buzz-admin add-member`), its kind-0 Buzz profile (display
    name, default "freehold") is published, and it is seated in the
    deterministic `#freehold` channel — all idempotent, so a rebuild keeps the
    same agent identity and presence.
*   The CPA pod now spawns the bundled Buzz CLI MCP server
    (`BUZZ_ACP_MCP_COMMAND=/usr/local/bin/buzz-dev-mcp`, with `/usr/local/bin`
    on PATH) so the harness actually exposes the message tools and the agent can
    reply to `@freehold` mentions. This was the missing piece that left the
    agent able to generate answers it could never post (its native HTTP fallback
    is 403'd by the relay; a kind-9 channel post is what the tools use).

### Fixed

*   The CPA talks litellm through the recorded NodePort URL (`cfg.Litellm.URL`,
    e.g. `http://192.168.30.8:31400/v1`) instead of the in-kube service name
    `litellm.litellm`, which a `hostNetwork` pod cannot resolve through the node
    resolver — previously every LLM call died at the transport layer.
*   `AgentManifestScript` deletes the (immutable-spec) Pod before applying, so a
    changed agent environment reliably recreates the pod instead of failing apply.

## [0.4.3] — Chunk 4 D4: rebuild reconciles the full desired world

### Changed

*   **`rebuild` is desired-world, not opt-in.** The opt-*in* `--with-k3s` /
    `--with-litellm` flags are gone; `rebuild` brings up relay/cp/k3s/litellm/CPA
    by default and is idempotent per stage (boot-if-missing, create-if-absent,
    first-run-wins) — teardown what you want replaced, rebuild, and it resurrects
    only the missing piece. `--no-k3s` / `--no-litellm` are explicit opt-outs for
    iterative/dev worlds. The CPA rides litellm (opt out of either and the CPA is
    skipped: a brainless pod is a dead pod). The TUI `B` rebuild form gained a
    litellm step (blank = on).
*   **The litellm provider key is sealed and reused, not re-demanded.** The
    provider (fireworks) key ships once into the litellm runner (ciphertext) and
    is reused on rebuilds; only a truly cold world prompts interactively for a
    fresh one (or hard-errors under `--yes`). Rebuild no longer asks for it when
    it already has it.

### Fixed

*   The CPA's system prompt no longer claims `create-agent`/`grant-agent`/
    `manage-agent` are callable tools (the MCP toolset was deferred for D1–D3) —
    it states the current phase honestly and instructs the CPA to describe-and-
    not-fake a delegation request (closes the review's tool-contract mismatch).

## [0.4.2] — Chunk 4 Phase B: the embed is real, the prompt rides the manifest

### Fixed

*   **`//go:embed CPA_SYSTEM_PROMPT.md`** in the new
    `orchestrator/prompts` package embeds the CPA's purpose into
    `freehold-orchestrator` at compile time (a `..` path or an absolute path
    is invalid in a `//go:embed` directive, so the file lives inside the
    `orchestrator/` Go module, not at the repo root). `stageCpa` passes the
    embedded text
    into `agent.AgentPodManifest`, which embeds it as the
    `<pod>-prompt` ConfigMap's block scalar (indented four spaces per line,
    so `kubectl apply` parses it) and the pod mounts that at
    `/srv/freehold/CPA_SYSTEM_PROMPT.md`, re-reading it fresh on every
    spawn.
*   **The CPA's reasoning model rides the litellm gateway.** The agent pod
    points the buzz-agent harness at the in-kube OpenAI-compatible endpoint as
    `BUZZ_AGENT_PROVIDER=openai-compat` +
    `OPENAI_COMPAT_BASE_URL=http://litellm.litellm:4000/v1` +
    `OPENAI_COMPAT_MODEL=ControlPlaneAgent` (litellm alias → the registered
    deepseek route), with the API key from a per-pod `<pod>-litellm-key` Secret
    (`secretKeyRef` — never a literal). `stageLitellm` now registers the model
    under the `ControlPlaneAgent` alias and mints a scoped key for the CPA pod.
    D1 (a live Buzz conversation) is the first end-to-end proof of this wiring
    against the packaged harness.

### Deferred (named, not lost)

*   A compute-only teardown/rebuild re-seeds the pod's prompt ConfigMap from
    the orchestrator's embedded bytes; serving the prompt from the CP's
    `/srv/data/cp` mount (so an edit survives a rebuild without a
    re-deploy) is a named follow-up — see `roadmap/POC_CHUNK4.md`.
*   **The CPA toolset (create/grant/manage) is not yet an MCP surface.** The
    Go methods stay built and unit-tested, but the pod no longer sets
    `BUZZ_ACP_MCP_COMMAND` (the `freehold-agent-tools` scaffold was dropped for
    D1–D3); wiring it as a real MCP server is the Phase E follow-up for
    agent-creates-agent.

## [0.4.0] — Chunk 4 Phase A: the CPA on a real-agent harness

### Added

*   **A1 — the operator names the CPA at install.** `freehold rebuild` gained
    an interactive "CPA agent name" step (TUI form, seeded from a recorded
    `cpa_name`, default `freehold`) and the `--agent-name`/`cpa_name` value now
    persists through the rebuild config path instead of being a dead flag.
*   **A2 — the CPA runs on Buzz's real-agent mechanism.** The CPA deploys as a
    bare k3s Pod (`restartPolicy: Never` — an intentional exit stays terminal)
    running the `ghcr.io/block/buzz-sprig` image (tracking the moving `:main`
    tag today; a build-time digest pin is a named follow-up) whose entrypoint
    `exec`s `buzz-acp`, joined to the community relay with its own
    nsec — the remote-agent mechanism from Buzz's `VISION_REMOTE_AGENTS.md`
    (a body on the k3s substrate, identity sealed in a first-run-wins k8s
    Secret). A durable CPA identity under the CP's state dir means the same
    agent returns after a compute-only teardown/rebuild.
*   **A3 — the stored name is the CPA's Buzz identity.** The CPA's display
    name rides the pod's `freehold.fh/agent-name` annotation and the config
    `cpa_name`, and it derives every one of the pod's object names (Pod,
    Service, `<pod>-identity` Secret) from its sanitized (DNS-1123) form — so
    the agent the operator named at install is the agent that appears in
    Buzz, and its objects never collide with another agent's.
*   **A4 — a dedicated create/grant/manage-agent toolset.** The CPA gets
    `create-agent` (deploy a named sprig pod whose Pod/Service/Secret are all
    derived from that agent's own sanitized name, with its own durable
    identity), `grant-agent` (agent ↔ runner whitelist), and `manage-agent`
    (list/remove registry rows) — the dedicated toolset the roadmap calls
    for, no skill-execution tools yet. (The toolset is built and
    unit-tested; registering it as an MCP surface the buzz-acp harness can
    call is a named Phase B deferral — see `roadmap/POC_CHUNK4.md`.)
*   **A5 — the CPA registers like any named agent.** The CPA row lands in the
    control-plane agent registry at deploy; live ●/○ availability is the CPA's
    own relay kind:20001 presence, read fresh per view.

## [0.3.0] — Chunk 3: the Rust→Go refactor, the durable plane, and the Kube-slot plan

### Added

*   **The orchestrator, installer, and TUI are Go (Chunk 3's headwork).** The
    orchestrator — the bootstrap/repair engine, 16 subcommands — moved to the Go
    module `freehold/orchestrator` (go 1.25) across 19 `internal/` packages
    (bootstrap, cli, client, config, console, crypto, delegate, deploy, drive,
    flows, harness, planebase, provisioner, relay, state, teardown, tui, wire);
    the Rust `tui` crate is removed and `installer/` deleted (zero external
    dependents; the Go rebuild engine reproduces the stage library
    byte-for-behavior; `Cargo.lock` regenerated in the same commit), and both
    binaries (`freehold`, `freehold-orchestrator`) are Go. The Rust `core`
    (crypto/identity/wire) and `runner` stay the byte-exact reference oracle:
    `orchestrator/harness/` (plus the Rust oracle crate `freehold-harness-oracle`,
    a workspace member) gates every primitive — BIP-340 Schnorr via btcec/v2
    (parity-negated scalar aux mask, parity-probe-gated), X25519+HKDF+
    ChaCha20-Poly1305 sealed box, bech32 nsec, ed25519, serde-matching
    sorted-map `SecretPackage`, NIP-98 canonical events, kind-48001 audit,
    NIP-44 v2 engrams — seal/open and sign/verify byte-exact both directions.
    `mcp::serve` is NOT reimplemented: `onboard` spawns the Rust `runner serve`
    as a subprocess, and `freehold install`/`rebuild` shell the
    `control-plane`/`runner` binaries resolved relative to the running
    executable (`resolveRebuildBins()`; cargo never regenerates the Go bins).
*   **The TUI is bubbletea in the alt screen** (`runTUI` passes
    `tea.WithAltScreen()`), with mode auto-detection (bootstrap / configure /
    running), the running dashboard (Services/Agents/Runners/DATA, Tab cycling,
    2s timer), and interactive console flows (l login NIP-98 / p provision /
    x revoke / g grant via textinput). ONE activity surface (`activity.go`)
    covers boot check, teardown, rebuild (incl. the door gate), bootstrap, and
    both deploys: spinner + live label on the top line, ✓/✗ result rows (boot
    probes — config → runner → relay → cp → k3s → world state, 6s each) or a
    streaming last-12-lines window (subprocess runs — the `freehold` binary
    re-execs itself; `activityExec` is injectable for tests), `ctrl+c`
    aborts — no dashboard, no shortcut footer, while active. The rebuild form
    is 6 steps (operator pk · domain · tenant LV size · thin-pool name · new
    pool size · boot k3s), PTY-verified; the `B`/`b`/`d`/`c`/`t` forms all
    exec the `freehold` binary (self), and `flowMsg` is just `{ok, err}`
    (login flows only) — the old dashboard `Model.Wait`/`rebuildArgs`/`reload`
    machinery is gone.
*   **The rebuild engine runs the whole world.** `freehold rebuild`
    (`internal/cli/rebuild.go`) threads the Rust installer's stage set verbatim
    into Go: ensure bins → provision (ssh keypair; reuse tolerated only with a
    real package) → door gate (interactive ENTER/r/q, `--yes` bails actionably
    with the key + install line) → grant → serve (`pkill` the stale listener,
    wait for the port to close, spawn detached, poll 20s) → verify door
    (self-subprocess exec) → **write initial config** (merge preserves surviving
    facts; AFTER verify, BEFORE storage, so the plane mapping has somewhere to
    record) → storage resolve + ensure×3 (relay/cp/k3s-volumes; honors the
    RECORDED `plane.backend_kind` over re-detection) → bootstrap relay → record
    LXC relay (fresh load → resolve vmid+ip through the runner → mutate → save)
    → bootstrap cp → record LXC cp → k3s stage (boot-if-missing + the 900s
    in-guest install script, verbatim) → deploy-relay (deploy dir from the
    guest's ACTUAL mounts, never hardcoded) → deploy-cp (from the release
    binaries; state/bin dirs from the guest's last mount) → NIP-11 relay pubkey
    (best-effort) → final merge save. Every parsing helper (STORAGE-
    POOL/MOUNT/BACKEND lines, `pct list` exact-name vmid, eth0 ip,
    `pct config` mounts, NIP-11 pubkey) is a pure function with a hermetic
    test; the fresh-load/mutate/save record discipline is regression-tested
    against clobbering (the Rust `lib.rs` test ported).
*   **Teardown keeps the config INTACT.** `internal/teardown/teardown.go`
    runs whole-world as COMPUTE teardown — destroys LXCs, KEEPS the recorded
    coords (operator-owned facts; `PruneLxcCoords` gone) so a rebuild re-boots
    the SAME world deterministically — and `--data` adds the tenant datasets
    and the freehold-created thin pool before the door key / world home /
    config go LAST (intentional divergence from `installer/src/teardown.rs`,
    which removes config; the new `Runner` interface (Exec / DestroyOneLxc /
    DestroyDataset / DestroyPool) makes `Run` hermetically testable).
    `stageLocalLvm` honors the plane: `ChownGuestUid` is NON-RECURSIVE (top
    dir only — PVE's own invariant: `LXC.pm::create_disks` chowns only newly
    ALLOCATED volumes, non-recursive, root-of-volume; bind-mp dirs are
    untouched by PVE, so the sweep was ours), and the ctime forensics (all four
    volumes, nanosecond-identical, exactly at the stage-3 moment) pinned it as
    the cause of the EACCES cascade.
*   **The plane-placement gate holds.** `storage resolve` emits
    `STORAGE-THINPOOL: <name|->` (absence = ZFS → `stagePlacement` skips);
    `--thin-pool` headless does adopt-or-carve (`--pool-size-gb`, default 40),
    a verbatim name adopts, `--confirm-storage` gates creation, and
    ZFS + `--thin-pool` bails actionably. `storage destroy-pool` was the ONLY
    storage subcommand missing `addCommonFlags`, so a `--data` teardown's pool
    destroy died `unknown flag: --addr` mid-flight (after LXCs + datasets,
    before the pool) — fixed at the registration site in `handlers3.go`, the
    same pattern every sibling uses; a regression test pins all five storage
    subcommands against `addr`/`agent-dir`/`runner-pubkey`/`target`. It
    re-points PVE's `local-lvm` to a surviving pool before removal, or leaves
    it (the next rebuild re-points once the new pool is carved).
    `TenantLVSizeGB = 10` / `FreshPoolSizeGB = 40` are the HALVED sizes proven
    on the test PVE box (the Rust hardcodes 20/40); the live world's four
    tenant LVs (`relay-docker-root`, `relay-deploy`, `cp`, `k3s-volumes`) were
    resized 20G→10G and every service revived (thin-provisioned — headroom
    only). `prompt()` now reads through ONE persistent `bufio.Reader` over
    stdin — a fresh reader per call read ahead past the first newline and broke
    back-to-back prompts (the carve size after the pool name).
*   **`freehold install` (Go) is a thin front-end to the same engine**
    (`internal/cli/install.go`, commit `ad5c2bf`): `collectAnswers` gathers
    domain/host/relay-gw/sizing/k3s into a `rebuildFlags` and hands the SAME
    `newRebuildEngine(f).run()` the `rebuildCmd` uses — no parallel pipeline.
    The ONE buffered stdin reader built during collect is handed to the engine
    (`eng.stdin = ui.in`), so the mid-pipeline prompts (thin-pool placement,
    door gate) never lose bytes.
*   **Operator identity (port of Rust `main.rs collect()`)**: have-key
    persists the operator's nsec as `identity.json` (their OWN nostr secret +
    a FRESH random enc keypair — `Identity::from_nostr_secret` semantics;
    derived-pubkey check bails on mismatch; refuses when `identity.json`
    already exists); generate REUSES an existing `identity.json` (Rust
    `mint_identity` loads on existence), else mints. Storage consent is asked
    up front in collect (same bool, same gate, flows as `f.confirmStorage`).
    `install_test.go` (10 tests, all green): nsec persist + skip +
    mismatch-bail, existing-identity refusal, pubkey re-prompt, generate mint +
    reuse (byte-identical file), defaults carry-through, abort-before-engine
    (the mint runs before the proceed gate; the engine is never constructed),
    and the exact flag handoff to the engine. `go.mod` promotes
    `charmbracelet/x/term` indirect→direct for the no-echo nsec read.
*   **Teardown is ACTIVE with checkboxes.** `teardown.Run` announces each LXC
    BEFORE it destroys it (`destroying relay LXC 100` streams via the same
    `say()`/Live hook before `DestroyOneLxc` runs — both WholeWorld and the
    tenant loops, vmid-guarded — and the CLI gets the raw bytes too), and the
    TUI renders teardown as checkboxes: `startSubprocessActivity` seeds one
    slot per MANAGED LXC from the config (`relay LXC 100` …),
    `feedTeardownLine` parses the streamed lines (`destroying` → running
    spinner row; `destroyed / already gone / never created` → ✓ with the
    reason), and `liveLabel` names the in-flight LXC. Caught a REAL bug: the
    CLI's Live hook INDENTS every line (two spaces), so the anchored
    `^destroying` regex never matched and the checkboxes stayed placeholder
    dots — `strings.TrimSpace` before matching fixes it
    (`TestTeardownLinesFlipCheckboxes`). `tail()` keeps the last 3 non-empty
    lines, trimmed, joined ` · `, capped at 200 chars — the old last-line-only
    tail made teardown's embedded-cause failure look blank.
*   **The first LIVE whole-world rebuild ran end to end on the real box**
    (2026-08-29; door pre-installed → no gate pause; VG `pve` had no thin
    pool → carved `freehold-thin` at 120 GB; relay 100 / cp 101 / k3s 102
    booted + recorded; the relay stack healthy, the CP serving on the operator
    admin seed, k3s active with kubeconfig). The four bugs it exposed each got
    a regression test (see `### Fixed / corrected`), and the live coords after
    the run are `192.168.30.225` (relay, 4/4 containers healthy,
    `/_liveness` ok), `192.168.30.205` (CP, `:8080`, admin `1dc07610…`) and
    k3s `10.10.0.7`.
*   **Static-IP wiring** (`cli/rebuild.go`): `--relay-ip`, `--cp-ip`,
    `--k3s-ip` (CIDR) + `--relay-gw` (default `192.168.30.1`);
    `bootstrapStaticIP(role, flags, cfg)` picks explicit flag → RECORDED
    `lxc.<role>.ip` → DHCP, and `stageBootstrap` reuses the recorded vmid on
    resume (the reuse path finds the existing guest by hostname instead of
    taking a new id and refusing the collision). CIDR validates fail-fast in
    `RunE` BEFORE `newRebuildEngine` — a bare host would only die deep at
    stage-7 `pct create`. Tests: flag-wins / recorded-fallback / none-is-DHCP.
*   **The second and third LIVE rebuilds proved the REUSE path** — 4 min 15 s
    vs ~70 min fresh. `rebuild --yes --relay-ip 192.168.30.8/24 --cp-ip
    192.168.30.9/24` passed all 10 stages + record on the SURVIVING plane
    (stages 0–7 idempotent; deploy-relay + deploy-cp green after the ownership
    fix); a destroyed-but-recorded world re-boots in minutes, same coordinates
    (100@.8, 101@.9, 102@.7 — `.7` verified free, then pinned static for
    determinism), same relay pubkey.
*   **`storage destroy-pool` and the activity view** (PR #131 review debt,
    `af26e44`; DEFER tier `e727a7b`) close all three open review items and
    six deferred follow-ups: `DestroyOneLxc` now reads the occupant's name and
    REFUSES when the recorded vmid holds a foreign guest (PVE hands a freed id
    to the next guest); no domain = fail closed; 4 tests drive the real
    `ExecRunner` through an injected `execFn`. `teardown --help` said
    "regenerated coords pruned" — the OPPOSITE of the keep-intact semantics —
    and the k3s seed defaulted blank-when-absent while blank = YES, so a
    k3s-off world would have booted k3s on accepted defaults; seeds `n` with
    a dispatch test pinning `--with-k3s=false`.
*   **k3s runs as a deterministic configure stage** (post-2.5, closing the
    "k3s unproven" flag). The ONE required flag is
    `INSTALL_K3S_EXEC="server --kubelet-arg feature-gates=KubeletInUserNamespace=true"`
    — the kubelet dies without `/dev/kmsg`, and a device-cgroup allow does NOT
    materialize it under userns. The dashboard lists `managed` pieces (relay/
    cp/k3s today); litellm appears the moment its coords land in the config
    (#115/#120), and the agents registry records named AI agents
    (delegate-peer registers itself at start) with ●/○ from relay kind-9
    presence (#118).

### Fixed / corrected

*   **Four live rebuilds corrected the Go port.** (1) `parseKind` returned a
    NON-NIL action on `Reuse` and every caller reads non-nil as "NO existing
    backend" — the very first `storage ensure` (nothing recorded, no `--kind`)
    always failed with "no storage backend to ensure onto" on a host that HAS
    a VG. `Reuse` returns `nil` action now (the caller drives the detected
    kind). (2) The operator pubkey was stored raw — Rust `installer` runs
    `parse_pubkey_input` BEFORE the pipeline, and `deploy-relay`/`deploy-cp`
    reject non-hex at stages 13–14. `newRebuildEngine` normalizes npub→64-hex
    up front (parity with `installer::main.rs`); `ParsePubkeyInput` trims +
    lowercases hex (an uppercase paste silently missed the CP admin whitelist).
    (3) `stageLocalLvmRepoint` used `pvesm set local-lvm --thinpool` —
    REJECTED ("Unknown option: thinpool"; thinpool is not mutable via pvesm's
    API), and its probe greped a colon form that PVE's whitespace
    `storage.cfg` never matches. Scoped in-place `storage.cfg` edit (PVE's
    sanctioned manual repair), whitespace probe + post-edit readback. Tests:
    carve / already-correct-skip / missing-block. (4) `firstField` panicked on
    `pvesm list local`'s blank trailing line (`Fields("")+" "` = empty slice
    → `[0]`); returns `""` (`TestFirstFieldBlankLine`).
*   **The persistent-plane chown bug is closed.** Stage-3 ran `chown -R
    100000:100000` UNCONDITIONALLY (the comment claimed "Fresh ext4 is
    root-owned" with nothing gating on freshness) over the SURVIVING plane
    (compute-only teardown keeps the LVs), re-rooting every container-owned
    subtree — docker volumes with per-service uids (redis 999,
    postgres 70/100070 on host, buzz 1000) → guest root — and every non-root
    service EACCESed: redis MISCONF/BGSAVE, postgres `pg_filenode.map`, relay
    `git pack cache` EACCES. Redis self-heals on restart; postgres/buzz
    don't. Live repair applied in-place (git volume via `gitDataChownCmd`,
    postgres to uid 70, redis self-healed) — no LXC/volume destroyed.
    `TestResolveLvmMountsSurvivingPlaneNeverRecursiveChowns` pins it; the
    relay/zfs assertions are rewritten to the top-dir form.
*   **`freehold-acceptance`** reproduces every Chunk 1 criterion hermetically
    on loopback.
*   **Door-key recovery closes the "pressed B twice" trap.** A second
    `rebuild` sees the existing package → provision REUSES → the door gate is
    SKIPPED → `stageVerify` fails `ssh error: authentication failed` with the
    first run's key gone. `recoverDoorKey()` unseals it, parses the
    `openssh-key-v1` container, and emits ONLY the public line
    (`crypto.ExtractED25519PublicKeyLine`: private half parsed past, never
    returned/written — nothing new leaves the machine); `stageVerify` bails with
    the same shape as the fresh-key gate, and the shared `doorKeyWaiting`
    marker ("the door needs") renders BOTH pauses. Unrecoverable package → the
    actionable `rm -rf ~/.freehold` fresh-start message. Tests: PEM round-trip,
    sealed-package recovery, stageVerify re-surface + non-auth pass-through,
    unrecoverable bail.
*   **The door gate stays IN the activity view.** A rebuild bailing at the
    door (`--yes` can't prompt) exits non-zero with "the door needs …" on
    stdout → classified as an EXPECTED pause (`a.wait`), rendered YELLOW
    ("waiting for the operator") with the install line; ENTER re-runs the SAME
    `rebuild --yes` in place (provision REUSES → no new key, grant/serve
    re-run, `stageVerify` re-probes and on success the pipeline CONTINUES
    through config/storage/deploys) — never back to the 6-field form. ESC
    cancels cleanly; `q`/`ctrl+c` quit; other keys are swallowed. The shared
    classifier `rebuildRun(args)` feeds both the form dispatch and the ENTER
    handler; `TestDoorGateEnterRetry` / `TestDoorGateSwallowsKeys` /
    `TestDoorKeyWaitingRenderedNotError` pin it.
*   **The `B` prefill and npub acceptance.** `beginPrompt` seeds each step
    from `flowDefaults` — the recorded operator pubkey, domain, carved
    thin-pool (`Plane.ThinPool`; a reused stock pool is never recorded), and
    k3s membership `y` — editable, cursor at end; absent config = unchanged
    fresh-world behavior. The B-rebuild prompt always took `npub1…` (the
    engine runs `ParsePubkeyInput` before any stage); only the label claimed
    "(64-hex)".

### Known limits at this version

*   The pre-C0 items (k3s as a deterministic `configure` stage, the
    ready-for-litellm Services view, the agents registry, the working relay
    scope, per-tenant teardown, the durable volume plane) are the only
    backlog the plan specifies; `roadmap/POC.md`'s Chunk 4/5/6/7 sections
    hold the forward plan. The old draft's `C0`/`C1–C7`/`D1–D4`/`E1–E6`/
    `F1–F3`/`G1–G7` labels (and the "carried Chunk 1–2.6.1" items — B2's
    live-account leg, the VPS block-volume surface, CP-restart memory
    persistence, audit-as-channel messages, secondary-relay onboarding, and
    nginx-through-NodePort) were never written down anywhere else.
*   A dedicated relay-down repair drill is later pre-MVP work — the repair
    path IS the local-expert flow, exercised on every
    teardown/rebuild/re-attach; a human opening a room/DM with `@freehold` is
    Chunk 4's (Chunk 3's POC agents are scripted NIP-42 clients; the
    mechanics — 30174 memory, kind-9 delegation, NIP-98 auth — are
    live-proven only via CLI/scripts).
*   A separate Vultr-API runner identity under a relay roster was never
    minted; the drivers' create/poll/wait/destroy shapes ran against the real
    API (45.76.255.185), and a live Backblaze leg stays open pending real
    credentials.

### Removed

*   The Rust `tui` and `installer` crates — superseded by the Go TUI and
    `freehold install`; the workspace is `core, runner, console-client, testkit,
    control-plane, acceptance, orchestrator/harness/oracle`.

### Still open

*   Nothing shipped a Workstation-terraform path: the `terraform/` plans
    (#76/#78) and flavors (#72/#73) wait in Chunk 4's plan.

## [0.2.0] — Chunk 2: relay scope

### Added
*   **Phase 0 surface research** (`roadmap/BUZZ_SURFACE.md`): every integration surface the
    port must consume is named against actual Buzz — workspace/membership, agent identity,
    event kinds, rooms/DMs, native memory. Most capabilities ride native kinds —
    membership 13534, agent memory 30174, audit 48001, jobs 43001–43006, DMs 41001;
    only **grants** needed a freehold custom kind (and that custom-kind path is dormant:
    the live relay refuses kinds outside `ingest.rs::scopes()`).
*   **Bootstrap provisioning** (`freehold bootstrap`) with `proxmox-lxc` / `vultr-vps` /
    `hetzner-vps` drivers, hermetic-tested, dry-run verified against the PVE host. A
    blocking domain gate requires `--domain` and holds until it resolves (directly, or via
    an operator-managed proxy) — bootstrap always establishes a real identity before
    continuing.
*   **Relay deploy driver:** docker gate, curl+tar bundle fetch, compose start,
    `/_liveness` verify, scope claim; idempotent (template-ensure by host arch,
    docker+compose in the guest).
*   **Create-new vs attach-existing (B0):** no operator relay → create it; operator already
    runs one → skip creation, verify liveness + membership feasibility, and attach the CP to
    it. Re-runs resume, never re-create.
*   **Live relay + CP, each on its own LXC**, attached rather than co-located: relay on
    `relay-box`, CP on `cp-box`, communicating over the network — co-location with the
    relay is convenience, never assumed. The CP ships as a base64 binary through the
    runner's exec-only primitive (every remote command routes through `pct exec`).
*   **The CP joins the relay it is pointed at (C2)** via the relay's own member management
    (`buzz-admin add-member`); it cannot self-add. The relay is authoritative for
    membership; local state stays authoritative for grants (the shipped-package flow); local
    state mirrors membership, readable offline and write-through.
*   **Console NIP-98 operator auth** — admin allowlist seeded by `--operator-pubkey`; the
    operator logs into the console with their own nsec, which never leaves their machine.
    Server-issued `{nonce, ts}` challenge (60s freshness), signature verified against the
    admin whitelist, session cookie (httponly, SameSite=Strict, Secure once TLS is up),
    Origin-check + login rate-limit. The bind guard is authn-conditional: with authn
    configured the console may bind the LAN; otherwise the loopback-only refusal holds.
*   **Relay-persisted, encrypted agent memory** (kind 30174, NIP-44 v2 self-encrypted
    engrams; the relay stores ciphertext only) — live-verified against a real relay.
*   **Delegation, live-proven:** CPA → relay kind-9 channel → peer agent → runner → relay →
    CPA, with request/result correlation by id.
*   **Audit publishing:** every runner audit row is a signed Nostr event, spooled locally
    (fail-closed, never silent) and published to the relay when configured — detached, so
    a wedged relay never blocks an exec.
*   **The whole appliance ran on Proxmox-on-Cloud-Compute (Chunk 2.5).**
    `bootstrap --kind vultr-vps | hetzner-vps` provisions a PVE host (Debian 13 via the
    apt route — the custom-ISO variant was dropped during the spike), then the same
    `proxmox-lxc` + `deploy-relay` + `deploy-cp` flows run verbatim: live on both
    providers (Vultr 45.76.255.185, Hetzner 178.156.179.204), with the console and
    relay reachable publicly through host DNAT (`/_liveness` ok). No nested KVM exists
    on either cloud (`cpuinfo` confirms); the LXC/pod appliance needs none.
    `orchestrator/src/bootstrap.rs` carries `bootstrap_vultr_vps`/`bootstrap_hetzner_vps`
    (create/poll/wait/destroy), hermetically tested in `orchestrator/tests/bootstrap.rs`.
*   **Single-public-NIC cloud networking (V2):** vmbr0 over eth0, a private vmbr1 for
    LXC-to-LXC, DNAT from the public IP to the relay LXC's Caddy and the CP console.
    Client reachability is domain → proxy → host public IP → DNAT → LXC — the home
    own-IP-on-LAN pattern does not apply. Cloud DHCP will NOT lease to LXC veths:
    guests need static IPs on the private bridge plus host NAT (driver: `--lxc-ip` /
    `--lxc-gw`). pve-firewall's nftables persist after `systemctl stop` (14 drop
    rules) — flush them, or every guest loses egress silently.
    download.proxmox.com serves CN=enterprise.proxmox.com, and the trixie release key
    exists only there (the docs URL 404s), so apt goes over http. The route needs
    /etc/hosts pointed at the non-loopback IP (pmxcfs refuses 127.0.1.1) and the pve
    node's lxc/qemu-server dirs; no vmbr0 by default. Guest DNS needs dnsmasq on vmbr1
    — the host's resolver won't answer NAT'd guests. Debian 13's compose package is
    `docker-compose` (not `docker-compose-v2`). Hetzner: NIC = eth0, and
    `chpasswd: expire: false` avoids the forced root-password change; the
    private-only + DNAT-off-the-NIC variant avoids the bridge-move lockout entirely
    (Vultr kept the public bridge move).
*   **k3s in an unprivileged LXC (post-2.5, closes the "k3s unproven" flag):** 10.10.0.7
    on a static private IP (v1.36.3+k3s1, containerd). The ONE required flag:
    `INSTALL_K3S_EXEC="server --kubelet-arg feature-gates=KubeletInUserNamespace=true"`
    — the kubelet otherwise dies without /dev/kmsg (absent in the unprivileged LXC;
    even a device-cgroup allow does NOT materialize it — userns). Real workload: an
    nginx pod Running, images pulled through the LXC's NAT egress; pod-ip:200,
    cluster-ip:200, NodePort:200 — reachable both inside the LXC and from the PVE host
    (10.10.0.7:31500 → pod). Conclusion: the k8s layer rides the same static-net LXC +
    NAT/DNAT path the appliance already uses; no nested virt needed.
*   **Chunk 2.6's runner-profile surface (for the record):** kind 30181
    (`RUNNER_PROFILE`) — a runner's lifecycle snapshot: identity pubkeys, connector
    kind/address, status, secret NAME only. Revocation rides the status flip and
    rotation the `rotated_at` flip: same d-tag (the runner pubkey), REPLACE, never
    append (revocation never appends). Published at EVERY lifecycle mutation under
    `--relay-url` (provision/adopt publish "active", rotate flips `rotated_at`,
    revoke flips `status` + the existing empty-grants cut-off), author-gated to the
    console and Schnorr-verified locally — same trust anchor as grants, so a rogue
    member can neither mint nor clobber runner records. `control-plane rebuild
    --relay-url` queries ALL profiles and reconstructs the store deterministically +
    idempotently (re-runs converge to the same state); restored records carry NO
    ciphertext or package path — the relay never holds secret material — and `adopt`
    per runner re-arms the package (the documented re-trust step: a rebuilt CP's new
    console pubkey reads nothing until re-admitted).
*   **Runners as NIP-29 channels (Chunk 2.6.1):** a runner is a private channel;
    grant/revoke is `buzz-admin add-member`/`remove-member` — the CP is the sole
    COMMANDER, the relay signs the resulting 39002 roster; the runner's whitelist is
    its own channel's roster, read fresh per call, fail-closed, and `rebuild` folds
    the kind-9 `fh-profile` envelope identically (G-C's pinned-message fallback).
    G-A (headless drive of the write path) resolved LIVE: `provision --relay-url`
    synced 9007 create + 9000 put + `fh-profile`, and `revoke-grant` drove 9001 —
    both headless through the relay-admin runner, on the rebuilt world.
*   **Durable volume plane** (Phase 0.12): per-tenant datasets under a common
    parent, ZFS → LVM-thin → bail resolution on Proxmox-lxc targets, compute-only
    teardown + reattach-by-reference for relay/CP/k3s tenants.
*   **LiteLLM staged on an LXC landing-strip harness** (goose + buzz-acp + a minted
    LiteLLM key) — the first real-agent-capable harness, ahead of the kube
    substrate.
*   Full-workspace `cargo test` green, clippy 0, fmt clean throughout these slices.

### Fixed / corrected

*   **Four real-Buzz behaviors corrected the design (all live).** (1) Channel ids
    are dashed UUIDs, client-suggested: buzz's `extract_channel_id` parses `h` as a
    `uuid::Uuid` (64-hex sha256 → `None` → `invalid: channel-scoped events must
    include an h tag`), and `create_channel_with_id` HONORS the client id
    (duplicate → idempotent `accept:false`) — the channel row id ==
    sha256(runner pk)[:16], confirmed live. (2) Rosters carry a `d` tag, not `h`:
    buzz mints 39002 with `["d", <dashed uuid>]` + one `p`-tag per member (`pk`,
    "", role), relay-signed (author == the BUZZ_RELAY_PRIVATE_KEY pubkey — the
    runner's `--relay-pubkey` anchor); NIP-01 tag filters match STRING-EXACTLY, so
    `query_channel_roster` filters `#d` — `#h` matches nothing. (3) Kind 39000
    (group metadata) is NOT in buzz's ingest scope (`restricted: unknown event
    kind`) — the runner profile rides a kind-9 message tagged `t`=`fh-profile`
    (G-C's pinned-message fallback); `rebuild` folds it identically. (4) TWO
    membership layers: 9000/9001 execute CHANNEL membership, but every relay QUERY
    additionally requires COMMUNITY membership (`buzz-admin add-member` /
    `freehold relay-member`) — a runner/agent not community-membered gets
    `403 relay_membership_required` and fails closed (correct exposure; never
    serves stale grants). Provisioning in relay mode therefore needs BOTH:
    `freehold relay-member` (community) + the channel put-user (via
    `control-plane … --relay-url`).

### Known limits at this version
*   The CPA itself was not yet a live, talkable Buzz agent — it was operated via
    CLI/orchestrator; a human meeting it in a Buzz room/DM was explicitly out of
    scope (addressed starting 0.3.0 / Chunk 3).
*   The Buzz-UI interaction surface and a dedicated emergency-repair drill were both
    moved out of scope: repair reuses the same idempotent local-expert flow already
    pre-MVP work.
*   The parameterized `freehold-acceptance` harness itself has never run against the
    real relay — create-new, attach-existing, non-member
    (`403 relay_membership_required`) and revoke-without-restart were each proven
    live ad hoc; B2 stays hermetic-only until real credentials exist, and no
    separate Vultr-API runner identity under a relay roster was ever minted.
    `uuid::Uuid` (64-hex sha256 → `None` → `invalid: channel-scoped events must
    include an h tag`), and `create_channel_with_id` HONORS the client id
    (duplicate → idempotent `accept:false`) — the channel row id ==
    sha256(runner pk)[:16], confirmed live. (2) Rosters carry a `d` tag, not `h`:
    buzz mints 39002 with `["d", <dashed uuid>]` + one `p`-tag per member (`pk`,
    "", role), relay-signed (author == the BUZZ_RELAY_PRIVATE_KEY pubkey — the
    runner's `--relay-pubkey` anchor); NIP-01 tag filters match STRING-EXACTLY, so
    `query_channel_roster` filters `#d` — `#h` matches nothing. (3) Kind 39000
    (group metadata) is NOT in buzz's ingest scope (`restricted: unknown event
    kind`) — the runner profile rides a kind-9 message tagged `t`=`fh-profile`
    (G-C's pinned-message fallback); `rebuild` folds it identically. (4) TWO
    membership layers: 9000/9001 execute CHANNEL membership, but every relay QUERY
    additionally requires COMMUNITY membership (`buzz-admin add-member` /
    `freehold relay-member`) — a runner/agent not community-membered gets
    `freehold relay-member` (community) + the channel put-user (via
    `control-plane … --relay-url`).

## [0.1.0] — Chunk 1: engine room

*   Runner core: Nostr identity + separate encryption keypair, MCP tool server
    (over HTTP, even co-located, to prove the real shape), the generic
    `exec(cmd, target, stream?)` primitive, self-check readiness
    (green/yellow/red).
*   Secret provisioner (CP-side): encrypt-to-runner-key, ship ciphertext,
    inject runner private key, rotate, revoke — no master key anywhere.
*   Three connectors: SSH (local/PVE host), Vultr (create/destroy/status),
    Backblaze B2 (S3-compatible read/write) — all hermetic-tested;
    live-account verification of Vultr followed in the Chunk 2.5 spike
    (0.2.0), B2's live-account leg is still open.
*   Coarse grants: agent ↔ runner, whitelist Nostr pubkeys, signed calls only;
    dedicated runner per service by default.
*   Scripted orchestrator (CPA stand-in) driving onboarding/readiness/exec/
    demo — no real reasoning agent at this stage, by design.
*   Local admin/ops web UI: services-at-a-glance + live readiness +
    runner/secret/grant management.
*   `freehold-acceptance` — hermetic acceptance script reproducing every
    Chunk 1 criterion on loopback.
*   `freehold-acceptance` fixtures: mock Vultr/B2 APIs + in-process sshd, and
    the Chunk 1 acceptance script.

