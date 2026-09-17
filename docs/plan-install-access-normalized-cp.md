# Plan — install access modes → normalized CP lifecycle

Status: **ready to execute** (PRs 1–3). `install --restore` is explicitly out of scope
for now (see "Deferred"), but the identity model below is designed so that grants
survive a restore when it lands.

Baseline: `main` after `0.6.7` (PRs #243/#244/#246 merged). CHANGELOG top is `0.6.7`.

The storage-scope work that preceded this (domain-scoped provenance + named
profiles + `<name>-<role>` LXCs, PRs #243/#244) is already merged; this plan is the
command/lifecycle follow-on.

---

## 1. Goal

Land a control plane with **transient privileged access** (root SSH / provider API),
then make the CP's co-located runner the only durable "hands". After install, every
box talks to the CP over signed HTTPS; installers differ only in **access mode**
(`ssh-root-proxmox`, later `api-vultr`), and all end at the same normalized CP.

Key property: the CP runner's **Nostr + encryption identity is permanent**. Grants,
relay channel/roster, and sealed secrets survive every re-install. Only the
**substrate SSH credential (the door) rotates**.

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
11. **The install box is thin after handoff.** PR3 removes the box's transient
    world-home runner package once the CP owns the identity and the doors are in
    place; box-side `exec` already rides the CP's `world_exec`
    (`noLocalRunner()`, `control-plane/cli/handlers.go:112`). Keeping the package
    is a harmless fallback, not the target state.

## 3. Current state (what exists, with refs)

- **Install pipeline** (`platform/provisioning/box/rebuild.go`): `RunBootstrap`
  provisions a runner over SSH (`stageProvision`, ~505), serves it on loopback
  (`stageServe`, 708), writes config, grants, then boots + deploys the CP
  (`stageBootstrap` ~1335, `stageDeployCp` 1523). Every stage shells the runner.
- **deploy-cp copies the box runner package** into the CP LXC and merges secrets
  (`install/cpdeploy/deploy_cp.go:334-394`) — it ships `identity.json`
  (`:352-358`), so today a re-install overwrites the plane's runner identity and
  orphans its grants. Fixed in **PR 1** by mint-or-adopt (below).
- **Bootstrap drivers take `McpClient`+target**: `BootstrapProxmoxLxc`,
  `BootstrapVultrVps`, `BootstrapHetznerVps`
  (`platform/provisioning/bootstrap/drivers.go:277,590,719`).
- **RotateSecret is identity-preserving** and preserves targets+grants
  (`control-plane/secret-management/provisioner2.go:18-74`) — reuse for the door.
  It **hardcodes `Address: before.Address`** (`:52`), so it cannot repoint a
  target host — see "Deferred".
- **ProvisionRunner refuses same-name/PackageDirInUse**
  (`provisioner.go:99-113`) — we avoid it by not re-minting identity.
- **CP-side secret re-seal source exists**: `reseedCoLocatedRunner`
  (`control-plane/api/cpbuild/cpbuild.go:819-862`) re-seals runner secrets from the
  CP's durable `world-secrets/litellm.json`.
- **ensureCpSecrets skips present secrets** (`control-plane/cli/rebuild.go:383-422`).
- **Cert durable-reuse gate** (`cpbuild.go:1053`): valid mirror (≥30d) → reseed, no LE.
- **DNS upsert**: `manageDomainDNS` calls `m.UpsertA` (`rebuild.go:548-549`);
  Manager contract creates/updates/removes (`platform/services/externaldns/cloudflare/dnsman.go:12-27`).
- **selectProfile refuses an existing profile** (`install/cli/install.go`) — must
  become life-cycle aware (mint vs re-adopt vs fail-if-live).
- **Config has no host field** (`contract/config/config.go`).
- **`teardown` currently destroys the CP last**, with a detached `setsid` step
  (`cpbuild.go:1423`) because it runs through the CP runner. Making teardown
  CP-preserving removes that hack; CP-destroy moves to `uninstall`.
- **`world_migrate` is CP schema migrations**, unrelated to data restore — do not
  conflate naming.
- **Known gap**: thin-box `teardown --data`/`--tenant` refused (AGENTS "Known gaps").

## 4. Target command surface

```
freehold-install install [--name] [--host] [--yes]     # fail if a live CP exists
                        [--relay-domain] [--cp-domain] [--proxy-ip]   # fresh plane only
freehold build                                          # world bring-up via CP (unchanged)
freehold teardown                                        # inverse of build; CP stays
freehold uninstall [--remove-data]                       # CP + doors removed; data kept by default
freehold login | world | exec | profiles | …             # unchanged
```

Drop `freehold-install bootstrap` (fold into `install --yes`). **`world teardown`
is a pure alias** of the CP-preserving `teardown`.

### Semantics

- **install (fresh plane):** requires `--name --host --operator-pubkey
  --relay-domain --cp-domain --proxy-ip`. There is **no base domain** — the relay/CP
  hosts and proxy IP are **not derivable from `--name`**; interactive/TUI prompts
  collect them. A fresh plane has nothing to resolve from.
- **install (re-adopt):** relay/CP hosts, proxy IP, and mounts **resolve from the
  surviving CP state/plane**, so those flags become optional; only `--name` +
  `--host` remain required (the host cannot be read without host access). If the
  CP guest is **live on the host** (`pct list` — works with or without a profile)
  → fail with guidance (`build` to bring up the world, `teardown` to drop the
  world, `uninstall` to drop the CP; a fresh box means `login`). Otherwise boot the CP
  and `deploy-cp`, which **adopts the plane's existing runner** when present (never
  overwriting `identity.json`), else mints.
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

## 5. PR plan

### PR 1 — one install surface + fail-if-live + identity-preserving adopt (`0.6.8`)

- Fold `bootstrap` into `install --yes`; remove/alias the old command.
- `install` requires `--name` + `--host` (+ the fresh-plane domains/proxy IP);
  interactive/TUI collects them.
- **Life-cycle gate** replaces the plain "profile exists → refuse":
  - profile exists + CP **live** (the recorded `cp_url` console probe) → fail
    (use `build`/`teardown`/`uninstall`).
  - profile exists + CP absent + plane recorded → proceed; **re-adopt**.
  - no profile / empty plane → mint.
  PR1 can only probe a **known** CP (a profile's URL). The authoritative
  **host-side** check — `pct list` for the `<name>-cp` guest
  (`bootstrap.LXCName(name, domain, "cp")`), which also covers a **profile-less**
  box pointed at a live world (→ fail, it means `freehold login`) — needs the
  transient `Access` seam and lands in **PR3**.
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

### PR 2 — teardown/uninstall split (`0.6.9`)

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
    the CP last); PR3 adds the **transient-access** path (dead CP / from a thin box)
    via the `Access` seam.
- `uninstall --name` resolves `--host` from the profile config.
- **Tests:** teardown leaves CP + runner + certs + Cloudflare, clears internal
  DNS/workloads/agent-tools; uninstall keep-data vs `--remove-data`; config/state
  wiped; invoking door + runner substrate key removed, other doors untouched;
  thin-box `--remove-data` refused.

### PR 3 — transient access refactor + door rotation (`0.6.10`)

- New `Access` seam in `platform/provisioning/bootstrap`: yields exec + provider
  create/destroy. Refactor `BootstrapProxmoxLxc`/`Vultr`/`Hetzner` to take an exec
  function instead of `McpClient`+target. Update callers in `box.Engine` and
  `install`.
- Implement `ssh-root-proxmox` (direct SSH root + `pct`); `api-vultr` next (stub
  acceptable if scoped).
- Install rotates the CP runner's substrate SSH door via the `Access` seam
  (generate → authorize → `RotateSecret` → restart → remove old). **Same-host only**
  (see Deferred for the target-repoint gap).
- `install` authorizes the deterministic DOOR_SPEC operator door + ensures the
  runner substrate door; after a successful handoff **remove the box's world-home
  runner package** (decision 11 — the box is thin thereafter; box-side `exec`
  rides the CP's `world_exec`).
- `uninstall` gains the **transient-access** path (remove a CP that is already down,
  or from a thin box) via the seam — the CP-alive path landed in PR2.
- **Guard:** assert every runner secret is CP-recoverable OR re-mintable (SSH is the
  only re-mintable one) — a test that fails if a runner-only secret appears.
- **Tests:** door rotation preserves runner pubkey + grants; old key removed; drivers
  work through the exec seam.

## 6. Docs

- `AGENTS.md`: command model (`install`/`build`/`teardown`/`uninstall`), `--host`,
  gate semantics. Keep docs current-state only — the `--remove-data` future default
  flip is a comment at the flag definition, not doc narration.
- `ARCHITECTURE.md`: access modes, transient access, CP runner identity + door
  rotation, teardown/uninstall scopes.
- `README.md`: update the CLI examples (replace `freehold-install bootstrap`).
- `CHANGELOG.md`: one entry per PR; `roadmap/ROADMAP.md`/`POC.md` where relevant.

## 7. Deferred / out of scope

- **`install --restore`** (whole-plane restore → re-adopt + door rotation). The
  identity model guarantees grants survive by construction: the runner Nostr/enc
  identity is restored with the plane, so grants/roster stay intact. **Open piece:**
  `RotateSecret` hardcodes the target `Address` (`provisioner2.go:52`), so a restore
  to a **new host** needs a **target-repoint** step on top of door rotation — PR 3's
  rotation is **same-host only**. Do not implement now, but don't preclude it.
- **Provider breadth** beyond `ssh-root-proxmox` (Vultr/Hetzner API) — build the
  seam, implement Proxmox first.
- **`--remove-data` becomes the default** — later; comment at the flag, not docs.

## 8. Risks

- **Secret provenance**: the substrate SSH credential is the only runner secret with
  no CP-side source; it is re-minted by design. Any *new* runner-only secret would
  be lost on re-adopt — the guard test covers this.
- **Fail-if-live detection** must be reliable (CP LXC present or console answer);
  a false negative would re-bootstrap over a live CP. PR1 covers the console
  answer when a profile exists; PR3 adds the host-side `pct list` check, which
  also closes the profile-less case.
- **Host resolution after a wipe**: by design — install takes `--host`; host access
  is the one thing that cannot be resolved from the plane.
- **Grants**: never re-mint identity; if any code path re-mints, grants/channel are
  orphaned. The rotation path must use `RotateSecret` (identity-preserving).
- **Door-key identification**: removals must target exactly the invoking box's door
  + the runner substrate key, so freehold-owned host keys need a recognizable marker
  (e.g. a `freehold-*` comment) at authorize time; never remove another box's door.
- **Thin-box uninstall**: `--remove-data` refused until the data path is sequenced.
- **Out-of-band first access**: after uninstall, the next install's first root access
  is PVE console/root SSH (Proxmox) or the provider API (Vultr).
