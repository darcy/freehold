# freehold Refactor Plan — control-plane as mechanism, platform as evolving world

This plan restructures the repo around what the system actually is today: **a control
plane that is the stable mechanism** and **a platform of services/agents it installs and
evolves**. It kills the `orchestrator/` name (nothing is "orchestrated" anymore), folds
the operator CLI in as the control plane's own interface, unifies the CP's two
read/action surfaces into one scoped API, and keeps Rust only where it genuinely earns
its keep (the runner + the byte-exact contract oracle).

---

## 1. High-level direction

- **`control-plane/` = the stable mechanism.** The unified scoped API, the operator CLI
  (as `control-plane/cli/`), the web UI, secret management, the CP's own state, the
  runner, and the byte-exact `core` contract. This is the thing you stabilize once.
- **`platform/` = the evolving world the mechanism installs.** Services, agents (CPA +
  future), migrations, terraform, provisioning, the durable-plane conventions. Adding a
  security agent or a new service means adding a `platform/` entry — never touching
  `control-plane/`.
- **One API, scope-gated.** `control-plane/api/` is the single surface for *every
  operator/world-management* action (world build/teardown/migrate,
  provision/rotate/revoke/grant, agents, status, DNS). Access scoped `admin` (operator +
  CPA) now; `read`/`write:controls` granularity later. The CLI and the CPA are just two
  channels onto it. **It deliberately stays out of the agent→runner exec path** — agents
  call runners directly with their own keypair (the locked model), unchanged.
- **Rust only where it earns it.** The runner (privileged exec endpoint) and `core`
  (byte-exact trust/wire contract oracle) stay Rust. All CP *logic* is Go; the Rust
  console's routes are reimplemented in Go as part of the unified API and the Rust
  `control-plane` crate is deleted when parity lands (finishing the Chunk 3 intent).
- **The CLI keeps a door.** The CP cannot bootstrap or tear down itself. The box's CLI
  holds its own door (SSH key authorized by the CP) for the day-0 `bootstrap-cp` and the
  `teardown-cp` lifecycle; world ops go through the CP API.
- **World lifecycle + teardown order + LXC ownership:** `bootstrap-cp` → the CP executes
  `platform` (terraform/migrations/services/agents). Teardown is CP-first: `world_teardown`
  (CP-run) destroys the relay + k3s LXCs through its co-located runner and unwinds
  runners/secrets/registry/DNS, then `teardown-cp` (box door) destroys only the CP LXC.

---

## 1.5. Roadmap & versioning position (when this runs)

**This is the next thing to deliver in Chunk 4** — a working, live CPA is current, and
Chunk 5 (agent workspaces + git) follows. It is **not** deferred to the k8s chunks
(Chunks 6–7, MVP): the tree is restructured now, against a *stable* CPA, precisely so the
Chunk 5 work lands in the right home instead of being moved later.

Two constraints this positions against:

- **`platform/services/{llmproxy, database, webproxy}` and `terraform/` are MVP-only
  workloads on the roadmap** (k8s arrives Chunks 6–7), yet the tree restructures them now.
  That is deliberate and bounded: the *directories* land now (so Chunk 5 has its true
  home), but the *interfaces* they hold stay as they are today — nothing in `platform/`
  becomes a live service before its chunk. The re-org is structural, not functional.
- **AGENTS.md's cadence applies unchanged:** this lands as a phase with a `0.x.y` bump
  (0.5.x territory, since Chunk 4 wraps and Chunk 5 begins) via branch → PR → main, one
  `0.x.y` per phase, tagged `v0.x.y` at merge. Each of §8's phases (0–3) is a separate
  reviewable PR; Phase 0 (the mechanical re-org) ships first so every later change lands
  in its true home.

Net: restructure now (Chunk 4 wrap / 0.5.x), so Chunk 5's agent-workspace work builds on
the new tree; the platform services' *capabilities* are staged behind their roadmap
chunks regardless of where their directories sit.

---

## 2. Target structure

```
contract/                 the shared wire/trust contract — imported by BOTH control-plane
                          and platform (the leaf everything builds on):
  crypto/  wire/  client/  state/  config/  console/  relay/
control-plane/            the stable mechanism (Go logic; Rust only runner+core)
  api/                    unified scoped API (console routes + world_* + agent toolset)
  cli/                    the operator interface: tui/ login/ build/ teardown/
                          world/ bootstrap-cp/ teardown-cp/  (+ the box's own door)
                          (imports the contract; holds no crypto/wire/client of its own)
  web-ui/                 embedded console HTML
  secret-management/      provision/rotate/revoke/grant (was provisioner/)
  state/                  the CP's operational registry INSTANCE (state.json; the
                          model lives in contract/state/)
  runner/                 (Rust) privileged exec endpoint + the CP's own door key
  core/                   (Rust) byte-exact contract oracle + harness/ (gates
                          contract's Go repro against it; test-only)
  tests/

platform/                 the evolving world the mechanism installs/evolves
  services/               capability/<implementation>/ (see §6)
    webproxy/caddy/             TLS edge (presents certs, owns edge config)
    llmproxy/litellm/           LLM gateway
    relay/buzz/                 the relay stack (incl. its own postgres/redis/minio)
    internaldns/dnsmasq/        split-horizon resolver (deploys onto the CP node)
    externaldns/cloudflare/     A-record management (provider registry)
    certificates/letsencrypt/   cert issuance (lego provider registry)
    database/postgres/          litellm/agents backing store
  provisioning/           compute+storage bring-up: LXC/VPS bootstrap, k3s, durable
                          plane, the shared stages library (imports contract/client)
  agents/                 freehold/ vault/ security/ templ/ (prompt + creation spec + …)
  migrations/             verify-gated migrations (config/prompt/repair)
  terraform/              IaC the CP executes (k3s, postgres, litellm…)
  data/                   /srv/data plane layout + tenant conventions
  tests/

docs/  roadmap/  CHANGELOG.md  README.md  AGENTS.md
```

### Top-level files that move or disappear

- `orchestrator/` → **deleted as a name.** Redistributed into `control-plane/{api,cli}`
  and `platform/` (mapping in §4).
- `control-plane/` (Rust crate) → **becomes the Go mechanism**; the Rust `web.rs` routes
  are reimplemented in `api/`, and the crate is deleted at parity.
- `runner/`, `core/`, `console-client/`, `testkit/`, `acceptance/` → fold under
  `control-plane/` (they are the CP's own exec/contract/test surface).
- `terraform/` → `platform/terraform/`.
- `orchestrator/harness/` + `orchestrator/harness/oracle` → `control-plane/core/harness/`
  (the Go↔Rust byte-gate sits with the contract it verifies).
- `migrate-go.md`, `orchestrator/WIRE.md` → superseded; their truth lands in
  `control-plane/core/` docs.

---

## 3. Principles (locked)

1. **One API, scope-gated — but it does NOT sit in the agent→runner data path.**
   `control-plane/api/` is the single *operator/world-management* surface (provision/
   rotate/revoke/grant, agents registry, world build/teardown/migrate, status, DNS),
   scoped `admin` (operator + CPA — full) now; `read` / `write:controls` later.
   **The runner remains an independent peer, and agents connect to it DIRECTLY with
   their own keypair** (grant = NIP-29 roster membership; `exec(cmd,target,stream?)`
   is the one generic tool), exactly as the locked model has it. The API manages the
   *grants on* runners (who is membered into a runner's roster) and the *secrets*
   (provisioned ciphertext), but never proxies agent exec — no router, no key vault.
   The CPA's "conversation + create only" boundary and a created agent's direct
   agent→runner path both keep working unchanged.
2. **Control plane = mechanism; platform = evolving world.** The CP applies platform
   via terraform/migrations; adding capabilities never touches `control-plane/`.
3. **Rust only for runner + core.** The privileged exec edge and the byte-exact
   contract oracle. Everything else is Go. The Go crypto/wire reproduces `core`
   byte-exactly, gated by the harness.
4. **The CLI is the CP's interface, not a peer.** It is `control-plane/cli/`. World ops
   are API calls; CP-lifecycle (`bootstrap-cp`, `teardown-cp`) uses the box's own door.
5. **"orchestrator" is gone** from terminology, paths, and binaries. The
   `freehold-orchestrator` binary folds into `freehold`, **with the sibling-resolution
   contract preserved**: the rebuild/teardown engines resolve binaries relative to
   `os.Executable()` (`resolveRebuildBins`, rebuild.go:309), so `freehold` must keep
   finding the runner/control-plane/toolset siblings wherever it is invoked. The toolset
   binary shipped to agent pods must stay a **static Alpine-compatible build**
   (`CGO_ENABLED=0`, rebuild.go:338) regardless of where it lands.
6. **Capability/implementation service naming.** `platform/services/<capability>/<impl>/`
   where the capability is a stable interface and the impl is swappable tech. The
   interface seam is real only where a swap is plausible (`externaldns`, `certificates`,
   arguably `llmproxy`); opinionated services keep the dir but no over-abstraction.
7. **State lives with its owner.** The CP's operational registry instance (`state.json`)
    is `control-plane/state/` (the mechanism's memory, not terraform state, not user data),
    built on the shared `contract/state/` model. Terraform state → `platform/terraform/`.
    User/tenant data → `platform/data/`.
   The box-local `state.json` mirror is deleted once the CLI is a pure API client.
   The planned MVP Postgres swap (AGENTS.md known gap) replaces this same store — the
   "control_plane Postgres" is the mechanism's store in its future form, not a platform
   service (§6).
8. **World lifecycle + teardown ordering + LXC ownership:** `bootstrap-cp` (box door) →
    the CP executes `platform` (terraform/migrations/services/agents). **Teardown is
    CP-first, preserving the 0.4.7 ordering.** LXC ownership is explicit:
    `world_teardown` (CP-run through its co-located runner → host `pct`) destroys the
    **relay + k3s LXCs** — the world the CP itself built (the same runner `world_build`
    used to boot them) — and unwinds runners/secrets/registry/DNS; `teardown-cp` (box
    door) destroys **only the CP LXC**. `world_teardown` runs BEFORE `teardown-cp`; the
    CP cannot unwind its managed state after it is gone, so the order is fixed.
    **The durable-plane split (compute vs data) survives per path.** Each teardown path
    is compute-only by default and destroys the tenant datasets (`/srv/data` LVs, the
    thin pool) only with an explicit `--data`, exactly as today (`teardown.go`): compute
    keeps the recorded coords + `/srv/data` LVs so a rebuild re-boots the SAME world;
    `--data` erases them. Per path: `world_teardown` (compute) destroys the relay+k3s
    LXCs but keeps their tenant LVs; `world_teardown --data` additionally destroys the
    relay + k3s-volumes datasets. `teardown-cp` (compute) destroys only the CP LXC,
    keeping the cp dataset; `teardown-cp --data` destroys it too. The "reconstructible
    from `/srv/data`" claim holds: compute-only never touches the durable plane.

---

## 4. Package mapping (today → target)

| Today | Is | Target |
|---|---|---|
| `orchestrator/internal/cli` | CLI verbs + rebuild engine | `control-plane/cli/` (split: build→bootstrap-cp + world trigger) |
| `orchestrator/internal/tui` | the operator dashboard | `control-plane/cli/tui/` |
| `orchestrator/internal/oplogin` | login ledger | `control-plane/cli/login/` |
| `orchestrator/internal/config` | config model | `contract/config/` (shared: cli + api + platform import it) |
| `orchestrator/internal/crypto` + `wire` | Go repro of `core` | `contract/crypto/` + `contract/wire/` (shared leaf — platform needs them too) |
| `orchestrator/internal/client` | signed MCP client | `contract/client/` (shared leaf — bootstrap/deploy/drive in platform import it) |
| `orchestrator/internal/console` | console client | `contract/console/` (shared leaf — cli + api + agent-tools import it) |
| `orchestrator/internal/relay` | relay HTTP client (wire layer) | `contract/relay/` (shared leaf — cli + agenttools + delegate import it) |
| `orchestrator/internal/flows` | signed flows | `control-plane/cli/` |
| `orchestrator/internal/agent` + `agenttools` | the world/agent toolset | `control-plane/api/` (the toolset folds under the unified API) |
| `orchestrator/cmd/freehold-agent-tools` | the MCP server binary | `control-plane/api/` |
| `orchestrator/internal/state` | CP store model | `contract/state/` (shared: flows + provisioner + cli import it) |
| `control-plane/src/provisioner.rs` | secret lifecycle | `control-plane/secret-management/` (Go port) |
| `control-plane/src/state.rs` | CP registry (Rust) | `control-plane/state/` (Go port at parity; `internal/state`'s model lives in `contract/state/`, the CP serve holds its own instance) |
| `control-plane/src/dns.rs` | split-horizon resolver | `platform/services/internaldns/dnsmasq/` (runs on the CP node — a deploy detail) |
| `internal/dnsman` | external A-record manager | `platform/services/externaldns/cloudflare/` |
| `internal/cert` | LE issuance | `platform/services/certificates/letsencrypt/` |
| `internal/deploy/deploy_relay.go` | relay deploy driver | `platform/services/relay/buzz/` |
| `internal/deploy/caddy.go` | caddy edge | `platform/services/webproxy/caddy/` |
| `terraform/manifests/litellm.yaml` | litellm kube | `platform/services/llmproxy/litellm/` |
| `terraform/manifests/postgres.yaml` | postgres kube | `platform/services/database/postgres/` |
| `internal/stages` | shared bring-up library | `platform/provisioning/` |
| `internal/bootstrap` | LXC/VPS provisioning drivers | `platform/provisioning/` |
| `internal/planebase` + `drive` | durable-plane storage | `platform/provisioning/` |
| `internal/migrations` | verify-gated migrations | `platform/migrations/` |
| `orchestrator/prompts/CPA_SYSTEM_PROMPT.md` | CPA prompt | `platform/agents/freehold/prompt.md` |
| `internal/deploy/deploy_cp.go` + `deploy_agent_tools.go` | installs the CP itself | `control-plane/cli/bootstrap-cp/` (day-0 mechanism install) |
| `internal/teardown` | world teardown | `control-plane/cli/teardown-cp/` (box door) + CP API `world_teardown` |
| `runner/` | privileged exec endpoint | `control-plane/runner/` (Rust, unchanged) |
| `core/` | byte-exact contract oracle | `control-plane/core/` (Rust, unchanged) |
| `console-client/`, `testkit/`, `acceptance/` | CP wire/tests | `console-client/` (Rust) is **deleted at parity with the Rust console** in Phase 3 — its only consumer is `web.rs`; the Go `cli/console` client + `api/` replace it. `testkit/`/`acceptance/` fold into `control-plane/` tests. |

### CLI verb inventory (every live verb gets a home)

README lists a dozen-plus live verbs beyond build/teardown/login. Under "world ops go
through the API, door for CP-lifecycle only," each has an explicit home:

| Verb | What it is | New home |
|---|---|---|
| `login` / `logout` | operator auth / local ledger | `control-plane/cli/login/` (unchanged) |
| `exec` | signed exec against a runner | **deleted as a CLI verb** — it is the runner's own MCP surface; the box's world ops use the API, and agents exec runners directly. (The verb remains internal for `bootstrap-cp`'s door work, but at day-0 it runs through the box's OWN provisioning runner/door — the co-located runner does not exist until `bootstrap-cp` creates it.) |
| `onboard` | provision + grant an existing service | **world op → `api/`** (`provision` + `grant`; the CP registers the runner + roster) |
| `demo` | local loopback demo world | `control-plane/cli/` (dev-only, kept local) |
| `readiness` | runner self-check | **world op → `api/`** (`world_status` reports readiness from the console overview) |
| `memory` | relay-persisted agent memory (kind 30174) | **neither CLI nor API** — the CPA/agents' own relay surface (unchanged; agents read/write memory against the relay, not the CP) |
| `delegate` / `delegate-peer` | agent delegation | **neither CLI nor API** — the CPA's relay surface (kind-9 channel delegation, unchanged) |
| `relay-profile` / `relay-join` / `relay-setup` | runner channel/roster setup | **CP-lifecycle/world op** — folds into `world_build`'s runner-channel sync + the API's grant; these ran through the box runner, so they move to the co-located-runner path or the API |
| `relay-member` | community membership via `buzz-admin` through the box runner | **world op → `api/`** — membering a new runner/agent is a world-management action the CP performs through its co-located runner (same `buzz-admin` in the relay compose), not a box-local verb |
| `console-login` | NIP-98 session setup | **folded into the unified API's auth** — the API's NIP-98 login is `console-login`; the standalone verb disappears (its single-use portal path already supersedes it per README) |
| `bootstrap` / `deploy-relay` / `deploy-cp` | world bring-up | `control-plane/cli/bootstrap-cp/` (day-0) + `world_build` (the CP deploys the relay stack) |
| `storage` / `destroy` / `destroy-pool` / `dns-cred` / `install` | storage/DNS/install helpers | `platform/provisioning/` (storage/destroy/dns-cred, invoked via the API) + `control-plane/cli/bootstrap-cp/` (install) |

**Ambiguities resolved:** `relay-member` becomes a world op (membering runners/agents is
what the CP manages, through its co-located runner's `buzz-admin` — the box holds no such
privilege). `console-login` is subsumed by the unified API's NIP-98 login; no standalone
verb.

### TUI + offline behavior (the slimmer CLI's dashboard)

Phase 2 deletes the box-local `state.json` mirror and makes world ops API-only. The six
TUI views map cleanly, and offline/degraded behavior is preserved:

| View | Source today | Source after the slim | Offline/CP-down |
|---|---|---|---|
| Services | `cfg.Managed` + coords + live probes | `world_status` (the CP reports the live inventory) | cached last-good snapshot from the config seed; "CP unreachable" row — never a blank dashboard |
| Agents | toolset `manage_agent` | `world_status`'s agents (same toolset read) | last-good snapshot; "toolset unreachable" row |
| Runners | console `/api/overview` (CP source) **or** local loopback `state.json` (`s` toggle) | console `/api/overview` only; **the `s` loopback toggle is deleted** (there is no local store to toggle to) | "console overview failed" row (as today) |
| DATA | plane config + local runner probe | `world_status`'s data (the CP probes its own plane) | last-good snapshot |
| DNS | `dnsRowsLive` via local runner + console `ListDNS` | console `ListDNS` (no local runner needed) | "resolver unreachable (mirror fallback)" — the config-mirror fallback stays |
| Certs | `cfg.Caddy.*` | `world_status`'s certs | last-good snapshot |

**Offline recovery is unchanged:** `login` seeds a local connection/desire profile from
`/api/world` (relay + CP + agent-tools coords + operator identity) — that seed *is* the
offline store now (config.toml), not a separate `state.json`. A box that can't reach the
CP renders the last-good config-seeded views with an explicit "CP unreachable" state; it
never requires the CP to render its own dashboard. The boot gate already treats a
runnerless profile as satisfied via the console session (login-only Running) — that stays.

### Go module topology (the hard mechanical seam — decided, not hand-waved)

`control-plane/`, `platform/`, and the shared contract are sibling dirs that must NOT be
one module (the CLI is `control-plane/cli/`, the CP's API is `control-plane/api/`, but
`platform/` is a separate concern the CP *imports*, and the bring-up code `platform/` owns
needs the signed-runner client). **Three Go modules:**

- **`freehold/contract`** — the shared wire/trust contract BOTH the mechanism and the
  platform import: `crypto/` (Go repro of Rust `core`), `wire/` (envelopes), `client/`
  (the signed MCP client), `relay/` (the relay HTTP client), `state/` (the store model),
  `config/`, `console/` (the console client). This is the leaf everything builds on; it
  imports nothing internal. It is the **contract**, not the mechanism — the plan's earlier
  "Go repro of `core` under `control-plane/core/`" is corrected: the repro lives here so
  `platform` can import it without a cycle.
- **`freehold/control-plane`** — the CP mechanism: `api/`, `cli/`, `web-ui/`,
  `secret-management/`. Everything that IS the CP.
- **`freehold/platform`** — the evolving world: `services/`, `provisioning/`, `agents/`,
  `migrations/`, `terraform/`, `data/`. Imported by `freehold/control-plane` (the CP runs
  the platform), and by `control-plane/cli` where the box executes platform bring-up.

**Why the third module is required (G4′):** `platform/provisioning` (from `bootstrap`,
`deploy`, `drive`) and `platform/services` (from `deploy`) import `client` (→ `contract`)
and `planebase` (→ `platform/provisioning`). If `client`/`crypto`/`wire` sat in
`freehold/control-plane`, then `platform` would import `control-plane` at the same time
`control-plane` imports `platform` — a Go module import cycle that will not build. The
contract must be a module neither side owns, so the edge is `platform → contract ←
control-plane`, never `platform → control-plane`. `state`/`config`/`console` sit in the
contract too because `flows` (CLI) and `provisioner` (CP) both build on them — a shared
home, not a CP-owned one.

**Module boundaries that must be explicit (they cascade):**

1. **Cross-module `//go:embed`.** The CPA prompt moves to `platform/agents/freehold/`, and
   `control-plane/api` must ship it. A Go package cannot `//go:embed` outside its own
   module — so `platform/agents` exposes the embedded prompt as a Go value
   (`package agents; //go:embed freehold/prompt.md`), and `control-plane/api` imports the
   `freehold/platform/agents` package for the bytes. The embedding stays *in* the module
   that owns the file; importers consume the value, never embed across the boundary.
2. **The harness byte-gate.** `control-plane/core/harness/` gates `contract`'s Go repro
   against the Rust `core`. It lives in `freehold/control-plane` and imports
   `freehold/contract/crypto` (and may import `freehold/platform/agents` if the gate needs
   the prompt bytes). It is test-only, so the dependency edges are `control-plane →
   contract` and `control-plane → platform` — one-way, never platform → control-plane.
3. **Cargo workspace unchanged in shape.** `runner`/`core` stay Rust workspace members
   under `control-plane/`; the three Go modules sit beside them. Rust and Go are
   independent build systems; nothing about the Go module split forces a Cargo change.
4. **The toolset's static build (`CGO_ENABLED=0`)** is per-binary, not per-module — it
   survives unchanged whichever module the toolset binary's `main` lives in
   (`control-plane/api`).

This is the decision Phase 0 executes; "split the Go module(s) as needed" is replaced by
"three modules: `freehold/contract` + `freehold/control-plane` + `freehold/platform`, with
the contract as the shared leaf."

---

## 5. The unified scoped API (`control-plane/api/`)

Today there are **two** CP surfaces that overlap, plus a box-local runner path:

| Surface | Auth | Talks to | Problem |
|---|---|---|---|
| console `/api/*` (Rust web.rs) | NIP-98 session | store | provision/rotate/grant/overview/world/dns/agents/teardown |
| agent-tools `/mcp` (Go) | roster-gated signed header | local registry + co-located runner | `world_*` + create/grant/manage agents |
| box `freehold build/teardown` | local runner + door key | host via SSH | the old local world-driving |

**Duplication found:** `world_status` and `manage_agent` are literally the same code
(both `return t.Console.Agents()`); and there are two agent registries (console
`state.json` agents vs toolset `registry.json`) that have already diverged.
**Resolution — `registry.json` (the toolset's, CP-durable) is authoritative:** the TUI
already reads it (`manage_agent`), 0.4.6 declared it the durable source of truth, and it
survives compute-only teardown; the console `state.json` agents map is folded into it with
a **one-time reconcile at the fold** (re-import console-registered agents into the
registry, then the console reads the registry — one source).

**The consolidation:** one Go API server under `control-plane/api/` that:

- exposes every action: `world_build` / `world_teardown` / `world_migrate`,
  `create_agent` / `grant_agent` / `manage_agent`, provision/rotate/revoke/grant,
  agents, DNS, status, portal;
- authorizes **by scope** (`admin` now; `read`/`write:controls` later) against the
  relay roster — "same tooling, different channels/users". **The roster is the admin
  list:** the NIP-98 session proves the caller's nsec corresponds to a pubkey that is a
  roster member, and the CLI's `freehold world` authenticates the same way login does
  today (NIP-98 against the console, session pubkey checked against the roster); the
  console admin whitelist and the toolset roster converge on that one membership source;
- drives `world_build` through the CP's **co-located runner** (the CP acting as a
  granted operator on its own runner) — this is CP-world-ops, distinct from the
  agent→runner data path below;
- makes **`world_status` the one inventory read** (services/runners/agents/dns/certs/data),
  using the console API underneath — the other views and tools consume it;
- slims the CLI to an API client (world ops) + a door holder (CP lifecycle).

**The agent→runner data path is NOT routed through `api/`.** It stays exactly as the
locked model has it: an agent (CPA or created) holds its own keypair, is membered into a
runner's NIP-29 roster (grant), and calls the runner's single `exec` MCP tool directly —
`exec(cmd, target, stream?)`, signed, audited, fail-closed. The API only *manages* the
grant (adds/removes the agent pubkey in the roster) and *provisions* the secret
(ciphertext); it never sits between agent and runner. Created agents currently ship
"conversational-only, no skill/target" (POC_CHUNK4 E2) — when a created agent gains a
target, it reaches the runner the same direct way, not through the CP.

**What un-wires `grant_agent`:** the toolset's `grant_agent` is today **not wired**
(`registry.go:110`, AGENTS.md known gap) — the runner's whitelist is its relay roster,
whose channel is *owned by the console*, and the toolset holds no channel-owner credential.
The unified API's *absorbing the console* is precisely what un-wires it: `api/` carries the
console's channel-owner credential, so "the API manages the grant" (the §5 promise) becomes
true only because the API now owns the console side of the roster. The plan makes this the
explicit mechanism for grant, rather than silently depending on the known gap — Phase 1
must wire `grant_agent` through the absorbed console-owner credential, and the AGENTS.md
known-gap entry is closed there.

**How the API tells an operator call from a CPA/agent call:** both authenticate as roster
members with the same signed-header scheme, so the rule is **scope = admin for both, but
agent-channel callers have `world_*` excluded by tool-visibility, keyed on a CP-side
identity class** — the distinction lives in the CP, not the roster. The roster is a plain
NIP-29 channel (9007) with plain pubkey membership and no role field (and native kinds
forbid adding one), so the operator-vs-agent split is keyed on **`registry.json` agent rows
vs the console admin/seed grants** (the operator/ops pubkeys seeded at bootstrap): a caller
whose pubkey is in `registry.json` is an agent (create/grant/manage + message tools only);
one in the admin/seed set is an operator (full toolset, incl. `world_*`); read fresh per
call. This is the same distinction the CPA's bridge makes today (`mcp.go:53`), now enforced
server-side on the unified API so it cannot be bypassed by calling the server directly.

`/api/world` (the login/connection seed) keeps its role: "where is the world, who is the
CP" — consumed once at login. It does **not** grow into the inventory; that's
`world_status`'s job. (See the earlier review: connection/recovery vs runtime status are
different concerns with different auth.)

---

## 6. Service naming: capability/implementation

`platform/services/<capability>/<implementation>/` — capability is the stable interface,
implementation is the swappable tech. This is what the code already does: `internal/dnsman`
is a `Manager` interface + `Register`/`For` provider registry; `internal/cert` is the same
over lego's provider registry.

| Capability (stable) | Implementation(s) | Evidence | Swap real? |
|---|---|---|---|
| `webproxy/` | `caddy/` | the TLS edge | unlikely, plain dir |
| `llmproxy/` | `litellm/` | OpenAI-compat gateway | possible, keep seam light |
| `relay/` | `buzz/` | the relay stack (incl. its own db/redis/minio) | locked, plain dir |
| `internaldns/` | `dnsmasq/` | split-horizon resolver | locked, plain dir |
| `externaldns/` | `cloudflare/` | `Manager` + `Register`/`For` | **yes — real interface** |
| `certificates/` | `letsencrypt/` | lego provider registry (Route53 already generated) | **yes — real interface** |
| `database/` | `postgres/` | litellm/agents backing store | possible, plain dir |

Couplings worth stating: `certificates` ↔ `externaldns` share the Cloudflare credential
(DNS-01 needs the TXT record); `webproxy` presents what `certificates` issues; the edge
services (`webproxy`, `internaldns`, `externaldns`, `certificates`) share the proxy IP.

### Agents

`platform/agents/<name>/` carries everything about a named agent — its prompt, creation
spec, harness config, memory layout — and grows beyond prompts over time:

```
platform/agents/
  freehold/prompt.md      (CPA)     → api/ ships this verbatim at create_agent
  vault/prompt.md         (data agent, future)
  security/prompt.md      (security agent, future)
  templ/prompt.md         (ad-hoc agent template) → create_agent copies + interpolates
```

The prompt currently `//go:embed`'d from `orchestrator/prompts/` moves with the agent; the
embedding package moves to a `platform/agents` Go package imported by `control-plane/api`
and `cli`. There is no separate `platform/prompts/` — it's redundant.

### Database (three Postgres, one platform service)

| DB | Owner | Backs | Lives | Tree home |
|---|---|---|---|---|
| Relay Postgres | the relay stack | buzz state | docker named volume under `/var/lib/docker` (backup=1) | `services/relay/buzz/` |
| Litellm Postgres | litellm | litellm state | k8s `litellm` ns, durable PVC | `services/database/postgres/` (standalone — first platform DB; more consumers plausible) |
| control_plane Postgres | the CP | CP state ("reconstructible from") | k8s PVC pinned to `/srv/data/k8s-volumes` | `control-plane/state/` (the mechanism store in its future Postgres form) |

`database/postgres/` stays standalone (not folded under litellm) because the durable-plane
plan and "reconstructible from" framing anticipate shared platform DBs; the interface seam
stays a plain dir until consumer #2 exists. **Reconciled:** the CP's registry is
`state.json` today (one Go store, §2/§4) with a planned Postgres swap at MVP (AGENTS.md
known gap) — that swapped store is `control-plane/state/`, NOT `platform/services/database/`
(the latter is the *platform services'* shared DB, today litellm's). Two different stores,
two different homes.

### Data plane + backup split (the `/srv/data` convention survives the re-org)

`platform/data/` is the **layout + tenant conventions** for the durable plane — it is NOT
a move of the mount points. The load-bearing backup rule (ARCHITECTURE.md "Filesystem
layout convention") is re-anchored, not lost:

- **The `/srv/data` split is a MOUNT-POINT rule, independent of directory structure.** The
  durable tenants (`/srv/data/relay`, `/srv/data/cp`, `/srv/data/k8s-volumes`) stay
  `backup=1` volume mounts born at `pct create`; `/srv/nobackup` stays `backup=0`; the
  relay's docker root stays at `/var/lib/docker` `backup=1`; k3s PVCs pin to
  `/srv/data/k8s-volumes`. `platform/data/` documents this convention (per-tenant dataset
  paths, backup flags, the reattach-by-reference rebuild rule) — it does not own or change
  where the mounts land.
- **`control-plane/state/` and `platform/data/` are different kinds of "data."** The CP's
  registry (`state.json`/Postgres) is `control-plane/state/` (mechanism memory, backed up
  under the cp tenant). `platform/data/` is the *tenant* data layout + conventions
  (services' durable state, agent memory layout) — backed up under their own tenant mounts.
- **The re-org must not re-derive the mount flags from the tree.** A future
  `platform/data` consumer reads the recorded plane mapping (`plane.mounts`), never infers
  backup-ness from a directory name. The "ensure is idempotent and runs every converge"
  rule (AGENTS.md) is carried into `platform/provisioning/`'s plane stage verbatim.

---

## 7. The door model (CP lifecycle)

The CP cannot bootstrap or tear down itself. The **box's CLI holds its own door** (SSH
key authorized on the host) for the CP-lifecycle jobs:

- **`bootstrap-cp` (day-0):** the box's door → bring up the barebones CP LXC → install the
  **CP's own separate door** (its co-located runner's SSH key) → hand off; the CP then
  executes the platform via terraform/migrations.
- **A fresh box logs in:** the box creates a keypair locally; the CP authorizes the box's
  pubkey onto the host door. **Mechanism:** the CP writes to the host's `authorized_keys`
  **through its own co-located runner** (the same runner `world_build` drives — the one
  that already holds a door to the host). This is a new elevated path, so it is scoped and
  explicit: the CP appends only keys it was asked to authorize for a logged-in operator,
  and only on the host it manages. (This *extends* 0.4.7's "login materializes a local
  agent-ops identity, does not fabricate a `[runner]` block" — the box still fabricates no
  runner block; the door is a separate host-level credential the CP grants, not a runner
  the box authors.)
- **`teardown-cp`:** the box's door tears down the CP itself (the CP cannot remove itself).
  **LXC ownership:** `world_teardown` destroys the relay + k3s LXCs (the world the CP
  built) through the co-located runner's host `pct` — the same runner `world_build` booted
  them with, so it is the verified path — and unwinds runners/secrets/registry/DNS.
  **Ordering is fixed (CP-first):** `world_teardown` runs BEFORE `teardown-cp` destroys the
  CP LXC — otherwise the CP is gone before it unwinds its own state.

**The absorbed channel-owner credential needs the same security treatment as the login
door.** When `api/` absorbs the console (G5), it inherits the console's channel-owner key
— the credential that mutates runner rosters, the highest-privilege secret the CP holds.
Its protection is specified, not implicit:

- **Protection:** the channel-owner private key is held in `control-plane/state/` (or its
  Postgres form) under the same 0600/0700 discipline as `state.json` today — never in
  config, argv, logs, or agent context; the API signs roster mutations only in-process and
  never exposes the key over any surface (the console already does this with its signing
  identity — `web.rs:528` — the channel-owner key follows the same rule).
- **Provisioning:** it is minted at `bootstrap-cp` (the console's identity is generated on
  the box at deploy today — `deploy_cp.go` readback) and persists in the durable plane, so
  a rebuild reuses the same owner (roster continuity), exactly as the console identity does
  today.
- **Rotation:** a rotate-channel-owner path lands in Phase 3 (post-console) — re-key the
  owner, re-publish the runner rosters it signs, and revoke the old key, mirroring the
  secret-rotation discipline in `secret-management/`. The AGENTS.md gap that
  "rotate/re-grant don't reach a running runner" applies here until a restarted runner
  picks up the new owner.

This is the same privilege class as the login-authorized door, so it gets the same §9
security-spec gate: **the channel-owner key's protection/provisioning/rotation is specified
and reviewed before Phase 3 deletes the Rust console.**

This is why the CLI is **not** a pure API client: world ops go through the API, CP-lifecycle
goes through the box's door. Both live in `control-plane/cli/`.

---

## 8. Phased execution

**Phase 0 — Re-org + contract re-wiring (mostly mechanical, but with wired seams that must
not silently break).** Move dirs, rewrite import paths (`freehold/orchestrator/...` →
`freehold/contract/...`, `freehold/control-plane/...`, `freehold/platform/...`), **split into
the three modules decided in §4 (`freehold/contract` + `freehold/control-plane` +
`freehold/platform`)** with the cross-module embed moved to the `platform/agents` Go value +
the harness kept in `control-plane/core/harness/` (edges `control-plane → contract` and
`control-plane → platform`, never platform → control-plane), delete the `orchestrator` name
everywhere, update docs. The non-mechanical seams, called out because they carry runtime
contracts:

- **The toolset's `mcp` stdio bridge** (`cmd/freehold-agent-tools` `mcp` mode) — the CPA
  pod fetches the *static* binary at boot (`agent.go:144`, from `api/`'s
  `/freehold-agent-tools-binary` handler) and spawns it via `BUZZ_ACP_MCP_COMMAND`. It must
  stay a **static Alpine-compatible build** (`CGO_ENABLED=0`, rebuild.go:338) and its fetch
  URL moves with `api/`. Not a path move — a build + serve-wiring contract.
- **`//go:embed CPA_SYSTEM_PROMPT.md`** — re-wired when the prompt moves to
  `platform/agents/freehold/prompt.md`: the embedding package moves to a `platform/agents`
  Go package and `api/`/`cli` import it (already noted in §6).
- **The console-AGENT signing identity** (`web.rs:528` `sign_call`) — the Rust console
  signs every readiness probe against each runner with its own identity. Porting "the
  routes" is not enough: `api/` must reproduce this identity's signing at parity (the Go
  repro is harness-gated), or readiness breaks.
- **The gates stay live through the move:** `freehold-harness-oracle` (Go↔Rust byte-gate)
  and `freehold-acceptance` (Chunk 1 hermetically) must run green at each move boundary,
  not only at the end.

Safe on the current stable tree; every later change then lands in its true home.

**Phase 1 — Unified scoped API (the behavior change).**
Build `control-plane/api/` in Go as the **unified front**: it consolidates the agent-tools
toolset + the new `world_*`/`world_status` surface into one server with scope auth, and it
**fronts the still-Rust console** (`world_status` reads the console API underneath; the
console's `/api/*` provision/rotate/grant continue to be served by the Rust console this
phase). The Rust console is *not* folded here — it is **replaced outright in Phase 3**;
Phase 1 adds the Go `api/` front, Phase 3 ports the console's remaining routes into it and
deletes the crate. `world_status` becomes the single inventory read; add `freehold world
<status|build|teardown|migrate>` CLI + TUI views from it. **The CPA/
world tool-visibility split survives the fold:** the one server exposes all tools to
roster-gated *operator* callers, but the CPA's stdio bridge keeps its existing
create/grant/manage-only filter (`mcp.go:53` `isFreeholdTool` — world_* never reaches the
harness today); the filter becomes a per-channel tool-visibility list, not a per-server
one, so the locked boundary ("world actions deliberately do NOT reach the CPA") holds.
**Agent registry reconcile (from §5):** one-time import of console `state.json` agents
into the authoritative `registry.json`, then the console reads the registry. **This
closes the "fresh box sees + drives the whole world" gap** (the original motivation for
this plan).

**Phase 2 — CLI as CP interface + the door model.**
`freehold build` slims to `bootstrap-cp` + trigger `world_build`; `teardown-cp` on the box's
door; login authorizes a fresh box's door (per the §9 security spec); delete the box-local
`state.json` mirror; **world ops** no longer need a local runner/door (they go through the
API) — the CLI keeps its door *only* for CP-lifecycle (`bootstrap-cp`/`teardown-cp`), not
for world ops.

**Phase 3 — Platform execution + port completion.**
The CP runs platform terraform/migrations for services/agents/DNS/cert; port the Rust
console (`web.rs` ~2.1k lines) into `api/` and delete the Rust `control-plane` crate **and
its now-dead `console-client/` (Rust)** — the Go `cli/console` client + `api/` replace both,
so "Rust only where it earns its keep" holds; `freehold-orchestrator` binary folds into
`freehold` **preserving sibling resolution** (`resolveRebuildBins`, rebuild.go:309 — the
engines find binaries relative to `os.Executable()`, so `freehold` must resolve its
runner/control-plane/toolset siblings from wherever it is invoked) and the toolset's static
build. **Console security guards survive the port, not just the routes:** the
loopback-only-until-authn bind guard (`main.rs`), the `HttpOnly; SameSite=Strict` session
cookie + login rate-limit (`web.rs:341`), and the single-use portal token
(`web.rs:205`/`352` consume-once) are reproduced in `api/` at parity — porting routes
without them is a silent security regression. Live-verify the whole lifecycle:
teardown → bootstrap-cp → platform build.

**The agent-registry reconcile (§5) rides the verify-gated migration runner**
(`internal/migrations`, already live on the CP via `world_migrate`): the one-time import of
console `state.json` agents into the authoritative `registry.json` is registered as a
migration whose postcondition (the registry contains every console agent) must verify — not
a bespoke one-shot, so a failed reconcile is retried/idempotent and recorded in
`/srv/data/cp/migrations.json`.

**Every phase carries the AGENTS.md known gaps forward.** A re-org is the natural moment
to re-home them, not lose them: the litellm admin-master-key follow-up (scoped per-agent
keys), the SSH connector pool/timeout limits, the replay cache, the console's signing-key
re-derivation, teardown/bring-up rollback gaps. Each lands in its new home under
`control-plane/` or `platform/` with its follow-up status preserved.

---

## 9. Open items / non-goals

- **`core/` name:** kept as `control-plane/core/` (it IS the contract); `contract/` is an
  acceptable rename if clarity wins — decide during Phase 0.
- **Database seam:** `database/postgres/` is a plain dir today; the interface appears only
  when a second consumer/impl exists.
- **The live world (cp.librem…) must eventually be torn down + rebuilt through
  `bootstrap-cp`** to prove the new lifecycle; Phase 3 validates on a scratch world first
  if the production world must keep running.
- **The login-authorized door (§7) is a new elevated path and needs its own security
  spec before Phase 2 implements it:** which keys the CP will authorize, how it proves the
  box is a logged-in operator (not a rogue), whether the append is scoped/removable, and
  how a revoked operator's key is removed from the host door. It is called out in §7's
  mechanism but deliberately left as a design gate here.
- **The absorbed channel-owner credential (§7) is the same privilege class as the door and
  gets the same design gate:** its protection (0600 durable plane, in-process-only signing),
  provisioning (minted at `bootstrap-cp`, reused across rebuilds for roster continuity),
  and rotation (Phase 3 re-key + re-publish + revoke) are specified in §7 but must be
  reviewed as a security spec before Phase 3 deletes the Rust console.
- **Harness ↔ platform dependency (from §4's module topology):** the harness
  (`control-plane/core/harness/`) may import `freehold/contract/crypto` (the Go repro it
  gates) and `freehold/platform/agents` (prompt bytes) — edges `control-plane → contract`
  and `control-plane → platform`, never platform → control-plane. The shared contract
  (`contract/`) is what makes this acyclic: `platform` imports `contract`, never
  `control-plane`. Confirmed in §4.
- **Not in scope:** migrating the runner or `core` to Go (they stay Rust); the granular
  permission model (scopes beyond `admin`) beyond the interface seam; changing the
  agent→runner direct exec contract (it is preserved, not redesigned).