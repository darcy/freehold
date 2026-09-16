# Plan: `bootstrap`/`build` split — CP-owned build on a real Terraform substrate

> Forward plan (tracked; not history). `CHANGELOG.md` holds the shipped changes;
> this describes the target and the committed decisions for the next phase.

## Goal (one sentence)

Turn the monolithic `freehold build` into a clean two-command boundary —
`freehold bootstrap` (box one creates only the CP) and `freehold build` (any
logged-in box asks the CP to bring up the whole world) — and drive every infra
primitive (the LXC substrate + the litellm/postgres kube workloads) through **real
Terraform** (A1) instead of ad-hoc `pct`/`kubectl` shells.

## Decisions (locked by the operator)

1. **Explicit `bootstrap` / `build` boundary; nothing bleeds.**
   - `freehold bootstrap` == "create the CP" (box one, the only path that touches
     the bare host). It does **not** boot the relay, deploy agent-tools, bring up
     k3s/litellm/caddy/cert, or trigger `world_build`.
   - `freehold build` == "the CP brings up the world," gated on a live console
     session. Identical from box one or box two after login.
2. **Bootstrap doesn't trigger world_build** (for now). They are separate; the
   boundary is explicit. (In principle the CP could also self-finish, but we keep
   the seam visible.)
3. **Teardown stays one combined command** (box-side unwind + CP `world_teardown`
   + Terraform `destroy` of the substrate), not split.
4. **Clean cut** — no `--legacy`/`--full` back-compat flag. This is pre-1.0.
5. **Relay + agent-tools live in `build`, not bootstrap.** This is only possible
   because the bring-up executor moves **off agent-tools** (which can't authorize
   without the relay) **onto the console** (`freehold-console`), which does not
   depend on the relay roster.
6. **Terraform from the start (A1):** the substrate is authored as real resources
   with state, and the kube workloads (litellm/postgres) are moved into Terraform.
   App overlay (Buzz compose, Caddy ConfigMap + lego certs, split-horizon
   dnsmasq, DNS creds, agent-tools deploy) stays scripted but **ordered** by the
   CP driver.

## Target architecture

```
freehold bootstrap  (box one, root-gated; the ONLY host-touching command)
  1. provision door + doorGate; grant ops identity; serve local runner
  2. capture world inputs IN MEMORY (domains, proxy IP, operator pubkey,
     DNS provider creds, litellm provider key, sizing)
  3. durable volume plane ensure (the substrate store)
  4. boot + record the CP LXC (through Terraform)
  5. deploy-cp: freehold-console + the co-located runner, AND hand the console
     the runner coords + DNS/litellm creds it needs to drive the world later
  6. record coords + this box's local ledger/runner/pubkeys
  → "CP created. run `freehold build` (from any box after `freehold login`)"

freehold build  (any box, login-gated)
  0. no console session? → "run `freehold bootstrap` first."
  1. ask the CONSOLE to run its world_build (operator-scoped; drives the
     co-located runner; no relay required to authorize)
  2. console world_build stages:
       a. Terraform substrate: cp/relay/k3s LXCs + durable-plane + mounts
       b. boot relay + deploy the Buzz stack
       c. deploy + seed agent-tools (roster now valid — relay is up)
       d. boot + install k3s + durable local-path
       e. DNS resolver register + point guests
       f. litellm/postgres kube workloads (Terraform) + model registration
       g. Caddy TLS edge
       h. cert: durable-reuse gate → in-process DNS-01 → file-transit install
       i. report world status back to the box
  3. box-side bookkeeping: record coords, register world facts, CPA/agent
     reconcile (through the now-up agent-tools/console)

freehold teardown  (combined — unchanged shape, + Terraform destroy)
```

## Part 1 — the `bootstrap`/`build` split (CP-owned build)

### 1a. `freehold bootstrap`
- New command (box one). Runs the current half-1 stages **minus** relay boot,
  agent-tools deploy, handoff-trigger, and `world_build`:
  door → config → DNS creds → durable plane → cp LXC → deploy-cp (console +
  co-located runner, now carrying the runner coords + creds) → record coords.
- Requires the host door + DNS creds (box-one inputs); errors clearly otherwise.
- No relay, no agent-tools, no world bring-up. Durable plane is created here (the
  CP LXC needs it to boot with its `--mpN` mounts).

### 1b. `freehold build`
- Gated on a live console session. If none → "run `freehold bootstrap` first."
- Triggers the **console's** world_build (Part 2), then the box-side bookkeeping
  (record coords / facts / CPA reconcile).
- Remove the half-1 stages from `build` so a thin box never tries to create a CP
  locally.

### 1c. CLI dispatch + docs
- `bootstrap` and `build` are distinct subcommands with distinct input/help; the
  `main.go` dispatch and README/ARCHITECTURE reflect the two-command model.
- Box two story documented: `freehold login` → `freehold build`.

## Part 2 — the console is the CP build executor

Why: relay + agent-tools must move into `build`, but `world_build` currently
lives in agent-tools, which can't authorize without the relay (circular). Move the
bring-up stages into `freehold-console`, which does **not** depend on the relay
roster, and which already owns the CP.

- **`deploy-cp` hands the console the runner coords** (runner pk/addr/target) and
  the creds it needs (DNS provider creds from the bootstrap handoff; litellm
  secrets) — the console quotes them to the runner by name (sealed, in-memory).
- Move the world bring-up stages out of agent-tools' `deploySpec` into a **CP-side
  builder** the console drives:
  - share the stage commands (`internal/stages`) as-is;
  - the console exposes an operator-scoped `world_build` (MCP or console API),
    authorized by the console's operator roster, driving the co-located runner.
- agent-tools becomes a **build output**: the console's `world_build` boots the
  relay, then deploys + seeds agent-tools (roster now readable). Afterward
  agent-tools remains the surfacer for exec / world verbs / create-grant-manage.
- Secret discipline (locked) is preserved: the console holds ciphertext + its own
  key, decrypts in memory, never writes plaintext; secrets are quoted to the
  runner **by name**.

## Part 3 — Terraform (A1) for the substrate + kube workloads

- **Provider (finalize during build):** resolve the bpg/proxmox 0.66.0 rewrite —
  pin a pre-rewrite bpg release, or adopt `telmate/proxmox`, or the bpg
  clone-based flow. Pick the least-risk working one; the executor drives it identically
  either way. Remove the vestigial "provider retained for follow-up" state.
- **Resources (real, with state) under `/srv/data/freehold-tf` (0600):**
  - **Durable volume plane**: the ZFS datasets / LVM volumes per tenant (relay /
    cp / k3s) that back the LXC `--mpN` mounts (`backup=` flags preserved).
  - **LXCs**: cp, relay, k3s — hostname (deterministic `DomainLXCName`), storage,
    rootfs, memory, bridge, network (k3s static IP/gw /24 from `Proxy.Ip`), and
    the durable mounts as `mount_point`.
  - **litellm/postgres/caddy kube workloads**: are now real **`kubernetes`
    provider** resources in `postgres.tf` / `litellm.tf` / `caddy.tf` — the
    deterministic static service definitions (Deployment/Service/PVC/ConfigMap/
    Secret). Secret VALUES ride the 0600 state as `TF_VAR_*` (option A); the
    k8s Secrets are first-run-wins (`lifecycle { ignore_changes = [data] }`).
- **Execution discipline (locked, carried from today):** Terraform runs ON the
  provisioning box via the co-located runner's exec (`tf.sh`); creds arrive as
  runner-injected env → `TF_VAR_*`, never tfvars/literals; state + the staged
  k3s kubeconfig are sensitive → 0600 under `/srv/data/freehold-tf`.
- **Two-phase apply (ordering):** phase 1 applies the SUBSTRATE via `-target`
  (plane/LXCs/k3s bring-up — the exec-first `null_resource` shell), bringing k3s
  up; phase 2 stages the kubeconfig (rewritten to the k3s node IP) and applies
  the `kubernetes`-provider SERVICE resources. The scripted overlay (agent-tools,
  DNS, CPA litellm key, cert) runs after in order; Caddy's cert/DNS issuance stays
  the CP's event-driven overlay.
- **Fix the stale `.243`** in the terraform k3s script + align static IPs with
  `Proxy.Ip`.

## Phasing

- **Phase A — Part 1 + 2 (DONE, v0.5.14→v0.5.18):** the `bootstrap`/`build`
  split + console-as-executor (relay + agent-tools move into `build`). Shipped
  the clean boundary + any-box `build`. Verified live on the PVE host.
- **Phase B — Part 3 (DONE, v0.5.19):** Terraform A1 substrate + litellm/postgres,
  driven by the console executor. Elected exec-first (null_resource + the proven
  pct/kubectl scripts) over the bpg/proxmox provider as the least-risk working
  one per Part 3 (PVE 9.2.2; bpg 0.66+ dropped the tarball-create path the live
  substrate was born from). The module is embedded in `cpbuild`, ships to the
  box at `/srv/data/freehold-tf` (0700), ADOPTS the plane/LXCs the CP creates
  (`terraform plan` clean), and `terraform destroy` tears the kube layer +
  substrate down in teardown. Litellm/postgres/caddy are declared as real
  `kubernetes`-provider resources in `postgres.tf`/`litellm.tf`/`caddy.tf`;
  `worldLiteLLM` retained only the CPA litellm-key seed.

## Landing (Phase B shipped shape)

The module lives in `control-plane/api/cpbuild/terraform/` (embedded, so it ships
with the console); the drifted `platform/terraform` leaf is gone. Per-command
surface:

```
freehold build      → BuildWorldApply: Go substrate staircase (plane + LXCs), then
                      terraform PHASE 1 (substrate -target: plane/lxc_*/k3s_bringup),
                      refresh vmids/IPs, then terraform PHASE 2 (stage kubeconfig +
                      apply the kubernetes-provider postgres/litellm/caddy .tf with
                      the rendered Caddyfile), then the overlay (agent-tools, DNS,
                      CPA litellm key, cert).
freehold teardown   → teardown.Run: terraform destroy (kube first, by depends_on)
                      BEFORE the k3s LXC pct-destroy; the Go stops/destroys then
                      no-op (idempotent). The durable plane survives by design.
```

Named follow-ups (kept current; see `followups.md` + AGENTS.md "Known gaps"):
the LXC create path still lives in Go + exec script (terraform adopts via
`null_resource`); vmid allocation for a genuinely fresh box is therefore a
follow-up; converting the adopt-managed substrate to a real bpg provider
(clone-based flow) remains the provider-integration follow-up; the relay Buzz
stack (inside the relay LXC) is not yet Terraform.

## Acceptance

- `freehold bootstrap` on a bare box: creates a CP (console + co-located runner)
  and stops; **no** relay/agent-tools/k3s/litellm/caddy. `world status` unreachable.
- `freehold build` (no session) errors → "run bootstrap first."
- After `freehold login` (on box one **or** a fresh box two), `freehold build`
  brings up the full world through the CP: relay/agent-tools/k3s/DNS/litellm/
  caddy/cert/CPA, byte-identical results regardless of which box drove it.
- The substrate + litellm/postgres are Terraform-managed: `terraform plan` run
  through the runner reports clean; `terraform destroy -target` tears the substrate
  down; teardown combines box-side + CP + Terraform destroy.
- Re-review of relay/agent-tools living in `build` (not bootstrap) is clean.
- Full `go build`/`vet`/`test` green; a real teardown + bootstrap + build on the
  PVE host re-converges the world.

## Risks / notes

- **Executor move is the hard part** (the circular dependency + moving stages to
  the console). Everything else hangs off it.
- Provider integration (bpg rewrite) must be resolved once, early.
- Terraform state on the provisioning box must stay 0600/sensitive (locked
  discipline) — the substrate holds no operator secrets beyond what the runner
  injects.
- Box two's `build` writes world coords/facts into **that box's** local config —
  intended (same world coords everywhere).
