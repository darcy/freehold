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
   freehold-orchestrator (Go) drives freehold-runner (Rust, independent)
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

*   **Agent pods are bare v1 Pods in the k3s LXC's `agents` namespace** — the
    CPA (Chunk 4) and every agent it creates. Each agent owns distinct objects
    named from its sanitized display name (`<pod>`, `<pod>-identity`,
    `app: <pod>`), so a second agent never applies over the first. The harness
    is the `ghcr.io/block/buzz-sprig` image (moving `:main` tag today; a
    build-time digest pin is a named follow-up); identity rides a
    first-run-wins `<pod>-identity` Secret via `secretKeyRef` (the nsec never
    rides the manifest); `restartPolicy: Never` keeps an intentional exit
    terminal (I5); no mgmt channel by design; no PVC — agent memory is
    relay-persisted (kind 30174).

*   **Agent placement:** every agent (CPA and created alike) runs on Buzz's
    `buzz-acp` remote-agent harness as a k3s pod (see `roadmap/POC.md`).
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
    reasoning-class. The CPA gets its purpose from `orchestrator/prompts/CPA_SYSTEM_PROMPT.md`.

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

### `freehold-orchestrator` (Go; the operator's toolchain)

*   **The whole operator toolchain is Go, three binaries.** `orchestrator/` is
    the Go module `freehold/orchestrator` (go 1.25): `freehold` (interactive
    TUI; a subcommand routes to the CLI), `freehold-orchestrator`, and
    `freehold-agent-tools` (the CP's agent-management MCP server). `internal/`
    carries agent, agenttools, bootstrap, cert, cli, client, config, console,
    crypto, delegate, deploy, dnsman, drive, flows, oplogin, planebase,
    provisioner, relay, state, teardown, tui, wire.

*   **The privileged `exec` funnel lives in the RUST runner, not the
    orchestrator.** `orchestrator` is the operator's CLI/TUI/installer: it
    drives a running runner over its MCP endpoint (`internal/client/mcp.go`,
    the shared signed-header scheme) and re-enters itself (`freehold exec …`)
    for its world-bring-up stages. The runner (`runner/`, Rust) is the dumb
    privileged hands — `exec(cmd, target, stream?)`, signed by a granted
    pubkey, fail-closed, auditors per command.

*   **`freehold-agent-tools` is a distinct SEMANTIC surface on the CP**, not
    the runner's `exec`. Its Go methods (`internal/agent/tools.go`,
    `create_agent`/`grant_agent`/`manage_agent`) are served in-process by
    `cmd/freehold-agent-tools` (`serve`, HTTP `/mcp`), authorized per call
    against the server's own relay roster (NIP-29 channel + 39002,
    fail-closed); its `mcp` stdio mode is the bridge agent pods fetch at boot.
    The build dogfoods `create_agent` to bring the CPA up. It also carries the
    CP world-action surface (`world_status` / `world_teardown` / `world_migrate` /
    `world_build`, roster-gated) so an operator box can "login + trigger" the
    world: `world_build` runs the CP's owned bring-up/reconcile stages
    (`internal/stages`) through its co-located runner — the direction
    `freehold build` (box) slims toward (CP-bring-up + trigger; the CP owns
    relay/storage/k3s/DNS/litellm/Caddy/cert). `world_migrate` runs `internal/migrations` —
    the CP's verify-gated migration runner (durable ledger at
    `/srv/data/cp/migrations.json`, a migration is done only when its
    postcondition verifies), for versioned config/prompt/repair changes that
    don't have clean desired-state semantics.

*   **`internal/cli/rebuild.go` is the slim CP-driven build.** `collectAnswers` →
    `rebuildFlags` → `newRebuildEngine` → `runSlim`: door → runner → durable
    plane → seed the litellm secrets into the box runner (deploy-cp ships it as
    the co-located runner) → boot the CP LXC → boot + deploy the relay stack
    (the agent-tools roster lives on it) → deploy-cp → deploy
    `freehold-agent-tools` → hand the world's secrets to the CP (DNS creds
    sealed to the agent-tools identity, litellm secrets sealed into the runner)
    → **trigger `world_build`** (the CP brings up k3s/storage/DNS/litellm/
    Caddy/cert through its co-located runner) → record the post-world coords →
    CPA + reconcile. `install` hands the same engine the TUI's answers.
    Teardown keeps the config (compute-only) unless `--data` erases the tenant
    datasets.

*   **`internal/state` is a file store** (`state.json`: runners, secrets,
    agents) under the operator/CP state dir; agent registries are
    CP-durable (see `freehold-agent-tools`).

### `freehold` TUI (bubbletea; the operator's console)

*   **It is bubbletea, not HTML.** `internal/tui/tui.go` is full-screen
    alt-screen (`tea.NewProgram(m, WithAltScreen(), …)`); `freehold` with no
    args enters it, a subcommand routes to the CLI.

*   **Six views**, cycled with `Tab` / `Shift-Tab`: Services · Agents ·
    Runners · DATA · DNS · Certs. Keys in running mode: `q` quit, `r`
    recheck the world, `s` toggle the Runners source (CP console ⇄ local
    state), `w` open the web console, `l` log in with the operator nsec,
    `p`/`x`/`g` provision/revoke/grant. Build/teardown run from the shell.

*   **Remote-CP access.** `freehold login` (root-free) authorizes this operator
    against the CP by **CP address + operator nsec** (NIP-98), then ends;
    `internal/oplogin` persists the nsec 0600 under the operator dir and seeds a
    local connection/desire profile from the CP's `/api/world` summary, so every
    launch auto-logs in and a fresh box recovers with nothing from a lost one.
    The operator key **is** the credential — the console only admits NIP-98
    operators whose pubkey was minted into its admin whitelist at deploy, so
    logging in as yourself from any box unlocks the world. The recorded
    `cp_pubkey` is the CP's *own* identity, adopted from its `/api/world`
    self-report (`resolveCPPubkey` normalizes to 64-hex) and informational —
    never typed, since a legitimate login to the actual CP needs no separately
    known pubkey. (The trust boundary for a wrong/hijacked `cp_url` is TLS/DNS
    on that URL, not this recorded anchor.) `freehold logout` clears this box's local
    ledger only. `/api/world` carries the relay coords (served from
    `state.json` — the CP records them when `serve` is started with
    `--relay-url`, paired or not with `--relay-pubkey`) plus the
    `agent_tools_url`/`agent_tools_pubkey` the Agents view needs. The Agents
    tab reads the CP toolset registry (`freehold-agent-tools manage_agent`);
    the Runners-CP view reads the console `/api/overview`. `w` opens the web
    console pre-authorized via a single-use portal token.
    **A login-only box (cp_url + cp_pubkey + operator, no local `[runner]`)
    reaches Running**: the boot gate treats a runnerless profile as having its
    runner reach satisfied and sources CP liveness from the console session
    (`/api/overview`), not the `pct exec` probe a box with a local runner uses.

*   **One activity surface for long ops.** `internal/tui/activity.go`
    streams the boot probe rows and the subprocess windows, `ctrl+c` aborts;
    the dashboard never scrolls under an open activity. The world-mutation
    forms re-exec `freehold` as a subprocess (the same CLI drivers).

*   **Testing the TUI means the BUILT binary** — `go test` under
    `internal/tui/` verifies form logic, not the running app (see AGENTS.md
    for the rebuild+tmux/herdr discipline).

### `orchestrator/prompts/CPA_SYSTEM_PROMPT.md` (the CPA's purpose)

*   **`orchestrator/prompts/CPA_SYSTEM_PROMPT.md`** is embedded into
    `freehold-orchestrator` via the `orchestrator/prompts` package's
    `//go:embed CPA_SYSTEM_PROMPT.md` and mounted into every agent pod as the
    `<pod>-prompt` ConfigMap at `/srv/freehold/CPA_SYSTEM_PROMPT.md`,
    re-read fresh on every spawn. `freehold-agent-tools` ships it verbatim for
    the CPA when the build creates it.

*   The CPA is a **reasoning agent that lives in Buzz** and is the system's
    **main user touchpoint**; it runs on the same `buzz-acp` harness
    (`buzz-agent`) as the agents it creates.

*   It is **conversation + agent-creation only** in this phase: it calls the
    CP toolset's `create_agent` / `grant_agent` / `manage_agent` (through the
    `freehold-agent-tools mcp` stdio bridge, signed as its own nsec and
    authorized by the server's roster). It does not run arbitrary `exec` or
    provision targets — that boundary is unchanged: reasoning decides *what*
    to do, the deterministic runner/CP layer does it auditably.

*   **It never sees plaintext secrets** and references credentials by name
    only.

*   The prompt is honest about what is actually callable: it does not claim
    tools the harness lacks; it reports tool errors plainly rather than
    inventing results.

### `control-plane` (Rust; the console + provisioner)

*   **The control plane console + secret provisioner is a Rust crate**
    (`control-plane/`: `src/state.rs`, `src/provisioner.rs`, `src/web.rs` —
    the loopback admin/ops web console with NIP-98 operator login —
    `src/console.rs`, `src/main.rs`). The console is the CP's own identity
    (0600) that signs readiness probes against each runner — no side door,
    the runner still fails closed.

*   **`secrets.json` holds ciphertext only** (pubkeys + sealed blobs; no
    master key). `providers.json` (control-plane only) holds opaque `params`
    per connector the system never parses.

*   **The Go toolchain mirrors the surfaces it drives:** `internal/provisioner`
    (`ProvisionRunner`) reproduces provision for onboarding existing
    services; `internal/client/mcp.go` is the signed MCP client that drives a
    runner (`exec`/`status`/`upload`); `internal/console` talks to the CP
    console's `/api/*` (overview/agents/portal) as an operator session.

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
    the CPA runs on the `buzz-acp`/`goose-class` harness as a k3s pod (see
    `roadmap/POC_CHUNK4.md`), with the CP's `freehold-agent-tools` toolset
    and the durable-plane identity/memory guarantees; `freehold-teardown`
    destroys LXCs but keeps the **recorded coordinates**; `freehold-install`
    is a thin front-end to the same engine.

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

*   **Agent placement:** every agent (CPA and created alike) runs on the
    `buzz-acp` harness as a bare k3s pod.

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
