# Plan — install access modes → normalized CP lifecycle

Status: the provider/transient work is **merged** — PR1 install surface (`0.6.9`),
PR2 teardown/uninstall (`0.6.10`), PR3 provider seam (`0.6.11`), PR4 transient
access (`0.6.12`), plus re-adopt substrate-key rotation (`0.6.13`) and the
substrate-key match fix (`0.6.14`). **PR 5 (the local/server split, `0.6.15`) is
implemented and bot-approved on PR #258** (branch `feat/local-server-split`):
5a/5b/5c landed, and 5d's command consolidation into `freehold-cli/internal/stages/`
landed. Still open on PR 5: the per-verb package reorg (5d remainder) and the
live integration gate — see §11. Then `vultr` (PR 6). `install --restore` is out
of scope for now (see "Deferred"), but the identity model below guarantees grants
survive a restore when it lands.

Baseline: `main` after `0.6.14`; PR 5 branches from it.

The storage-scope work (#243/#244) and the provider/transient phases are merged;
the remaining work is the **local/server split** (implemented, pending merge) and
provider breadth.

**For the implementing agent:** this plan states the goal, the locked decisions, and
the guardrails; it deliberately leaves internal structure, package layout, and naming
to you. Where a detail is unspecified, pick the smallest change that satisfies the
guardrails + acceptance criteria and record the choice in the PR. If something would
contradict a locked decision, surface it rather than working around it.

---

## 1. Goal

Land a control plane with **transient privileged access** (root SSH / provider API),
then make the CP's co-located runner the only durable "hands". After install, every
box talks to the CP over signed HTTPS; installers differ only in **access mode**
(`ssh-root-proxmox`, later `api-vultr`), and all end at the same normalized CP.

Key property: the CP runner's **Nostr + encryption identity is permanent**. Grants,
relay channel/roster, and sealed secrets survive every re-install. Only the
**substrate SSH credential (the door) rotates**.

Second property: **`platform/` is provider-independent.** Every substrate-specific
command (Proxmox `pct`/LVM/ZFS, later Vultr/Hetzner APIs) lives behind a `Provider`
in a top-level `providers/` module; `install`/`uninstall`/`build`/`teardown` are
orchestrators over it. See §5.

## 2. Locked decisions

1. **One install surface.** Fold `freehold-install bootstrap` into `install`
   (`--yes` for non-interactive). `install` fails if a live CP exists.
2. **Install takes `--host`** (also enterable in the TUI). It writes host + access
   mode into the profile config so `uninstall --name` can resolve the host without
   a `--host` flag. Re-install after a wipe re-supplies `--host`.
3. **Runner identity is preserved.** Never re-mint the Nostr/enc keypair on
   re-adopt. Rotate **only** the substrate SSH credential.
4. **`teardown` = inverse of `build`, CP preserved.** Removes world + **internal
   DNS**; keeps the base CP, the durable plane, Cloudflare records, and the cert
   mirror.
5. **`uninstall` new verb.** Removes the CP + world + the **invoking box's** door
   and the **runner substrate** key; it does **not** remove other boxes' doors —
   it warns and lists any that remain. It wipes the local config/state; **keeps
   data by default**. `--remove-data` adds plane removal (opt-in now; a future
   default flip is recorded as a code comment at the flag, not as
   future-narration in docs).
6. **Install creates both host doors** — the existing deterministic **DOOR_SPEC
   operator door** (derived from the box's agent-ops seed; `door.go`) and the
   **runner substrate** key. It **reuses DOOR_SPEC**; it does **not** generate a
   new operator keypair. `login` refreshes the operator door.
7. **DNS:** Cloudflare is **upserted** (already); teardown removes only internal DNS.
   Certs stay (domain-scoped, reused from the durable mirror).
8. **Coords resolve from the CP/plane** on re-install; the local config is not the
   source of truth.
9. **`world teardown` is a pure alias** of the new CP-preserving `teardown`; only
   `uninstall` drops the CP.
10. **Thin-box `uninstall --remove-data` is refused** until the data path is
    sequenced (same known gap as `teardown --data`).
11. **The install box is thin after handoff.** PR4 removes the box's transient
    world-home runner package once the CP owns the identity and the doors are in
    place; box-side `exec` already rides the CP's `world_exec`
    (`noLocalRunner()`, `control-plane/cli/handlers.go:112`). Keeping the package
    is a harmless fallback, not the target state.

## 3. Current state (post PR4 + fixes)

Landed: one install surface + life-cycle gate + identity-preserving adopt (PR1);
teardown CP-preserving + `world teardown` alias + `uninstall` + the single
`confirmDestructive` gate (PR2); `platform/` provider-independent with a top-level
`providers/` module and the two guards (PR3); direct root-SSH transient access +
host-side fail-if-live + transient uninstall + the secret-provenance guard (PR4);
re-adopt substrate-key rotation (`0.6.13`) and the substrate-key match fix
(`0.6.14`).

**Remaining: the local/server split** (§6), then `vultr`. The CLI tree still lives
inside `control-plane/` and the two sides import each other in a handful of places:

- **server → local:** `api/agent/agentpod.go`, `api/cmd/freehold-agent-tools/main.go`,
  and `api/cpbuild/cpbuild.go` import `cli/flows`; `cpbuild` also imports `cli/teardown`.
- **local → server:** `cli/flows` + `cli/helpers3.go` → `secret-management`;
  `cli/rebuild.go` → `api/agent` + `api/agenttools`; `cli/tui/*` → `api/agenttools`.

No other module imports `control-plane/cli`.

## 4. Target command surface

```
freehold install [--name] [--host] [--yes]     # fail if a live CP exists
                 [--relay-domain] [--cp-domain] [--proxy-ip]   # fresh plane only
freehold build                                  # world bring-up via CP (unchanged)
freehold teardown                                # inverse of build; CP stays
freehold uninstall [--remove-data]               # CP + doors removed; data kept by default
freehold login | world | exec | profiles | …     # unchanged
```

Until **PR 5**, install lives on the separate `freehold-install` binary; PR 5 folds
it into `freehold install` and drops the binary (the hidden `bootstrap` alias stays
as `install --yes`). **`world teardown` is a pure alias** of the CP-preserving
`teardown`.

### Semantics

- **install (fresh plane):** requires `--name --host --operator-pubkey
  --relay-domain --cp-domain --proxy-ip`. There is **no base domain** — the relay/CP
  hosts and proxy IP are **not derivable from `--name`**; interactive/TUI prompts
  collect them. A fresh plane has nothing to resolve from.
- **install (re-adopt):** relay/CP hosts, proxy IP, and mounts **resolve from the
  surviving CP state/plane**, so those flags become optional; only `--name` +
  `--host` remain required (the host cannot be read without host access). If the
  CP guest is **live on the host** (the provider's guest list — works with or without
  a profile) → fail with guidance (`build` to bring up the world, `teardown` to drop
  the world, `uninstall` to drop the CP; a fresh box means `login`). Otherwise boot
  the CP and `deploy-cp`, which **adopts the plane's existing runner** when present
  (never overwriting `identity.json`), else mints.
- **doors:** install authorizes the box's existing deterministic **DOOR_SPEC**
  operator door (not a new keypair) and ensures the **runner substrate** key is
  present.
- **door rotation (install, same-host only):** generate a new substrate SSH keypair
  → authorize the new pubkey on the host via transient access →
  `RotateSecret(<target>, newPriv)` → restart the runner → remove the old
  `authorized_keys` entry. Repointing a target to a **new host is out of scope**
  (`RotateSecret` cannot change `Address`) — that is the open piece for
  `install --restore` (see Deferred).
- **teardown:** through the CP, remove the **world** — relay LXC/stack, k3s +
  workloads, and **internal DNS** (`DnsSpec.Records` + CP resolver records) —
  and **stop** the CP-side `freehold-agent-tools` process. agent-tools is not
  world-owned: it runs **inside the cp guest** with durable state on the **CP
  plane** (`/srv/data/cp/agent-tools`, `cpbuild.go:549`), and its only world
  coupling is dialing the relay + seeding relay membership. Teardown stops the
  process (its world work is dead with the relay) and leaves that durable state —
  identity, roster seed, and runner grant survive; build step 2.5 re-launches it
  idempotently (it kills any prior serve first, `cpbuild.go:558`). Do **not** wipe
  `/srv/data/cp/agent-tools` (a re-mint would churn the relay roster and stale
  every box's recorded `AgentToolsPubkey`). Leave the CP and its co-located
  runner, the plane, Cloudflare records, and the cert mirror.
- **uninstall:** transient access; remove the CP + world; remove the **invoking
  box's** door and the **runner substrate** key (leave and list other boxes' doors);
  wipe local config/state; keep data. `--remove-data` also removes the plane.
- **DNS:** build upserts A records (existing IP changed → update). Teardown clears
  internal DNS only.
- **first access after uninstall:** out-of-band — Proxmox: PVE console/root SSH (or
  another still-authorized door); Vultr: the provider API.

## 5. Provider boundary (architecture)

**Goal:** `platform/` is provider-independent; every substrate-specific command lives
behind a `Provider`; `install`/`uninstall`/`build`/`teardown` orchestrate it.

Module graph (a DAG — no cycles):

```
contract/     wire/trust leaf (crypto, wire, client, config, console, relay, state)
   ↑          UNCHANGED; do not move it under platform/
platform/     Provider interface + box engine + planebase (pure) + generic helpers
   ↑          MUST NOT import providers/
providers/    top-level module: concrete providers (compute + storage together)
   ↑          imports platform + contract
install/  control-plane/   composition roots: pick the provider from the access mode,
                           inject it into the engine, call specific ops
```

- **The `Provider` interface lives in `platform/`** (`platform/provisioning`). It is a
  provisioning-domain abstraction whose value types (`GuestSpec`, `Mount`, `planebase`
  types) also live there. `contract/` is the wire/trust leaf and sits *below* platform
  today (`platform → contract`); it is **not** the provider contract and stays put.
- **A provider is substrate ops, not a lifecycle.** There is no `provider.Install()`.
  Methods are low-level: host exec, guest exec, create/destroy/list guest, and storage
  ops. `install`/`uninstall`/`build`/`teardown` decide the sequence.
- **Compute + storage come together under one provider.** `platform/provisioning/drive`
  (LVM/ZFS/`pvesm`) moves to `providers/proxmox/`, as do the provider-command helpers
  (`deploy.LxcCmd` = `pct exec`, the pct stage scripts). The generic half of
  `bootstrap/` (`Exec`, `ExpectOK`, `PlainPath`, `ParseMount`) stays in `platform`.
- **Future shape (design for it, do not build it now).** A provider may later expose
  sub-options behind one config — e.g. `providers/aws/provider.go` with ebs (storage) +
  ecs/eks (compute). Keep the interface small and the provider constructor open to
  options; don't add option machinery until a second option exists.
- **Naming.** "provider" now means the **compute/storage substrate**. Qualify the two
  existing senses so they don't collide: the **DNS provider** (cloudflare, `dnsman`)
  and the **service/secret kind** (`provisioner.go:50`). Rename identifiers only where
  it removes real ambiguity.
- **No registry/init magic.** The composition root constructs the concrete provider and
  injects it. This is what keeps `platform` free of provider imports.

**Guardrails (must hold every step):**
- Nothing under `platform/` imports `freehold/providers` (add a test/CI grep guard).
- No provider-specific type or string (`pct`, `pvesm`, `zfs`, cloud API structs) in a
  `platform` interface signature.
- The extraction PR is **behavior-preserving**: same commands, same order, existing
  tests pass unchanged apart from moved packages.

## 6. Local/server split (architecture) — the next phase

**Goal:** two apps with **zero cross-imports**. `control-plane/` is the server + the
engines that run against the CP; `freehold-cli/` is the local operator surface; the
shared layer is a **thin protocol/format leaf** both may import.

Rule (locked):
- `control-plane/` **must never import** `freehold-cli/`.
- `freehold-cli/` **must never import** `control-plane/` — local drives the server
  through the CP API (console HTTP / agent-tools MCP) or by invoking its binaries,
  never by linking its packages.
- Anything both sides genuinely need moves **down** into the shared leaf
  (`contract/`).

Module graph (no edge between `control-plane/` and `freehold-cli/`):

```
contract/        THIN leaf: client, config, console, crypto, wire   (only)
   ↑
platform/ providers/            (as in §5)
   ↑
control-plane/   SERVER + engines: api/cpbuild (build) + api/*, secret-management,
                 teardown (beside build), state/, relay/, delegate/
freehold-cli/    LOCAL: login/profiles/TUI, install/uninstall (transient provider),
                 thin build/teardown triggers → CP, world/exec → CP MCP
```

Decisions:

- **`teardown` sits with `build`** (server-side, beside `cpbuild`); local `build` and
  `teardown` become **thin CP triggers** (`/api/world-build`, `/api/world-teardown`).
- **`flows` dissolves:** the identity loader → `contract/`; the operator helpers
  (`demo`/`readiness`/`exec`) → `freehold-cli`; the server uses the contract loader.
- **`onboard` is removed.** Service-runner provisioning is a CP operation
  (`freehold-console provision` + grants); `onboard` ran the CP provisioner
  in-process on the local box. It is **not** in the DNS/cert path — Cloudflare creds
  flow `dns-cred` (local, sealed) → CP `world-secrets` → the CP's `worldCert` /
  `manageDomainDNS` — so removing it is a no-op for DNS.
- **Shrink `contract/`.** `state`/`relay`/`delegate` are server-heavy and move into
  `control-plane/`; the leaf keeps only `client`/`config`/`console`/`crypto`/`wire`.
  The `contract/` → `shared/` rename was considered and **dropped** — the functional
  goal is met by the moves; the name stays.
- **`install/` dissolves into `freehold-cli/`** (`cpdeploy` comes with it; it is
  already server-free); `freehold-install` folds into `freehold install` (drop the
  binary, or keep a one-release shim).

**Build/teardown split (precise)** — the split's one behavior change. Today `runBuild`
(`control-plane/cli/rebuild.go:864`) does pre-work and owner-only bookkeeping around
`client.WorldBuild()` (line 918):

- **Move CP-side (into `cpbuild`'s `BuildWorldApply`):** CPA creation (`stageCpa`),
  departments (`stageDepartments`), agent reconcile (`reconcileCreatedAgents`), world
  facts (`registerWorldFacts`) — currently run from the driving box signed as the
  **operator**; server-side they use the CP's own identity (`BuildCreateAgentFn`,
  `cpbuild.go:1424`). Also `manageDomainDNS` (Cloudflare A-record upsert): the CP
  already owns the DNS cred and does DNS-01, so record management joins it. The `--data`
  audience-adoption hack (`rebuild.go:929`) is deleted.
- **Stay local (inherently the operator box):** console login (NIP-98) and
  `ensureCpSecrets` (it *collects secrets from the operator* and hands them to the CP
  sealed to the console identity — a headless CP cannot collect them); `certIdent`; and
  the local config-cache writes (`RecordPostWorld`/`FinalSave`), or drop
  `RecordPostWorld` in favor of reading coords from the CP (decision 8).
- **Collapses:** the `owner` branch (`rebuild.go:948` — `cfg.Runner.Addr != ""`): once
  bookkeeping is server-side, local `build` is a uniform thin trigger and the branch
  goes away.

Guardrails:

- Two import-graph tests: nothing under `control-plane/` imports `freehold-cli/`;
  nothing under `freehold-cli/` imports `control-plane/` (each runs in its module's
  `go test ./...`; both modules in CI).
- 5a/5c/5d are **behavior-preserving**; the intended behavior changes are 5b
  (server-side agent creation + DNS, CP-signed) and the `onboard` removal.

## 7. PR plan

### PR 1 — one install surface + fail-if-live + identity-preserving adopt (`0.6.9`, **merged**)

- Fold `bootstrap` into `install --yes`; remove/alias the old command.
- `install` requires `--name` + `--host` (+ the fresh-plane domains/proxy IP);
  interactive/TUI collects them.
- **Life-cycle gate** replaces the plain "profile exists → refuse":
  - profile exists + CP **live** (the recorded `cp_url` console probe) → fail
    (use `build`/`teardown`/`uninstall`).
  - profile exists + CP absent + plane recorded → proceed; **re-adopt**.
  - no profile / empty plane → mint.
  PR1 can only probe a **known** CP (a profile's URL). The authoritative
  **host-side** check — the provider's guest list for the `<name>-cp` guest
  (`bootstrap.LXCName(name, domain, "cp")`), which also covers a **profile-less**
  box pointed at a live world (→ fail, it means `freehold login`) — needs the
  transient transport and lands in **PR4**.
- **Identity-preserving adopt in `deploy-cp`** (must land with the gate — the gate
  must not enable re-adopt without it): if the plane already carries a runner
  package (`/srv/data/cp/control-plane/runner/<target>/identity.json` — the CP
  `DefaultCPStateDir()` `install/cpdeploy/deploy_helpers.go:48` plus
  `<StateDir>/runner/<name>` `deploy_cp.go:337`), **adopt it** — ship only the
  binary + `known_hosts`, keep the plane's `identity.json` **and** its sealed
  `secrets.json` (the box's secrets are sealed to a different encryption key and
  would be unopenable; the plane's substrate credential still opens). Else ship
  the box package + merge secrets as today. This does **not** need the access
  refactor — it runs through the current box runner.
- Write `host` + `access_mode` into the profile config
  (`contract/config/config.go` new fields).
- TUI: the bootstrap-mode guidance names the unified install surface incl. the
  host (`freehold-install install … --name + --host`). The TUI hosts no install
  form — the guided CLI owns `--host`.
- **Tests:** gate matrix (mint/re-adopt/fail-live); re-adopt keeps the runner pubkey
  and does not overwrite `identity.json`; host persisted and read back.

### PR 2 — teardown/uninstall split (`0.6.10`, **merged**)

- `teardown` → CP-preserving inverse of `build`: stop/remove the relay LXC/stack,
  k3s + workloads; **stop** the CP-side `freehold-agent-tools` process (its durable
  state stays on the CP plane — see Semantics); remove internal DNS
  (`DnsSpec.Records` + CP resolver records); keep the CP + its co-located
  runner, plane, Cloudflare records, cert mirror. Drop the `setsid` CP-destroy step
  from this path. **`world teardown` becomes a pure alias** of this.
- New `uninstall [--remove-data]`: remove CP + world + the **invoking box's** door
  + the **runner substrate** key (leave and list other boxes' doors); wipe local
  config/state. Default keeps data; `--remove-data` removes the plane. **Thin-box
  `--remove-data` is refused** until the data path is sequenced.
  - PR2 handles the **CP-alive** case by reusing the existing CP-driven teardown for
    the remote work (revoke the operator door via `world_revoke_door`, then destroy
    the CP last); PR4 adds the **transient-access** path (dead CP / from a thin box)
    via the provider's direct transport.
- `uninstall --name` resolves `--host` from the profile config.
- **Tests:** teardown leaves CP + runner + certs + Cloudflare, clears internal
  DNS/workloads/agent-tools; uninstall keep-data vs `--remove-data`; config/state
  wiped; invoking door + runner substrate key removed, other doors untouched;
  thin-box `--remove-data` refused.

### PR 3 — provider seam + extraction, **no behavior change** (`0.6.11`, **merged**)

The structural refactor: make `platform/` provider-independent by moving every
substrate-specific command behind a `Provider` and into a top-level `providers/`
module. **Structure only** — same commands, same order; the proxmox provider wraps
today's `McpClient`+`pct` path so the diff is mechanical and reviewable.

- Stand up `providers/` (`freehold/providers`) with `providers/proxmox/`.
- Define the `Provider` interface in `platform/provisioning`: host exec, guest exec,
  create/destroy/list guest, storage ops. Generic/`planebase` types only — no `pct`,
  no cloud structs.
- Move `platform/provisioning/drive/` (LVM/ZFS/`pvesm`) and the provider-command
  helpers (`deploy.LxcCmd`, pct stage scripts) into `providers/proxmox/`. Split the
  generic half of `bootstrap/` (`Exec`, `ExpectOK`, `PlainPath`, `ParseMount`) from
  the driver half (`ProxmoxLxcSpec`, `BootstrapProxmoxLxc`, the vultr/hetzner stubs).
- Flip `box`, `install`, `cpbuild`, `teardown`, `handlers3` to take an **injected**
  `Provider` instead of hardcoding `pct`/`drive`.
- **Tests/guard:** existing tests pass with packages moved (no behavioral diff); add
  the "platform does not import providers" guard.
- **Acceptance:** `grep` finds no `pct`/provider strings under `platform/`; the
  install/build/teardown pipelines behave identically.

**PR3 boundary cases (decided).** Two leaks exist beyond the obvious driver files;
the strict guardrail requires closing both.

- **Service deployers.** `platform/services/relay/buzz/deploy_relay.go` (7 sites)
  and `platform/provisioning/deploy/deploy.go` (`CheckDocker`) call `deploy.LxcCmd`
  (`pct exec`). They use a **generic guest-exec seam on the `Provider`** instead.
  Prefer `GuestExec(guest, cmd, timeout) (Outcome, error)` (the provider owns both
  the wrapping and the transport) over returning a command string, so platform code
  stays transport-agnostic for PR4. `install/cpdeploy/*` may import `providers/`
  directly (it is a composition-root-adjacent module).
- **`planebase/pve.go` scripts** (`LocalLvmProbeScript`/`LocalLvmRepointScript`/
  `LocalLvmRidersScript`) and the pct builders in
  `platform/provisioning/stages/stages.go` (`DnsAddCmd`/`DnsApexCmd`/`DnsPointCmd`/
  `DnsVerifyCmd`/`ParsePctGateway`/`CaddyCertInstallScript`) move to
  `providers/proxmox/`; their generic structs/constants stay. **Hidden cost:**
  `box.stageLocalLvmRepoint` (platform) calls those scripts, so the `Provider` needs
  a **storage op** for the local-lvm repoint (probe/riders/repoint) — or
  `stageLocalLvmRepoint` itself moves into the proxmox provider. That is `box`'s
  only direct storage dependency (tenant ensure/destroy already shell
  `storage ensure|destroy` from `install`).
- **Keep the pure name derivations in `planebase`** (`DatasetPath`/`RelayChildDataset`/
  `LvmLVName`/`LvmRelayChildLVName`) — pure, and paired with the classifier that
  parses the same convention. `VpsVolumeLabel` is the only cloud-flavored one; leave
  it or move it later.
- **Guard implementation (avoid false positives):** (1) an **import guard** — assert
  nothing under `platform/` imports `freehold/providers`, via the import graph
  (`go list -deps`), as a test; (2) a **curated token grep** over non-test
  `platform/` files for provider commands (`pct`, `pvesm`, `vzdump`, `qm `,
  `/etc/pve`, `zfs `, `zpool `, `lvs `). Do **not** grep the word "provider" — the
  DNS providers would false-positive.
- **Seam:** one generic guest-exec method on `Provider`; the constructor takes a
  minimal **`Executor`** (the existing `McpClient`+target adapter implements it) so
  PR4's direct-SSH transport stays provider-internal. No options machinery yet.

### PR 4 — transient access + door rotation (`0.6.12`–`0.6.14`, **merged**)

The behavior change the original plan called PR3, now riding a clean provider boundary.
Landed in `0.6.12`; **record correction:** the re-adopt substrate-key rotation (below)
landed in `0.6.13`, and the substrate-key *match* fix in `0.6.14`.

- Add the **direct `ssh-root-proxmox` transport** (an executor the proxmox provider
  accepts) so install no longer needs a local served runner for bootstrap.
- **Door rotation on every install, including re-adopt:** generate the substrate SSH
  keypair → authorize via transient access → `RotateSecret` → restart → remove the old
  key. This is what makes `uninstall` → `install` whole again (uninstall removes the
  substrate key). **Same-host only** (see Deferred for target-repoint).
- Install authorizes the deterministic DOOR_SPEC operator door; after handoff,
  **remove the box's world-home runner package** (decision 11).
- `uninstall` gains the **transient-access path** (dead CP / thin box), and the
  **other-boxes-door warning** (decision 5).
- Add the **host-side fail-if-live** check (the provider's guest list for
  `<name>-cp`), closing the profile-less case.
- **Guard:** every runner secret is CP-recoverable OR re-mintable (SSH is the only
  re-mintable one) — a test that fails on a runner-only secret.
- **Tests:** rotation preserves the runner pubkey + grants and runs on re-adopt; old
  key removed; transient uninstall removes a dead CP; other boxes' doors untouched.

### PR 5 — local/server split (`0.6.15`)

The §6 structural refactor, staged so each step is reviewable:

- **5a — de-invert (behavior-preserving):** move `state`/`relay`/`delegate` out of
  `contract/` into `control-plane/` (keep the `contract/` name — no rename); move
  `cli/teardown` → `control-plane/` (beside build); move the `flows` identity loader
  into `contract/`; repoint server + local callers; add the two no-cross-import guards.
- **5b — server-ify the engine (behavior change — the precise split in §6):** move CPA
  creation, departments, agent reconcile, `WorldFacts` registration, and
  `manageDomainDNS` into `cpbuild`'s `BuildWorldApply`; make local `build`/`teardown`
  pure CP triggers (the `owner` branch and the `--data` audience hack go away); remove
  the local `onboard` command; dissolve `flows`.
  - **Acceptance/tests:** a fresh build creates the CPA + departments via the CP; a
    `--data` rebuild still creates agents (validates deleting the audience hack); the
    CP identity is a relay/roster member for `create_agent`; and a world with stored
    DNS creds still issues/reuses certs **and** manages A-records after `onboard`
    removal (now server-side).
- **5c — create `freehold-cli/`:** new top-level Go module; move the local CLI/TUI tree
  + `install/` (`cpdeploy`) there; dedupe the twice-defined box self-staged commands
  (`exec`/`provision`/`storage`/`deploy-cp`) into one `freehold-cli/internal/stages/`
  (local-only — the CP's `freehold-console` has a different command set and `cpbuild`
  does not run `box.Engine`); drop `freehold-install` (or a one-release shim); add
  `freehold-cli` to `justfile` build/test and the CI Go matrix, and update
  `ResolveBins`.
- **5d — per-verb reorg:** `install/`, `uninstall/`, `login/`, `build/`, `teardown/`,
  `default/` (TUI), `internal/stages/`. `update/`/`export/` only when real.
- **Acceptance:** the two import guards run in `go test ./...` (one per module; both
  modules in CI); commands/behavior unchanged except the 5b changes + `onboard`
  removal; `justfile`/docs updated.

### PR 6 — `vultr` provider (`0.6.16`)

- `providers/vultr/` (provider API + SSH) implementing `Provider`; an `api-vultr`
  access mode. Same orchestration, different provider. `hetzner` later.

## 8. Docs

- `AGENTS.md`: command model (`install`/`build`/`teardown`/`uninstall`), `--host`,
  gate semantics; the module list gains `freehold-cli/` (and `install/` dissolves).
  Keep docs current-state only — the `--remove-data` future default flip is a comment
  at the flag definition, not doc narration.
- `ARCHITECTURE.md`: the provider boundary (§5) — module graph, "platform is
  provider-independent", the `Provider` interface home, what moved under `providers/`;
  access modes, transient access, CP runner identity + door rotation,
  teardown/uninstall scopes. Add the local/server split (§6): the two-app graph,
  "`control-plane` never imports the local CLI and vice versa", and `contract/` as the
  thin protocol leaf.
- `README.md`: update the CLI examples (replace `freehold-install bootstrap`; note the
  provider/access-mode model; drop the `onboard` example).
- `CHANGELOG.md`: one entry per PR; `roadmap/ROADMAP.md`/`POC.md` where relevant.

## 9. Deferred / out of scope

- **`install --restore`** (whole-plane restore → re-adopt + door rotation). The
  identity model guarantees grants survive by construction: the runner Nostr/enc
  identity is restored with the plane, so grants/roster stay intact. **Open piece:**
  `RotateSecret` hardcodes the target `Address` (`provisioner2.go:52`), so a restore
  to a **new host** needs a **target-repoint** step on top of door rotation — PR 4's
  rotation is **same-host only**. Do not implement now, but don't preclude it.
- **Provider breadth** — `vultr` (PR 6) and later `hetzner`: the seam is in place;
  each is a new `Provider` implementation, not a new branch.
- **`--remove-data` becomes the default** — later; comment at the flag, not docs.

## 10. Risks

- **Secret provenance**: the substrate SSH credential is the only runner secret with
  no CP-side source; it is re-minted by design. Any *new* runner-only secret would
  be lost on re-adopt — the guard test (`0.6.12`) covers this.
- **Host resolution after a wipe**: by design — install takes `--host`; host access
  is the one thing that cannot be resolved from the plane.
- **Grants**: never re-mint identity; if any code path re-mints, grants/channel are
  orphaned. Rotation uses identity-preserving `RotateSecret`.
- **Thin-box uninstall**: `--remove-data` still needs the build box.
- **Cross-import regressions (PR 5)**: the two "no cross-import" rules must be
  enforced by import-graph tests, not convention, or the entanglement returns one
  PR at a time.
- **`onboard` removal**: it is not in the DNS/cert path (creds flow `dns-cred` →
  CP `world-secrets` → `worldCert`/`manageDomainDNS`), so dropping it must not change
  `build`/cert behavior — cover with a test that a world with stored DNS creds still
  issues/reuses certs.
- **Out-of-band first access**: after uninstall, the next install's first root access
  is PVE console/root SSH (Proxmox) or the provider API (Vultr).

---

## 11. PR 5 implementation status, deviations, and open questions

PR 5 is on `feat/local-server-split` (PR #258, `0.6.15`). CI green; the bot
review approved after three rounds. **Not merged** (awaiting the operator).

### 11.1 What landed

- **5a — de-invert.** `contract/` is the thin leaf again: `crypto/wire/client/
  config/console` plus the protocol clients `relay`/`delegate`, the `identity`
  loader, and a `worldfacts` wire shape. The CP `state` store moved into
  `control-plane/`. `cli/teardown` moved to `providers/proxmox/teardown/` (it
  shells `pct`, so `platform/`'s guard correctly rejected it there). The
  `contract/` → `shared/` rename was dropped, per the updated §6.
- **5b — CP-owned engine.** New `control-plane/api/cpbuild/agents.go`:
  `reconcileAgents` (CPA → departments → registry reconcile),
  `registerWorldFactsServer`, `manageDomainDNS`, durable cert-expiry parse;
  wired into `BuildWorldApply` as steps 0.5 and 8. Local `build` is a uniform
  thin trigger; the `owner` branch and the `--data` audience-adoption hack are
  gone. `onboard` removed; `flows` dissolved to operator helpers.
- **5c — `freehold-cli/`.** New top-level module: CLI + TUI, `login/`, `flows/`,
  and the install surface (`install/` + `cpdeploy/`). `install/` module
  dissolved; `freehold-install` folds into `freehold install` (hidden
  `bootstrap` alias intact). Both import-graph guards pass
  (`control-plane/isolation_test.go`, `freehold-cli/isolation_test.go`).
  `justfile` + CI build/test `freehold-cli/` in place of `install/`.
- **5d — consolidation (functional half).** The box self-staged
  `exec`/`provision`/`storage`/`deploy-cp` commands now live in one place,
  `freehold-cli/internal/stages/`, and the dead duplicate implementations in
  `cli/handlers3.go` were deleted.

### 11.2 Deviations from the plan text

- **`state`/`relay`/`delegate` split.** §6 said all three move into
  `control-plane/`. `relay` and `delegate` are protocol clients used by BOTH
  sides, so they stayed in the leaf; only the server-only `state` store moved.
  Moving all three would have created the local→server edges that make the
  zero-cross-import rule impossible.
- **`teardown` home.** §6 said "beside build" server-side. The local transient
  uninstall also needs it, and it shells `pct`, so it lives in
  `providers/proxmox/teardown/` — importable by both the CP build and
  `freehold-cli`, with no cross-module edge and no `platform/` guard violation.
- **`manageDomainDNS` is now automatic CP-side.** The local opt-in prompt is
  gone. For a non-Cloudflare stored credential it now skips (no-op) rather than
  failing the whole build (a non-CF cred is still valid for cert DNS-01).
- **5d per-verb package reorg deferred.** Moving `build`/`uninstall`/`teardown`/
  `default` into subpackages requires exporting the `cli` package's shared
  helper layer (`connect`, `addCommonFlags`, `resolveExecProfile`,
  `confirmDestructive`, `runnerKeyRefs`, `worldExecThroughCP`, …) and touching
  every call site — no behavior change, real regression surface, on an
  already-approved diff. Recorded in the 5d commit.

### 11.3 Issues encountered

1. **Leaf placement contradiction (resolved).** §6 moved the protocol clients
   server-side while the local CLI still imported them. Resolved by keeping
   `relay`/`delegate` in the leaf and moving only `state`. See §11.2.
2. **Identity root across two processes (fixed).** `BuildCreateAgentFn` minted
   agent identities under `Spec.StateDir`. In the agent-tools server that is
   `<root>/agent-tools`; in the console executor it is the console dir. A
   console-side reconcile would therefore have minted a FRESH CPA keypair in the
   wrong dir and orphaned every grant. Fixed with `Spec.AgentIdentityDir`, set
   by both roots to `<root>/agent-tools`.
3. **Registry ownership across two processes (designed around).** The
   agent-tools serve process holds the registry/facts in memory; the console
   executor writes the files and then restarts the serve process to reload.
   `Spec.AgentRegistry`/`Spec.FactsStore` let the in-process `world_build` path
   write its own stores with no restart. Verified by reasoning only — no live
   exercise.
4. **The dedupe hid a live command (caught by a test).** The deleted
   `storage` tree looked like a duplicate, but `storage destroy-pool` is driven
   by the teardown engine (`providers/proxmox/teardown/teardown.go:392`). It was
   ported into `internal/stages` along with `storage info`, and the regression
   test that every storage subcommand registers the self-stage flags was kept.
5. **Deleting the `--data` audience-adoption hack has unverified edges (open —
   see §11.4).** The box still signs local agent-tools MCP calls with
   `cfg.AgentToolsPubkey` (`worldMcp` in `freehold-cli/cli/world.go:141`), used
   by `freehold door authorize/revoke`, `freehold world migrate`, and
   `freehold world build`. After `teardown --data` wipes `/srv/data`, the
   agent-tools server mints a fresh identity, and the only remaining re-adopter
   of that pubkey is `freehold login`. The main `freehold build` path uses the
   console client and survives; the three edge commands can fail signature
   verification until a login.

### 11.4 Open questions / decisions needed

1. **Audience staleness (§11.3.5): fix or record?** Options: (a) have `build`
   re-read/adopt the agent-tools pubkey from the console `/api/world` snapshot
   (restores the old behavior server-safely), (b) route `door`/`world migrate`
   through the console API instead of the LAN agent-tools MCP, or (c) record it
   as a known gap. Recommendation: (a) — smallest, restores the guarantee the
   deleted hack provided.
2. **Live integration gate.** 5b's acceptance (fresh build creates the CPA +
   departments via the CP; a `--data` rebuild still creates agents; stored DNS
   creds still issue/reuse certs) needs a real box. Unit coverage exists only
   for the pure pieces (`reconciledChannels`, `cpaNameOrDefault`,
   `agentIdentityDir`). Do we gate the merge on a live run, or ship with these
   listed as unverified?
3. **5d per-verb reorg: do it or drop it?** It is cosmetic; the functional
   dedupe is done. Keep deferred, or spend the churn?
4. **When to merge.** PR #258 is `MERGE-READY`; the repo rule defers merging to
   the operator.
