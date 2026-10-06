# freehold

> [!WARNING]
> **Pre-alpha, under heavy development.** freehold is **not ready to use**. It changes
> meaningfully day to day, so expect breaking changes, missing features, and possible data
> loss. Don't run it anywhere you care about yet.

freehold is an open-source appliance that is operated by AI agents. It lands a full
self-hosted stack on a Proxmox VE host or VPS — a self-hosted
[Buzz](https://github.com/block/buzz) relay as its hub, Kubernetes, reverse proxy and TLS,
and its own control plane. An Orchestrator and four department agents — Network, Data,
Compute, and AI — run it and create custom agents on request. Whatever you want can be
answered, built, hosted, and delivered.

## Contents

- [Agents](#agents)
- [Vision, Architecture & Roadmap](#vision-architecture--roadmap)
- [Getting started](#getting-started)
  - [The appliance: one binary, two surfaces](#the-appliance-one-binary-two-surfaces)
  - [The config](#the-config)
  - [The other binaries](#the-other-binaries)
- [How it works](#how-it-works)
- [Repository layout (what things do in the code)](#repository-layout-what-things-do-in-the-code)
- [Contributing / review](#contributing--review)

## Agents

freehold is run by two tiers of agents, all living in Buzz:

**Orchestrator**

| Agent | Role | Services managed |
| --- | --- | --- |
| **@freehold** | The main touchpoint, on Buzz's `buzz-acp` harness (prompt in `agents/freehold/`). Holds the conversation, plans, delegates, and creates custom agents. | Agent lifecycle, agent grants |

**Departments**

Each department is scoped to one domain and the capabilities it owns.

| Agent | Role | Services managed |
| --- | --- | --- |
| **@network** | The network surface: access and exposure (external proxy, DNS, ingress). | Caddy, TLS, dnsmasq, Cloudflare, Tailscale |
| **@data** | The data plane: backups, storage, durability. | Proxmox storage (LVM/ZFS), PBS, TrueNAS, Backblaze (future) |
| **@compute** | The box itself and provisioning: CPU/RAM/disk, LXC and kube, remote hosts, monitoring. | Proxmox, k3s, Vultr, Hetzner, monitoring |
| **@ai** | Models, providers, and agents, plus local AI hardware. | LiteLLM, local AI (RTX 3090 / DGX Spark), benchmarking |

Talk is unrestricted; capability execution is bounded, and the first run the operator
meets is freehold's, not the desktop app's — see
[`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) (the agent org) and
[`docs/AI.md`](docs/AI.md) (the runtime).

## Vision, Architecture & Roadmap

- [`docs/VISION.md`](docs/VISION.md) — the narrative and the "why".
- [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) — the system design and the locked decisions.
- [`docs/ROADMAP.md`](docs/ROADMAP.md) — the ordered view: Now / Next / Later, the
  cross-domain milestones, and the MVP definition.
- One doc per domain (the same buckets as the agents): current architecture, known gaps,
  and future work —
  [`docs/AI.md`](docs/AI.md) (LiteLLM, the agent runtime),
  [`docs/NETWORK.md`](docs/NETWORK.md) (gateway, edge, public path),
  [`docs/DATA.md`](docs/DATA.md) (durable plane, snapshot/export/backup),
  [`docs/COMPUTE.md`](docs/COMPUTE.md) (providers, guests, storage, k3s), and
  [`docs/FREEHOLD.md`](docs/FREEHOLD.md) (the core platform's gaps, plans, and UI/UX).
- [GitHub Releases](https://github.com/darcy/freehold/releases) — the released versions,
  their notes, and assets.

## Getting started

Build once, then operate the world — the justfile only builds and installs, it never drives
the world itself.

Prereqs:

- **[mise](https://mise.jdx.dev/)** — installs and pins the toolchains (Go 1.25.0, Rust 1.98.0)
  and runs them through `mise exec`, so Rust and Go need no separate install
  (`curl https://mise.run | sh`, or `brew install mise`).
- **[just](https://github.com/casey/just)** — `mise use -g just`, or `brew install just`.

`just install` builds everything and puts `freehold` (plus the siblings it resolves at
runtime) on your PATH; `just build` alone leaves them in `target/`.

```sh
just build    # build the full binary set into target/debug + target/release
just install  # build, then place freehold + siblings on PATH
just test     # the full gate: Rust fmt/build/test + Go build/vet/test + the acceptance gate
```

Then operate the world yourself:

```sh
freehold install --non-interactive --name <world> --host root@<box> \
                     --relay-domain <relay.host> --cp-domain <cp.host> --proxy-ip <ip/cidr> \
                     --operator-pubkey <64-hex> [--operator-identity <dir>]  # box one: create the CP only (door -> cp LXC + console + co-located runner), then STOP
freehold build       # ANY box (login-gated): trigger the console's /api/world-build — the CP brings up relay/agent-tools/k3s/storage/DNS/litellm/caddy/cert through its co-located runner
freehold teardown    # drop the WORLD (the inverse of build): relay/k3s + the CP-side
                     #  agent-tools process go, internal DNS clears — the CP, its runner,
                     #  the durable plane, and the certs all STAY; data is kept
freehold uninstall [--remove-data]  # drop the CP too (this box's doors + local state go;
                     #  --remove-data also drops the durable plane). Runs from the build box
                     #  or, for a thin box / dead CP, over direct root SSH; --remove-data
                     #  still needs the build box
freehold            # the TUI dashboard
```

`freehold install` (guided) or `install --non-interactive` (headless) requires
`--name` + `--host`: the profile name scopes the config + state to
`profiles/<name>/` and prefixes the guest LXCs `<name>-<role>`; the host is
recorded in the profile so `uninstall --name` can resolve it. A fresh plane also
needs the relay/CP domains + proxy IP (the guided flow prompts for them) and
the operator identity: `--operator-pubkey` (headless; `--operator-identity`
seeds this box's login ledger from a keypair dir, verified against the
pubkey — the guided flow pastes or mints it). An
existing name whose CP is absent is re-adopted (the plane keeps the runner
identity); a **live** CP is refused — reconcile the world with `freehold build`,
drop it with `teardown`/`uninstall`, or join it with `freehold login`.
`install --non-interactive` is the headless surface. (The gateway, subnet, and
runner port need no answers — derived or picked; `docs/NETWORK.md`,
`docs/FREEHOLD.md`.)

### The appliance: one binary, two surfaces

```sh
freehold                      # no args → the interactive TUI (bubbletea); a subcommand → the CLI
freehold login                # root-free: CP address + operator nsec (NIP-98) → authorize,
freehold                      #   seed a local connection profile from the CP, then END — just run `freehold`
freehold logout               # clear THIS box's login ledger (CP/world untouched)
freehold status               # the CP's single inventory (read via public /api/world)
freehold build                # trigger the CP's world-build (co-located runner)
freehold teardown             # CP-preserving world teardown
freehold update               # update the world's CP (release assets / ref / dev),
                              #  run pending migrations, repin the version
freehold uninstall [--remove-data]  # remove the CP + world + this box's doors (data kept;
                              #  --remove-data drops the durable plane); a thin box / dead CP
                              #  uninstalls over direct root SSH
freehold snapshot [label]     # snapshot the whole durable plane under one name
                              #  (--list / --rm / snapshot rollback — guarded,
                              #  guests stop, the CP comes back via the update flow)
freehold export [outfile]     # the durable plane's data + profile config into one
                              #  gzip bundle (du estimate first, confirm; no rootfs)
freehold backup init/run/snapshots  # restic off-site backup of the durable plane's
                              #  /srv/data mounts to a repo URI (init settles the
                              #  repo password with the repo as arbiter; run also
                              #  ships the profile config; snapshots lists them)
freehold backup install-timer # the host-side nightly backup + weekly repo-check
                              #  systemd timers, rendered from the verb's own
                              #  restic line (re-run after plane changes)
freehold door authorize       # authorize this box's door key on the host (DOOR_SPEC)
freehold door revoke          # remove this box's door key from the host door
freehold exec <target> "cmd"  # exec through a local runner, or (thin box, no
                              #  [runner]) through the CP's runner via world_exec
freehold provision --kind …   #   self-staged stages: provision, deploy-cp,
freehold deploy-cp …          #   storage, add-relay-member
freehold --help               # both surfaces
```

**Tenants (profiles)** — every box can hold several tenants, one per **profile**.
Each profile is its own config file (`~/.config/freehold/profiles/<name>/config.toml`)
plus its own scoped state dir (`~/.freehold/profiles/<name>/`). The filesystem is
the registry; `freehold profiles` lists them. `freehold login` **adds** a named
profile (default name = the CP host), and the TUI plus `build` / `install` /
`teardown` / `world` pick which profile (tenant) to operate when more than one is
registered (a picker), failing closed with "run `freehold login` first" when none
are. There is no implicit "default" profile.

**TUI modes** (auto-detected from the selected profile's config):

- **bootstrap** — no tenant profile selected: `freehold login` adds one, then
  `freehold build` runs the bring-up stages
  (provision → install the SSH door → grant → serve → verify the door with a
  real exec) and writes the profile's config.
- **configure** — config present, world not converged: an idempotent
  check-then-run pipeline (relay/cp LXCs, deploy relay + cp). Failed stages
  show their tail; `r` retries.
- **running** — the post-bring-up dashboard: six views cycled with `Tab` /
  `Shift-Tab` — **Services** · **Agents** · **Runners** · **Data** · **DNS** ·
  **Certs** — plus a one-line world strip. `l` re-logs into the CP console,
  `w` opens the web console already authenticated (single-use portal token),
  `s` edits the operator settings (today: the timezone agent pods run).
- **Remote-CP access** — `freehold login` is root-free: it authorizes this
  operator against the CP by address + nsec (NIP-98), seeds the profile from
  the CP's `/api/world` summary, and ends — a fresh box recovers with nothing
  that lived only on a lost one. `freehold logout` clears the local ledger only.
  (The trust model, the identities, and the login-only-box behavior:
  `docs/ARCHITECTURE.md`.)

#### The config

Each tenant profile's config lives at `~/.config/freehold/profiles/<name>/config.toml`
with its state under `~/.freehold/profiles/<name>/` (overridden by `FREEHOLD_HOME`).
The config is that tenant's CONNECTION/DESIRE profile:

```toml
domain = "freehold-test.example.com"
relay_url = "https://freehold-test.example.com"
cp_url = "https://cp-freehold-test.example.com"
operator_pubkey = "1dc07610…"           # console admin + relay owner
operator_identity = "/home/you/.freehold/control-plane/operator"   # YOUR key, 0600 —
                                        # the TUI auto-logs in with it; optional when
                                        # you pasted an npub (use the portal to authenticate)
managed = ["relay", "cp"]      # what WE operate — an invited relay wouldn't be here

[runner]                       # the provisioning door (the exec path into the host)
addr = "127.0.0.1:8787"
pubkey = "f7510b07…"           # the runner's own identity (filled at config-write)
target = "pve-ssh-root"        # the fixed capability name (--target pre-0.8 worlds may differ)

[lxc.relay]                    # connect/status coords only; sizing is bootstrap-time
vmid = 100
ip = "192.168.30.8/24"

[lxc.cp]
vmid = 101
ip = "192.168.30.9/24"
```

### The other binaries

`freehold-console` (the CP's admin/ops web surface + its CLI verbs),
`freehold-agent-tools` (the CP toolset the agent pods bridge), and `runner`
(the privileged exec connector) run **inside the world or its engines** — you
never type them to operate a world. Their command surfaces, flags, and the
state-dir layout are in [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md)
(The console; Runners) and [`docs/AI.md`](docs/AI.md) (Runners and secrets).

## How it works

Three roles, one primitive. **Agents** are the brain; **runners** are dumb privileged
hands — an agent signs a call and the runner executes one generic primitive,
`exec(cmd, target, stream?)`, with no semantic tools. The **control plane** (admin/ops
only — chat is Buzz's job) provisions secrets: it seals a credential TO a runner's key and
ships ciphertext — no master key, plaintext never on disk, agents reference credentials by
name only. Agents live in Buzz as pods; one control plane is exactly one relay scope.
Host-flexible: Proxmox leads, VPS/cloud are first-class.

Walk the lifecycle (bootstrap, a runner from credential to first exec, one exec call, the
security model) in [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) and
[`docs/AI.md`](docs/AI.md) — both carry the sequence diagrams.

## Repository layout (what things do in the code)

```
Cargo.toml            Rust workspace: control-plane/core, control-plane/runner,
                      control-plane/testkit, control-plane/core/harness/oracle
contract/             freehold/contract — the shared wire/trust leaf: crypto/ (Go repro
                      of the Rust core, byte-exact cross-verified), wire/, client/,
                      config/, console/, litellm/ (the curated gateway-provider
                      table), relay/, identity/, worldfacts/, delegate/,
                      nipoa/, version/. Its own Go module — the edge is
                      platform → contract ← control-plane (no module cycle).
control-plane/        freehold/control-plane — the stable mechanism (Go, plus Rust for
                      runner + core): api/ (the scoped API + cpbuild — the world build
                      engine + the embedded terraform module), api/console/ (the
                      admin/ops web surface), api/agenttools/ (the agent registry +
                      tool server), api/agent/ (the agent pod runtime),
                      secret-management/ (the provisioner), state/ (the CP store),
                      acceptance/ (the Go acceptance gate driving the real runner),
                      core/ + runner/ + testkit/ (Rust: the byte-exact oracle, the
                      privileged exec connector, the hermetic fixtures).
freehold-cli/         freehold/freehold-cli — the local operator surface (never imported
                      by control-plane/): one dir per verb (install/, uninstall/,
                      build/, teardown/, status/, update/, exec/, profiles/, door/,
                      dns-cred/, backup/), plus login/, tui/, internal/ (common,
                      cpdeploy, certcred, stages).
providers/            freehold/providers — the substrate providers; proxmox/ holds
                      guest create/exec/list, storage, the pct stage/DNS builders,
                      and the world-destroy engine. Imports platform/ + contract/;
                      never the reverse.
platform/             freehold/platform — the provider-independent world the mechanism
                      installs/evolves: services/<capability>/<impl>/ (relay/buzz,
                      webproxy/caddy, externaldns/cloudflare,
                      certificates/letsencrypt), provisioning/ (bootstrap, box,
                      deploy, planebase, stages, the provider seam), migrations/.
                      Adding a service touches only this module — never control-plane/.
migrations/           the box-applied migration scripts
AGENTS.md             agent guidance: locked model, conventions, known gaps
docs/                 VISION.md, ARCHITECTURE.md, ROADMAP.md, FREEHOLD.md, AI.md,
                      NETWORK.md, DATA.md, COMPUTE.md, BUZZ_SURFACE.md, DOOR_SPEC.md
```

Per-file and per-package detail: `docs/ARCHITECTURE.md` ("The pieces") and the module
READMEs.

## Contributing / review

Every PR runs two gates:

- **CI** (`ci.yml`): rust (`cargo fmt --check`, `build`, `clippy -D warnings`, `test`),
  Go (build/vet/test per module), and the review harness's node tests — fanned into a
  required `check`. Green/red, no exceptions.
- **AI review** (`ai-pr-review.yml`, "Bot Review"): an opencode headless session explores
  the repo with read-only tools and verifies the diff's claims against the actual code.
  It runs `pull_request_target`-only (what executes is always trusted `main` — a PR can
  never rewrite what reviews it) and ends with a computed verdict submitted as a PR
  review state: `APPROVE` when clean, `REQUEST_CHANGES` with findings — so branch
  protection gates the merge. Findings are tiered 🛑 BLOCKING / ⚠️ IMPORTANT /
  💡 SUGGESTION / NIT; the operator can dismiss a review. README/ARCHITECTURE drift is
  rated like any other finding.

Read `AGENTS.md` before changing code: the locked model (relay-as-scope, generic exec, no
master key, host flexibility) is not open for reinterpretation. Never commit secrets,
private keys, or plaintext credentials.
