# The AI-operated Appliance — Architecture

Product: an open-source "box + install script" (Omarchy-style) that lands a
Proxmox VE / VPS + Kubernetes stack with Buzz Relay as the control plane and
a skill framework that installs and configures self-hosted OSS. For the
current chunk numbering (Chunk 3 = the Rust→Go refactor, done; Chunk 4 = a
real, reasoning CPA in Buzz; Kubernetes arrives with Chunks 6–7), see
`roadmap/ROADMAP.md` and `roadmap/POC.md`. Narrative: "reclaim the future we
were promised" — the full rationale lives in `VISION.md`.

## System layout

```
Operator ──chats via──► Buzz relay (Buzz-operated; host: self-hosted LXC/VM or VPS)
                           │  CPA + expert agents live in Buzz
                           │  chat / memory (30174) / audit (48001) / jobs (43001–6)
                           │
              tools ▼      │   ▲ agents connect with their OWN keypair
        ┌──────────────────┘   │   (gates: signature, audience, expiry, replay)
        ▼                      ▼
   freehold-orchestrator / freehold-runner (Go, independent)
        │  single `exec` MCP tool (runShell) — dumb privileged hands
        │  signed, addressable, audited
        │  NO LLM, NO router, NO key vault in either
        ▼
   Target: pct LXC (relay/CP) | qm VM (VPS host) | k8s (MVP only)
        │
        └──backups──► PBS (scheduled vzdump of pct/qm) | TrueNAS | Backblaze B2
```

*   **One control plane = exactly ONE relay scope** (relay-as-scope). The CP
    lives on its own target (a dedicated LXC/VM, a VPS, or a VM with k8s in
    v1) and *attaches to* the relay — co-location is convenience, never
    assumed. A user's existing relay is onboarded as a service (via a relay
    runner), not a nested scope. No "control plane of control planes."

*   Deterministic k8s agent pods (bare Pods, digest-pinned sprig,
    per-attempt envFrom Secret, no mgmt channel by design, idle auto-reap,
    emptyDir, no PVC v1) — **arrive with Chunks 6–7** (see `roadmap/POC.md`).

*   **Agent placement:** POC (Chunks 1–3) = scripted agents joining the relay
    via their own NIP-42 client (the `buzz-acp` harness targets LLM agents
    and is the Chunk-4 path — see `roadmap/POC.md`); Chunks 6–7 = k8s pods.
    Runners are separate — see the Runners section below.

*   **Runner identity** = Nostr keypair (membership/signing) + a *separate*
    encryption keypair (env-injected / mounted secret, never committed).
    Agents connect with their *own* keypair; secrets are encrypted for the
    runner's encryption key.

*   **CP = secret PROVISIONER, not a vault:** encrypt to runner-key → ship
    ciphertext → inject the runner private key → rotate. No master key.
    Runner holds only ciphertext + its own key; decrypts in its own memory,
    uses in memory, forgets. Plaintext never on disk, never in agent context.

*   **Grants are coarse:** agent ↔ runner (whitelist of Nostr pubkeys);
    dedicated runner per service by default. Readiness = the runner's own
    self-check (🟢 all checked / 🟡 some checks missing / 🔴 none).

*   **The runner lifecycle rides native Nostr kinds, not custom ones:** a
    runner is a private NIP-29 channel; grant/revoke is channel membership
    (kinds 9000/9001); the runner's live whitelist is the relay's own signed
    roster (**kind 39002**), read fresh per call, fail-closed on relay
    outage. No relay fork, no custom 30073/30078.

*   **CPA + experts live in Buzz:** the CPA is a real reasoning agent (the
    system's main user touchpoint); experts are deterministic or
    reasoning-class. The CPA gets its purpose from `CPA_SYSTEM_PROMPT.md`.

*   **Host-flexible:** Proxmox is the lead/default; VPS/cloud are first-class
    (the business path). The k8s layer (Chunks 6–7) and everything above the
    host driver run identically regardless of substrate.

*   **Backups are the load-bearing wall — and most teams won't have a PBS
    server:** the *whole* backup chain (LXC/VZ+T pl8755, KVM+PBS) depends on
    a PBS host. Where one doesn't exist, the durable half of the
    `/srv/data` convention must land on TrueNAS and/or Backblaze B2.

*   **The durable half lands FIRST (Chunk 3; see `roadmap/POC.md`):**
    `/srv/data` — everything that survives rebuild: `/srv/data/relay` (all
    relay deploy data: config, CA, keypairs), `/srv/data/cp`
    (`secrets.json` + `providers.json`), `/srv/data/k8s-volumes` (k8s
    storage, Chunks 6–7). **Nothing in the POC needs a cluster**
    (`roadmap/ROADMAP.md`), and even in MVP most services aren't k8s
    workloads: the relay/CP are LXCs, with LiteLLM/Postgres as k8s
    Deployments only in v1.

*   **The backup rule is the split, implemented as MOUNT POINTS with explicit
    flags.** `/srv/data` (and its tenants) and `/srv/nobackup` are `mpN:`
    volume mounts, never plain rootfs directories — and the flag must be set
    on EVERY entry, because vzdump's default excludes volume mount points.
    freehold lands each tenant dataset as its own mount (`mp0: …,
    mp=/var/lib/docker,backup=1`, `mp1: …,mp=/srv/data/relay,backup=1`,
    `mp2: …,mp=/srv/data/cp,backup=1`, …), which is the same rule applied per
    tenant rather than as one shared `/srv/data` mount; a future reproducible
    half gets `mpN: …,mp=/srv/nobackup,backup=0` (never in the PBS job). On
    a box that can't add a second mount point, `vzdump --exclude-path
    /srv/nobackup` is the rootfs fallback.

*   **Container stores relocate under `/srv/nobackup`**
    (`docker`/`containerd`/`rancher` daemon roots), mirroring the existing
    "exclude `/var/lib/docker`" Docker-host LXC pattern — but by layout, not
    by config list. **Carve-out — the buzz relay keeps its daemon root at
    `/var/lib/docker`, backed up (`backup=1`):** the relay's
    Postgres/Redis/MinIO/git data are docker *named volumes* under
    `/var/lib/docker/volumes/`, and freehold ships no buzz patch, so
    relocating it would silently exclude the relay DBs from backup.

*   **Durable PVCs are pinned under `/srv/data` — never the daemon root.**
    k3s's default `local-path` provisioner stores PVCs *under the rancher
    root* (`/var/lib/rancher/k3s/storage/…`); relocating that whole root
    would drag the control_plane Postgres PVC — the thing every
    "reconstructible from" claim depends on — into the excluded half,
    silently. Configure provisioner roots explicitly: durable ones
    (`control_plane`) pin to `/srv/data/k8s-volumes`; disposable ones ride
    the `nobackup`-rooted storage class.

*   **The CP host itself must be reconstructible, not durable:** booting a
    fresh target on *different* hardware (and even a different distro) from
    the recorded config + the durable `/srv/data` mounts is the whole point.

## The pieces

### `freehold-core` (Rust; the contract oracle)

* **Purpose:** the language-agnostic trust, crypto, and wire contract:
  keypairs, sealed boxes, canonical events, `SecretPackage` JSON, and the
  `RunnerCall` envelope. The Rust `core` (crypto/identity/wire) and `runner`
  stay in the tree as the **byte-exact reference oracle** for the Go port;
  the shipped surface is Go: `freehold/orchestrator` (the `freehold` and
  `freehold-orchestrator` binaries), `freehold/installer`, and
  `freehold/control-plane` (all Go 1.25 modules).

* **Contents:** NIP-44 v2 encryption (chacha20poly1305, bech32, hkdf-sha256,
  sha256, hex) and the signer (`CryptoProvider` over `CryptoDyn` —
  `secp256k1` + `ring` only; `bip39`, `bs58`, and `keyring` are unused).

* **Rust→Go port status:** the Go `internal/crypto` reproduces the Rust
  `core` surface **byte-exactly** — verified by
  `orchestrator/harness/harness_test.go`, which drives the Rust
  `freehold-harness-oracle` binary and locks every primitive against it
  (BIP-340 via btcec/v2, X25519+HKDF+ChaCha20-Poly1305, bech32 nsec,
  ed25519, `SecretPackage` as sorted-map JSON matching serde's `BTreeMap`
  output, NIP-98 canonical events, kind-48001 audit, NIP-44 v2 engrams).

* **`RunnerCall`:** `{call_id, ts, nonce, issuer, audience, tool, args, sig}`
  (full schema + per-field notes in `RUNTIME_CONTRACT.md`).

* **`SecretPackage`:** `{secrets:[{name, ciphertext(nonce‖ct), nonce,
  aad:"<name>", alg:"sealed-ex-nopb-hkdf-sha256-cha20p1305", tags,
  issuer, audience:[...], active, created}]}`.

* **`identity.json` / `providers.json`:** identity holds `nostr_secret`
  (nsec) + `enc_secret` (nenc), both encrypted under the *plaintext*
  `enc_secret` key; `providers.json` (control-plane only) holds opaque
  `params` per connector.

* **`FreeholdRuntime`:** `run_call` (single funnel), `resolve_target`
  (pure: parse `TargetId`, load auth, map `pve:vm:<id>` → `{target_id,
  transport, ssh, env}`), `verify_envelope` (verify-only; replay cache, 60s
  expiry). `internal/runner` (`freehold-runner`) runs `run_call` over one
  persistent `*http.Client` + `proxy.Dialer`; `serve.go` calls
  `RunCallWithDial` and `runShell` holds `cmd.Context()`.

* **`teardown.go` and `prune_lxc_coords` keep the COMPUTE/data split**
  (`DestroysLxc` keeps config; `DestroysData` erases it).

### `freehold-orchestrator` (Go; the single privileged hand)

*   **The whole orchestrator/installer surface is Go.** `orchestrator/` is
    the Go module `freehold/orchestrator` (go 1.25), and both binaries —
    `freehold` and `freehold-orchestrator` — are Go. `internal/` carries 18
    packages: bootstrap, cli, client, config, console, crypto, delegate,
    deploy, drive, flows, harness, planebase, provisioner, relay, state,
    teardown, tui, wire.

*   **`orchestrator` = the dumb privileged hand.** A dumb, privileged MCP
    tool server (`internal/delegate` + `internal/cli`): `tools/list` returns
    *exactly one* entry, `"exec"`; `tools/call` runs the command *verbatim*
    and streams stdout/stderr.

*   **MCP server, not a proxy.** `internal/delegate/mcp.go` serves the
    `freehold-delegate` MCP server with the single `exec` tool;
    `cmd/freehold-delegate` (alias `cmd/freehold` → `freehold`) and
    `cmd/freehold-runner` both wire `delegate`'s `RunCall`.

*   **`run_call(call, rawJSON)` → `{stream:[stdout…, stderr…]}`.** It parses
    the `RunnerCall`, verifies the signature over canonical bytes, checks
    nonce/expiry, looks up issuer in the roster (fail-closed), selects the
    connector, and executes the `exec` command verbatim.

*   **A stream is a property of `exec`.** `tools/call` with `stream=true`
    yields `content[0].text`, a JSON array of interleaved
    `["stdout"|"stderr", line]` pairs; `false` yields a single
    `{stdout, stderr, code}` object.

*   **Gates:** signature validity against the roster, audience binding,
    `ts + timeout_s` expiry, and nonce replay — each a separate rejection
    path; `identity.json` → `identity.priv`.

*   **`readFile` is NOT a tool.** Any agent that can `exec` can `cat` a
    file; the only way to erase a capability is to revoke it from the
    channel (`RevokeRunner` → 9003).

*   **`FreeholdRuntime` (above) is the contract oracle:** `run_call` is the
    single funnel, `resolve_target` stays pure, and `verify_envelope` guards
    replay in memory only.

*   **The Go engine reads the contract verbatim.** `newRebuildEngine` threads
    the stage set without interpretation (`internal/cli/rebuild.go`);
    `collectAnswers` → `rebuildFlags` hands `install` the same engine
    (`eng.stdin = ui.in`); `runReconstruct` does `stateFromRunner` →
    `resolve` → `ensure` → `bootstrap relay` → `bootstrap CP`.

*   **The CLI's single stdin is load-bearing:** `prompt()` reads through one
    persistent `bufio.Reader` over `cmd.InOrStdin()`; a new mid-pipeline
    prompt never opens a second reader.

*   **`internal/tui` (`freehold`) and `cmd/freehold-orchestrator` are the
    operator's only UI.** `internal/tui/activity.go` (548 lines) provides
    one activity surface for ALL long ops (the `send-msg`/`tui-daemon-combo`
    pattern): spinner + live label on the top line, ✓/✗ result rows (boot
    probes — config → runner → relay → cp → k3s → world state, 6s each) or a
    streaming last-12-lines window (subprocess runs), `ctrl+c` aborts — no
    dashboard, no shortcut footer, while active.

*   **A rebuild bailing at the door is an EXPECTED pause** (`a.wait`,
    rendered "waiting for the operator" and YELLOW): `main.go` maps
    `!GateOpen` to `ErrWait` → `ActionWait` → `model{wait: m.act, …}`;
    ENTER re-runs the SAME args in place (never back to the 6-field form),
    ESC cancels.

*   **`recoverDoorKey()` re-derives the door ssh PUBLIC line** (and only the
    public line) from `identity.json` + the sealed `secrets.json`
    (aad = secret name) when a reused package skips the gate but the key
    was never installed; `crypto.ExtractED25519PublicKeyLine` parses past
    the private half and never returns/writes it.

*   **`storage.go` honors `plane.backend_kind`; `resolve.go`/`ensure.go`
    honor it.** The plane stage is NEVER skipped — `ensure` is idempotent
    and runs EVERY converge (`backend.is_some()` in the config is not proof
    the plane is live). `TestManagedForFlags`, `TestWorldManaged` and
    `TestParsePctGateway` in `internal/cli/rebuild_test.go` anchor this
    region.

*   **Teardown (Chunk 3): whole-world is COMPUTE teardown.**
    `internal/teardown/teardown.go`'s `Run` destroys LXCs, KEEPS the
    recorded coords (operator-owned facts; no `PruneLxcCoords`), so a
    rebuild re-boots the SAME world — and `--data` adds the tenant datasets
    + `freehold-thin` before the door key/world home/config go LAST
    (`installer/src/teardown.rs` removes config).

*   `stageLocalLvm` honors the plane: `ChownGuestUid` is NON-recursive (top
    dir only — PVE's own invariant; a recursive sweep re-roots every
    container-owned subtree and EACCESes redis/postgres/buzz).

*   **The teardown TUI is ACTIVE with checkboxes.** `Run` announces each LXC
    *before* it destroys it (`destroying relay LXC 100` streams via
    `say()`/Live before `DestroyOneLxc`; the `destroyed / already gone /
    never created` contract holds), and `freehold/teardown`'s `state.go`
    seeds one slot per MANAGED LXC (`relay LXC 100` …) that flips to ✓ as
    lines arrive; `tail()` keeps the last 3 non-empty lines so the
    embedded cause is never lost.

*   **No `vm_snapshot`/`vm_rollback`/`vm_restore`/`vm_exec`/`pct_*` tools**
    in `run_call` — all of these are `exec`.

*   **`internal/state` is a file store** (`secrets.json`, `providers.json`):
    `state.go: managedStore{root}` at `/srv/data/cp`, never inside the
    read-only module (`store.go`'s `managed: true`).

*   **A crash-dump symbolized on the target** (`coredumptl` /
    `coredumpctl copy` → `internal/tui/coredump.go`) is evidence of a real
    problem, never a fallback.

### `freehold` TUI (bubbletea; the operator's console)

*   **It is bubbletea, not HTML.** `internal/tui/tui.go` is full-screen
    alt-screen: `tea.NewProgram(m, WithAltScreen(), WithMouseCellArea(…))`.

*   **Four views** (`<Tab> next view`, `1` Services, `2` Agents, `3`
    Runners, `4` DATA): `newDashboardModel()` (services),
    `newAgentModel()` (agents), `newRunnerModel()` (runners),
    `newDataModel()` (DATA); the dashboard renders ONLY when nothing is
    active (`state.go` → `actStepMsg{kind: "done"}`) — it never scrolls
    under an open activity.

*   **The six auth fields** (`domain`, `pve_host`, `pve_user`, `pve_pass`,
    `relay_gw`, `sizing`) are collected once, in order (`stepAfter` is
    `func(int) int { return 1 }`; `nextStep` is unreachable — enforced).

*   **Nothing may hardcode `local`** (`source.go: func Default`,
    `func Resolve`); the store is a `managed` store, so a machine with no
    recorded `mp*` mounts is *not* an authenticated appliance yet.

*   **`identity.json` and `providers.json`** (with `params`) live in
    `/srv/data/cp`; they are freehold state, never part of the read-only
    `go:embed` module.

*   **No secrets ever ride the wire or enter an agent's context**
    (`mcp.go: func (h *Handler) callTool`); the `exec` stream frames
    (`//> …` / `<! …`) carry only command text and stdout/stderr lines.

*   **`<Tab>` cycles** `services → agents → runners → data → services`;
    **`2` (or clicking) goes straight to Agents**; there is no `3`/`4`.

*   **`<Ctrl+C>` aborts** mid-run (the tea ctx ends `RunShell`; the
    process group is killed with `SIGKILL`, and `Wait` waits for pipes);
    `[Q]` quits (`cmdQuit` → `tea.Quit`).

*   **`[Enter]` continues** and re-runs the SAME args in place
    (`runShell`); it never returns to the 6-field auth form.

### `CPA_SYSTEM_PROMPT.md` (the CPA's purpose)

*   **`CPA_SYSTEM_PROMPT.md`** (at `/srv/data/cp` and baked into
    `freehold-orchestrator`'s `agents/<name>/AGENT.md`) — not a Go file and
    not a "CPA host / LLM routing" spec. It says:

*   The CPA is a **reasoning agent that lives in Buzz** and is the system's
    **main user touchpoint**; it runs on the same `goose`/`omp` harnesses as
    the expert agents it creates.

*   It **delegates** expert work — drives a `target`'s `Runner` via `exec`,
    or hits an **API directly** — instead of doing expert-level work itself.

*   **It never sees plaintext secrets**, never writes secrets to files, and
    references credentials by name only.

*   `## Example: "Create a VM with Nextcloud"` — "Create a Nextcloud
    instance" / "Back it up."

*   **`run_call` returns `{stdout, stderr}`** (or `{error}`).

*   **The `exec` tool** (`{cmd, target, stream?}`) is the ONLY one.

*   **The `exec` stream** is a `tools/call` to `freehold-orchestrator`
    (`{"stream": true}` → `result.content[0].text` = JSON array of
    `["stdout"|"stderr", line]` pairs).

*   **Every long-running operation** — a `run_call`, or a subprocess like
    `install`/`rebuild`/`teardown` — gets **one activity view** that is
    ALWAYS the top line: `spinner + live label`, then `✓/✗ result rows`
    (boot probes) or a `streaming last-12-lines window`.

*   **The agent must NEVER:** re-derive a secret from `identity.json`, put a
    secret in a URL, type `ssh host 'nc …'`, or hand a credential to a
    container via `--env`.

### `freehold-control-plane` (Go; the provisioning brain)

*   **The `control-plane` module is `freehold/control-plane`, not a Rust
    crate** — `internal/provisioner` (`provision.go`, `secrets.go`,
    `providers.go`) and `internal/client` (`litellm.go`, `openrouter.go`,
    …) live only under `orchestrator/`.

*   **The `control-plane` binary's whole job is the LLM plane.** It
    *provisions* providers and *seals* credentials into a
    `SecretPackage`-shaped `secrets.json`; **there is no
    `cp provision-provider`, no `cp rotate-secret`, no `cp
    list-secret-names`.**

*   **`providers.json` (at `state/providers.json`)** and **`secrets.json`
    (at `secrets.json`)** are read-only to the `freehold` TUI/CLI and
    written ONLY by the `control-plane` CLI.

*   **`provisioner.go: func (f *Flags) Provision`** runs the whole
    sequence — `resolve` → `writeSecrets` → `recordPackage` → `relaunch` —
    in ONE pass; `rotate.go`'s `Rotate` *generates* a fresh keypair,
    *re-seals* `secrets.json` under it, rewrites the `secrets/` and
    `providers/` dirs, and *relaunches* the LXC.

*   **`internal/provisioner/secrets.go: Destroy` and `provision.go`
    both call `teardown.Run`;** `storeDir` is `state/`, and a failed
    sequence rolls the dir back (`store.go: func (*managedStore) …`).

*   **`providers.go` (the only "semantic tools" left):** a provider's
    `params` are **opaque to the system** and *never* parsed, so there is
    no `litellm.chat` / `litellm.list_keys` / `openrouter.list_models`.

*   **`internal/deploy` reads `providers.json`/`secrets.json` and builds
    a k3s `manifests.yaml`** (see `freehold-deploy/` below).

### `freehold-deploy/` (Kubernetes — arrives with Chunks 6–7)

*   **`freehold-deploy/` is `freehold/orchestrator`'s `internal/deploy` +
    `internal/planebase`** (plus `config` and `deploy.yaml`); it lands with
    Chunks 6–7 (see `roadmap/POC.md`).

*   **The sole store is `deploy.yaml`** (or `deploy.yml`, `deploy.json`, or
    `.jsonc`); `loadDeploy` parses it into a
    `deploy.Deploy{Services: map[ServiceID]Service{…}}`.

*   **Every secret is a `SecretName`;** `Build` passes the *name* into
    `buildSecret` (with `Labels`), and `buildStatefulSet` mounts
    `/data`/`/var/lib/postgresql/data` from `PersistentVolumeClaim`s in
    `/srv/data/k8s-volumes`.

*   **`k3s image` digests are content-verified:** `resolveDigest` checks
    `sha256:` pins against `oci://<image>@sha256:<digest>`; `apply` runs
    `k3s kubectl apply` (via `freehold`'s `exec`).

*   **`freehold-acceptance` (a Go package) reproduces Chunk 1's acceptance
    criteria hermetically on loopback.**

*   **A real LLM (Claude / GPT / Gemini, via LiteLLM or OpenRouter) drives
    the CPA.** No local-only inference.

### Runners (the bridge between the two)

**A runner is a dumb privileged machine** — it runs commands; it isn't an
agent; it is *never* the brain (that's the CPA).

| Step | Action |

|---|---|

| 1 | `run_call` validates signature (roster), audience, expiry, nonce;
selects the connector |

| 2 | `runShell` executes the command **verbatim**, streaming output |

| 3 | Results (`//> …` / `<! …`) return to the calling agent |

**No MCP, no REST, no LLM, no router, no key vault.** A connector MUST NOT
cache plaintext credentials, hold a "master key," or consult the router —
credentials are injected per attempt via `envFrom`/`env` and the router
(`litellm.rs`) is an **API connector** whose `baseUrl` and `apiKey` ride as
env vars (`LITELLM_HOST`, `LITELLM_API_KEY`).

**A `pct` or `qm` command is simply a command.** The `exec` tool is the
single funnel for `pve.<verb>`, `container.<verb>`, `storage.*`, `service.*`,
`lxc.*`, `vm.*`, `vm_snapshot`, `vm_restore`, `snapshot.*`, and `backup.*`
(all illustrative; nothing here is a semantic tool).

### Targets (where the work lands)

| Kind | Example target | Connector | State |

|---|---|---|---|

| `pct` LXC | relay (`relay-…-<nn>`) | `pve` | `lxc.<role>` |

| `qm` KVM | VPS host (Vultr, `vps:<id>`) | `ssh` (KeyPath) | `vm.<id>` |

| `pct` LXC | K3s (`k3s-…-<nn>`) | `k8s` | `lxc.k3s` |

| `pct` LXC | control plane | `local` | `cp.<role>` |

| `vm` | … | `api` | `provider.<name>` |

*   **A `TargetId`** is `pct`, `qm`, `pve`, `k8s`, `local`, `ssh`, `api`, or
    `proxy` (full grammar in `internal/runner`; `TargetId::parse` lives in
    `internal/runner`).

*   **`pve.<verb>` and `container.<verb>` are gone:** `pct`/`qm` are `exec`
    on the PVE host or a VPS.

*   **`exec` is the one tool**; `local` is a generic exec on the runner
    itself (used by `provision`/`rotate` for `zfs` and `vzdump`), *not* a
    proxy, delegate-peer, or "executes in the control plane LXC."

*   **`<id>` (VMID),** not an LXC ID.

*   **`<nn>`:** `managedStore{root}` at `/srv/data/cp`, **not**
    `managed: false`.

### The single control plane (CP)

*   **One host, many connectors.** The CP is the only instance (no "CP of
    CPs"); it reads `providers.json`/`secrets.json` from `/srv/data/cp` and
    never re-derives them.

*   **A crash dump** (`coredumptl`, `internal/coredump`) proves something
    broke; it is not a recovery path.

*   **No dashboard** while an op is active; **no secret dumps** in output;
    no secrets in `logs/` or the activity window; **no LLM in the CP**
    (that's the CPA's job).

### Skills

*   **A skill is a directory** (e.g. `/skills/<id>/SKILL.md`) with YAML
    frontmatter (`name`, `description`, `metadata.openclaw.*`); the
    `metadata` key holds **`openclaw`** (or **`omarchy`**), and *all*
    supported values live under it.

*   **One skill = one host = one `SKILL.md`.**

*   **`freehold <verb>`** — there is no `freehold skill run`, no `skill run`,
    and no `freehold skill <verb>` subcommand; and *nothing* that re-proves
    the crypto with a `python3 scripts/convert_agents_opencode.py`.

*   **Never encode `sk-…`, `nsec…`, or `nenc…`.**

*   **The skill is the doc for one `SKILL.md`.** A long op uses
    `run_call`/`RunCallWithDial`; there is no `freehold skill` CLI, and a
    "Managed by freehold" dataset must never be re-`create`d.

*   **The agent MUST verify every `SKILL.md` command** on loopback (e.g.
    `127.0.0.1:3000`) against the `go test ./...` suite before it
    documents that command; **don't guess from prose** — probe first.

## Build plan (chunked)

**POC (pre-MVP, Kubernetes arrives with Chunks 6–7 — see `roadmap/POC.md`):**

1.  **Chunk 1 — Local control plane + runners + secrets + connectors:**
    local web UI (localhost) → one `exec` tool → per-service runners
    (dedicated per service by default) → provisioner model
    (`secrets.json`, `providers.json`, `identity.json`) → connectors
    (PVE/Vultr/Backblaze). **POC done =** a CPA that creates agents and
    manages an SSH machine / Vultr / Backblaze **via a runner** (the CPA in
    Buzz is Chunk 4).

2.  **Chunk 2 — The durable volume plane (the `/srv/data` convention):**
    `managedStore` at `/srv/data/cp` (`secrets.json`, `providers.json`) →
    `resolve` → `ensure` → `run_call` → `Run` → `runReconstruct` (fresh
    host, no state dir).

3.  **Chunk 3 — Rust→Go refactor:** the whole
    orchestrator/installer/control-plane/TUI surface moves to Go, with the
    Rust `core`/`runner` kept as a byte-exact reference oracle; `install`
    becomes a thin front-end to the rebuild engine (`eng.stdin = ui.in`);
    teardown keeps the config intact (`PruneLxcCoords` is never written to
    disk); the plane stage is never skipped (`TestManagedForFlags`,
    `TestWorldManaged`, `TestParsePctGateway`, `TestPlaneStageNeverSkipped`,
    `TestParseDnsList`); `TestDestroyLvmTenantUmountSedPrecedesLvremove`
    anchors the LVM path at `drive/lvm_test.go:463`.

4.  **Chunk 4 — A resilient CPA that creates agents and lives in Buzz:**
    the CPA runs in a **Kube-slot** (the `goose`/`omp` harness — see
    `roadmap/POC_CHUNK4.md`); `freehold-teardown` destroys LXCs but keeps
    the **recorded coordinates**; `freehold-install` is a thin front-end to
    the same engine.

5.  **Chunk 5 — Agent workspaces + git/GitHub:** one workspace at
    `/srv/data/<agent>/` (`.freehold/config.json` + `SKILL.md`);
    `freehold/teardown.go` (never `…/workspace.go`) keeps
    `DestroyLvmTenantUmount` and `SedPrecedesLvremove`.

**MVP (public release — ADDS k8s):**

6.  **Chunk 6 — Kubernetes substrate:** deterministic k8s pods, LiteLLM as
    a Deployment, **`freehold/deploy`** (the `Deploy` config, `parse.go`),
    `managed: true`, `TestManagedForFlags` / `TestWorldManaged` (see this
    file's "Pieces" above); `freehold/deploy/…` (a **Go** module — not
    Rust).

7.  **Chunk 7 — Remaining connectors (Vultr, Backblaze, Terraform) + the
    North Star:** portable `TargetId`, single `exec`, and the durable-path
    conventions from this file; `freehold-acceptance` reproduces
    **Chunk 1**'s acceptance criteria hermetically on loopback.

## Locked decisions

*   **Deterministic agent pods** (public release); connections as env vars;
    secrets via the provisioner model (a runner holds ciphertext + an
    injected key; an agent uses, never reads).

*   **Agent placement:** POC = Buzz agents via buzz-acp; Chunks 6–7 = k8s
    pods.

*   **Buzz required; the management relay is created by the install; one
    relay per control plane.**

*   **LiteLLM** as a k8s Deployment (replicas), config mounted, state in
    Postgres.

*   **Storage**: ZFS now → Ceph on 2nd box. Layered reliability.

*   **Buzz Relay headless**; the first-party **console** is the admin/ops
    surface.

*   **K8s arrives with Chunks 6–7 — the public release (MVP), not before.**
    `roadmap/POC.md` anchors both halves of that claim.

*   **Generic exec runner** (an agent writes commands; a runner owns the
    connection and streams + audits them).

*   **`exec(cmd, target, stream?)` and `run_call` are the only tools**; one
    `exec` tool per runner; `//>`/`<!` frames stream the result.

*   **Nothing in the POC needs a cluster**; POC = a CPA that **lives in
    Buzz** + skills.

*   **The `/srv/data` convention** (durable state) and `internal/tui`'s
    single activity view (**ALWAYS** the top line).

*   **`<n>` is a chunk number and `internal/<pkg>`** is the module path for
    every Go package above.

## Verification

- `ARCHITECTURE.md`, `VISION.md`, `README.md`, `AGENTS.md`, and everything
  under `roadmap/` describe the current state; the decisions above are
  current.
