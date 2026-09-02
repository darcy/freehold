# Chunk 3 — Shipped Scope

Status: COMPLETE. What remains — C0 (litellm-kube apply + Postgres + master-key
re-mint + the Services row), C1–C7, D1–D4, E1–E6, F1–F3, G1–G7, and the
carried Chunk 1–2.6.1 items — waits in `roadmap/POC.md`'s chunk sections.

## The Rust→Go refactor (orchestrator, installer, control-plane)

*   **The whole orchestrator/installer surface is Go.** `orchestrator/` is the
    Go module `freehold/orchestrator` (go 1.25), and both binaries —
    `freehold` (no args = the bubbletea TUI; `freehold <subcmd>` = the CLI)
    and `freehold-orchestrator` — are Go. `internal/` carries 18 packages:
    bootstrap, cli, client, config, console, crypto, delegate, deploy, drive,
    flows, harness, planebase, provisioner, relay, state, teardown, tui,
    wire.

*   **The Rust `core` (crypto/identity/wire) and `runner` stay Rust** as the
    byte-exact reference oracle. `orchestrator/harness/` (`harness_test.go`
    drives `target/debug/freehold-harness-oracle`, the Rust oracle crate) is
    the release gate: the Go crypto reproduces the Rust `core` surface
    byte-exactly — BIP-340 Schnorr via btcec/v2 (parity-negated scalar aux
    mask, gated by a parity-probe test), X25519+HKDF+ChaCha20-Poly1305
    sealed box, bech32 nsec, ed25519, `SecretPackage` as sorted-map JSON
    matching serde's `BTreeMap` output, NIP-98 canonical events, kind-48001
    audit, NIP-44 v2 engrams. `onboard` executes `runner serve` as a
    subprocess (never in-process); the MCP serve layer is not reimplemented
    in Go.

*   **The TUI is fully Go (bubbletea, full-screen alt-screen).** One activity
    surface for ALL long ops (`internal/tui/activity.go`, 548 lines, from the
    `send-msg`/`tui-daemon-combo` pattern): spinner + live label on the top
    line, ✓/✗ result rows (boot probes — config → runner → relay → cp → k3s →
    world state, 6s each) or a streaming last-12-lines window (subprocess
    runs), `ctrl+c` aborts — no dashboard, no shortcut footer, while active.
    The dashboard (Services/Agents/Runners/DATA views, Tab cycling, 2s timer)
    only renders when nothing is active. Subprocess runs stream line-by-line
    (the `freehold` binary re-execs itself, `activityExec` injectable for
    tests; child stdout+stderr piped through `io.Pipe` → `bufio.Scanner`, a
    re-arming `pumpActLine` Cmd per goroutine, owner-tagged
    `actLineMsg{a}`/`actDoneMsg{a}` dropping stale pumps).

*   **The rebuild engine runs the whole world.** `freehold rebuild`
    (`internal/cli/rebuild.go`) threads the stage set verbatim into Go:
    ensure_bins → provision (ssh keypair; reuse tolerated only with a real
    package) → door gate (interactive ENTER/r/q, `--yes` bails actionably
    with the key + install line) → grant → serve (`pkill` the stale
    listener, wait for the port to close, spawn detached, poll 20s) →
    verify door (self-subprocess exec) → **write initial config** (merge
    preserves surviving facts; AFTER verify, BEFORE storage, so the plane
    mapping has somewhere to record) → storage resolve + ensure×3
    (relay/cp/k3s-volumes; honors the recorded `plane.backend_kind` over
    re-detection — `TestManagedForFlags`, `TestWorldManaged` and
    `TestParsePctGateway` in `internal/cli/rebuild_test.go` anchor this
    region) → bootstrap relay → record LXC relay (fresh load → resolve
    vmid+ip through the runner → mutate → save) → bootstrap cp → record LXC
    cp → k3s stage (boot-if-missing + the 900s in-guest install script,
    verbatim) → deploy-relay (deploy dir from the guest's ACTUAL mounts) →
    deploy-cp (from the release binaries; state/bin dirs from the guest's
    last mount) → NIP-11 relay pubkey (best-effort) → final merge save.
    Every parsing helper (STORAGE-POOL/MOUNT/BACKEND lines, `pct list`
    exact-name vmid, eth0 ip, `pct config` mounts, NIP-11 pubkey) is a pure
    function with a hermetic test; the fresh-load/mutate/save record
    discipline is regression-tested against clobbering. `TestParseDnsList`
    lives in `internal/tui/tui_running_test.go` — there is no
    `TestPlaneStageNeverSkipped`: the plane stage is never skipped, because
    `ensure` is idempotent and runs every converge (`backend.is_some()` in
    the config is not proof the plane is live).

*   **The door gate lives INSIDE the activity view.** A rebuild bailing at the
    door (`--yes` can't prompt) exits non-zero with "the door needs …" on
    stdout → classified as an EXPECTED pause (`a.wait`), rendered YELLOW
    ("waiting for the operator") with the full install line; ENTER re-runs the
    SAME args in place (never back to the 6-field form), ESC cancels.
    `recoverDoorKey()` re-derives the door ssh PUBLIC line from
    `identity.json` + the sealed `secrets.json` (aad = secret name) when a
    reused package skips the gate but the key was never installed —
    `crypto.ExtractED25519PublicKeyLine` parses past the private half and
    never returns/writes it.

*   **Teardown keeps the config INTACT.** `internal/teardown/teardown.go`
    runs whole-world as COMPUTE teardown — destroys LXCs, KEEPS the recorded
    coords (operator-owned facts; no `PruneLxcCoords`), so a rebuild
    re-boots the SAME world — and `--data` adds the tenant datasets +
    `freehold-thin` before the door key/world home/config go LAST
    (`installer/src/teardown.rs` removes config; that behavior is gone).
    `stageLocalLvm` honors the plane: `ChownGuestUid` is NON-recursive (top
    dir only — PVE's own invariant; a recursive sweep re-roots every
    container-owned subtree and EACCESes redis/postgres/buzz).
    `--thin-pool` headless does adopt-if-present / carve at
    `--pool-size-gb`; a name typed verbatim adopts; `--confirm-storage` is
    required before a carve; ZFS + `--thin-pool` is an actionable error.
    `stagePlacement` (rebuild stage 7a) keys the plane placement off the
    `STORAGE-THINPOOL:` line the resolve stage emits.

*   **`freehold install` is a thin front-end to the same engine**
    (`internal/cli/install.go`): `collectAnswers` gathers
    domain/host/relay-gw/sizing/k3s into `rebuildFlags` and hands the SAME
    `newRebuildEngine(f).run()` the `rebuildCmd` uses; the ONE buffered stdin
    reader built during collect goes to the engine (`eng.stdin = ui.in`) so
    mid-pipeline prompts never lose bytes. Storage consent is gathered up
    front (same bool, same gate, flows as `f.confirmStorage`); `have-key`
    persists the operator's nsec as `identity.json` (their OWN nostr secret +
    a FRESH random enc keypair — `Identity::from_nostr_secret` semantics,
    derived-pubkey check, refuses when `identity.json` exists); `generate`
    REUSES an existing `identity.json`, else mints.

*   **Teardown is ACTIVE with checkboxes; the CLI streams.** `teardown.Run`
    announces each LXC BEFORE it destroys it (`destroying relay LXC 100`
    streams via `say()`/Live before `DestroyOneLxc`; the
    `destroyed / already gone / never created` contract holds), and the
    TUI seeds one slot per MANAGED LXC (`relay LXC 100` …) that flips to ✓
    as lines arrive; `tail()` keeps the last 3 non-empty lines so the
    embedded cause is never lost.

*   **Storage + CLI contracts.** `storage ensure` takes `--size-gb` /
    `--pool-size-gb`; `storage resolve` takes `--confirm-storage` and emits
    `STORAGE-THINPOOL: <name|->` (absence = ZFS; `stagePlacement` at rebuild
    stage 7a keys the plane placement off that line); all five storage
    subcommands register `addr`/`agent-dir`/`runner-pubkey`/`target`
    (`storage destroy-pool` had been the outlier); `prompt()` reads through
    one persistent `bufio.Reader` over stdin.

*   **Go↔Rust gates stay green.** `cargo build --workspace` clean, `cargo
    test --workspace` 0 fail, `go vet ./...` clean, `go test ./...` green
    (harness byte-exact gate + all packages); both Go binaries build from
    `target/debug/` (`go build -C orchestrator -o ../target/debug/…`), and
    `go test ./harness/` is the release gate for every primitive.

## Pre-C0 and Chunk 4 planning

*   **k3s is a deterministic CONFIGURE STAGE** (`freehold configure`,
    `internal/cli/rebuild.go`) — boots the k3s LXC (auto vmid, coords
    recorded to `lxc.k3s` + `managed += k3s`) and installs k3s inside with
    the spike-verified unprivileged posture (`INSTALL_K3S_EXEC="server
    --kubelet-arg feature-gates=KubeletInUserNamespace=true"` — without it
    the kubelet dies for lack of `/dev/kmsg`; even a device-cgroup allow
    does NOT materialize it under userns); an nginx pod serves 200 on pod /
    ClusterIP / NodePort, and 31500 from the PVE host. The dashboard lists
    the `managed` pieces (relay/cp/k3s today), and the agents registry
    records named AI agents (delegate-peer registers itself at start) with
    ●/○ availability from relay kind-9 presence.

## What remains

C0 (litellm-kube apply + Postgres + master-key re-mint + the Services row),
C1–C7, D1–D4, E1–E6, F1–F3, G1–G7, and the carried Chunk 1–2.6.1 items —
waits in `roadmap/POC.md`'s chunk sections.
