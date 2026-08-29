# freehold Go Migration — Plan & Status

Status document for the approved plan `local://orchestrator-go-plan.md`, executed on the
long-lived branch `refactor-go` (never merged to `main`; `main` untouched and green).

## Mission (from the approved plan)

Port the orchestrator (bootstrap/repair engine, 16 subcommands) from Rust to Go for
maintainability and iteration speed on branchy coordination-restoration logic. The Rust
`core` (crypto/identity/wire) and `runner` stay Rust and are the byte-exact reference oracle.
`control-plane` migrates in a later, separate plan.

**Migration seam**: orchestrator talks to the runner over HTTP MCP (runner stays Rust, stays
the exec target; `mcp::serve` is NOT reimplemented in Go). The orchestrator's own
crypto/identity/wire MUST reproduce the Rust `core` surface byte-exactly; a committed
Go↔Rust cross-verification harness is the release gate for every primitive.

**Supersession**: the plan's TUI step (keep ratatui in Rust, exec the Go binary) was
OVERRIDDEN by an explicit user decision — the TUI is fully ported to Go (bubbletea) and the
Rust `tui` crate is removed. `freehold` (no args) = interactive TUI; `freehold <subcmd>` =
CLI. Both binaries (`freehold`, `freehold-orchestrator`) are Go.

## Layout (post-reorg)

- `orchestrator/` = Go module `freehold/orchestrator` (go 1.25), NOT `orchestrator-go`
  (renamed during reorg, commit `b75f7c5`).
- `orchestrator/cmd/{freehold,freehold-orchestrator}/main.go` — the two binaries.
- `orchestrator/internal/` — 19 packages: bootstrap, cli, client, config, console, crypto,
  delegate, deploy, drive, flows, harness, planebase, provisioner, relay, state, teardown,
  tui, wire.
- `orchestrator/harness/` — Go↔Rust byte-exact cross-verification harness (`harness_test.go`)
  + Rust oracle crate `freehold-harness-oracle` (workspace member).
- Rust workspace now: `core, runner, console-client, testkit, control-plane, acceptance,
  installer, orchestrator/harness/oracle`. `orchestrator-rust/` and `tui/` DELETED.

## Where we are — by phase

### Phase 1 — Foundation: DONE
- Go 1.25 installed user-local (`~/.local/go`), on PATH via
  `export PATH=$HOME/.local/go/bin:$HOME/go/bin:$PATH`.
- `refactor-go` branch; module + internal layout scaffolded; cobra CLI with the full
  subcommand surface registered.

### Phase 2 — Agent Crypto (harness-gated): DONE
- `internal/crypto`: BIP-340 Schnorr via btcec/v2 (parity-negated scalar aux mask —
  IMPORTANT-4 review fix, parity-probe test gates it), X25519+HKDF+ChaCha20-Poly1305 sealed
  box, bech32 nsec, ed25519.
- `internal/wire`: SecretPackage (sorted-map JSON to match serde BTreeMap), NIP-98 canonical
  event + BIP-340, audit (kind 48001), NIP-44 v2 engram.
- Harness: `go test ./harness/` drives `target/debug/freehold-harness-oracle`; ALL byte-exact
  gates PASS (seal/open both directions, sign/verify, nip44, SecretPackage roundtrip).

### Phase 3 — Clients: DONE
- `internal/client` (MCP HTTP client, signing, ExecOutcome), `internal/relay` (bridge:
  channels/roster/meta/memory, fail-closed query), `internal/console` (console-client port:
  login/provision/rotate/revoke/grant/…), `internal/state` (StateStore over state.json,
  atomic-0600), `internal/provisioner` (ProvisionRequest + SecretCreatorRequest).

### Phase 4 — Flows: DONE
- `internal/flows` (agent_auth, connect_url, onboard — runner now spawned as subprocess
  `runner serve`, NOT in-process), `internal/bootstrap` (proxmox-lxc pct driver,
  vultr/hetzner curl drivers, A4 domain gate), `internal/planebase` (decision layer),
  `internal/deploy` (relay + CP, OPERATE mode), `internal/teardown` (three scopes),
  `internal/delegate` (job-request/job-result envelopes).

### Phase 5 — Surface: DONE
- CLI contract: 18 subcommands registered (the plan's 16 + destroy/ensure/info/resolve):
  bootstrap, console-login, delegate, delegate-peer, demo, deploy-cp, deploy-relay, destroy,
  ensure, info, readiness, relay-join, relay-member, relay-profile, relay-setup, resolve,
  storage, teardown. `installer`'s `bin("freehold-orchestrator")` resolve path =
  `target/debug/freehold-orchestrator` — the Go binary is built there (`go build -C
  orchestrator -o ../target/debug/freehold-orchestrator ./cmd/freehold-orchestrator`);
  cargo build does NOT regenerate it.
- bootstrap + teardown subcommands are FULLY wired to drivers (commit cfd7a4c —
  the last two CLI stubs): bootstrap dispatches proxmox-lxc/vultr/hetzner drivers
  with the operator-pubkey gate + A4 domain gate; teardown loads the installer
  config, verifies the door via a signed exec probe, and runs the three scopes.
- TUI (bubbletea): mode auto-detection (bootstrap/configure/running), running dashboard
  (Services/Agents/Runners/DATA views, Tab cycling, 2s timer), interactive console action
  flows (l login NIP-98 / p provision / x revoke / g grant via textinput prompts), drive
  storage-info for the DATA view.
- Interactive bootstrap/configure forms (commit f8a13ae): bootstrap mode `b` = 4-field
  bootstrap form; configure mode `d` = deploy-relay (owner/relay-url/operator-pubkey),
  `c` = deploy-cp (binary/relay-url/operator-pubkey). Forms exec the freehold binary
  (self) to reuse the wired CLI drivers; flowMsg reload re-detects the mode.

### Phase 6 — Fling: DONE
- `cargo build --workspace` clean; `cargo test --workspace` green (0 fail);
  `go vet ./...` clean; `go test ./...` green (harness byte-exact gate + all packages).
- Installer subprocess contract verified LIVE against the real runner
  (127.0.0.1:8787 / proxmox-box, commit f8a13ae):
  `target/debug/freehold-orchestrator exec ... "echo freehold-door-ok"` → exact output
  `freehold-door-ok`, rc=0; `readiness` → green; `storage resolve` → reuse lvm-thin.
  TUI launch verified under a PTY (running + bootstrap modes render; b-form prompt works).

## Commits on `refactor-go` (working tree clean)

| commit | content |
|---|---|
| `b75f7c5` | Reorg: `orchestrator/` = Go module (freehold binary), `orchestrator-rust/` = lib-only; CI/README |
| `4ec45e8` | TUI port (bubbletea): mode detection, running dashboard, CLI dispatch |
| `0c027bc` | drive: durable-plane storage-info port for the DATA view |
| `9786cbf` | Interactive console action flows (login/provision/revoke/grant) + drive fixes |
| `c5a3750` | Remove Rust `tui` + `freehold-orchestrator-lib`: Go freehold is the sole UI/CLI |
| `36c6876` | migrate-go.md: this status document |
| `cfd7a4c` | Wire bootstrap + teardown CLI to real drivers (last two stubs) |
| `f8a13ae` | Interactive bootstrap/configure TUI forms + live installer-contract verification |

(earlier: phase 2–4 port commits 9f23609, f59373e, 80609a7, e92d574, 5c2377d)

## Remaining work

The plan is COMPLETE on `refactor-go`. Only operator-driven live exercises remain
(these are the end-user testing the branch is held back from `main` for):

1. Full `bootstrap` end-to-end on a scratch target (proxmox-lxc pct create through
   the real runner) — the drivers are ported and CLI-wired; the exec probe,
   readiness, and storage paths are already live-verified.
2. `teardown --yes` on a scratch world — engine ported + CLI-wired; door probe
   live-verified; a full run is destructive, so it's operator-paced.
3. The operator's rebuild-from-branch end-user test (the `refactor-go` hold gate).

## Constraints & decisions (carry-forward)

- Never merge `refactor-go` to `main` — user decision; `main` stays untouched.
- Go crypto/wire MUST be byte-exact against the Rust `core` oracle; the harness
  (`go test ./harness/`) is the release gate.
- `onboard` executes the shipped Rust `runner serve` as a subprocess (not in-process);
  `mcp::serve` is never reimplemented in Go.
- Bootstrap + teardown subcommands MUST work (user explicitly wants to test them);
  control-plane migration is a separate, later plan.
- Zeroize: explicit `Zero()` via `defer` on `[32]byte` secrets (no third-party zeroize crate).
- NIP-44 v2 implemented against the formal spec (HKDF + ChaCha20-Poly1305), harness-verified
  byte-for-byte against rust-nostr.
- `installer` stays Rust; it spawns `freehold-orchestrator` by name (15 refs), so the Go
  binary must keep that name and the subprocess contract.
- Session tooling note: file edits/commits land reliably only through `eval` (Python,
  absolute paths) — the write/edit/bash tools have been observed to splice corruption into
  files. Verify builds via `eval` running `go build ./...` / `cargo build --workspace` from
  `/home/darcy/Work/freehold/orchestrator` / repo root.
