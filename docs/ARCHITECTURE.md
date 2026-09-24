# The AI-operated Appliance — Architecture

Product: an open-source "box + install script" (Omarchy-style) that lands a
Proxmox VE / VPS + Kubernetes stack with Buzz Relay as the control plane and
a skill framework that installs and configures self-hosted OSS. For the
current chunk numbering (Chunks 1–4 done — the engine room, relay scope, the
durable plane, the Rust→Go refactor, and a real reasoning CPA in Buzz; Chunk 5
= agent workspaces + git/GitHub; Kubernetes arrives with Chunks 6–7), see
`docs/ROADMAP.md` and `docs/POC.md`. Narrative: "reclaim the future we
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
   freehold / the CP (Go) drives freehold-runner (Rust, independent)
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
    `buzz-acp` remote-agent harness as a k3s pod (see `docs/POC.md`).
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
    reasoning-class. The CPA gets its purpose from `agents/freehold/prompt.md`.

*   **Host-flexible:** Proxmox is the lead/default; VPS/cloud are first-class
    (the business path). The k8s layer (Chunks 6–7) and everything above the
    host driver run identically regardless of substrate.

*   **Backups are the load-bearing wall — and most teams won't have a PBS
    server:** the *whole* backup chain (LXC/VZ+T pl8755, KVM+PBS) depends on
    a PBS host. Where one doesn't exist, the durable half of the
    `/srv/data` convention must land on TrueNAS and/or Backblaze B2.

*   **The durable half lands FIRST (Chunk 3; see `docs/POC.md`):**
    `/srv/data` — everything that survives rebuild: `/srv/data/relay` (all
    relay deploy data: config, CA, keypairs), `/srv/data/cp`
    (`secrets.json` + `providers.json`), `/srv/data/k8s-volumes` (k8s
    storage, Chunks 6–7). **Nothing in the POC needs a cluster**
    (`docs/ROADMAP.md`), and even in MVP most services aren't k8s
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

### `contract/` (`freehold/contract` — the shared wire/trust leaf)

* **Purpose:** the language-agnostic trust, crypto, and wire contract that
  BOTH the control plane and the local CLI + platform import — the leaf
  everything builds on. It is its own Go module so the edge is `platform →
  contract ← control-plane` and `platform → contract ← freehold-cli`, never
  `platform → control-plane` (no module cycle). It carries `crypto/` (the Go
  repro of the Rust `core`), `wire/` (envelopes), `client/` (the signed MCP
  client), `config/`, `console/` (the console client), `relay/` (the relay HTTP
  client), `delegate/` (the kind-9 delegation envelopes), `identity/` (the
  identity.json format loader), and `worldfacts/` (the world-inventory wire
  shape). The CP's `state/` store lives in `control-plane/` (server-only).

* **Contents:** NIP-44 v2 encryption (chacha20poly1305, bech32, hkdf-sha256,
  sha256, hex) and the signer (`CryptoProvider` over `CryptoDyn` —
  `secp256k1` + `ring` only; `bip39`, `bs58`, and `keyring` are unused).

* **Rust→Go port status:** the Go `contract/crypto` reproduces the Rust
  `core` surface **byte-exactly** — verified by
  `control-plane/core/harness/harness_test.go`, which drives the Rust
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

### `control-plane/` (`freehold/control-plane` — the stable mechanism)

*   **The mechanism is one Go module** (go 1.25): `api/` (the unified scoped
    API — agent toolset + world actions + `cpbuild`), `secret-management/`
    (provision/rotate/revoke/grant), `state/` (the CP store), and the Rust
    crates `core/` (the byte-exact contract oracle + the `harness/` Go
    byte-gate), `runner/`, and `testkit/` (the runner's hermetic fixtures). The
    Chunk-1/2 acceptance gate is Go under `acceptance/`, driving the real
    `runner` binary as a subprocess. The local operator interface (`freehold`
    CLI + TUI, `login/`, `flows/`, install) lives in the sibling
    `freehold-cli/` module; the server never imports it.

*   **Two apps, zero cross-imports (local/server split).** The server
    (`control-plane/`) and the local operator surface (`freehold-cli/`) are
    separate Go modules. `control-plane/` never imports `freehold-cli/`;
    `freehold-cli/` never imports `control-plane/` — the local side drives the
    server through the CP API (console HTTP / agent-tools MCP) or by invoking
    its binaries, never by linking its packages. An import-graph guard test in
    each module enforces both directions. Anything both sides genuinely need
    (crypto/wire/client/config/console, the relay + delegation protocol
    clients, the identity loader, the world-facts shape) lives in the
    `contract/` leaf. The build engine (world bring-up/teardown) is server-side
    (`api/cpbuild`); local `build`/`teardown` are thin CP triggers, and CP
    creation is `freehold install` (in `freehold-cli/`).

*   **The privileged `exec` funnel lives in the RUST runner, not the CP.**
    The CLI is the operator's interface: it drives a running runner over its
    MCP endpoint (`contract/client/mcp.go`, the shared signed-header scheme)
    for world-bring-up. The runner (`control-plane/runner/`, Rust) is the
    dumb privileged hands — `exec(cmd, target, stream?)`, signed by a granted
    pubkey, fail-closed, auditors per command.

*   **`freehold-agent-tools` is a distinct SEMANTIC surface on the CP**, not
    the runner's `exec`. Its Go methods (`control-plane/api/agent/tools.go`,
    `create_agent`/`provision_runner`/`grant_agent`/`manage_agent`) are served
    in-process by
     `control-plane/api/cmd/freehold-agent-tools` (`serve`, HTTP `/mcp`),
     authorized per call against the server's own relay roster (NIP-29 channel
     + 39002, fail-closed) **and scoped by caller class**: a pubkey in the CP's
     agent registry is an AGENT (create/manage + the `provision_runner`
     carve-out — the world_* actions AND `grant_agent` are denied server-side,
     so the CPA's boundary cannot be bypassed by calling the server directly);
     a roster
     member not in the registry is an OPERATOR (full toolset incl. world_* and
     grant). The Caddy CP vhost exposes `/mcp` publicly (→ `:8089`) and
     `/api/world` serves `agent_tools_url` as the public `https://<cp>/mcp` plus
     `console_enc_pubkey` (the console identity's X25519 encryption public key,
     the seal recipient for CP-owned secrets — so a thin box hands them off with
     no runner to exec into the CP), so a REMOTE thin box drives the world
     (build/exec/migrate/door) over the edge — the drive-through-CP transport. Its `mcp` stdio mode is the bridge agent
     pods fetch at boot (same create/manage-only filter, now defense-in-depth).
     The build dogfoods
     `create_agent` to bring the CPA up. It
    also carries the CP world-action surface (`world_status` / `world_teardown`
    / `world_migrate` / `world_build` / `world_exec` / `world_register_facts` /
    `world_authorize_door` / `world_revoke_door`, operator-scoped) so an
    operator box can
    "login + trigger" the world: `world_build` runs the CP's owned
    bring-up/reconcile stages (`platform/provisioning/stages`) through its
    co-located runner — the direction `freehold build` (box) slims toward
    (CP-bring-up + trigger; the CP owns relay/storage/k3s/DNS/litellm/Caddy/
    cert). The stages live in the shared **`cpbuild`** package, and the console
    is the **CP build executor too**: `freehold-console serve` builds a
    `cpbuild.Spec` (from the `--world-config` coords deploy-cp hands it, signed
    as the console's own identity and self-granted on the co-located runner)
    and exposes an operator-scoped **`/api/world-build`** — a thin box can
    bring the world up through the CP WITHOUT the relay roster agent-tools
    needs, so relay+agent-tools can live in `build` (the install/build split).
    Its **`/api/world-teardown`** mirror runs the
    shared teardown engine through the co-located runner — relay/k3s removed and
    the CP-side agent-tools process stopped, while the CP + its runner survive
    (`teardown` is the inverse of `build`; `uninstall` removes the CP) — so a
    login-only box can also tear the world down. `world_exec` is the **drive-through-CP exec**
    surface: a THIN login
    box (no local `[runner]`) runs commands on the CP's co-located runner via
    this tool — so a login box is functionally equivalent to the box that
    bootstrapped, an authorized operator client rather than a runner host.
    `world_status` assembles the **single inventory read** (agents + the
    console's runners/DNS read underneath — the console's state.json on the
    box — plus the deployer-side world facts (`world_register_facts`: the
    durable-plane layout, canonical domains, and edge cert metadata the box
    registers at the end of `freehold build`)). It is served **twice from one
    assembly** (`agenttools.WorldStatus`): the console folds it into its public
    `/api/world` (consumed by the TUI and `freehold status`), and the
    `/mcp world_status` tool shares that same assembly for direct MCP callers —
    so the two surfaces can never diverge. `grant_agent` is
    **operator-scoped and wired through the absorbed console-owner
    credential**: the server loads
    the console's own identity from its state dir (0600 durable plane) and
    publishes the kind-9000 put-user to the runner's channel in-process — the
    runner re-reads its signed 39002 roster per call, so the grant lands
     without a restart (missing credential fails closed; agents are denied with
      `-32003`, since a grant hands direct exec access to the runner). The
      ONE carve-out is the CPA's `provision_runner` (grants on the fly): it
      stages a NEW capability runner — keypair + sealed credential + private
      channel + a systemd unit on the CP guest — records it as a dynamic
      capability (re-staged adopt-only on every build; fixed port so pod
      coords stay stable), grants the named agents onto its roster live, and
      re-applies the grantees' pods with coords resolved from state. It never
      widens an existing runner, `agent_grants: off` on the CP state is the
      server-side kill switch (`freehold-console grants-mode`), and the
      in-thread-vs-DM confirmation discipline lives in the granting skill
      (`docs/POC_GRANTS.md`). The
     `platform/migrations` queue — the CP's repair/catch-up scripts for
     versioned config/prompt/repair changes that don't have clean desired-state
     semantics — runs from two entry points: the `world_migrate` tool and the
     tail of `world_build`. Migrations are **versioned script files** (Omarchy's
     `<epoch>.sh` convention — one timestamped shell file per migration, shipped
     to the CP by `install`/`update`, run in ascending order with
     `bash -euo pipefail`).
     Completion is an Omarchy-style **marker file** named for the script
     (`<stateDir>/migrations/<epoch>.sh`, scripts in `migrations/scripts/`); a
     script runs exactly once per world — a fresh install included, at the end
     of world bring-up — and a failure stops the queue unmarked.
     The agent-registry reconcile (the console
     state.json `agents` map folded into the authoritative `registry.json`) rides
     that runner as a script migration, driven by the `freehold-agent-tools
     registry import-console` subcommand.

*   **`platform/provisioning/box` is the shared provisioning engine.**
    `install`'s wizard → `box.Flags` → `box.NewEngine` → `box.RunBootstrap`
    (CP bootstrap); `freehold build` runs the world through the CP:
    door → runner → durable plane → boot the CP LXC → **`install`** (box one)
    = the CP only (console + co-located runner) — no secrets are collected.
    `freehold install` requires **`--name` + `--host`**: it scopes the
    config + state to `profiles/<name>/` instead of the base home, records the
    host + access mode in that profile, and names the guest LXCs
    `<name>-<relay|cp|k3s>`. A life-cycle gate **mints** when no profile exists,
    **re-adopts** an existing profile whose CP is absent (the durable plane keeps
    the runner identity; deploy-cp never overwrites it), and **refuses a live
    CP** (`build`/`teardown`/`uninstall`/`login`); `bootstrap` is a hidden alias
    for `install --yes`. A world with no recorded name (installed before
    names) keeps the domain-derived `<domain-dashed>-<role>` names, so it still
    reconciles; durable-plane names stay domain-keyed either way.
    **`build`** (any box, login-gated) ensures the **CP-owned secrets** (DNS
    creds + litellm; sealed to the **console** identity in `world-secrets/`,
    asked only when missing), points the public A records (`manageDomainDNS`),
    then triggers the console's `/api/world-build` — the CP brings up relay/
    agent-tools/k3s/storage/litellm/Caddy/cert through its co-located runner,
    re-seeding litellm into the runner from the CP store →
    record the post-world coords → CPA + reconcile. `install` hands the same
    engine the TUI's answers; on an interactive terminal it collects every
    answer (incl. relay/CP domains + the proxy IP) up front in a bubbletea
    wizard so `runBootstrap` never re-prompts them. Pre-DNS steps connect to the
    recorded guest IPs (`config.ResolveTarget`) until the public domain
    resolves. Teardown keeps the config, the coords, and the data.

*   **`providers/proxmox/teardown/` is the world-removal engine** (shared by
    `teardown` and `uninstall`). Whole-world `teardown` is CP-driven and
    CP-preserving (`/api/world-teardown`); `uninstall` additionally removes the
    CP + this box's doors + the local profile, and `--remove-data` erases the
    datasets + the freehold-created thin pool. From a thin box or against a
    dead CP, `uninstall` reaches the host over the transient root-SSH seam
    (the box's DOOR_SPEC key) instead of a local runner.

*   **`control-plane/secret-management/` is the provisioner** (`ProvisionRunner`
    reproduces provision for onboarding existing services; `contract/client`
    is the signed MCP client; `contract/console` talks to the console's
    `/api/*`).

### `freehold` TUI (bubbletea; the operator's console)

*   **It is bubbletea, not HTML.** `freehold-cli/tui/tui.go` is full-screen
    alt-screen (`tea.NewProgram(m, WithAltScreen(), …)`); `freehold` with no
    args enters it, a subcommand routes to the CLI.

*   **Six views**, cycled with `Tab` / `Shift-Tab`: Services · Agents ·
    Runners · DATA · DNS · Certs. Keys in running mode: `q` quit, `r`
    recheck the world, `w` open the web console, `l` log in with the operator nsec,
    `p`/`x`/`g` provision/revoke/grant. Build/teardown run from the shell.
    (The `s` Runners-source toggle is gone: the box-local `state.json` mirror
    is deleted — the Runners view reads the console `/api/overview` only. The
    CP-lifecycle door model — implemented as `world_authorize_door` /
    `world_revoke_door` + `freehold door authorize|revoke`, spec'd in
    `docs/DOOR_SPEC.md` — lets a fresh logged-in box authorize its own door key
    on the host.)

*   **Remote-CP access.** `freehold login` (root-free) — instead of one box-wide
    connection profile — **adds a named tenant profile** to the box: a profile is
    a single logged-in tenant with its own config file
    (`~/.config/freehold/profiles/<name>/config.toml`) and its own scoped state
    dir (`<FREEHOLD_HOME|~/.freehold>/profiles/<name>/`), the filesystem being the
    registry. `login` authorizes this operator against the CP by **CP address +
    operator nsec** (NIP-98), then ends;
    `freehold-cli/login` persists the nsec 0600 under the profile's operator
    dir and seeds that profile's connection/desire config from the CP's
    `/api/world` summary, so every launch auto-logs in and a fresh box recovers
    with nothing from a lost one. The TUI and `build`/`install`/`teardown`/
    `world` pick which profile (tenant) to operate via an interactive picker
    (`freehold profiles` lists them), and fail closed with "run `freehold login`
    first" when none are registered. There is no implicit "default" profile or
    legacy single-config layout. The operator key **is** the credential — the
    console only admits NIP-98 operators whose pubkey was minted into its
    admin whitelist at deploy, so logging in as yourself from any box unlocks
    the world. The recorded `cp_pubkey` is the CP's *own* identity, adopted
    from its `/api/world` self-report (`resolveCPPubkey` normalizes to
    64-hex) and informational — never typed, since a legitimate login to the
    actual CP needs no separately known pubkey. (The trust boundary for a
    wrong/hijacked `cp_url` is TLS/DNS on that URL, not this recorded anchor.)
    `freehold logout` clears the chosen profile's local ledger only. `/api/world`
    serves the relay's **public edge** (derived as `https://<relay_host>` when a
    relay host is recorded, else the raw `state.json` `--relay-url`) plus the
    `agent_tools_url`/`agent_tools_pubkey` the
    toolset exposes — a client adopts the domain, not the internal LAN dial.
    The whole Services pane — relay, control plane, and the world services a
    box renders — comes from the CP.
    The Agents tab reads the authorized agent registry +
    world facts folded into `/api/world` itself (served from the toolset's
    durable state via the shared `agenttools.WorldStatus` assembly — the same
    one `/mcp world_status` uses), so a logged-in box renders Agents/DATA/Certs
    with no local agent-tools coords and no separate MCP hop; the Runners-CP
    view reads the console `/api/overview`. `w` opens the web console
    pre-authorized via a single-use portal token.
    **A login-only box (cp_url + cp_pubkey + operator, no local `[runner]`)
    reaches Running**: the boot gate treats a runnerless profile as having its
    runner reach satisfied and sources CP liveness from the console session
    (`/api/overview`), not the `pct exec` probe a box with a local runner uses.

*   **One activity surface for long ops.** `freehold-cli/tui/activity.go`
    streams the boot probe rows and the subprocess windows, `ctrl+c` aborts;
    the dashboard never scrolls under an open activity. The world-mutation
    forms re-exec `freehold` as a subprocess (the same CLI drivers).

*   **Testing the TUI means the BUILT binary** — `go test` under
    `freehold-cli/tui/` verifies form logic, not the running app (see
    AGENTS.md for the rebuild+tmux/herdr discipline).

### `platform/` (`freehold/platform` — the evolving world)

*   **The evolving world the mechanism installs and evolves** — services,
    migrations, provisioning, terraform, data conventions. Adding a new service
    means adding a `platform/` entry — never touching `control-plane/` (adding a
    named agent means an `agents/<name>/` entry instead, see below). It is its
    own Go module (imports `contract`,
    never `control-plane`), so `control-plane → platform → contract` is a
    one-way edge.

*   **`platform/services/<capability>/<impl>/`** names services by stable
    capability + swappable implementation: `relay/buzz/` (the relay deploy
    driver), `webproxy/caddy/` (the TLS edge), `llmproxy/litellm/` (kube
    manifests), `database/postgres/`, `internaldns/dnsmasq/`,
    `externaldns/cloudflare/` (`dnsman`), `certificates/letsencrypt/` (`cert`,
    lego provider registry).

*   **`platform/` is provider-independent.** Substrate-specific commands
    (Proxmox `pct`, LVM/ZFS, later Vultr/Hetzner APIs) live behind the
    `Provider` seam in `platform/provisioning` and in the top-level
    `providers/` module; `install`/`uninstall`/`build`/`teardown` are
    orchestrators that inject the concrete provider. `platform/` imports
    `contract`, never `providers/` or `control-plane/`, so
    `control-plane → platform → contract` is a one-way edge. A guard test
    enforces both the import direction and the absence of provider command
    strings in `platform/`.

*   **`platform/provisioning/`** carries the provider-independent
    compute+storage orchestration: `bootstrap/` (the generic exec/naming
    helpers), `planebase/` (the pure plane math + storage classifier),
    `stages/` (the generic manifest/coordinate builders), `deploy/` (generic
    deploy helpers), and the `Provider` interface + engine wrappers. The
    concrete Proxmox driver (guest create/exec/list, storage, pct stage/DNS
    builders) lives in `providers/proxmox/`.

### `providers/` (`freehold/providers` — the substrate providers)

*   Its own Go module; imports `platform/` + `contract/`, never the reverse.
    `providers/proxmox/` is the Proxmox VE substrate: guest create/exec/list,
    LVM/ZFS/thin-pool storage, the PVE `local-lvm` pointer discipline, and the
    pct stage/DNS command builders (`providers/proxmox/drive/` holds the
    storage driver). A provider is substrate ops, not a lifecycle — there is no
    `provider.Install()`; the composition roots (`freehold-cli/`,
    `control-plane/`) decide the sequence and inject the provider.

*   **`platform/migrations/`** enumerates the CP's shipped migration scripts
    (`<stateDir>/migrations/scripts/<epoch>.sh`) and tracks completion with
    marker files named for the script (no embed, ledger, or verify gate), run
    ascending via `bash`; the CP-owned
    build's IaC is the Terraform module embedded in
    **`control-plane/api/cpbuild/terraform/`** (shipped by the console to the
    box at `/srv/data/freehold-tf`): the substrate (durable plane + cp/relay/k3s
    LXCs + k3s bring-up) is exec-first `null_resource` shell, while the SERVICE
    definitions (`postgres.tf` / `litellm.tf` / `caddy.tf`) are real
    `kubernetes`-provider resources — the deterministic static files that define
    each service, secret values riding the 0600 state.

### `agents/` (`freehold/agents` — the top-level home for agent definitions)

*   **`agents/`** is its own Go module — `freehold/` (the freehold named agent:
    the CPA's purpose + skills), `custom/` (the template for agents the CPA
    creates on the fly), `common/orientation.md` (the shared system-orientation
    block composed onto every non-custom prompt), the four **department
     definitions** (`network/`, `data/`, `compute/`, `ai/` — each a
    distinct identity scoped to one domain), and named
    agents that grow over time. It is
    embedded by the `freehold/agents` Go package and shipped by the control
    plane; the module carries its own `go.mod` because a Go package cannot
    `//go:embed` outside its own module.

### The agent org (two tiers)

*   **The org is two tiers: the CPA and four departments.** The CPA is the sole
    user touchpoint; the departments are its direct reports:

    | Department | Domain |
    | --- | --- |
    | **Network** | The network surface: access & exposure — external/public proxy, Tailscale, internal proxy, exposure verification |
    | **Data** | Data plane: backup/off-site, DR planning, scheduling, restore verification |
    | **Compute** | The box itself: CPU/RAM/disk, Proxmox LXC/Kube provisioning, remote (Vultr-type) provisioning, plus the monitoring tooling it needs. Not what runs on top |
    | **AI** | LiteLLM/provider setup & aliases, local AI config, **AI hardware** (local-AI accelerators like an RTX 3090 or DGX Spark — provisioned and tuned by AI, separate from Compute's general resources), agent optimization, prompt/skill management, agent debugging |

*   **Service lifecycle is not a department.** Whichever agent created a service
    — a freehold-delegate or a custom agent — owns its install/config/operation,
    ad hoc and unvetted as before. No dedicated department aggregates or monitors
    installed OSS.

*   **Communication is unrestricted.** The operator and any agent may converse
    with any department or agent directly — talk is not gated.

*   **Capability execution is bounded.** A capability a department owns (external
    proxy, backup, compute/LXC, model registration, AI hardware) is executed by
    that department's identity; the raw grant for it attaches to department
    identities, never to a custom agent that would then self-serve a second,
    ungoverned path to the exact capability the department exists to own and
    audit. A custom agent that self-serves a department-owned capability is a
    containment failure even if a grant would technically allow it — the
    department's prompt is the first line of defense, the grant the second.
    Today's enforcement is the existing grant model: custom agents hold no raw
    capability grants on the build-time runners (`grant_agent` is
    operator-scoped), and the CPA's `provision_runner` grants only onto
    NEW capability doors it stages — the department that gets the grant is
    named in the flow itself.

*   **The four departments are installed as part of the core build** — each a
    pod on the same harness as the CPA, created through the same audited
    `create_agent`. Each joins the private `#freehold` plus its own **private**
    `#freehold-<department>` channel, and the CPA is added to every channel. Each
    department also holds **capability runners** (`stageDepartmentRunners`):
    one runner per capability its role needs — named
    `<target>-<protocol>-<identity>` (`pve-ssh-root` shared by
    network+compute+data, `kube-api-root`/`kube-api-caddysa`/`kube-api-litellmsa`
    SA-token kube doors, `litellm-api-admin`, `cloudflare-api-<zone>`,
    `dnsmasq-local-root`) — each in its own private NIP-29 channel/audit
    stream, bound LAN-reachable with the relay roster. The pod receives the
    runners' coords (`FREEHOLD_RUNNER_*`) and its bridge advertises a scoped
    `exec`/`list` that routes by target to the pinned runner+credential,
    signing as the agent's own nsec — so the department's capability grants
    attach to the department identity, and the CPA/custom agents (with no
    coords) never see exec. A capability a department doesn't hold yet ships
    as its runner when the capability lands — never by widening a runner's
    package; the CPA's `provision_runner` stages such a door on the fly (a
    dynamic capability record re-staged adopt-only every build). Status
    language is
    uniform, runner → service → department: 🟢 all checked / 🟡 some checks
    missing / 🔴 none.

*   **Departments check in rather than wait to be asked.** When a new
    service/compute is requested through the CPA's provision path, the relevant
    department raises the question itself (Data: "back this up?"; Network:
    "reachable outside your network?"). A "no" is final — the point is that the
    gap is a visible choice, not a silent one.

*   **Every non-custom agent is oriented the same way.** The CPA and the four
    departments are prefixed with a shared system-orientation block: the source
    repo URL (configurable; upstream default), read-it-on-boot + keep a memory +
    re-check periodically because the repo is active, and the "be loud" rule —
    surface problems and missing access to freehold and the operator, never
    silently. Custom agents (those the CPA creates on the fly) are exempt.

### `agents/freehold/prompt.md` (the CPA's purpose)

*   **`agents/freehold/prompt.md`** is embedded into the `freehold/agents` Go
    package (`//go:embed freehold/prompt.md`) and mounted into every agent pod
    as the `<pod>-prompt` ConfigMap at `/srv/freehold/SYSTEM_PROMPT.md`,
    re-read fresh on every spawn. The package has its own `go.mod` (a Go
    package cannot embed outside its own module), so `control-plane` imports
    the value, never re-embeds. `freehold-agent-tools` ships it verbatim for
    the CPA when the build creates it.

*   The CPA is a **reasoning agent that lives in Buzz** and is the system's
    **main user touchpoint** and **sole first-contact surface**; it runs on the
    same `buzz-acp` harness (`buzz-agent`) as the agents it creates.

*   It **delegates to the four departments** (see "The agent org" above) rather
    than doing expert-level work itself: a capability request is routed to the
    department that owns that domain, whose identity executes it. The
    departments are reachable by anyone; what the CPA routes is capability work,
    not conversation.

*   It is **conversation + agent-creation + grant-giving** in this phase: it
    calls the CP toolset's `create_agent` / `provision_runner` /
    `manage_agent` (through the
    `freehold-agent-tools mcp` stdio bridge, signed as its own nsec and
    authorized by the server's roster). `provision_runner` stages a NEW
    capability runner and grants agents onto it under the granting skill's
    rules (`agents/freehold/skills/granting.md`, composed into its prompt);
    grants onto runners it did not provision stay operator-scoped. It does not
    run arbitrary `exec` or
    provision targets itself — that boundary is unchanged: reasoning decides
    *what* to do, the deterministic runner/CP layer does it auditably.

*   **It never sees plaintext secrets** and references credentials by name
    only.

*   The prompt is honest about what is actually callable: it does not claim
    tools the harness lacks; it reports tool errors plainly rather than
    inventing results.

### The console (Go; the loopback admin/ops web surface)

*   **The control plane console is a Go server + CP CLI** (`control-plane/api/console/`
    + `control-plane/api/cmd/freehold-console`): the `/api/*` routes (auth/overview/world/teardown/
    provision/rotate/revoke/grant/DNS/agents/portal) with the SAME security
    guards — NIP-98 operator login (challenge/session), `HttpOnly;
    SameSite=Strict` session cookies, single-use portal tokens, login
    freshness windows, the DNS-rebinding `Origin` guard, and the
    loopback-only-until-authn bind guard. It also carries the box-side CP CLI
    verbs (`provision`/`grant`/`adopt`/`add-secret`/`identity`), so the deploy
    and the rebuild engine ship + drive a Go console end to end. The console
    is the CP's own identity (0600, minted on the box at first serve — never
    shipped) that signs readiness probes against each runner — no side door,
    the runner still fails closed. `contract/console` is the Go client that
    talks to it.

*   **`secrets.json` holds ciphertext only** (pubkeys + sealed blobs; no
    master key). `providers.json` (control-plane only) holds opaque `params`
    per connector the system never parses.

*   **The Go toolchain mirrors the surfaces it drives:**
    `control-plane/secret-management/` reproduces the full provisioner
    (provision/rotate/revoke/grant/adopt/add-secret + the relay channel sync);
    `contract/client/mcp.go` is the signed MCP client that drives a runner
    (`exec`/`status`/`upload`); `contract/console` talks to the console's
    `/api/*` (overview/agents/portal) as an operator session.

*   **`freehold-cli/install/` reads `providers.json`/`secrets.json`
    and builds the CP + co-located runner** (the box bootstrap that exists
    before any terraform; the CP-owned service definitions live in the
    `cpbuild/terraform` module above).

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

### Transient access (install/uninstall before the runner exists)

A box reaches the Proxmox host **as root over SSH** before (or without) the CP's
co-located runner, using a DOOR_SPEC key it derives deterministically from its
agent-ops identity seed (`crypto.SSHPrivateKeyPEMFromSeed`). `providers/proxmox.SSHExec`
is the `ExecFunc` transport; install's `provision`/`storage`/`deploy-cp` stages and
`uninstall`'s dead-CP / thin-box path ride it, and `install`'s host-side fail-if-live
probe uses it to refuse re-deploying over a live `<name>-cp`. The CP's co-located runner
remains the durable hands once it is up; the transient key is the door that gets it there
(and removes it). A box's own `Box.Engine` never imports the provider — the composition
root injects the transport through the `Provider`/`ProviderFactory` seam.

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
    `proxy` (full grammar in `control-plane/runner`; `TargetId::parse` lives in
    `control-plane/runner`).

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

*   **A crash dump** (`coredumptl`) proves something
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

### Versions, channels, and updates

*   **A version exists only at release** — an annotated `vX.Y.Z` tag and a
    GitHub Release with binary assets (`freehold`, `freehold-console`, `runner`,
    `freehold-agent-tools`, `migrations.tar.gz`, `checksums.txt`). There is no
    changelog file; the GitHub Release (its notes + assets) is the record.
    `0.x.y` is pre-MVP; `vX.Y.Z-rc.N` is the only prerelease vocabulary.
*   **The build stamps its identity** (`contract/version`: `Version`/`Commit`
    via `-ldflags`, the runner's `build.rs` into its MCP handshake); the
    justfile computes `git describe`, CI passes the tag.
*   **The CP carries a version pin** at `<stateDir>/version.json`
    (`{version, channel, commit}`). `install`/`update` write it; `serve` only
    reads it (surfaced on `/healthz` JSON, `/api/world`, `world_status`, and
    `freehold status`); `build`/`teardown` never promote it.
*   **Channels** are `stable` (newest non-prerelease tag), `rc` (newest
    `vX.Y.Z-rc.N`), and `dev` (local tree); any untagged ref is `--ref`/`--sha`
    (no `edge` channel). The channel is a parameter, not a verb: `install
    --channel` seeds it and `update --stable|--rc|--dev` re-stamps it to the
    source it deployed; `freehold status` shows it.
*   **`freehold update`** is remote-world only (never the local CLI): resolve
    source → acquire (release assets sha256-verified, or sandbox clone+build
    with the box's toolchain) → redeploy the CP's binaries → copy migration
    scripts → run pending → repin the version last. `--check` reports without
    changing anything; thin boxes use the transient root-SSH door.
*   **Migrations are scripts, never compiled in** — top-level
    `migrations/<epoch>.sh` (POSIX-sh, no shebang, `0644`), shipped to the CP
    and run with `bash -euo pipefail`; completion is an Omarchy-style marker
    file named for the script (`<stateDir>/migrations/<epoch>.sh`). `install`
    ships the scripts **unmarked**; every world runs each one exactly once —
    a fresh install included, at the end of world bring-up, once the relay,
    k3s and the agent org exist for the scripts to act on. There are no
    reverse migrations (a named gap).

## Build plan (chunked)

**POC (pre-MVP, Kubernetes arrives with Chunks 6–7 — see `docs/POC.md`):**

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

3.  **Chunk 3 — Rust→Go refactor + the modular Go tree:** the operator
    surface is Go across modules — `contract/` (`freehold/contract`, the
    shared wire/trust leaf), `platform/` (`freehold/platform`, the evolving world),
    `agents/` (`freehold/agents`, the agent definitions),
    `freehold-cli/` (`freehold/freehold-cli`, the local operator CLI + install),
    and `control-plane/`
    (`freehold/control-plane`, the stable mechanism) — with the Rust
    `core`/`runner` kept as a byte-exact reference oracle; the local CLI
    drives the shared box engine directly
    (`eng.stdin = ui.in`); teardown keeps the config intact (`PruneLxcCoords`
    is never written to disk); the plane stage is never skipped
    (`TestManagedForFlags`, `TestWorldManaged`, `TestParsePctGateway`,
    `TestPlaneStageNeverSkipped`, `TestParseDnsList`);
    `TestDestroyLvmTenantUmountSedPrecedesLvremove` anchors the LVM path at
    `drive/lvm_test.go:463`.

4.  **Chunk 4 — A resilient CPA that creates agents and lives in Buzz:**
    the CPA runs on the `buzz-acp`/`goose-class` harness as a k3s pod, with the
    CP's `freehold-agent-tools` toolset
    and the durable-plane identity/memory guarantees; `freehold-teardown`
    destroys LXCs but keeps the **recorded coordinates**; `freehold install`
    drives the shared box engine for CP bootstrap.

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
    conventions from this file; the Go acceptance gate
    (`control-plane/acceptance/`) reproduces **Chunk 1**'s acceptance criteria
    hermetically on loopback.

## Locked decisions

*   **Deterministic agent pods** (public release); connections as env vars;
    secrets via the provisioner model (a runner holds ciphertext + an
    injected key; an agent uses, never reads).

*   **Agent placement:** every agent (CPA and created alike) runs on the
    `buzz-acp` harness as a bare k3s pod. The four departments are agents on the
    same harness class, installed as part of the core build.

*   **The agent org is two tiers: the CPA and four departments.** The CPA is the
    sole user touchpoint; Network / Data / Compute / AI
    are its direct reports, each a distinct identity scoped to one
    domain. Communication is unrestricted; capability execution is bounded — a
    department-owned capability is executed by that department's identity and
    its raw grant attaches there, never to a custom agent. Service lifecycle is
    not a department — the agent that created a service owns it. The four are installed
    at build, in the private `#freehold` plus a private `#freehold-<department>` channel with the CPA
    in all; only the identity/grant separation is locked.

*   **Buzz required; the management relay is created by the install; one
    relay per control plane.**

*   **LiteLLM** as a k8s Deployment (replicas), config mounted, state in
    Postgres.

*   **Storage**: ZFS now → Ceph on 2nd box. Layered reliability.

*   **Buzz Relay headless**; the first-party **console** is the admin/ops
    surface.

*   **K8s arrives with Chunks 6–7 — the public release (MVP), not before.**
    `docs/POC.md` anchors both halves of that claim.

*   **Generic exec runner** (an agent writes commands; a runner owns the
    connection and streams + audits them).

*   **`exec(cmd, target, stream?)` and `run_call` are the only tools**; one
    `exec` tool per runner; `//>`/`<!` frames stream the result.

*   **Nothing in the POC needs a cluster**; POC = a CPA that **lives in
    Buzz** + skills.

*   **The `/srv/data` convention** (durable state) and `freehold-cli/tui`'s
    single activity view (**ALWAYS** the top line).

*   **`<n>` is a chunk number and `<module>/<pkg>`** is the module path for
    every Go package above.

## Verification

- Everything under `docs/` (this file, `VISION.md`, `ROADMAP.md`, `POC.md`,
  `BUZZ_SURFACE.md`, `followups.md`) plus the root docs (`README.md`,
  `AGENTS.md`) describe the current state; the decisions above are current.
