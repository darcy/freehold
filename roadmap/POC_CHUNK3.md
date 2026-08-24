# Chunk 3 — Detailed Build Plan (draft v5)

Status: DRAFT — for review (v5, folds the implementor review of v4); NOT yet locked.
Consolidates v3 (with its Phase-0 resolutions) + v5's changes: two structural supersessions
(onboarding inversion, k8s pull-forward) plus the sequencing/mechanics corrections from the
review. Everything below marked v3-unchanged stands as locked-in-v3; supersessions are
recorded explicitly, not silently.

## Locked decisions — SUPERSEDED or NEW this revision

* **v3's "one dedicated agent-harness LXC" placement is RETIRED in the target
  architecture.** Agents are pods, one identity per pod, on the k8s substrate pulled forward
  into Chunk 3 (ROADMAP.md:63 "**NO Kubernetes** in POC." is SUPERSEDED for Chunk 3 forward —
  that line is amended at lock; it remains true for Chunks 1–2). Deterministic pods,
  `envFrom` Secret, config pulled fresh per pod-start (ARCHITECTURE's already-specified
  model). **Landing-strip exception (execution, not architecture):** the FIRST expert
  (`@pihole` / `@tailscale`) runs on the already-staged LXC harness (goose 1.47.0 +
  buzz-acp + a minted LiteLLM key — all live on-box as of 2026-08-24) while C0 stands up the
  substrate in parallel. Pod migration is a separate, C5-gated cutover — NOT a precondition
  for G1/G2.
* **Onboarding order INVERTED (supersedes ARCHITECTURE's + POC.md's "CPA provisions target →
  creates runner → creates agent" order),** for Chunk 3 forward: CPA creates the expert
  (identity + prompt: "you are the expert for `<service>`, install it") → the expert reasons
  about its own target (the skill's `target: lxc | pod | either` is a HINT it may accept or
  override, not a directive CPA acted on) → the expert asks **CPA** (never a peer directly)
  for what it needs → CPA resolves which named peer fulfills it and whether an
  approval/cost gate applies → delegates → the peer provisions + grants access → CPA/peer
  tells the expert via Buzz → expert installs. The expert never learns which peer it landed
  on — it receives only a slot descriptor (Phase 0.10). Default channel membership stays
  operator + CPA (the expert only ever talks to CPA — no widening needed).
* **"The provisioner" is not a generic role.** Every hardware-hosting box gets its own
  NAMED peer identity (`@vultr`, `@hetzner`, `@macmini`, `@<operator-named>`) — the direct
  relay duplicate of POC_CHUNK2's local provisioning expert, second application of the locked
  duplicate-not-promote rule (new identity, re-encrypted credentials, never promoted in
  place). A named peer's job = capacity fulfillment ("give me an LXC"/"give me a kube slot")
  via the runner bootstrap already granted it. **Box lifecycle (create, reachability,
  teardown, rebuild) stays with the deterministic bootstrap tool, always, with no exception,
  forever** — a box-hosted agent cannot report or recover from its own box's unreachability.
  Peers are exempt from disposability/rebuild treatment but still get a C5-style
  postcondition check on their service.
* **LiteLLM target flips from LXC to kube** (deterministic, C0). The existing live LXC
  deployment (LXC 105, model registered, key minting proven) is the REFERENCE implementation
  C0 re-targets, kept serving until the kube deployment passes its own C5 postcondition, then
  torn down after its soak window — per the blue/green narrowing rule, never before.
* **Terraform is the expert's write-down artifact, not an infra gate (C6, softened).** The
  goal: the expert captures what it learned so it doesn't re-derive it — location
  `/srv/data/agents/<expert>/` unchanged, "skills may ship a starter" unchanged. For Chunk
  3's actual LXC/kube service targets the artifact is declarative capture in whatever form
  fits (script, manifest, .tf with remote-exec if the expert finds it natural).
* **Terraform becomes the BOOTSTRAP substrate driver — SUPERSEDES C6's "no Terraform
  consumer in Chunk 3" line (the substrate IS the consumer).** Each `bootstrap --kind`
  (proxmox-lxc, vultr-vps, hetzner-vps, k3s, litellm-kube) = one Terraform plan;
  `freehold bootstrap` stays the operator surface and executes the plans THROUGH the
  provisioning runner's exec (the runner's injected key remains the only door). Disciplines,
  all locked: (1) runner-exec only — never a workstation-side terraform with its own
  credentials; (2) operator-supplied secrets enter ONLY as runner-injected env
  (`TF_VAR_*`), never tfvars/provider-block literals; TF-GENERATED secrets (random_password,
  kube tokens, provider-returned keys) are acknowledged to live in state, so state is
  sensitive-by-default — encrypted backend or `/srv/data` at 0600; (3) host-sysadmin steps
  (kernel modules, sysctls, LXC features, cluster DNS pins) ride remote-exec inside the
  plans (C6's in-scope remote-exec decision); (4) the no-master-key acceptance gains a
  guard: after every apply, TF state contains none of the operator-supplied variable
  values; (5) service installs stay agent-improvised + write-down (unchanged).
* **Blue/green narrows unilateral destroy.** Once a service has a live, currently-serving
  instance, the expert's normal mode is never destroy-in-place: new version alongside →
  verify (C5) → flip a traffic pointer the expert owns → soak → remove old. Destroy-in-place
  stays available (D2: offer, don't perform) only for never-successfully-brought-up services.
  LXC = in-place rolling (second instance, same `/srv/data` mount, own reverse-proxy/upstream
  flip — proportionate for lightweight services; heavier services = a named scale boundary).
  k8s = single-cluster, two Deployments + Service-selector flip — a single-node k3s hosting
  both colors during a flip adds NO node. VM-targeted deploys are explicitly deferred (the
  provisioner simply doesn't offer VM; an expert asking for one gets "not available, pick
  LXC or kube").
* **Postgres comes into scope now** (deterministic pod config depends on prompt-as-DB-row;
  LiteLLM kube needs it): ONE instance, multiple logical databases (control_plane, litellm,
  per-client) per ROADMAP's "single cluster, many DBs" model. Runs as a kube Deployment with
  its durable volume PINNED to `/srv/data/k8s-volumes` (never the default local-path root —
  ARCHITECTURE's "durable PVCs never on the daemon root" rule, first application).
  Migrations via the CP-owned tooling pattern — no new admin surface.
* **Kube-slot mechanics:** on a single-node k3s, "give me a kube slot" = a dedicated
  namespace + ResourceQuota, created by the named peer via its own runner grant — the
  pod-world equivalent of "an LXC": bounded, revocable, handed to the expert.
* **Cost-gate criteria (Phase 0.11):** an operator approval gate applies iff the request is
  cost-bearing (paid cloud spend) or host-irreversible (box teardown). Local LXC creation /
  kube namespace+quota on already-owned hardware flows WITHOUT approval.
* **Slot descriptor (Phase 0.10):** what an expert actually receives, independent of which
  peer fulfilled it — architecture (amd64/arm), rough resource bounds (cores/RAM), compute
  kind (LXC with ssh access details / kube namespace + kubeconfig context), NOT peer identity.
  Until C0's cutover completes, LXC is the only offered kind; "pod" answers "not yet
  available, pick LXC" (same shape as the VM deferral).
* **G6's convergence is concrete, re-tasked onto the existing (shelved) chunk2-closeout
  live-verify harness** — extended, not reinvented: relay + CP + the named hardware peer +
  its grants + its memory + one running service (pihole) all present and correct after
  re-bootstrap. Un-shelving chunk2-closeout is the implementation vehicle for G6.
* **G7 explicitly un-shelves the workspace/git runner + a minimal image-build/push path**
  (the forcing function from the blue/green discussion): registry default = a local
  `registry:2` on the k3s node LXC; retention = bounded (last 3 images/state versions, named
  requirement, mechanics at G7 execution). No general CI/CD.
* **Wall-clock budget:** once agents are pods, natively enforceable as a pod
  deadline/TTL; for the LXC landing-strip experts the spawner-side timeout applies. A7's
  setsid/killpg fix stays REQUIRED for LXC-targeted installs (pod teardown contains pod
  targets — A7's container analog is free).

## Locked decisions carried over unchanged from v3

* Outbound default-deny via capability runners; safe/risky classification with the
  risky-install/risky-host sub-classes and risky-host CPA-only grants.
* Policy-level enforcement honesty (no network-level egress gate implied): harness-native
  web/browse/search AND shell/exec/file-write tools disabled at spawn (A5/A5b); MCP runner
  tools are the only sanctioned outbound; codex EXCLUDED (no tool-deny mechanism — SURFACE §4).
* Inbound default-deny via per-service private channels (9007/9000/9001, live-proven);
  channel membership IS the gate (403 at ingest); `--respond-to allowlist` is spawn-time
  state (STARTUP-ONLY — SURFACE §2) → CPA derives it from the roster at every (re)spawn,
  restarts the agent unit on membership change, and reconciles at CPA start (idempotent fold).
* Prompt-injection bounded by membership (operator + CPA only; buzz-acp DM hardening).
* Skills stay agent-improvised for the software layer (explicit supersession of POC.md's
  "skills instead of free-form shell"); readiness is postcondition-gated (C5), never
  agent-reported.
* A6 per-service install runners (generalized to "the expert's granted compute, LXC or
  pod"); A7 process-group-kill for runner timeouts (LXC case); risk_level metadata
  end-to-end (implemented, merged #72).
* Harness verdict: goose + claude-code comply; codex excluded (SURFACE §4).
* First skills: tailscale, pihole — deliberately NOT LiteLLM (C0 proves LiteLLM's
  deterministic path separately; it must not be the first case of an expert's free
  target-reasoning).

## Goal (one sentence)

Prove that real reasoning agents, under capability-brokering and inbound-gating, install and
operate services end to end — the first expert via the LXC landing strip (immediate, human-
facing), the architecture via deterministic pods — with every side effect brokered through an
audited, classed runner or contained to disposable compute.

---

## Phase 0 — Capability & risk research

01–06 RESOLVED (2026-08-23, deliverable `roadmap/POC_CHUNK3_SURFACE.md`) — capability runner
list + classes, --respond-to STARTUP-ONLY restart-sync, LiteLLM key path, harness matrix,
audit 48001+spool, prompt-injection record. New this revision:

* [ ] 07. **Base image variants.** Single generic agent base image (buzz-acp + harness)
      carries every needed harness: buzz-acp BUILDS (workstation, 52s, release binary
      verified 2026-08-24), goose 1.47.0 runs (harness LXC verified). Confirm no
      codex-only need exists (codex excluded) → lock the single image.
* [ ] 08. **k3s-on-librem re-verification.** Repeat the Chunk-2.5 spike shape on the CURRENT
      test world (librem): unprivileged LXC, `INSTALL_K3S_EXEC="server --kubelet-arg
      feature-gates=KubeletInUserNamespace=true"`, nginx pod through NodePort from the PVE
      host. The 2.5 spike proved it on the cloud hosts (10.10.0.7) — librem has NOT run it.
      PREREQUISITE for C0.
* [ ] 09. **Postgres placement — LOCKED** (new locked decision above): single instance,
      multiple logical DBs, kube Deployment, durable volume pinned to `/srv/data/k8s-volumes`
      (never the default local-path root), CP-owned migrations.
* [ ] 10. **Slot descriptor surface — LOCKED** (new locked decision above): architecture,
      resource bounds, compute kind + kind-specific access, NOT peer identity; LXC-only until
      C0 cutover.
* [ ] 11. **Cost-gate criteria — LOCKED** (new locked decision above): approval iff
      cost-bearing or host-irreversible; local flows un-gated.

## Phase A — Capability runners (v3-unchanged)

A1–A7 as v3 (github/websearch/litellm flavors IMPLEMENTED + merged #72/#73 with risk_level
end-to-end). A6's "service LXC" language generalizes to "the expert's granted compute, LXC or
pod" — no runner-design change (targets are kind strings; pods change nothing in the
dispatch).

## Phase B — Inbound gating (v3-unchanged)

B1–B4 as v3 (RESOLVED for SURFACE §2). Default membership stays operator + CPA — confirmed
NOT widened by the onboarding inversion.

## Phase C — Skill framework v1

* [ ] C0. **k3s → LiteLLM, deterministic, operator/CPA-driven.** Sequenced AFTER Phase 0.08's
      re-verification; NOT blocking Phase E's first expert (landing strip). Proves the
      substrate carries a real workload + stands up Postgres for real (ahead of pod-config
      need). The live LXC LiteLLM stays serving until the kube deploy passes C5, torn down
      after its soak window — never before.
* [ ] C1. Skill schema: `target: lxc | pod | either` documented as a default HINT the expert
      may override (the inversion, recorded in schema docs).
* [ ] C2. Disposability per compute type: LXC installs `/srv/nobackup` + `/srv/data/<service>`
      (unchanged); pod installs use the deterministic-pod model (recreate pod, config
      re-pulled from Postgres, no local state by design).
* [ ] C3. First skills: tailscale, pihole (unchanged).
* [ ] C4. Readiness 🟢/🟡/🔴, postcondition-gated (unchanged).
* [ ] C5. Independent postcondition, unchanged from v3 (implemented as `verify:` — per-skill
      external check run by CPA via a runner after the agent reports done).
* [ ] C6. Declarative-capture artifact, softened per the new locked decision (service layer:
      script/manifest capture; Terraform-proper reserved for cloud-resource graphs).
* [ ] C7. **Terraform bootstrap substrate** (per the new locked decision): `terraform/` plans
      per kind (proxmox-lxc, k3s, litellm-kube, later vultr-vps/hetzner-vps), executed via
      the runner's exec with the four disciplines; G6's teardown/rebuild runs on
      `terraform destroy` / `terraform apply` + the verify harness.

## Phase D — Retry/escalation budget

D1–D3 unchanged from v3, plus the pod-deadline enforcement note (new locked decision) and:

* [ ] D4. Blue/green narrows destroy-in-place: once a service has a live instance,
      budget-exceeded escalation applies to GETTING THE NEW VERSION WORKING; the old,
      currently-serving instance is torn down only after a successful flip + soak window.

## Phase E — Named hardware peers + expert onboarding (rewritten)

* [ ] E1. Bootstrap deploys the first named hardware peer (`@<operator-named>` — the PVE
      host on the test world) — the relay duplicate of the Chunk-2 local provisioning expert
      (duplicate-not-promote, second application). Runs on the LXC harness for now (landing
      strip); migrates to a pod after C0's cutover. Its skill = capacity fulfillment for its
      own box; it does NOT own that box's lifecycle.
* [ ] E2. CPA creates the new expert identity with only a service-name + install prompt — no
      target pre-assigned.
* [ ] E3. The expert reasons about its own target (overriding the skill hint if warranted),
      asks CPA; CPA resolves the named peer (+ approval gate per Phase 0.11) and delegates.
* [ ] E4. The named peer provisions (LXC or kube namespace+ResourceQuota) via its runner
      grant, hands access back through CPA to the expert (slot descriptor, Phase 0.10).
* [ ] E5. Expert installs via its skill (Phase C), verified via C5, capturing what it learns
      per C6.
* [ ] E6. Secondary-relay onboarding remains explicitly out of this chunk (unchanged).

## Phase F — LiteLLM

F1 SUPERSEDED by C0 (kube-deployed, deterministic) — this phase confirms F2/F3 against the
kube deployment, nothing fresh. F2 operator-config unchanged. F3 per-agent keys minted by
the CP via the litellm runner (proven live) and surfaced via pod `envFrom` Secret (replaces
the retired colocated-runner-on-shared-LXC mechanism).

## Phase G — Acceptance / real dogfood UAT

* [ ] G1. The @tailscale/@pihole experts install + configure via skills end-to-end, with
      capability runners + channel gating enforced — run against the LXC-harness-hosted
      first expert, NOT gated behind C0.
* [ ] G2. Unscripted UAT: an operator asks `@pihole` in Buzz and gets a correct result — PVE
      test world first; home dogfood recorded, not an acceptance blocker.
* [ ] G3. Non-granted capability refused + non-member channel write gets the 403.
* [ ] G4. Budget-exceeded escalation: agent stops, reports, offers (never performs) a
      rebuild for non-ephemeral state.
* [ ] G5. Confidently-wrong acceptance: agent reports success on a broken install →
      readiness stays 🔴 (C5 postcondition fails).
* [ ] G6. **Full-cycle repeatability** via the EXTENDED chunk2-closeout harness: bootstrap
      (relay + CP + named peer live) → operator manually requests an LXC from the peer and
      sets up pihole by hand (fulfillment path isolated from expert target-reasoning) → full
      teardown → re-bootstrap → CONVERGENCE = relay + CP + peer + grants + memory + pihole
      all present and correct.
* [ ] G7. **Blue/green proof:** new LiteLLM version via the kube two-Deployment selector flip
      with zero dropped requests across the flip, old Deployment retained through soak.
      UN-SHELVES the workspace/git runner + minimal image-build/push (registry:2 local
      default, bounded retention).

## Open items carried forward from Chunk 1–2.6.1

Unchanged from v3 (B2 live-account leg, Vultr relay-scoped runner identity, relay-terminated
TLS, consolidated live-acceptance harness [now the G6 vehicle], CP-restart memory
persistence, audit-as-channel-messages, secondary-relay onboarding).

## Explicitly not in Chunk 3

Unchanged from v3, plus: multi-cluster k8s (single cluster, occasionally two nodes hosting
both colors during a flip, is the ceiling); VM-targeted provisioning or blue/green (deferred);
image-build/registry automation beyond the workspace/git-runner minimal path (no CI/CD
system).
