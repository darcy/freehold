# Versioned Updates — release assets, channels, and migrations

Status: plan — not started. This is the implementation spec; tick the phase
checkboxes as work lands. `AGENTS.md` "Releases" is the release-time contract;
this file covers the *runtime* side: putting a version on the CP, querying it,
and updating it.

## Goal

A world knows exactly what version it runs, any box can query it, and
`freehold update` moves it to another version — pulling release assets for
tagged releases, or building an untagged ref — then runs migrations and repins.

## Locked decisions

*   **A version exists only at release** (see `AGENTS.md` "Releases"). Every
    release is a `CHANGELOG.md` entry + an annotated tag + a GitHub Release
    **with binary assets**. There is no version bump per merge.
*   **Sources are assets or builds, nothing else.** Tagged releases have
    assets; untagged refs do not. No always-on "edge" CI job.
*   **`edge` is not a channel.** `main` is reached with `--ref main`.
*   **Migrations are scripts, never compiled into a binary.** They are the
    Omarchy-style one-time repair/catch-up scripts; they run as scripts.
*   **No separate `freehold migrate` verb** — migrations run inside
    `freehold update` (and `freehold install`).
*   **`freehold update` is remote-world only.** It redeploys the CP + runner +
    agent-tools and runs migrations; the local CLI is updated separately.
*   **The CP stays toolchain-free.** Untagged-ref builds happen on the box
    (`go`/`rust` via mise), in a sandbox temp dir, then ship to the CP.

## The model

### Version string

Computed from git, one helper shared by `justfile` and CI:

```sh
git describe --tags --always --dirty   # v0.7.0 | v0.7.0-rc.1 | main-gabc123 | v0.7.0-4-gabc123-dirty
```

*   Tagged release → `vX.Y.Z` (or `vX.Y.Z-rc.N` for a prerelease tag).
*   Local/untagged → the describe string; a dev build additionally records the
    ref it came from (`dev(<ref>@<sha>)`).

Embedded at build time:

*   Go — a new leaf package `contract/version` (`Version`, `Commit`), stamped
    with `-ldflags "-X freehold/contract/version.Version=… -X …Commit=…"`.
    Consumed by `freehold --version`, `freehold-console version`, and the CP.
*   Rust runner — `build.rs` emits the version; the runner reports it in its
    MCP handshake (today it reports `CARGO_PKG_VERSION`).

`justfile` gains `VERSION`/`COMMIT` variables so local builds and CI stamp
identically. CI passes the tag explicitly.

### Sources (channels)

| Flag | Channel | Source | Box behavior |
|---|---|---|---|
| `--stable` **(default)** | `stable` | newest non-prerelease `v*` tag | pull release assets → verify → ship |
| `--rc` | `rc` | newest `v*-rc.*` prerelease tag | pull release assets → verify → ship |
| `--dev` | `dev` | local working tree | build locally → ship |
| `--ref <ref>` / `--sha <sha>` | (pinned) | any untagged ref, incl. `main` | clone into sandbox → build → ship |

*   `main` = `--ref main`. There is no `edge` channel and no rolling CI job.
*   The CP records its channel; `freehold update` with no flag uses it.
    `--stable`/`--rc`/`--dev`/`--ref`/`--sha` override for one run.
*   Downloads and sandbox clones live under the profile's cache dir, never in
    `target/` (they are not the local tree).

### CP pin + query

*   `install`/`deploy-cp`/`update` pass `{version, channel, commit}` to the CP.
*   `freehold-console serve` writes `<stateDir>/version.json` (0600) — a small
    dedicated file rather than a field on `ControlPlaneState` (which mirrors the
    Rust serde repr; keep that contract untouched).
*   Surfaced on `/healthz` (JSON body), `/api/world`, `agenttools.WorldStatus`,
    and `console.WorldSummary`; rendered by `freehold status` and the TUI.
*   `freehold status` shows the CP's version + channel and, when reachable,
    whether the channel's newest release is newer.

### Release artifacts

`.github/workflows/release.yml`, triggered on `push` of a `v*` tag **only**
(no main/edge job). Steps:

1. Build the full sibling set via the existing `just build` recipes:
   `freehold`, `freehold-console`, `runner`, `freehold-agent-tools`
   (`freehold-agent-tools` stays `CGO_ENABLED=0` static).
2. Package `migrations/` → `migrations.tar.gz`.
3. Generate `checksums.txt` (sha256) over every asset.
4. Upload to the GitHub Release; `v*-rc.*` / prerelease tags are marked
   prerelease.

The `release` skill gains an artifacts stage: push the tag → CI builds and
uploads assets → the skill publishes the Release with curated notes → verify
the assets are present. `--generate-notes` is not used.

`workflow_dispatch(ref)` for on-demand ref builds is a later convenience (see
Out of scope); the box builds untagged refs itself for now.

## Migrations

### Shape

*   Canonical scripts: top-level `migrations/<epoch>.sh` (epoch = the
    Omarchy-style timestamp/filename identity). **No `.verify.sh`** — apply
    success is done.
*   Scripts are POSIX-sh, idempotent, and run on the CP through `bash`. They
    receive the durable paths + the agent-tools binary via env
    (`FREEHOLD_AGENT_TOOLS`, `REGISTRY`, `CONSOLE_STATE`, `STATE_DIR`) — never
    argv, so no credential crosses the audit. A migration that renames an agent
    is a script calling an agent-tools/console subcommand, not compiled code.
*   `platform/migrations` stops embedding (`//go:embed` removed); it enumerates
    a **script directory** and tracks completion in **marker files**.

### Shipping

Scripts travel with the release, not the binary:

*   `--stable`/`--rc` → `migrations.tar.gz` from the release assets.
*   `--dev`/`--ref`/`--sha` → the `migrations/` dir from the local or sandbox
    tree.

`freehold install` **and** `freehold update` copy the scripts to the CP's
migrations dir (via the deploy transport) and then run pending ones. A fresh
CP starts with an empty marker set, so install applies every migration once
(most are cheap no-ops on a fresh world, but they are recorded).

### Completion (marker files)

*   Markers live under `<stateDir>/migrations/done/`, one file per applied
    migration, named for the script. This is the analog of Omarchy's per-user
    marker folder and, unlike the current single JSON ledger, survives
    **jumping between channels/versions**: a migration done on `stable` stays
    done when you move to `--dev` and back.
*   Pending = scripts present in the migrations dir minus markers present.
*   Ordering is strictly by epoch; a migration that fails exits non-zero, stays
    unmarked, stops the queue, and is retried on the next run. Never mark a
    migration done on failure.
*   `world_migrate` still exposes the run; its description drops the
    verify-gate language. The CP can also report pending/done counts for
    `--check` and `status`.

### Ordering within an update

Deploy binaries → restart → `/healthz` green → **copy scripts → run pending
migrations** → repin `version.json`. Migrations always run against the new
code. Failure reports and stops; re-running `freehold update` retries pending.

## Package layout

*   Move `freehold-cli/install/cpdeploy/` → `freehold-cli/internal/cpdeploy/`
    (a shared engine, mirroring `internal/stages` and `internal/certcred`).
    Repoint `internal/stages/stages.go`.
*   Expose a standalone **ship-bytes-to-CP** step on `box.Engine`
    (`platform/provisioning/box`), parameterized by resolved `Bins` + `Coords`,
    so `install` and `update` share one path.
*   Put artifact acquisition (resolve source → assets or sandbox build → verify)
    and the bin/migration resolution in `internal/` (not under `install/`).
*   Dependency direction stays acyclic:
    `verbs (install/, update/, …) → internal/{cpdeploy,stages,common,artifact} → platform/provisioning/box`.

## `freehold update` flow

1.  Lock (one operator update at a time) + preflight (profile, disk space,
    confirm unless `--yes`).
2.  Resolve the source: CP's recorded channel, or `--stable`/`--rc`/`--dev`/
    `--ref`/`--sha`.
3.  Acquire artifacts: download + sha256-verify release assets, or clone the
    ref into a sandbox and build, or build the local tree.
4.  Ship binaries to the CP; restart; wait for `/healthz`.
5.  Copy `migrations/` to the CP; run pending; stop on failure.
6.  Repin `version.json`; refresh status.
7.  `--check` performs steps 1–3 in dry-run form only: report the available
    version and pending migration count, change nothing.
8.  Thin box / no local runner: use the transient root-SSH path (as
    install/uninstall already do).

## Phases

### Phase A — shared deploy engine

*   [ ] `install/cpdeploy` → `internal/cpdeploy`; repoint `internal/stages`.
*   [ ] Expose the ship-to-CP step on `box.Engine`; `install` uses it.
*   [ ] `go build`/`vet`/`test` green across modules; import guards pass.

### Phase B — version embedding + CP pin/query

*   [ ] `contract/version` (Version, Commit); `justfile` + CI stamp via
        `git describe`.
*   [ ] Runner `build.rs` version; runner handshake reports it.
*   [ ] `freehold --version`, `freehold-console version` wired.
*   [ ] `install`/`deploy-cp` pass `{version, channel, commit}`; `serve` writes
        `<stateDir>/version.json`.
*   [ ] `/healthz` JSON; `/api/world`, `WorldStatus`, `WorldSummary` carry it;
        `status` + TUI render it.
*   [ ] Acceptance: thin box `freehold status` prints the CP version+channel;
        `/healthz` returns JSON.

### Phase C — release artifacts

*   [ ] `.github/workflows/release.yml` (tag `v*` only): build set, package
        `migrations.tar.gz`, `checksums.txt`, upload; prerelease tags marked.
*   [ ] `release` skill artifacts stage; verify assets after publish.
*   [ ] Acceptance: cutting a test `vX.Y.Z-rc.N` produces a prerelease with all
        assets + checksums; a box can pull and verify them.

### Phase D — migrations as scripts

*   [ ] Move scripts to top-level `migrations/<epoch>.sh`; delete `.verify.sh`
        and every verify reference.
*   [ ] `platform/migrations`: enumerate a script dir; marker files under
        `<stateDir>/migrations/done/`; drop the JSON ledger and verify gate.
*   [ ] `BuildMigrator`/`world_migrate` run from the CP migrations dir;
        `install` and `update` copy scripts first.
*   [ ] Acceptance: a script migration applies on install and update; a
        completed marker survives a channel switch; a failing script stops the
        queue and is retried.

### Phase E — `freehold update`

*   [ ] `update/update.go` grows into the real updater (resolve → acquire →
        deploy → migrate → repin). No `migrate` verb.
*   [ ] Sandbox clone+build for `--ref`/`--sha`; local build for `--dev`.
*   [ ] `--check`; thin-box transient path; update lock + preflight.
*   [ ] Acceptance: a dev CP on `v0.7.0-beta.1` updates to a newer ref, runs
        migrations, repins; re-running is a no-op; `--check` reports without
        changing anything.

### Phase F — channels + docs

*   [ ] `freehold channel set <stable|rc|dev>` recorded on the CP;
        `install --channel`/`--version`; default `stable`.
*   [ ] Docs: `AGENTS.md` (channels/update/version-query + migrations-as-scripts
        rule), `README.md`/`ARCHITECTURE.md` version surface; `CHANGELOG.md` at
        the next release.
*   [ ] Acceptance: the dev cycle works end to end — local build → `--dev`;
        cut `vX.Y.Z-rc.N` → `--rc`; tag `vX.Y.Z` → `--stable`; CP version and
        channel track throughout.

## Out of scope / follow-ups

*   **Snapshots/rollback.** No automatic binary rollback; a failed migration
    stays pending and re-runs. Rolling back is `update --version <older>`.
    A snapshot-based escape hatch (Omarchy's snapper analog) is a later phase.
*   **On-demand CI ref builds** (`workflow_dispatch`) so a toolchain-free box
    can test a sha. Untagged refs are box-built for now.
*   **Remote compile on the CP.** The CP stays toolchain-free.
*   **Multi-arch.** linux/amd64 only; arm64 (e.g. DGX Spark) later.
*   **CLI self-update.** `update` is remote-world only; the local `freehold`
    binary is updated separately.
*   **External image pinning.** The `ghcr.io/block/buzz-sprig:main` image is
    outside this version contract.

## Affected docs

*   `AGENTS.md` — update/channel rules, version query, "migrations are scripts".
*   `.agents/skills/release/SKILL.md` — assets + `migrations.tar.gz` stage.
*   `README.md`, `ARCHITECTURE.md` — version surface / update flow.
*   `CHANGELOG.md` — written at the next release.
