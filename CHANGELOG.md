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

## [0.6.5] — installs are named profiles; new LXCs are `<name>-<role>`

`freehold-install` wrote the single base config/state (`~/.config/freehold`,
`~/.freehold`), so a second install on a box that already hosted a world would
have collided with the first. And guest LXCs were named from the relay domain
(`<domain-dashed>-<role>`), which cannot distinguish two worlds on one host.

- **`install`/`bootstrap` require `--name`.** The profile name scopes the config
  (`profiles/<name>/config.toml`) and state (`<FREEHOLD_HOME>/profiles/<name>/`)
  — the profile registry `freehold profiles` already reads. A name that already
  has a profile is refused ("use `freehold build` to reconcile"), so a fresh
  install can never clobber a live world. Interactive `install` prompts for it.
- **Self-staged children stay in the profile.** A child `exec`/`storage`/
  `provision` process has no in-process profile, so `installConnect` derives the
  state root from the `--agent-dir` it is handed
  (`<root>/control-plane/agent-ops`) — no new flag, and it cannot be forgotten
  when a stage is added.
- **`config.Config.Name` records the world name**, set at install and preserved
  across rebuilds.
- **New guest LXCs are `<name>-<relay|cp|k3s>`.** `bootstrap.LXCName` prefers
  the profile name and falls back to the domain-derived name when none is
  recorded, so worlds installed before this (librem2 included) keep reconciling.
  The name threads through the box engine, `cpbuild` (boot/reconcile/terraform),
  and the console's `--world-config` (`config.Coords.Name ⇄ cpbuild.Spec.Name`).
  Durable-plane names remain domain-keyed — the name-as-data-prefix change is a
  deliberate follow-up.

## [0.6.4] — storage provenance is scoped to this world

The 0.6.3 selection treated ANY freehold data on a backend as *this world's*
previous plane — so on a shared box it offered a "reconnect" to another world's
data, and its erase path destroyed EVERY freehold domain it found on the
backend. Both are wrong on a box that hosts more than one world.

- **Domain-scoped classification.** `planebase.BuildOptions` now takes the
  current relay domain. A backend is this world's plane only when its provenance
  carries a domain EQUAL to it; another world's freehold data (or a name-only
  `freehold-*` pool with no domains) is ordinary reuse. `storage resolve` gains
  `--relay-domain`; the box engine and the CP's `worldStorage` pass their own.
- **Cross-world sharing is visible.** A thin pool's `OtherVolumes` counts riders
  that are not this world's own freehold entries — another world's freehold LVs
  now count as capacity being shared (they previously slipped through the
  `guest` rider filter). Reconnecting to your own pool still needs no share
  consent; sharing with another world does.
- **Erase is scoped and only ever removes this world's domain.** The keep/erase
  prompt names this world and says plainly that another world's data on the same
  backend will not be touched; the destroy loop runs for the matching domain
  only. A backend carrying only another world's data offers no erase at all.
- **The CP-side reconnect guard is gone.** `worldStorage` no longer needs its
  bolt-on `DomainMatches` check — classification is now the single owner of the
  rule — and a foreign freehold zpool is ordinary safe reuse (its datasets live
  in their own namespace).

## [0.6.3] — storage selection is a data-safety decision, not a guess

Freehold resolved the durable-plane backend by taking the host's first VG
(`vgs[0]`) and first thin pool. On a Proxmox box with several volume groups and
live guests that could land the plane on a busy pool — and, when it carved a new
pool, re-point PVE's `local-lvm` out from under every running VM's rootfs
(observed stranding a world's disks).

- **A read-only inventory is the decision input.** `storage resolve` now emits
  every zpool, VG/thin-pool (with riders and the `local-lvm` pointer), and whole
  disk, each CLASSIFIED by the data it carries. The rebuild engine consumes it and
  makes the placement a plain-language choice: a single safe backend is a
  one-keystroke confirm, several require an explicit pick, and `--yes` refuses to
  guess (it names `--plane-pool` instead).
- **freehold never erases a device that carries data.** A disk with any
  filesystem/PV/zpool/mount/OS/swap signature is listed with its reason and cannot
  be chosen. `EnsureZpool` now enforces the same clean-device precondition, so a
  `zpool create` can never run blind. Creating a new backend (LVM VG / zpool) on a
  clean device is a later phase.
- **Reconnect to freehold's own previous plane.** Entries matching freehold's
  exact naming (`freehold-<domain>-<tenant>` LVs, `<pool>/freehold/<domain>/…`
  datasets, `freehold-*` pools) are recognized as freehold's: a fresh install can
  keep and reconnect to them (requires the SAME relay domain — names are
  domain-derived) or erase them (only freehold-namespaced entries; a new
  `storage destroy` stage in the install module). Foreign data stays untouchable.
- **The `local-lvm` re-point is guarded.** It happens only when the pointer
  already targets the pool freehold carved, or its current pool holds no riders —
  so a coexisting world's guest disks are never stranded.
- **Non-expert wording.** Menus state consequences ("your VMs could run out of
  room") rather than mechanisms; risky sharing needs the typed word `share`; the
  install's up-front consent prompt is now accurate ("If this host has no usable
  storage…", freehold never erases existing data).
- **Fix: `freehold storage` was unregistered.** The CP-bootstrap module split left
  the operator `storage` command tree out of the control-plane root, so
  `freehold teardown --data` shelled a `storage destroy` that did not exist. It is
  registered again.

## [0.6.2] — thin-box teardown: any logged-in box can tear the world down

`freehold build` could be driven from any logged-in box (the CP-owned
`/api/world-build`), but `freehold teardown` still hard-required a locally
deployed provisioning runner and a `[runner]` block — which `freehold login`
deliberately never writes — so a login-only box could not tear down a world it
owned, even though login had authorized its host door.

- **The console gained an operator-scoped `/api/world-teardown`** (the mirror of
  `/api/world-build`): `cpbuild.BuildWorldTeardownApply` runs the shared teardown
  engine through the co-located runner — terraform destroy (the kube layer), then
  `pct` stop/destroy of relay + k3s. The CP LXC is destroyed **last and detached**
  (`setsid` + a short sleep), because the console and co-located runner live
  INSIDE it, so the endpoint returns before its own container goes. The CP's
  managed state (runners + secrets, agent registry, DNS store) is cleared AFTER
  the runner-driven work, so the very runner the teardown runs through is never
  removed out from under it.
- **`freehold teardown` routes through the CP when there is no local `[runner]`** —
  the thin-box branch `build` already had: confirmation → best-effort DNS →
  `/api/world-teardown` → local coordinate cleanup.
- **Compute-only from a thin box.** `--tenant` and `--data` still need the build
  box (the cp dataset stays mounted by the still-running cp LXC until that final
  step) — a named follow-up, see AGENTS.md "Known gaps".

## [0.6.1] — agent definitions move to a top-level `agents/` module

Agent definitions are a core piece of the puzzle and will grow (named agents,
their skills), so `platform/agents/` is pulled up to the repo top level as its
own Go module, `freehold/agents`.

- **`agents/` is now a fifth Go module** (`freehold/agents`, no
  dependencies). It carries `freehold/prompt.md` (the CPA — the `freehold`
  named agent) plus a `freehold/skills/` placeholder, and `custom/prompt.md`
  (the template for agents the CPA creates on the fly, previously a hardcoded
  Go string). A Go package cannot `//go:embed` outside its own module, so
  `control-plane` imports the embedded bytes (`replace freehold/agents => ../agents`)
  rather than re-embedding them.
- `AgentSystemPrompt(name, purpose)` now renders `custom/prompt.md` through
  stdlib `text/template`; same signature, so callers are unchanged. The CPA's
  `CPASystemPrompt` value is unchanged.
- `platform/` no longer carries `agents/`; `ci.yml` and `just test` build/vet/test
  the new module alongside the other four. Skill *shipping* to pods is still
  Chunk 5 (no Go consumer yet).

## [0.6.0] — bootstrap split out to a top-level `install/` module

`freehold build` (world via the CP) and the CP *bootstrap* are now two CLIs. The
bootstrap/install path — getting a control plane up in an environment + a door — moved
out of `control-plane/cli` into a top-level `install/` Go module producing the
`freehold-install` binary (`install` / `bootstrap` / and the self-staged
`exec`/`provision`/`storage`/`deploy-cp` subcommands). After bootstrap, world bring-up is
`freehold build` from any box via the CP, regardless of environment; the environment
(Proxmox now; Vultr/Hetzner providers later) only shapes install.

- The CP deploy package moved `control-plane/cli/bootstrap-cp` → `install/cpdeploy`.
- The shared provisioning engine (LXC boot, storage plane, deploy-cp, the CP bootstrap
  `Engine`) extracted to `platform/provisioning/box`, imported by both CLIs (control-plane
  and install); the contract owns `config.Coords` + `AgentToolsPort` so `install` never
  imports control-plane.
- `freehold` (operator) keeps `build`/`world`/`teardown`/`login`/TUI; `install`/`bootstrap`/
  `provision`/`storage`/`deploy-cp` are no longer `freehold` verbs.
- Introduced `freehold/install` as the fourth Go module (CI + `justfile` updated);
  before-and-after gates stay green (cargo + 4 Go modules + harness + acceptance).
- Named follow-up: `freehold-install`'s `install` still prompts collectively; a proper
  provider seam (Proxmox only today) is the next step, with the Vultr/Hetzner drivers
  already in `platform/provisioning/bootstrap`.

## [0.5.24] — Phase 3 complete: the Rust console is gone (Rust is runner + core)

The REFACTOR-PLAN's final Phase-3 step. The Rust `control-plane/console` and
`console-client` crates are deleted from the tree and the Cargo workspace, and
the Chunk-1/2 acceptance gate is Go — so "Rust only where it earns its keep"
now holds for real: the privileged `runner` and the byte-exact `core` oracle,
plus `testkit` (the runner's hermetic fixtures).

### Changed

- **Deleted the Rust `control-plane/console` + `console-client` crates** and
  dropped them (and the Rust `acceptance` crate) from the Cargo workspace. The
  Go console has been the shipped runtime since 0.5.5, and `console-client`'s
  only consumer was a Rust test in `web.rs`; the Go `contract/console` client
  replaced it. `testkit` stays — the staying runner tests depend on it.
- **The Chunk-1/2 acceptance gate is Go** (`control-plane/acceptance/`): it
  ports the provisioner lifecycle, the console HTTP surface, and the
  relay-channel fold against a hermetic fake relay (real NIP-98 auth + NIP-01
  filters + relay-signed rosters), and drives the real `runner` binary as a
  subprocess for the live-readiness leg. Connector/relay behavior the runner
  owns stays in its Rust tests. `just test` drops `cargo run -p freehold-acceptance`;
  the Go test gate (`go test ./...` in the control-plane module) covers it.
- **Docs state the current tree** — ARCHITECTURE.md, README.md, AGENTS.md no
  longer describe the Rust console as an acceptance fixture, and `REFACTOR-PLAN.md`
  is deleted (the plan is complete).

### Notes

- The Rust console's non-HTTP CLI verbs that the Go console never carried
  (`rotate-secret`, `revoke-grant`, `list`, `dns rm/list/sync`, and the
  relay-fold `rebuild`) remain reachable through the web/API, or are tracked
  follow-ups — see `followups.md`.

## [0.5.23] — install/build runtime fixes from live verification

Bringing a fresh world up end-to-end through the install wizard surfaced a
chain of runtime bugs the hermetic gate couldn't see; all are fixed here.

- **Co-located runner loopback**: the console runs INSIDE the CP LXC guest but
  was shipped the box-side host runner address (`cfg.Runner.Addr`, e.g.
  `127.0.0.1:8788`), a different netns, so `/api/world-build` died
  "connection refused". The world coords + the bootstrap-cp `adopt` now use
  `config.CoLocatedRunnerMCPAddr` (`127.0.0.1:8787`), the runner's own guest
  loopback. Box-side `build`/`bootstrap` honor the profile's recorded
  `Runner.Addr` unless `--addr` is explicit, so a multi-world box routes to
  this world's runner, not a foreign one.
- **install applies the flag-layer defaults**: the install path builds its
  flags from the wizard / prompts directly, never through `setupBuild`, so it
  never got the `registerBuildFlags` defaults. Empty `bridge` booted the relay
  LXC with a malformed `net0` ("invalid format - missing key"); empty `storage`
  failed the relay rootfs ("unable to parse volume ID ':16'"); empty
  `agent-name` shipped a nameless CPA. `applyInstallDefaults` now fills
  `bridge=vmbr0`, `storage=local-lvm`, and `agent-name=freehold` on both the
  wizard and sequential install paths.
- **exec profile on the ops + storage commands**: `readiness`, `storage
  resolve/ensure/destroy/destroy-pool/info`, `provision`, `deploy-relay`, and
  `deploy-cp` now resolve the active tenant profile (like `exec`), so a
  multi-world box signs for the right runner audience instead of the legacy
  default.
- **initial-config ordering**: bootstrap writes the config BEFORE the
  grant/serve/verify exec stages (they resolve the runner from it) and merges
  the recorded relay/CP hosts when this run supplied none, so an early write
  never clobbers a prior world's domains with a bare scheme.
- **`--relay-pubkey` flag-shift**: the world-build's agent-tools serve emitted
  a bare `--relay-pubkey` (empty — the relay's signing key is learned only
  after the relay boots), which makes Go's flag parser swallow the NEXT flag as
  its value and stop, silently dropping `--runner-pubkey`/`--runner-target`
  ("serve needs --runner-pubkey ..."). Emit it only when non-empty.
- **co-located runner secrets survive a re-deploy**: `deploy-cp` re-ships the
  box runner package's `secrets.json` on every install, wiping the litellm
  secrets the build added to the CP's co-located runner (and the box can't
  re-derive them — the CP is the durable owner). The re-ship now MERGES: the
  box wins on its own target credential, while the CP's extra names (litellm /
  postgres-pw / provider-key) and the console's grants survive. `world_build`
  also re-provisions the co-located runner from the CP's own durable litellm
  store whenever it finds them missing (re-seal → restart → wait for the port),
  so an older package heals on the next build.

## [0.5.22] — install wizard + pre-DNS IP fallback

Workstream B (in-place): `freehold install` on an interactive terminal is now a
bubbletea wizard that collects every answer — including the relay/CP domains
and the proxy IP, which `runBootstrap` would otherwise re-prompt at runtime —
up front, then hands the engine the collected `rebuildFlags`. Non-TTY input
(tests, piped runs) keeps the sequential prompts. Pre-DNS steps now connect to
the recorded guest IPs (`config.LxcIP` / `config.ResolveTarget`) until the
public domain resolves, replacing the two inline IP-fallbacks (console login
URL + agent-tools URL) with the shared helper. The shared provider-picker stays
in `package cli` (both consumers — the dashboard TUI and the wizard — live in
the same module) rather than adding bubbletea to the lean `contract` leaf.

## [0.5.21] — multi-tenant profiles (no implicit default)

The box-side CLI grows real multi-tenancy. Previously there was exactly ONE
connection profile living at `~/.config/freehold/config.toml` with state at
`~/.freehold`; `freehold login` overwrote it. Now a **profile** is one logged-in
tenant with its own config file (`~/.config/freehold/profiles/<name>/config.toml`)
and its own scoped state dir (`~/.freehold/profiles/<name>/`, or under
`FREEHOLD_HOME`), and the filesystem is the registry. There is **no implicit
"default" profile and no legacy single-config layout** — every tenant is a named
profile, and a pre-profiles box isn't auto-adopted (re-run `freehold login`).

- `freehold login` adds (or refreshes) a NAMED profile — prompts the CP address,
  defaults the profile name to the CP-host slug, seeds the world summary into
  that profile's config, and refuses to duplicate a CP under a *different*
  profile name. Re-login into the same profile preserves `[runner]`.
- `freehold profiles` lists the registered tenants (name + config path + CP).
- `build` / `bootstrap` / `install` / `world` negotiate the tenant via an
  interactive picker; with **zero** profiles they fail closed:
  "no tenant profiles — run `freehold login` first."
- `teardown` picks the tenant (its state, `WorldHome`, DNS creds, identities are
  all scoped) and passes `--config` to its child `freehold exec` subprocess so the
  runner resolves from the right profile.
- `exec` stays scripted/deterministic: an explicit `--config` wins, a single
  profile is used implicitly, multiple profiles without `--config` fail closed
  naming them.
- All state-path helpers (`freeholdHome`, the operator/agent-ops ledger,
  runner-pubkey + agent-dir resolution) read the negotiated profile's scoped
  paths instead of the hardcoded `~/.config/freehold` / `~/.freehold`.

## [0.5.20] — per-service Terraform + Omarchy-style script migrations

The infra slice settles on single sources of truth. The kube workloads graduate
from shell here-docs to **declarative `kubernetes_manifest` (server-side apply)
resources** (`postgres.tf` / `litellm.tf` / `caddy.tf` — the deterministic
static files that define each service; SSA so a `kubernetes_persistent_volume_claim`
can't serialize-deadlock against local-path's `WaitForFirstConsumer`), with the
substrate still exec-first `null_resource` shell. Migrations
move from Go closures in `BuildMigrator` to **versioned script files**
(Omarchy's `<epoch>.sh` + `<epoch>.verify.sh`), run ascending via `bash` on the
CP against a durable ledger. Dead reconciled-duplicate stage builders
(`K3sInstallScript`/`K3sLocalPathDurableScript`/`LitellmManifestScript`/
`CaddyManifestScript` + manifests) are deleted; the shell (`k3s-bringup.sh`) is
now the single owner of k3s install, and the per-service `.tf` files are the
single owner of the service definitions. Live verification on the PVE host also
fixed pre-existing blockers that kept the world from deploying: the relay bundle
pins minio images Docker Hub now denies (re-pointed to quay.io), k3s disables
traefik/servicelb so Caddy's hostNetwork can bind 80/443, the local-path
nodePathMap uses the node wildcard, the postgres password is read back
first-run-wins on rebuild (litellm/rebuild.go), and the world-build DNS step now
drives a `freehold-console dns add` subcommand (the rust `control-plane` binary
is gone).

### Added
- **`postgres.tf` / `litellm.tf` / `caddy.tf`** — the service definitions as
  `kubernetes_manifest` (server-side apply) resources: Deployment/Service/PVC/
  ConfigMap/Secret. Secret VALUES arrive as runner-injected env → `TF_VAR_*`
  (never argv) and ride the 0600 state (option A); first-run-wins is enforced at
  the Go layer (master-key + postgres-pw read back from the canonical Secret on
  rebuild, never re-minted). Model registration stays an event
  (null_resource local-exec; the provider key rides env, never state).
- **Two-phase apply + kubeconfig leg** — `cpbuild.tfRun` applies the substrate
  via `-target` (`TF_NO_SECRETS=1`) so k3s comes up, then `stageKubeconfig`
  fetches the k3s kubeconfig, rewrites the server to the node IP, and applies
  the services. The kubernetes provider downloads at `init` (online, like the
  cert flow).
- **Versioned script migrations** — `platform/migrations/files/<epoch>.sh` +
  `<epoch>.verify.sh` (go:embed), enumerated ascending, executed via `bash` on
  the CP with `FREEHOLD_AGENT_TOOLS`/`REGISTRY`/`CONSOLE_STATE` env; the ledger
  marks a migration done only when its verify gate passes. Added the
  `freehold-agent-tools registry verify|import-console` subcommand the scripts
  drive; migrations `1799900000` (registry loadable) + `1799910000`
  (import-console, additive) replace the old Go closures.
- **`followups.md`** — tracks the deferred relay-stack (A) and substrate-provider
  (B) pieces.

### Changed
- k3s install + the durable local-path carve-out are owned solely by
  `scripts/k3s-bringup.sh` (the Go `worldBootK3s` / `stages` builders are gone);
  Caddy's static edge is `caddy.tf`, its cert/DNS issuance stays the CP's
  event-driven overlay.
- Terraform runs from `tf.sh` with conditional secret requirements (destroy and
  substrate phase need neither the secret values nor a kubeconfig).

### Removed
- Dead reconciled-duplicate substrate builders in `stages.go`
  (`K3sInstallScript`, `K3sLocalPathDurableScript`, `LitellmManifestScript`,
  `CaddyManifestScript`, the embedded litellm manifest consts) + their orphaned
  tests; `shell scripts/{kube-apply.sh,kube-destroy.sh}` (superseded by the
  provider); `worldBootK3s`; `CaddyManifest`/`yamlBlockIndent`/`splitLines` in
  `services/webproxy/caddy` (only `RenderCaddyfile` remains).

## [0.5.19] — the substrate + kube workloads are Terraform-managed (A1)

Increment 5/6 of the CP-owned build (`roadmap/CP_OWNED_BUILD.md` Phase B): the
durable-plane LXCs and the litellm/postgres kube workloads are driven through a
real Terraform module instead of ad-hoc `pct`/`kubectl` shells with nothing but
the CP's own idempotence. The module is executed-first (null_resource + the
proven pct/kubectl scripts), embedded into the `cpbuild` package, and ADOPTS the
same resources the CP creates — so `terraform plan` runs clean against the
live world, and `terraform destroy` tears the kube layer + substrate LXCs down
during teardown.

### Added

- **`cpbuild/terraform`** — the exec-first Terraform A1 module (embedded, so it
  ships with the console): durable plane (`plane.sh`, mirrors
  planebase/drive LVM ensure), the cp/relay/k3s LXCs with their `backup=1`
  mounts (`lxc.sh`, mirrors `BootstrapProxmoxLxc`), k3s bring-up
  (`k3s-bringup.sh`, reconciled: drops the stale `.243`, no hostname `k3s`),
  and the litellm/postgres kube workloads (`kube-apply.sh`, now secret-based +
  idempotent registration — removes the hardcoded `llmproxy-db-pass`). The
  module's scripts are the same idempotent shapes the Go drivers produce, so
  they adopt the existing world plan-clean. Replaces the drifted
  `platform/terraform` leaf copy (superseded by the embedded module).
- **`cpbuild.stageDeployTf` + `worldTerraform`** — ship the module to the box
  at `/srv/data/freehold-tf` (0700) and drive `terraform apply` through the
  co-located runner; the litellm keys ride the runner by name → `TF_VAR_*`
  (never argv). Wired into `BuildWorldApply` after the substrate steps.
- **Terraform teardown** — `teardown.Run` runs `terraform destroy` (the kube
  workloads first, by depends_on order) before the k3s LXC is pct-destroyed;
  the Go LXC stops/destroys then find the guests already gone (idempotent).
  `tf.sh` tolerates a secret-less destroy exec.
- **`worldLiteLLM` shrinks** to the CPA litellm-key seed — the kube manifests
  + model registration now live in terraform (`kube-apply.sh`).

### Notes

- Exec-first (not the bpg/proxmox provider) was elected as the least-risk A1
  per the plan's own clause: PVE is 9.2.2 and bpg 0.66+ removed the
  tarball-create path (no `template_file_id`) that the live substrate was born
  from. Converting the adopt-managed resources to real provider resources
  (or the clone-based flow) is the named follow-up; the module is structured so
  a provider can slot in without changing the executor.
- The substrate LXC create path in Go remains the creator; terraform adopts +
  manages + destroys. Moving creation fully into terraform (vmid allocation)
  is a named follow-up.

## [0.5.18] — the boot checker is CP-driven and identical for every box

The "checking the world" screen did **different work** on the build box vs a
thin box, so one took ~10s (+ a hang) while the other flashed — a visible
asymmetry that kept saying "not yet at parity." Root causes were local probes
only the build box had coords to exercise. Now every box runs the SAME
CP-driven check.

### Fixed

- **Boot health steps** (relay/k3s/litellm/caddy/dns) no longer fall back to
  LOCAL probes when the CP session is pending: the control-plane step waits
  (bounded) for auto-login, then the steps report the CP's report. The build
  box's ~9s litellm HTTP probe + real relay/k3s probes are gone.
- **`refreshData`** dropped the local durable-plane storage probe — the DATA
  view is the CP facts for every box.
- **Runner** reports the CP's runner (from `/api/overview`), uniform; no local
  runner probe.
- The auto-login wait only blocks when a session is actually expected (persisted
  operator identity + CP URL) — a configured-but-never-logged-in box does NOT
  stall.

Verified: the build box and a thin box both land the dashboard in **~1s** with
byte-identical, all-green output. Only bootstrap remains special (no CP yet →
press `l`).

## [0.5.17] — drive-through-CP operator surface: a thin login box is the build box

Finally closed the last capability gap so ANY logged-in box (the "login box")
is functionally equivalent to the box that bootstrapped — it can take over 100%
of its duties. The only special case is the initial bootstrap (no CP exists →
build one); after that every box drives the world through the CP.

### Added

- **`world_exec`** (operator-scoped on agent-tools): runs a command through the
  CP's co-located runner, **validating the target** against the CP's own runner
  (error on mismatch, so a box never silently execs on a host it didn't name).
- **`freehold exec` on a box with no local `[runner]` routes here** — a thin box
  has the full exec surface without hosting a runner.

### Changed

- **`/api/world` serves `agent_tools_url` as the public `https://<cp>/mcp`**, and
  the Caddy CP vhost exposes `/mcp` (→ `:8089`), so a **remote** thin box can
  drive world build/exec/migrate/door over the public edge (not the LAN dial).
- Docs (ARCHITECTURE/README) updated for the drive-through-CP transport.

Verified live: a thin box whose config has no `[runner]` ran `freehold exec
proxmox-box 'df -h /'` through the CP (returned the PVE host's output) and drove
`freehold world build` through the CP (full reconcile report). A mismatched
target errors clearly.

## [0.5.16] — the boot checker settles on CP truth (relay header no longer sticks red)

A login-only box whose config held a stale `relay_url` could show the relay
**red in the header** even after auto-login, while its Services pane showed it
green + the CP domain. Race: the boot checker's relay step runs a **pre-session
local probe** (`config.RelayLive` on the stale IP) when `m.cpWorld` is nil at
the moment it starts; that probe finishes *after* `applyCPWorldHealth` sets
`m.RelayLive` true from the CP, clobbering it back to false. Nothing re-applied
CP truth, so the header stuck red.

### Fixed

- **`bootDone` re-runs `applyCPWorldHealth()`** when the world check settles, so
  a box that logged in mid-check lands **CP-driven** — the header flags never
  sit on a pre-session local-probe answer. No session ⇒ no-op (the local flags
  stand, the initial-bootstrap case).

## [0.5.15] — a box reads the whole world from the CP (relay/cp included)

Closed the last local-config leak in the box's world view. The relay + control
plane rows of a box's Services pane were built from the box's **local config**,
so a box whose `relay_url` held a stale IP showed the relay down against that
IP (the "why is it using the IP instead of the domain" a remote box hit) —
even after the CP was fixed to serve `https://<relay_host>`, only a side-channel
adoption picked it up. A logged-in box is now CP-driven for the whole Services
pane; local config matters only for the initial-bootstrap (no-CP-session)
fallback.

### Changed

- **Console `/api/world`** reports **relay + control plane as world services**
  (URL + a co-located health probe) alongside k3s/litellm/caddy. The relay
  row's URL is the **public edge** (`https://<relay_host>`), never the internal
  LAN dial the console uses for its own roster/event reads.
- **TUI `buildCpServices`** renders every pillar straight from the CP's
  `/api/world` services report — no explicit local-config relay/cp rows, no
  local probe.
- **TUI `applyCPWorldHealth`** sets the relay/cp/k3s/litellm/caddy flags from
  the CP's services report (`applyPillarFlag`), dropping the local
  `config.RelayLive(cfg)` probe.

Tests: `TestWorldServesPublicRelayURL` (world serves the public domain, not the
LAN dial) + `TestManagementBoxFullyPopulatedFromCP` (relay row URL must come
from the CP report, not `cfg.RelayURL`).

## [0.5.14] — world status is served from the console's public /api/world (DRY with /mcp)

Closed the last reason a remote (off-LAN) box couldn't reach parity with the
deployer box: the TUI's Agents/DATA/Certs views and `freehold world status`
dialed the agent-tools MCP server over the LAN (`:8089/mcp` → `world_status`)
and needed `cfg.AgentToolsURL`/`AgentToolsPubkey` — a LAN-only coord seeded
from login. A box off the subnet read an empty inventory and (mis)concluded
the world was down. Status reads now come from the public CP.

### Changed

- **Console `/api/world`** (public, session-gated) now folds the authoritative
  agent registry + world facts on top of the pillar services it already served
  — read live from the toolset's durable `registry.json`/`facts.json`, so every
  logged-in box (LAN or remote) renders the same CP-sourced status with **no
  local agent-tools coords**. This corrects PR #207's intent: coords became
  CP-sourced, but status still rode a LAN-only transport.
- **DRY:** both the `/api/world` route and the `/mcp world_status` tool now
  resolve the **same** `agenttools.WorldStatus` assembly — one implementation,
  two surfaces, never divergent.
- **`freehold world status`** reads the public `/api/world` (via the operator's
  persisted nsec, `oplogin.NsecToSecret`); the mutating world verbs
  (`build`/`teardown`/`migrate`) stay roster-gated on `/mcp`.
- The console now logs a broken inventory (malformed registry/facts) instead of
  silently serving an empty "world down" view.

`contract/console`: `WorldSummary` gained `Agents`/`Runners`/`DNS`/`Facts`
(raw, to keep contract free of control-plane types) + helpers.

## [0.5.13] — world facts: the CP carries the deployer-side inventory (DATA + Certs parity)

The real asymmetry closed: a management/login-only box can now render the DATA
and Certs views from the CP, instead of needing the deployer box's local config
+ host probes.

### Added

- **`world_register_facts`** (operator-scoped tool on the agent-tools server) —
  the box registers the deployer-side world facts at the end of `freehold
  build` (the "register-at-build" mechanism): the durable-plane layout
  (backend/kind/pool + the `/srv/data` tenant mounts with their `backup` flags),
  the canonical domains (relay/cp/proxy), and the edge cert metadata. Stored
  durably under the agent-tools state dir (`facts.json` — survives compute-only
  teardown, like the registry).
- **`world_status` now carries `facts`** — the single-inventory read serves the
  plane/certs/domains a management box needs.
- **The build reads each edge cert's `notAfter` from the durable mirror**
  (`/srv/data/k8s-volumes/caddy-edge/<slot>/fullchain.pem` via `openssl x509
  -enddate`) and registers it — the Certs view finally shows a REAL expiry
  (the config fields were never populated before).
- **The TUI DATA + Certs views render from the facts** on a box without a local
  runner: `refreshLocal` (post-auto-login) re-runs `buildCerts` + `refreshData`,
  so a login-only box shows the plane layout (live usage marked "—" — the live
  probe needs the deployer box) and the cert domains/expiry/issuer.

### Verified live

A rebuild registered the facts on the CP; a login-only box (fresh config, no
`[runner]`) auto-logged in and rendered DATA (the 4 durable mounts) and Certs
(relay + cp with real 2026 expiry) from `world_status` — full parity with the
deployer box's views.

## [0.5.12] — pin the k3s version (bypasses the flaky update.k3s.io channel lookup)

`world_build`'s k3s install failed on this network: the installer's version
resolution follows `update.k3s.io` → github.com, and `update.k3s.io` presented a
self-signed cert (a network-level MITM — github.com itself worked), so the
channel lookup errored and the installer fell back to a literal `stable` tag
which 404'd. `K3sInstallScript` now sets a PINNED `INSTALL_K3S_VERSION`
(`v1.36.4+k3s1`), skipping the channel lookup entirely — deterministic (the
same k3s on every bring-up) and immune to `update.k3s.io` being down/MITM'd.
Verified live: a fresh install with the pinned version completes and k3s comes
up active.

## [0.5.7] — UAT: a real teardown→rebuild→TUI→separate-box-login, and the `justfile`

A full UAT against the live world (teardown → rebuild shipping the Go console →
TUI on the operator box → login + operate from a separate box) surfaced two
real bugs and added the build ergonomics. Both bugs are fixed.

### Added

- **`justfile`** — one-command build ergonomics for a UAT/rebuild box: `just
  build` (all five sibling binaries a `rebuild`/`teardown` resolves),
  `just install`, `just check-siblings`, `just test`, `just teardown`, and
  `just build-world`. `build`/`teardown` run `./target/debug/freehold` — the
  COLOCATED binary — because `resolveRebuildBins` finds siblings relative to
  the running executable, so a copy installed elsewhere (e.g.
  `~/.cargo/bin/freehold`) has no siblings and `build` bails "sibling binaries
  missing". AGENTS.md documents the exact binary set + one-liners.

### Fixed (UAT-surfaced)

- **`agent_tools_url` froze at the deploy-time IP (a DHCP-lease change
  mid-build left it pointing at a dead IP).** `stageDeployAgentTools` records
  `cfg.AgentToolsURL` from the cp IP at that moment, and `recordPostWorld`
  updated the cp LXC coords but not `agent_tools_url` — so after a rebuild
  where the DHCP-assigned cp IP changed, `world status` and a fresh box's
  Agents view hit a dead `192.168.30.x`. `recordPostWorld` now records the cp
  coords too and reconciles `cfg.AgentToolsURL` to the CURRENT cp IP (the
  pubkey is durable/unchanged).
- **The `world`/`door` CLI verbs signed as the box's `agent-ops` identity,
  which a fresh login-only box's locally-minted agent-ops is NOT a toolset
  roster member of — `world status` got `-32001` denied on a fresh box.** The
  TUI already signs as the OPERATOR identity (a console admin, in the roster).
  `worldMcp` now signs as the operator identity too, so a fresh login-only box
  can operate the world (verified: fresh-box `world status` returns the full
  inventory).

### Notes

- The console's `/api/overview` reads its in-memory state, loaded at serve —
  a runner adopted into state.json AFTER serve (the co-located runner during
  `deploy-cp`) is stale until the console restarts (the Runners view shows
  "(no runners on the console)"). This matches the Rust console's behavior
  (pre-existing, not a regression); a `deploy-cp` re-run restarts the console
  and it reloads the adopted runner (verified: `proxmox-box` appears).

## [0.5.6] — Phase 2 part 2: the login-authorized door (DOOR_SPEC implemented)

The §9-gated door mechanism from `docs/DOOR_SPEC.md` is implemented. A fresh
box that logs into the CP can now authorize its own door key on the host and
perform CP-lifecycle work — not just the box that first built the world.

### Added

- **`world_authorize_door` / `world_revoke_door`** (operator-scoped tools on
  the agent-tools server). The CP appends/removes the caller-presented public
  door key on the host door **through its co-located runner** (the same runner
  that already holds the host door and drives `world_build`). Both are
  operator-only (denied to registry agents with `-32003`, like the world_* and
  grant tools).
- **The DOOR_SPEC §2.5 shell-injection gate** is enforced at the API boundary:
  the pubkey must match the strict `authorized_keys`-line regex (key type +
  base64 body + optional safe comment, no whitespace runs / quotes / backticks /
  `$` / `(` / `;` / `&` / `|` / newlines) BEFORE it is ever single-quoted into
  the append/remove shell command. `TestDoorKeyRe` pins the rejects.
- **`freehold door authorize|revoke`** — the box derives its door key
  **deterministically** from its agent-ops identity `nostr_secret` seed — the
  SAME key that signs its world API calls (DOOR_SPEC §3) —
  (`crypto.SSHPublicKeyFromSeed`: the private half never leaves the box, only
  the public line is presented, and the key is stable across re-logins so a
  revoke actually removes it) and calls the world tool signed as its ops
  identity. Revoke is exact-line removal with a real error on a real failure
  (no masked `|| true` — a false "revoked" for the lost/compromised-box lever
  would be a silent security lie).

### Notes

- The append is idempotent (grep-before-append) and scoped to the caller's
  presented pubkey; revocation is exact-line removal. Only operators (NIP-98 →
  admin whitelist → roster) can authorize a door.

## [0.5.5] — Phase 3: the deploy ships the Go console (runtime is Go end to end)

The REFACTOR-PLAN's "Rust only where it earns its keep" now holds for the
mechanism's runtime: the CP console that `deploy-cp` ships and the box-side CP
CLI verbs are Go. The Rust console crate survives only as the `acceptance`
harness's hermetic fixture (the crate deletion is the final Phase-3 step, tied
to porting that gate to Go).

### Changed

- **`deploy-cp` ships `freehold-console` (Go) instead of the Rust
  `control-plane` binary.** `DeployCp` serves it with the Go flag shape
  (`--admin-pubkeys`, `--public-origin`, `--relay-*`, `--agent-tools-*`),
  reads the console identity back via `freehold-console identity`, and
  adopts/grants the co-located runner via `freehold-console adopt`/`grant`.
- **The box's own CP CLI verbs are Go.** `freehold-console` now carries
  `provision` (incl. the ssh-door keypair generation that prints the public
  line for `authorized_keys`), `grant` (defaulting to the console identity —
  the first-run grant needs no argument), `adopt`, `add-secret`, `identity`,
  and `serve`. `resolveRebuildBins` resolves `freehold-console` (was the Rust
  `control-plane`) as the console sibling; `stageProvision`/`stageGrant`/
  `sealRunnerSecret`/`deploy_agent_tools` use it.
- **`contract/crypto.GenerateSSHKeypair`** is exported (the byte-exact
  openssh-key-v1 generator) for the ssh-door provision path.
- The interspersed-flags parser (`provision <name> --flag …`) matches the box's
  call shape (Go's `flag` stops at the first positional).

### Notes

- The Rust `control-plane/console` + `console-client` crates are still in the
  tree as the `acceptance` gate's dependency; the deploy no longer ships them.
  Deleting them requires porting `freehold-acceptance`'s console-dependent
  checks to the Go provisioner/console — the tracked final Phase-3 step.

## [0.5.4] — Phase 3 core: the Go console server (web.rs ported at parity)

The big Phase 3 piece: the Rust console's loopback admin/ops web surface
(`control-plane/console/src/web.rs`, ~2.1k lines) is ported to Go with the
SAME routes and the SAME security guards. The Rust console crate still ships
until the deploy switch (the next PR); this PR lands the Go replacement,
hermetically tested.

### Added

- **`control-plane/api/console/`** — the Go console server:
  - `auth.go` — the NIP-98 operator auth (challenge/session/portal) with the
    web.rs guard constants (60s freshness, 120s challenge, 24h sliding session,
    60s single-use portal), `HttpOnly; SameSite=Strict` cookies, the
    DNS-rebinding `Origin` guard (loopback + the configured public origin
    only), and the loopback-until-authn bind guard.
  - `server.go` — every `/api/*` route at parity: auth challenge/login/portal,
    overview (with the console-signed LIVE readiness probe — the runner still
    fails closed; a `-32001` denial reads as "console not granted"),
    world (the fresh-box seed), teardown, provision/rotate/revoke/grant/
    revoke-grant/runner-addr (each re-syncing the runner's relay channel when a
    relay scope is set), DNS list/upsert/remove (with the dnsmasq addn-hosts
    render + reload), the runner channel view (relay roster/profile/messages),
    and agents list/register/remove (with kind-9 presence probes).
  - `dns.go` — the internal resolver renderer (port of dns.rs): validate_name/
    ip, render_addn_hosts, upsert/remove, dnsmasq conf + sync + the
    dnsmasq-readable permissions sweep.
  - `index.html` — the single self-contained admin page, extracted verbatim
    from the Rust source.
- **`control-plane/api/cmd/freehold-console`** — the Go serve binary (port of
  the Rust `serve`): the bind guard (loopback-only until an admin whitelist is
  seeded), the admin/relay/agent-tools scope flags persisted to state.json, and
  first-serve console identity minting (a keypair is NEVER shipped — it is
  born on the box, 0600).
- **`contract/state`** extended to full `ControlPlaneState` parity: `dns`,
  `resolver_domain`, `resolver_wildcard`, `agent_tools_url/pubkey` + accessors.
- **`secret-management` extended to full provisioner parity** (port of
  provisioner.rs): `RotateSecret` (B2), `RevokeRunner` (B3), `GrantAgent`/
  `RevokeGrant` (D2 shipped-package), `AdoptRunner`, `AddSecret`, and the
  relay channel sync (`SyncRunnerChannel`, `PutUserMembership`,
  `RemoveUserMembership`, `RevokeRunnerChannel`) through the absorbed
  console-owner credential (`cpstate.ConsoleSecret`).

### Tests

`TestAuthChallengeSessionPortal`, `TestCheckOrigin`, `TestLoginRoundtrip`
(+ rejects: stale timestamp, non-admin), `TestPortalSingleUse`,
`TestWorldRequiresAuthWhenConfigured`, `TestOverviewListsRunners`,
`TestLoopbackBindGuard` — the security-guard core is pinned hermetically.

### Notes

- The Rust console crate is NOT deleted yet: the deploy switch (ship
  `freehold-console` instead of the Rust `control-plane` binary, switch
  `resolveRebuildBins`, delete the crate + `console-client`) is the next PR —
  the Go console lands tested first, so the live bring-up swaps to a proven
  surface.

## [0.5.3] — Phase 3 (part 1): `freehold-orchestrator` folds into `freehold`

REFACTOR-PLAN §3.5/§8 Phase 3's binary fold. The `freehold-orchestrator`
binary was vestigial: the `freehold` binary already routes every subcommand to
the same cobra tree (`cli.Run`), and the rebuild/teardown engines re-exec the
CURRENT binary (`os.Executable()` — `OrchestratorBin: self`), never
`freehold-orchestrator` by name. `resolveRebuildBins` resolves the
runner/control-plane/agent-tools siblings, not this binary.

### Removed

- **`control-plane/cli/cmd/freehold-orchestrator/` (the binary).** One binary,
  two surfaces: `freehold` with no args = the TUI, `freehold <subcmd>` = the
  CLI. The sibling-resolution contract is preserved (teardown re-execs the
  running binary). The cobra root's `Use` is now `freehold` (was
  `freehold-orchestrator`).

### Notes

- The Rust console port + `console-client` deletion remain Phase 3's core
  work; this PR ships the fold so the build/docs stop promising a second
  binary.

## [0.5.2] — Phase 2 (part 1): the CLI is an API client — the box-local mirror is gone

REFACTOR-PLAN Phase 2's first slice: the box stops holding a second, local
picture of the world. The TUI's Runners view read either the console
`/api/overview` (CP) or the box-local `~/.freehold/control-plane/state.json`
mirror, toggled with `s`. The mirror is a stale parallel reality the plan
deletes.

### Removed

- **The `s` Runners-source toggle + the box-local `state.json` mirror.** The
  Runners view now reads the console `/api/overview` only (the CP's
  authoritative runner list); `readLocalRunners`, `RunnerSourceLocal`,
  `runnerSourceLabel`, and the `s` keybinding are deleted. The `contract/state`
  import is gone from the TUI. World ops are API calls; the box holds no local
  world-state mirror.

### Added

- **`docs/DOOR_SPEC.md` — the login-authorized door security spec** (the §9
  design gate that gates Phase 2's door work). The spec names what the CP
  authorizes (the box's own agent-ops public key), how it proves the box is a
  logged-in operator (NIP-98 session → admin whitelist, appended through the
  CP's co-located runner), scoping (idempotent, caller-presented pubkey only),
  and revocation (`world_revoke_door` removes the exact line). The door
  mechanism is not implemented yet — the spec is the reviewable gate.

### Notes

- The CP-lifecycle side of Phase 2 (`bootstrap-cp`/`teardown-cp` on the box's
  door, the slim build) stays behind the §9 gate until the door spec is
  reviewed; this PR ships the mirror deletion + the spec.

## [0.5.1] — Phase 1: the unified scoped API (the one inventory, scope-gated, grant wired)

The REFACTOR-PLAN's Phase 1 behavior change lands on the 0.5.0 tree: the
agent-tools server becomes the unified api/ front — scope-gated by caller
class, with `world_status` as the single inventory read, the agent-registry
reconcile, and `grant_agent` finally wired through the console-owner
credential.

### Added

- **Server-side scope auth (per-channel tool visibility).** `agenttools.Server`
  now classifies each caller: a pubkey in the CP's agent registry is an AGENT
  (create/manage only), a roster member not in the registry is an
  OPERATOR (full toolset incl. world_* and `grant_agent`). The world_* actions
  AND `grant_agent` are denied to agents with a distinct `-32003` — so the CPA's
  "conversation + create only" boundary, previously only enforced by its stdio
  bridge's client-side filter, is now enforced on the server and cannot be
  bypassed by calling the server directly. `grant_agent` is operator-scoped
  because a grant hands direct exec access to a runner's MCP surface (a
  prompt-reachable agent binding an arbitrary pubkey onto the CP's own
  co-located runner would bypass this very boundary). `TestServerScopeAuth`
  pins it.
- **`world_status` is the single inventory read.** It now returns agents + the
  console's runners/DNS read underneath (the console's state.json on the box —
  what `/api/overview` + `/api/dns` serve), via the new `control-plane/api/cpstate`
  package. The TUI's Agents view and the new `freehold world status` verb
  consume it. `TestWorldStatusInventory` pins the shape.
- **`freehold world <status|build|teardown|migrate>` CLI verb.** The box's
  post-login world surface: every world op goes through the CP's api/ over MCP,
  signed as the box's ops identity — no local runner/door needed (world ops are
  API calls; the CP drives the world through its co-located runner).
- **`grant_agent` is wired.** The AGENTS.md known-gap entry is closed: the
  server loads the console's own identity (its channel-owner credential) from
  the console's state dir (`/srv/data/cp/control-plane/console`, 0600 durable)
  and publishes the kind-9000 put-user to the runner's channel in-process — the
  runner re-reads its signed 39002 roster per call, so the grant lands without
  a restart. Missing wiring fails closed, never silently succeeds
  (`TestRegistryGrantFailClosed`).
- **Agent-registry reconcile.** Migration `002-import-console-agents` folds the
  console state.json `agents` map into the authoritative `registry.json`,
  ADDITIVE-ONLY (a name already in the registry keeps its current row — a stale
  console pubkey never clobbers a current one), verify-gated (postcondition:
  every console agent is in the registry) — the two-registry divergence from the
  console era converges on one source. `TestMigrateImportConsoleAgentsAdditiveOnly`
  proves an existing row survives.

### Changed

- The toolset's `serve` takes `--console-state-dir` (default
  `/srv/data/cp/control-plane`) so it can front the console's store + identity.
- The TUI Agents view reads `world_status` (the single inventory) instead of
  `manage_agent` directly.

### Notes

- The Rust console is NOT folded here — it stays the `/api/*` surface this
  phase (the api/ server reads its store, it does not serve its routes); the
  full port + crate deletion is Phase 3.
- Docs updated to current state (AGENTS.md known-gap entry closed,
  ARCHITECTURE.md toolset section).

## [0.5.0] — the three-module tree (REFACTOR-PLAN Phase 0: contract / control-plane / platform)

The REFACTOR-PLAN's Phase 0 lands: the repo is restructured around what the
system actually is — **a control plane that is the stable mechanism** and **a
platform of services/agents it installs and evolves**. The `orchestrator/`
name is gone; the operator surface is now three Go modules with one-way
dependency edges.

### Changed

- **The tree is three Go modules.** `contract/` (`freehold/contract`) is the
  shared wire/trust leaf BOTH the mechanism and the platform import;
  `control-plane/` (`freehold/control-plane`) is the stable mechanism (api,
  cli, secret-management, plus the Rust runner/core/console crates);
  `platform/` (`freehold/platform`) is the evolving world (services,
  provisioning, migrations, agents, terraform). The contract being its own
  module is what keeps the edge acyclic (`platform → contract ←
  control-plane`, never `platform → control-plane`), so `platform/provisioning`
  and `platform/services` can import the signed-runner client without a cycle
  (REFACTOR-PLAN §4 G4′).
- **`orchestrator/` is deleted as a name** — packages, paths, binaries, and
  prose. `internal/{crypto,wire,client,config,console,relay,state,delegate}`
  → `contract/`; `internal/{bootstrap,planebase,drive,stages,migrations,dnsman,
  cert}` → `platform/`; `internal/{cli,tui,flows,oplogin,teardown,agent,
  agenttools,provisioner}` + `cmd/*` → `control-plane/`. The `freehold` /
  `freehold-orchestrator` binaries keep their names (the sibling-resolution
  contract — engines resolve binaries relative to `os.Executable()` — is
  preserved); `freehold-agent-tools` moves to `control-plane/api/cmd/` and its
  static `CGO_ENABLED=0` build contract is unchanged.
- **`internal/deploy` is split to its target homes.** `deploy_relay.go` →
  `platform/services/relay/buzz/`, `caddy.go` →
  `platform/services/webproxy/caddy/`, `deploy_cp.go` + `deploy_agent_tools.go`
  → `control-plane/cli/bootstrap-cp/` (the day-0 mechanism install), and the
  generic runner-exec helpers (`LxcCmd`, `SafeDeployDir`, `CheckDocker`) →
  `platform/provisioning/deploy/` (exported, since the split moved them to a
  shared package both sides import).
- **The CPA prompt moves to its platform home.** `orchestrator/prompts/
  CPA_SYSTEM_PROMPT.md` → `platform/agents/freehold/prompt.md`, embedded by
  the new `platform/agents` Go package (a Go package cannot `//go:embed`
  outside its own module, so `control-plane` imports the value, never
  re-embeds — REFACTOR-PLAN §4 seam 1). The pod mount path
  (`/srv/freehold/CPA_SYSTEM_PROMPT.md`) is unchanged.
- **The harness byte-gate moves with the contract it verifies.**
  `orchestrator/harness/` → `control-plane/core/harness/` (Go test-only
  package in `freehold/control-plane`, gating `contract/crypto` against the
  Rust `freehold-harness-oracle`); the oracle crate nests at
  `control-plane/core/harness/oracle/`.
- **The Rust crates fold under `control-plane/`.** `core/`, `runner/`,
  `console-client/`, `testkit/`, `acceptance/`, and the `control-plane`
  console crate (→ `control-plane/console/`) move; the Cargo workspace member
  list and the two changed path deps (acceptance→console, oracle→core) are
  updated. The console crate lives at `control-plane/console/` until Phase 3
  ports its routes into the Go `api/` and deletes it.
- **`terraform/` → `platform/terraform/`.** `migrate-go.md` and
  `orchestrator/WIRE.md` are deleted (superseded; the plan's truth now lives
  in the tree's current docs).
- **CI runs the three modules.** `ci.yml` builds/vets/tests each module; the
  cargo gates are unchanged.
- **Docs updated to current state** (AGENTS.md, ARCHITECTURE.md, README.md,
  roadmap, docs/phase-0.12-plane-gate.md) — the docs-hygiene rule applied, so
  the old paths are gone from current-state prose. `REFACTOR-PLAN.md` stays as
  the plan document for Phases 1–3.

### Notes

- The functional behavior is unchanged by this phase — it is the mechanical
  re-org so every later change (Phases 1–3, Chunk 5) lands in its true home.
  All Rust + Go gates run green; `freehold-acceptance` and the harness byte-gate
  pass at the move boundary.
- `RUNTIME_CONTRACT.md` (referenced by ARCHITECTURE.md for the `RunnerCall`
  schema) is a pre-existing dangling reference, carried forward unchanged.

## [0.4.9] — login stops asking for the CP pubkey (the operator key is the credential)

### Changed

- **`freehold login` no longer prompts for a CP pubkey.** The operator key
  (`nsec`) is the only credential: the console only admits NIP-98 operators
  whose pubkey was minted into its admin whitelist at deploy (`--operator-pubkey`
  → `web.rs is_admin`), so a legitimate login to the actual CP needs no
  separately-known CP pubkey. The recorded `cp_pubkey` is therefore the CP's
  *own* identity, adopted from its `/api/world` self-report and normalized to
  64-hex (`resolveCPPubkey(world)`), never typed by the operator. (It is an
  informational anchor — the trust boundary for a wrong-or-hijacked `cp_url` is
  TLS/DNS on that URL, not this recorded field.)
- **The cross-check footgun is removed.** Previously login prompted "CP pubkey
  (64-hex or npub1…)" and compared the operator-typed value against the CP's
  report; operators plausibly pasted their OWN operator pubkey there and hit a
  hard "CP pubkey mismatch" abort — which is correct only if a competing CP
  identity is expected, but wrong for the operator's own key, and invited
  exactly the confusion it was meant to prevent. No operator-typed value
  can disagree with the anchor now: the CP's self-report is authoritative, and a
  stale/wrong `cp_pubkey` in the config is overwritten by that report rather
  than allowed to hard-fail login.
- `config.CpPubkey`'s meaning is now recorded purely as the CP's own identity
  for future signed-CP calls (an informational trust anchor), not a user-input
  validation gate. README / ARCHITECTURE / POC_CHUNK4.G1 updated accordingly.

## [0.4.8] — Chunk 4 Phase G wrap: a fresh box truly operates the CP (login-only Running + world coords)

Phase G's `login` foundation shipped the *mechanics* but left two gaps that
blocked the actual "fresh box operates the world" claim: the deployed CP
rarely carried its relay coords (so `/api/world` seeded no relay), and a
login-only box (cp_url + cp_pubkey + operator identity, no local `[runner]`)
could never reach Running because the mode gate demanded a local runner's
reachability. This phase closes both, plus the agent-tools coords a fresh box
needs for the Agents view.

### Fixed

- **The deployed CP now serves its relay coords.** `deploy-cp` only passed
  `--relay-url … --relay-pubkey` to `control-plane serve` when a relay pubkey
  was *supplied*, and the box never supplied one (NIP-11 best-effort is often
  empty) — so the CP started with no relay scope at all and `/api/world`
  returned `relay_url: null`, leaving a fresh login box with nothing to seed.
  `stageDeployCp` now resolves the relay signing pubkey **deterministically**
  from the relay's own compose `.env` (`BUZZ_RELAY_PRIVATE_KEY`, read in-guest
  through the runner — the secret never leaves the relay, only the derived
  pubkey is returned), falling back to NIP-11 then the recorded config. A
  rebuild's surviving `agent_tools_*` coords are re-passed too.
- **`--relay-url` alone no longer refuses to serve.** The CP's `serve` treated
  `--relay-url`/`--relay-pubkey` as an inseparable pair and *refused to start*
  with only one. The pairing is now a soft guard: URL alone records (so
  `/api/world` seeds a relay even before the pubkey is known); the verified
  roster view stays disabled until both land.
- **A login-only box reaches Running.** The boot gate
  `Converged = RelayLive && CPLive && RunnerReach` demanded a LOCAL
  provisioning runner, which a login-only box deliberately has none of (and
  `CPLive` probed *through* it via `pct exec`). The runner probe is now
  satisfied on a runnerless profile ("no local runner — operating through the
  CP"), and `CPLive` sources from the console session (`/api/overview`) with a
  bare-host reachability fallback — no `pct exec` needed. A fresh
  `freehold login` box enters Running immediately and operates the CP console
  (Runners-CP, provision/grant/revoke, portal).
- **`/api/world` carries the agent-tools coords.** A fresh box's Agents view
  (`buildAgents`) needed `agent_tools_url`/`agent_tools_pubkey`, which login
  never seeded and `/api/world` never served. `control-plane serve` now accepts
  `--agent-tools-url`/`--agent-tools-pubkey` (persisted in `state.json`),
  `/api/world` serves them, `console.WorldSummary` + `oplogin.seed()` record
  them, and the box feeds them at deploy: `stageDeployCp` re-passes surviving
  coords on rebuild, and `stageDeployAgentTools` re-runs `deploy-cp` (idempotent
  — stops the prior serve, restarts with the new flags) on the fresh-build path
  where agent-tools was deployed after the CP.

### Tests

- `control-plane/tests/web.rs`'s `world_serves_operator_seed_after_login` now
  asserts the agent-tools coords round-trip through `/api/world`.
- `internal/oplogin`'s interactive-seed test asserts the mock CP's
  `agent_tools_url`/`agent_tools_pubkey` land in the seeded config.
- New `internal/tui/tui_login_only_test.go`: the runnerless runner-probe branch
  and `cpConsoleLive`'s reachability fallback (live up / dead down).

### Docs

- `ARCHITECTURE.md`, `AGENTS.md`, and `roadmap/POC_CHUNK4.md` updated to the
  current reality: the login-only Running path and the CP's served world coords
  (see the docs-hygiene rule — current-state only, history lives here).

### Handoff

- The **live CP** must be re-deployed once with the fixed code path (a normal
  `freehold build`/rebuild, or a manual `deploy-cp` with
  `--relay-url --relay-pubkey`) for `/api/world` to serve the relay coords —
  the code change makes a rebuild produce it automatically. The **other box**
  takes the fixed `freehold` binary (TUI mode gate + login seed) and re-runs
  `freehold login` — it now reaches Running and operates the CP without any
  local runner.

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
- **`freehold teardown` is CP-first.** A whole-world teardown first asks the CP
  (via `POST /api/teardown` on the console) to remove what IT manages — runners
  + sealed secrets, the agent registry, DNS records — before the box destroys
  the CP LXC, so the world unwinds gracefully instead of the substrate dying
  under managed state. Best-effort: if the CP is down or this box lacks the
  operator session, teardown warns and still runs (it is the last resource
  standing). Server endpoint + Go client + box wiring are hermetic-tested.
- **A CP world-action surface on the agent-tools toolset.** Roster-gated `world_status`
  (what the CP manages — its agent registry), `world_teardown` (clears it), and
  `world_build` (the CP runs its owned bring-up/reconcile stages through the co-located
  runner via the shared `internal/stages`), giving the box a post-login "trigger" for
  the world. `world_build` boots the relay + k3s LXCs (baking the durable-plane
  mounts at create), deploys the relay stack + installs k3s, re-asserts the k3s
  durable local-path, re-applies the Caddy TLS edge (edge coords ride the
  agent-tools serve `--relay-host/--relay-ip/--cp-ip`), registers + points the
  CP-owned split-horizon resolver (`--cp-lxc/--proxy-ip/--litellm-ip` carry the
  coords), re-ensures the durable volume plane (`--plane-pool/--plane-kind/
  --thin-pool`), brings the litellm gateway up CP-side (kube workloads + model
  registration through the co-located runner — the operator's provider key
  rides the sealed co-located runner package, never argv), and resolves the edge
  certs CP-side (durable-reuse gate — no LE order when the durable mirror has a
  valid cert — else an in-process resumable DNS-01 issue, sealed cred on the
  CP, install through the co-located runner); the box `build` is now
  CP-bring-up + trigger: door → CP LXC → boot + deploy the relay stack (the
  agent-tools roster lives on it) → deploy CP + agent-tools → hand-off
  (DNS creds sealed to the agent-tools identity, litellm secrets sealed into
  the shipped runner; the relay LAN dial + canonical NIP-98 auth URL let the
  seed/roster work pre-Caddy) → trigger world_build → record coords → CPA
  (`--full` keeps the old box-side pipeline reachable as a fallback). A full
  teardown + slim `freehold build` reconverges the whole world with the CP
  doing the bring-up (fresh-world coords are resolved by hostname + the relay
  host is re-pinned into the CP guest pre-Caddy). The retired `--full`
  box-side pipeline + the now-dead box stages (`run()` / `stageK3s` /
  `stageDnsRegister`/`Point` / `stageCaddy` / `stageLitellm` / the box cert
  start/await) are deleted — the slim path is the only build, and the relay
  client's `ReadMemory` presents the bare-domain Host. The CPA
  harness is
  deliberately NOT given the mutating world tools (locked "conversation + create only").
- **A shared stage library (`internal/stages`).** The pure world-bring-up command
  builders (k3s install/local-path scripts, litellm + Caddy kube manifests, the
  cert-install script, DNS/LXC coord + secret-mint helpers) are extracted from
  `internal/cli/rebuild.go` into a dependency-light `internal/stages` — net-zero
  wire for the box, and the foundation for the CP `world_build` executor to run
  the SAME stage bytes through its co-located runner. The stage builders keep the
  secret discipline (provider/cert keys never in argv — they ride the runner
  package and are injected by name); single-quote-free invariant is tested.
- **A verify-gated migration runner on the CP (`internal/migrations`).** The
  Omarchy-style model for versioned config/prompt/repair changes, but with the
  community critique addressed: completion is recorded against an explicit
  postcondition, not a bare "ran" marker. The durable ledger (`/srv/data/cp/
  migrations.json`) records `{name, status, verify_at, last_error}`; a migration
  is DONE only when `Apply` succeeds AND `Verify` confirms convergence (🟢) —
  an apply-that-fails-verify is left pending and retried (idempotent). Exposed as
  the roster-gated `world_migrate` tool on the toolset; the first registered
  migration asserts the agent-tools registry is a usable store. The live-world
  CPA-prompt/config migrations (which need the deployed pods) ride this same
  runner.

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

