# Plan — install access modes → normalized CP lifecycle

Status: **ready to execute** (PRs 1–3). `install --restore` is explicitly out of scope
for now (see "Deferred"), but the identity model below is designed so that grants
survive a restore when it lands.

Baseline: `main` after `0.6.5` (PRs #243/#244 merged). CHANGELOG top is `0.6.5`.

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
5. **`uninstall` new verb.** Removes CP + world + **both host doors**; wipes the
   local config/state; **keeps data by default**. `--remove-data` adds plane
   removal (future default flip — mark it).
6. **Install creates both doors** (runner substrate key + operator host door);
   uninstall removes both; `login` refreshes the operator door.
7. **DNS:** Cloudflare is **upserted** (already); teardown removes only internal DNS.
   Certs stay (domain-scoped, reused from the durable mirror).
8. **Coords resolve from the CP/plane** on re-install; the local config is not the
   source of truth.

## 3. Current state (what exists, with refs)

- **Install pipeline** (`platform/provisioning/box/rebuild.go`): `RunBootstrap`
  provisions a runner over SSH (`stageProvision`, ~505), serves it on loopback
  (`stageServe`, 708), writes config, grants, then boots + deploys the CP
  (`stageBootstrap` ~1335, `stageDeployCp` 1523). Every stage shells the runner.
- **deploy-cp copies the box runner package** into the CP LXC and merges secrets
  (`install/cpdeploy/deploy_cp.go:334-394`) — the shared-identity shortcut to fix.
- **Bootstrap drivers take `McpClient`+target**: `BootstrapProxmoxLxc`,
  `BootstrapVultrVps`, `BootstrapHetznerVps`
  (`platform/provisioning/bootstrap/drivers.go:277,590,719`).
- **RotateSecret is identity-preserving** and preserves targets+grants
  (`control-plane/secret-management/provisioner2.go:18-74`) — reuse for the door.
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
freehold build                                          # world bring-up via CP (unchanged)
freehold teardown                                        # inverse of build; CP stays
freehold uninstall [--remove-data]                       # CP + doors removed; data kept by default
freehold login | world | exec | profiles | …             # unchanged
```

Drop `freehold-install bootstrap` (fold into `install --yes`). Keep `world teardown`
as a sub-action of `world`.

### Semantics

- **install:** if the CP LXC/console is live → fail with guidance (`build` to bring
  up the world, `teardown` to drop the world, `uninstall` to drop the CP). If the
  CP is absent: boot it, then `deploy-cp`, which **adopts** the existing runner if
  the plane carries one, else mints. Rotate the substrate SSH door. Authorize the
  operator host door. Re-resolve coords (relay/cp hosts, proxy IP, mounts) from the
  surviving CP state/plane; only `--host` + `--name` are required for a fresh plane.
- **door rotation (install):** generate a new SSH keypair → authorize the new pubkey
  on the host via transient access → `RotateSecret(<target>, newPriv)` → restart the
  runner → remove the old `authorized_keys` entry. Same-host and (future) new-host
  restore are the same flow.
- **teardown:** through the CP (`/api/world-teardown`), remove relay/agent-tools/k3s/
  workloads + internal DNS; leave CP, plane, Cloudflare records, cert mirror.
- **uninstall:** transient access; remove the CP + world; remove both host doors;
  wipe local config/state; keep data. `--remove-data` also removes the plane.
- **DNS:** build upserts A records (existing IP changed → update). Teardown clears
  internal DNS only.

## 5. PR plan

### PR 1 — one install surface + fail-if-live (`0.6.6`)

- Fold `bootstrap` into `install --yes`; remove/alias the old command.
- `install` requires `--name` + `--host`; interactive/TUI collects them.
- **Life-cycle gate** replaces the plain "profile exists → refuse":
  - profile exists + CP **live** → fail (use `build`/`teardown`/`uninstall`).
  - profile exists + CP absent + plane present → proceed; **re-adopt**.
  - no profile / empty plane → mint.
- Write `host` + `access_mode` into the profile config
  (`contract/config/config.go` new fields).
- TUI: allow entering host for install.
- **Tests:** gate matrix (mint/re-adopt/fail-live); host persisted and read back.

### PR 2 — teardown/uninstall split (`0.6.7`)

- `teardown` → CP-preserving inverse of `build`: stop/remove relay/agent-tools/k3s/
  workloads; remove internal DNS (`DnsSpec.Records` + CP resolver records); keep CP,
  plane, Cloudflare records, cert mirror. Drop the `setsid` CP-destroy step from
  this path.
- New `uninstall [--remove-data]`: transient access; remove CP + world + both host
  doors; wipe local config/state. Default keeps data; `--remove-data` removes the
  plane (and marks the future default flip in CHANGELOG/AGENTS).
- `uninstall --name` resolves `--host` from the profile config.
- **Tests:** teardown leaves CP + certs + Cloudflare, clears internal DNS/workloads;
  uninstall keep-data vs `--remove-data`; config/state wiped; doors removed.

### PR 3 — transient access refactor + door rotation (`0.6.8`)

- New `Access` seam in `platform/provisioning/bootstrap`: yields exec + provider
  create/destroy. Refactor `BootstrapProxmoxLxc`/`Vultr`/`Hetzner` to take an exec
  function instead of `McpClient`+target. Update callers in `box.Engine` and
  `install`.
- Implement `ssh-root-proxmox` (direct SSH root + `pct`); `api-vultr` next (stub
  acceptable if scoped).
- Install rotates the CP runner's substrate SSH door via the `Access` seam
  (generate → authorize → `RotateSecret` → restart → remove old).
- `install` authorizes both doors; `deploy-cp` stops copying the box runner package
  and instead **mints-or-adopts in-guest** (no `identity.json` overwrite).
- **Guard:** assert every runner secret is CP-recoverable OR re-mintable (SSH is the
  only re-mintable one) — a test that fails if a runner-only secret appears.
- **Tests:** door rotation preserves runner pubkey + grants; old key removed; drivers
  work through the exec seam; deploy-cp adopt path doesn't overwrite identity.

## 6. Docs

- `AGENTS.md`: command model (`install`/`build`/`teardown`/`uninstall`), `--host`,
  destructive `--remove-data` (future default), gate semantics.
- `ARCHITECTURE.md`: access modes, transient access, CP runner identity + door
  rotation, teardown/uninstall scopes.
- `README.md`: update the CLI examples (replace `freehold-install bootstrap`).
- `CHANGELOG.md`: one entry per PR; `roadmap/ROADMAP.md`/`POC.md` where relevant.

## 7. Deferred / out of scope

- **`install --restore`** (whole-plane restore → re-adopt + door rotation). The
  identity model here guarantees grants survive by construction when it lands:
  runner Nostr/enc identity is restored with the plane, so only the SSH door rotates
  for the new host. Do not implement now, but don't preclude it.
- **Provider breadth** beyond `ssh-root-proxmox` (Vultr/Hetzner API) — build the
  seam, implement Proxmox first.
- **`--remove-data` becomes the default** — later; mark it.

## 8. Risks

- **Secret provenance**: the substrate SSH credential is the only runner secret with
  no CP-side source; it is re-minted by design. Any *new* runner-only secret would
  be lost on re-adopt — the guard test covers this.
- **Fail-if-live detection** must be reliable (CP LXC present or console answer);
  a false negative would re-bootstrap over a live CP.
- **Host resolution after a wipe**: unresolved by design — install takes `--host`.
- **Grants**: never re-mint identity; if any code path re-mints, grants/channel are
  orphaned. The rotation path must use `RotateSecret` (identity-preserving).
