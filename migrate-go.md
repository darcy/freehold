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

### Phase 7 — World lifecycle (teardown/rebuild from the TUI): DONE
- **LVM lifecycle port** (`internal/drive/lvm.go`, 12 hermetic tests): full
  LVM-thin plane support. Defaults are the HALVED sizes proven live on the
  test PVE box: `TenantLVSizeGB = 10`, `FreshPoolSizeGB = 40` (the old Rust
  hardcoded 20/40). The live world's four tenant LVs
  (`relay-docker-root`, `relay-deploy`, `cp`, `k3s-volumes`) were resized
  20G→10G and all services revived (thin-provisioned — no physical VG space
  freed, just headroom).
- **Storage CLI rewired to the Rust contracts**: `storage ensure` gained
  `--size-gb` / `--pool-size-gb`; `storage resolve` gained `--confirm-storage`.
  All four storage subcommands + the bootstrap/teardown/storage drivers are
  hermetic-tested and live-verified.
- **`freehold rebuild` CLI** (`internal/cli/rebuild.go`): the whole-world
  bring-up pipeline — the Rust installer's stage set ported to Go, shelling
  the REAL sibling binaries (`control-plane` + `runner` stay Rust; self via
  `os.Executable()`), resolved relative to the running binary
  (`target/debug/*` + `../release/{control-plane,runner}` with the exact
  build one-liner when missing). Stage order: ensure_bins → provision (ssh
  keypair; reuse tolerated only with a real package) → door gate (fresh key:
  interactive ENTER/r/q, `--yes` bails actionably with the key + install
  line) → grant → serve (pkill the stale listener, wait for the port to
  close, spawn detached, poll 20s) → verify door (self-subprocess exec) →
  **write initial config** (merge preserves surviving facts; placed AFTER
  verify, BEFORE storage so the plane mapping has somewhere to record) →
  storage resolve + ensure×3 (relay/cp/k3s-volumes; honors the RECORDED
  `plane.backend_kind` over re-detection) → bootstrap relay → record_lxc
  relay (fresh load → resolve vmid+ip through the runner → mutate → save) →
  bootstrap cp → record_lxc cp → k3s stage (boot-if-missing + the 900s
  in-guest install script, verbatim) → deploy-relay (deploy dir from the
  guest's actual mounts, never hardcoded) → deploy-cp (from the release
  binaries; state/bin dirs from the guest's last mount) → NIP-11 relay
  pubkey (best-effort) → final merge save. Every parsing helper
  (STORAGE-POOL/MOUNT/BACKEND lines, `pct list` exact-name vmid, eth0 ip,
  `pct config` mounts, NIP-11 pubkey) is a pure function with a hermetic
  test; the fresh-load/mutate/save record discipline is regression-tested
  against clobbering (the Rust lib.rs test ported).
- **TUI teardown + rebuild flows**: running mode `t` = 1-field teardown form
  ("destroy tenant data too? yes/no") → `teardown --yes [--data]`, reload
  re-detects bootstrap mode; bootstrap + configure modes `B` = 5-field
  rebuild form (operator pubkey · domain · tenant LV size GB · fresh
  thin-pool size GB · boot k3s y/n) → `rebuild --yes …`, reload after.
  Both exec the freehold binary (self) like the existing forms. Footer +
  view hints wired and PTY-verified in both modes.

### Phase 8 — Plane-placement gate + teardown semantics (committed; round-4 fixes in 85732a7)
- **Plane-placement gate** (`rebuild`): the tenant LVs must land in a NAMED
  thin pool. `storage resolve` now also emits `STORAGE-THINPOOL: <name|->`
  (LVM-thin backend only; the line's ABSENCE = ZFS, where the gate skips).
  `stagePlacement` (rebuild.go, stage 7a) resolves it: `--thin-pool` flag
  headless (adopt-if-present / carve-at-`--pool-size-gb`), no flag + no
  detected pool → carve `freehold-thin`, `--yes` + detected → reuse; the
  interactive prompt offers `r`/blank = reuse, a name = carve, with a
  SECOND prompt for the carve size. Typing the detected pool's name verbatim
  adopts (no size prompt). ZFS + `--thin-pool` = actionable error.
- **`EnsureLvmLv` / `ResolveLvmMounts`** thread a `thinPool` arg: `""` reuses
  the VG's pool; a name is adopt-or-carve that exact name (Rust-parity
  command strings preserved byte-for-byte). Config records
  `plane.thin_pool` ONLY when rebuild CARVES — the guard that keeps `--data`
  teardown off reused stock pools.
- **`storage destroy-pool`** CLI + `drive.RemoveThinPool`: absent = no-op;
  refuses while LVs RIDE the pool (rider detection via `lvs -o pool_lv,lv_name`
  — per-pool, NOT a global LV count; a surviving pool's own LVs don't block);
  re-points PVE `local-lvm` to a surviving pool before removal, or leaves it
  (next rebuild re-points once the new pool is carved).
- **`stageLocalLvmRepoint`** (rebuild, carve path only): keeps PVE's
  `local-lvm` storage pointed at the carved pool so `pct create --rootfs
  local-lvm:…` resolves. Idempotent (probe skips already-correct).
- **Teardown semantics fixed** (`internal/teardown/teardown.go`): default
  whole-world = COMPUTE teardown — destroys LXCs, KEEPS config **INTACT**
  (LXC vmid+ip coordinates preserved — the `PruneLxcCoords` knob is GONE:
  recorded coords are operator-owned facts — proxy targets + static IPs out
  of the DHCP range — so rebuild re-boots the SAME world deterministically),
  world home `~/.freehold`, and the door key (rebuild reuses all three → no
  door gate, cheap rebuild). `--data` = FULL teardown: tenant datasets + the
  freehold-created thin pool, then door key + world home + config LAST. New
  `Runner` interface (Exec / DestroyOneLxc / DestroyDataset / DestroyPool)
  makes `Run` hermetically testable. Intentional divergence from the Rust
  oracle (`installer/src/teardown.rs` removes config) — do NOT "fix" it back.
- **`prompt()` fixed**: one persistent `bufio.Reader` over stdin — a fresh
  reader per call read ahead past the first newline, breaking back-to-back
  prompts (the carve size after the pool name).
- **TUI rebuild form** is now 6 steps (operator pk · domain · tenant LV size
  GB · thin-pool name · new thin-pool size GB · boot k3s), PTY-verified.
- Hermetic tests added: `drive/lvm_pool_test.go` (placement adopt/carve +
  RemoveThinPool rider/survivor/re-point semantics, 8 tests),
  `cli/rebuild_placement_test.go` (parseStorageThinPool 3-way, parseGB,
  stagePlacement ×11), `teardown/teardown_test.go` (compute-keeps /
  data-removes / pool-guard / tenant-scopes / confirm, 6 tests).
  `go build ./... && go vet ./internal/... && gofmt -l . && go test ./...`
  GREEN; both Go binaries rebuilt.
- **TUI door-gate waiting state** (post-first-run fix): the TUI's `B` form
  runs `rebuild --yes` as a subprocess; a fresh provision mints a NEW door
  key and `--yes` bails actionably (a subprocess can't prompt the operator).
  That pause is EXPECTED, not a failure — the TUI now detects it
  (`doorKeyWaiting` on "needs a NEW ssh key"), renders it yellow as
  "waiting for the operator" with the full install line (the OLD behavior
  painted it red and showed only the truncated tail), and clears it on the
  next flow/error. Both CLI bail texts now read "re-run rebuild" (the
  parenthetical TUI hint is gone — the TUI owns the retry mechanic, below).
- **Door-key recovery on reuse + auth failure** (the "pressed B twice before
  installing the key" trap): a second `rebuild` sees the existing package →
  provision reuses → the door gate is SKIPPED (no fresh key) → `stageVerify`
  fails with `ssh error: authentication failed`, and the key printed on the
  first run was gone. Fix: the door ssh PUBLIC line is re-derivable from the
  runner package itself — `identity.json` holds the runner's own X25519
  enc key, `secrets.json` holds the door PEM sealed TO it (aad = secret
  name), exactly the runner's boot path — so `recoverDoorKey()` unseals it,
  parses the openssh-key-v1 container, and emits ONLY the public line
  (`crypto.ExtractED25519PublicKeyLine`: private half parsed past, never
  returned/written — nothing new leaves the machine). `stageVerify` on an
  auth refusal now bails with the same shape as the fresh-key gate
  ("the door needs its ssh key..."); the TUI's `doorKeyWaiting` marker is
  the shared "the door needs" prefix so BOTH pauses render yellow.
  Unrecoverable package → the actionable
  `rm -rf ~/.freehold` fresh-start message. Tests: PEM round-trip,
  sealed-package recovery, stageVerify re-surface + non-auth passthrough,
  unrecoverable bail.
- **In-place door retry — the Rust loop in the TUI** (supersedes "press B
  again"): a rebuild paused at the door gate keeps the operator INSIDE the
  gate. The pause carries the EXACT rebuild args (`flowMsg.rebuildArgs` →
  `Model.rebuildArgs`); the TUI renders the key + "install the key, then
  press ENTER to re-test the door and resume — esc to cancel"; ENTER re-runs
  the SAME `rebuild --yes` in place (stages are idempotent: provision REUSES
  the package → no new key, grant/serve re-run, `stageVerify` re-probes the
  door and, on success, the pipeline CONTINUES through config/storage/deploys)
  — never back to the 6-field form. ESC cancels the pause (no error); q /
  ctrl+c still quit; every other key is swallowed at the gate. A second
  pause (key still not installed) re-arms the gate with the same args.
  Shared classifier `rebuildRun(args) flowMsg` used by both the form
  dispatch and the ENTER handler. Tests: `TestDoorGateEnterRetry` (ENTER
  dispatches + never reopens the form, re-pause re-arms, ESC cancels clean),
  `TestDoorGateSwallowsKeys` (B at the gate opens no form),
  `TestDoorKeyWaitingRenderedNotError` extended (args kept on wait, cleared
  on error/beginPrompt, ENTER hint rendered).
- **First LIVE whole-world rebuild (2026-08-29)**: the pipeline ran end to
  end on the real box (door pre-installed → no gate pause; VG `pve` had no
  thin pool → carved `freehold-thin` at 120 GB; relay 100 / cp 101 / k3s 102
  booted + recorded; relay stack healthy; CP serving with the operator admin
  seed; k3s active with kubeconfig). The live run exposed + fixed 4 bugs,
  each with a regression test or live proof:
  1. `parseKind` returned a NON-NIL action on `Reuse`, but every caller
     treats non-nil as "NO existing backend" — the very first `storage
     ensure` (nothing recorded yet, no `--kind`) always failed with
     "no storage backend to ensure onto" on a host that HAS a VG. Now
     Reuse returns `nil` action (the caller drives the detected kind).
  2. The operator pubkey was stored raw — the Rust installer runs
     `parse_pubkey_input` BEFORE the pipeline; `deploy-relay`/`deploy-cp`
     reject non-hex at stages 13-14. `newRebuildEngine` now normalizes
     npub→64-hex up front (parity with installer::main.rs).
  3. `stageLocalLvmRepoint` used `pvesm set local-lvm --thinpool` —
     REJECTED live ("Unknown option: thinpool"; thinpool is not mutable
     via pvesm's API), and its probe greped a colon form that PVE's
     whitespace `storage.cfg` never matches. Now: scoped in-place
     `storage.cfg` edit (PVE's sanctioned manual repair), whitespace
     probe + post-edit readback. Tests: carve / already-correct skip /
     missing-block error.
  4. `firstField` panicked on the blank trailing line of `pvesm list
     local` (`Fields("")+" "` = empty slice → `[0]`). Now returns "".
     Test: `TestFirstFieldBlankLine`.
- Live world coords after the run: relay LXC 100 @ 192.168.30.225 (docker
  stack healthy, `/_liveness` ok, NIP-11 self `dee88751…` recorded as
  `relay_pubkey`), CP LXC 101 @ 192.168.30.205 (:8080, admin = operator
  1dc07610…), k3s LXC 102 @ 192.168.30.253 (k3s active, kubeconfig
  present). Open tail: the operator's truenas proxy still forwards the
  domain hostnames to the PREVIOUS world's guest IPs (.238/.254) — guests
  are live direct on the LAN; the proxy repoint is operator-side.
- **Static-IP wiring** (`cli/rebuild.go`): `--relay-ip`, `--cp-ip`, `--k3s-ip`
  (CIDR) + `--relay-gw` (default `192.168.30.1`). `bootstrapStaticIP(role,
  flags, cfg)`: explicit flag wins → else the RECORDED `lxc.<role>.ip` rides
  again (Rust `Answers::from_config` parity) → else `""` = DHCP. `stageBootstrap`
  passes `--lxc-ip/--lxc-gw` when static AND reuses the recorded `vmid` on
  resume (reuse path finds the existing guest by hostname instead of picking
  a new id and refusing the collision; Rust parity re-booted the same vmid).
  CIDR validated fail-fast (host + prefix) in `RunE` BEFORE `newRebuildEngine`
  — a bare host would only die deep at stage-7 `pct create`. Tests:
  flag-wins / recorded-fallback / none-is-DHCP.
- **Persistent-plane recursive-chown bug — root-caused + fixed (the
  deploy-relay EACCES cascade).** Stage-3 mount resolution ran `chown -R
  100000:100000 <mount>` UNCONDITIONALLY (the comment claimed "Fresh ext4 is
  root-owned" but nothing gated on freshness). On a rebuild over the
  SURVIVING plane (compute-only teardown keeps the LVs) that recursive sweep
  re-rooted every container-owned subtree — docker volumes with per-service
  uids (redis 999, postgres 70/100070 on host, buzz 1000) → guest root —
  and every non-root service EACCESed: redis MISCONF/BGSAVE, postgres
  `pg_filenode.map`, relay `git pack cache ... Permission denied` panic.
  Redis's docker-entrypoint self-heals on restart (proven live: re-root
  `/data`, restart, entrypoint chowns back); postgres/buzz don't. ctime
  forensics (all four volumes, one sweep, nanosecond-identical, exactly at
  the stage-3 moment) pinned it; PVE itself only chowns newly ALLOCATED
  volumes, non-recursive, root-of-volume (`LXC.pm::create_disks`) — bind-mp
  dirs are untouched by PVE, so the sweep was ours. FIX: `ChownGuestUid` is
  now NON-RECURSIVE (top dir only, matching PVE's own invariant: the guest
  needs the mount ROOT writable; the subtree belongs to the container
  stack). Live repair applied in-place: git volume `chown buzz:buzz` via the
  deploy's own `gitDataChownCmd`, postgres dir to uid 70, redis self-healed;
  no LXC/volume destroyed. Regression test
  `TestResolveLvmMountsSurvivingPlaneNeverRecursiveChowns` (surviving
  relay plane: non-recursive root chown, zero `chown -R` / lvcreate / mkfs /
  re-mount churn) + the relay/zfs assertions rewritten to the top-dir form.
- **Second LIVE rebuild — green end to end with STATIC IPs (2026-08-29).**
  `rebuild --yes --relay-ip 192.168.30.8/24 --cp-ip 192.168.30.9/24` passed
  all 10 stages + record on the SURVIVING plane (stages 0–7 idempotent;
  deploy-relay + deploy-cp green after the ownership fix). Live coords:
  relay LXC 100 @ **192.168.30.8** (4/4 containers healthy, `/_liveness`
  ok), CP LXC 101 @ **192.168.30.9** (:8080, NIP-98 gate), k3s LXC 102 @
  **192.168.30.7** (k3s active), all recorded in config. Operator action:
  repoint the truenas proxy upstreams to `.8:3000` (relay) and `.9:8080`
  (CP) — the domain hosts still 502 until then.

  k3s addressing note (2026-08-30): the `.213` in earlier rebuilds was
  DHCP-learned at boot and only recorded after the fact; it was NOT pinned
  as static (config had no k3s ip, so the recorded-ip reuse didn't apply
  yet). k3s is reachable only inside the operator's network — no proxy
  upstream points at it — so it does not REQUIRE a static IP for the
  world to function. But a static-out-of-DHCP IP keeps the rebuild
  deterministic (a known API endpoint, no DHCP-range collision if the
  relay/CP rebuild ever re-records a learned address), so it was set to
  **192.168.30.7/24** per operator. `.7` was verified free (no arp,
  ping-free) before assignment, and the relay/CP proxy upstreams are
  unaffected (they stay at `.8`/`.9`).

- **Full-screen activity view — the TUI never stares at a blank dashboard
  while the world changes (2026-08-30).** New `internal/tui/activity.go`
  (~440 lines). ONE activity surface for ALL long ops — boot check,
  teardown, rebuild (incl. the door gate), bootstrap, both deploys.
  Modeled on bubbletea's `send-msg` / `tui-daemon-combo` examples (the
  user's pick after the package-manager sketch). While active it REPLACES
  the entire UI: spinner + live label on the top line, ✓/✗ result rows
  (boot probes) or a streaming last-12-lines window (subprocess runs),
  `ctrl+c aborts` at the bottom — no dashboard, no shortcut footer.
  Subprocess runs stream line-by-line: the freehold binary re-execs itself
  (`os.Executable()`; `activityExec` var is injectable for tests), child
  stdout+stderr piped through `io.Pipe` → `bufio.Scanner`, and a
  re-arming `pumpActLine` Cmd reads one line per dispatch (each Cmd on its
  own goroutine, so a blocked read never stalls the loop). Owner-tagged
  msgs (`actLineMsg{a}` / `actDoneMsg{a}`) drop stale pumps from
  superseded activities; `a.done` is the dedupe guard.
- **Door gate lives INSIDE the activity view.** A rebuild bailing at the
  door (`--yes` can't prompt) exits non-zero with "the door needs …" on
  stdout → classified as an EXPECTED pause (`a.wait`), rendered yellow
  ("waiting for the operator") with the install line; ENTER re-runs the
  SAME args in place (fresh subprocess activity — never back to the
  6-field form), esc cancels. The old dashboard-level `Model.Wait` /
  `Model.rebuildArgs` / `flowMsg.reload` machinery is DELETED. `flowMsg`
  is now just `{ok, err}` (login flows only). Tests: door-pause rendered
  not error, ENTER in-place retry + re-arm + esc, gate swallows stray
  keys, activity replaces dashboard chrome, streamed lines land in order.
- **Boot check is the activity view too.** `load()` is now FAST (config
  file only, no network; `Mode=ModeRunning` placeholder), `Init()` chains
  `startBootActivity` — sequential probes (config → runner → relay → cp →
  k3s → world state, bounded 6s each) streamed as ✓/✗ rows, then the mode
  settles (converged = running, else configure/bootstrap). `r` re-runs it;
  the post-run re-check rides on activity completion.
- **Third LIVE rebuild — through the ACTIVITY VIEW (2026-08-30).** The
  destroyed world (LXCs gone, config kept INTACT per above) was re-booted
  from the TUI: `B` form (operator pk + domain, coords blanks ride the
  recorded config) → `activityStartMsg` → full-screen streaming rebuild
  (door pre-installed → no gate pause) → "✓ rebuilding — done" → done-key
  → boot re-check. All three LXCs came back on the RESTORED coordinates:
  relay 100 @ `.8` (4/4 containers healthy, `/_liveness` ok through the
  domain), CP 101 @ `.9` (healthz 200), k3s 102 @ `.7` (k3s active);
  config re-recorded vmid 100/101/102 + `.8/.9/.7` + the SAME
  `relay_pubkey` — the same world, re-booted.
- **Fourth LIVE rebuild — the fast REUSE path, 4 min 15 s vs ~70 min fresh
  (2026-08-30).** A repeat teardown (config kept INTACT, plane LVs intact)
  then `rebuild --yes` skipped every expensive re-provision: door REUSED
  (no gate pause — the key stayed installed), plane adopted, and the three
  LXCs re-booted on the recorded vmids + static IPs (100/.8, 101/.9,
  102/.7) — relay 4/4 healthy + `/_liveness` ok through the domain, CP
  healthz 200, k3s active. This is the design payoff of "teardown keeps
  config INTACT + plane-reuse": a destroyed-but-recorded world re-boots in
  minutes, same coordinates, same relay pubkey.
- **PTY lesson (operator-tooling, not a TUI bug):** while a session was
  driving the live TUI rebuild, a stray `tail -c /proc/<tui-pid>/fd/1`
  opened the PTY SLAVE and raced bubbletea for stdin bytes — the TUI went
  input-deaf (looked frozen). The goroutine dump showed the event loop
  IDLE in `epoll_wait` (alive, not deadlocked); a fresh session took keys
  fine. NEVER read the TUI's PTY slave fd while it runs; observe via the
  hub's `logs` only.

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
| `6d7719a` | Phase 8: placement gate + RemoveThinPool + teardown semantics + door-key recovery |
| `85732a7` | Review round 4 fixes: rider guard, storage.cfg re-point, placement created probe |

(earlier: phase 2–4 port commits 9f23609, f59373e, 80609a7, e92d574, 5c2377d)

## Remaining work

The plan is COMPLETE on `refactor-go`. Only operator-driven live exercises remain
(these are the end-user testing the branch is held back from `main` for):

1. Full `bootstrap` end-to-end on a scratch target (proxmox-lxc pct create through
   the real runner) — the drivers are ported and CLI-wired; the exec probe,
   readiness, and storage paths are already live-verified.
2. `teardown` from the TUI (`t` in running mode) — engine ported + CLI-wired;
   door probe live-verified; a full run is destructive, so it's operator-paced.
3. ~~The operator's whole-world teardown → `rebuild` end-user test from the
   TUI (`t` then `B`) — the hold gate.~~ **DONE (2026-08-29)** — the live
   rebuild above; the pipeline's four live bugs are fixed + regression-tested.
4. Operator-side: re-point the truenas proxy upstreams at the current
   guest IPs (relay `.8:3000`, CP `.9:8080`) — the ONLY remaining step
   before the domain URLs work.

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
