# Chunk 3 — Detailed Build Plan (draft)

Status: DRAFT — for review (2026-08-20, revised v3 folding both review passes); NOT yet
locked or approved; NOT committed. Once reviewed: reconcile with `roadmap/POC.md`'s Chunk 3
section (which stays the short goal/acceptance summary, mirroring POC_CHUNK1/CHUNK2).

Scope: POC, first real reasoning agents. Adds the skill framework, real relay-addressable
service agents, LiteLLM, and — new relative to the original plan — the capability-brokering
and inbound-gating model that makes it safe to let those agents reason freely instead of
running scripts. This revision folds the 2026-08-20 review: enforcement honesty, placement,
and five recorded decisions that would otherwise read as silent drift from locked docs.

## Locked decisions (from the Chunk 3 security posture discussion + review)

* **Outbound is default-deny, brokered by runners — extended beyond secrets to any
  capability.** GitHub access, websearch, anything that leaves the pod/LXC — each is its own
  runner, granted like any other. Absent a grant, the agent has no sanctioned path out.
  This is the same primitive Chunks 1–2 already built (runner = privileged bridge + audit),
  applied to capabilities generally.
* **Enforcement is POLICY-level in the POC, and the model is stated honestly:**
  "no path out" means *no tool path*. Harness-native web/browse/search AND shell/exec/file-write tools are
  DISABLED at
  spawn — MCP runner tools are the only sanctioned way to affect anything outside the
  process (A5b carries the verification) — so the runner grant is the only sanctioned
  outbound. An agent
  granted only capability runners (github, websearch) has no shell anywhere — that is the
  safe-class posture. An agent granted an exec/install runner has a shell bound to that
  target's disposable environment; the shell can reach the network from there, which is the RISKY class — accepted, audited per call, blast-bounded by the disposable env — EXCEPT the risky-host sub-class below (the PVE-host runner), where no disposable backstop exists and the blast is undiluted. Network-level
  egress restriction (per-LXC firewall on vmbr; the pve-firewall nft-persistence trap from the
  Chunk 2.5 spike) is named FUTURE hardening, not a POC requirement. The grant model is real;
  the network gate is not implied.
* **Runners are classed safe vs. risky, and the class is a first-class, visible property —
  not tribal knowledge.** API-scoped runners (github, websearch, vultr, b2, hetzner) are
  "safe"; SSH/generic-exec runners are "risky". Risky runners are flagged in the console and
  in the runner's own metadata. **Not solved, explicitly deferred:** scoping SSH down to a
  less-privileged account per use case. Tracked, not blocking Chunk 3.
* **Sub-classification (named, not tribal): risky-install vs risky-host.** _risky-install_ =
  an exec runner scoped to a disposable service LXC — the blast IS the disposable env
  (destroy-and-rebuild is the backstop). _risky-host_ = an exec runner reaching the PVE
  host/hypervisor or any non-rebuildable target — the Chunk-1 SSH runner (proxmox-box → the
  PVE host) is the live example, and it is exactly how service LXCs get provisioned in the
  first place: there is NO rebuild-the-box backstop for its blast radius. risky-host runners
  are granted CPA/operator-only by default; experts never get one unless explicitly named.
  D3's caveat applies to it literally, not abstractly.
* **Inbound is default-deny, mirroring the outbound grant model.** Every service agent gets
  a private channel. Default membership = operator + CPA only. **The channel membership IS
  the gate** — buzz's ingest runs `check_channel_membership` on channel-scoped events
  (non-member writes refused; non-member reads refused with 403, both live-proven on the
  2.6.1 worlds). `buzz-acp --respond-to` is the second, client-side gate: set to `allowlist`
  and SYNCED by the CPA on every membership change (single source of truth = membership, so
  the two never drift). Failure mode, documented: a member not in the allowlist sees the
  channel but gets no replies; a non-member gets 403 at write, before the agent ever sees
  anything.
* **Prompt-injection exposure is bounded by membership, not left open.** Private channels
  default to operator + CPA membership, so the injection surface IS operator prompts
  (trusted). Multi-member channels later re-open the question; content-level defense
  (distinguishing operator instructions from other members' messages) stays future work,
  revisited after Chunk 3's UAT — not an open design question for this chunk.
* **Skills stay agent-improvised — SUPERSEDING POC.md's "agent executes skills instead of
  free-form shell" (explicit locked-change record).** Skills declare inputs/needs/environment
  (target: lxc | pod | either); there is no deterministic check→apply step list. The value
  of an LLM here is debugging the weird stuff (networks, half-broken state, unexpected
  package managers); reducing it to a fixed script throws that away. Blast radius is bounded
  by grants + the disposable environment, not by constraining reasoning. The POC.md line is
  superseded BY THIS DECISION, not silently abandoned.
* **Agent placement (POC): one dedicated agent-harness LXC on the PVE host.** All expert
  agents run via buzz-acp + harness (goose/claude code/codex) inside it — per ROADMAP's
  "POC = Buzz agents via buzz-acp (local)" — one buzz-acp process per expert identity, each
  with its own BUZZ_PRIVATE_KEY. The harness LXC holds a COLOCATED runner per the
  ARCHITECTURE team-runner precedent (local daemon, in-memory ciphertext, never on disk) that
  holds the service credentials (ssh keys, API keys) and surfaces them via grantable exec /
  credential-helper protocols. The harness LXC's own network egress: none required — every
  sanctioned outbound rides a runner. Agents are ephemeral in the same sense as pods:
  destroy the harness LXC → CPA respawns processes + re-pulls AGENTS.md; durable agent state
  lives under `/srv/data/agents` (the storage-tier rule: harness working state is NOT
  relay-backed — ARCHITECTURE). **Shared-blast named limitation:** one harness LXC hosts
  EVERY expert identity (all BUZZ_PRIVATE_KEYs) plus the colocated runner's in-memory
  decrypted service credentials — a compromise of the LXC (not of any single agent's
  reasoning) exposes all expert identities (impersonation until relay-revoke) and all
  resident credentials at once: a real concentration against the project's "dedicated
  runner per service" isolation principle. Accepted for POC (no k8s yet); k8s per-agent
  pods + per-pod envFrom Secrets are the eventual fix; response posture = relay membership
  revoke per identity + rotate/re-seal.
* **The environment bound for skill installs is disposability.** Skill installs run in a
  dedicated, ephemeral service LXC with no durable state of its own (`/srv/nobackup`),
  durable data mounted in from `/srv/data/<service>` (two mpN entries, `backup=1`/`backup=0`
  — the locked storage-split rule). If the agent makes a mess: destroy the service LXC,
  provision a fresh one, re-run the skill. Durable side effects on OTHER targets (risky SSH
  exec elsewhere, external admin consoles) are NOT undone by this mechanism — named
  limitation, per the locked model.
* **Retry/escalation budget (decided, superseding the open question):** wall-clock hard cap
  enforced OUTSIDE the agent (the spawner runs each buzz-acp process under a `timeout` /
  systemd-run deadline — trivially enforceable on the harness LXC, same shape as k8s
  deadlines later) + attempt guidance carried as strong instruction inside the prompt
  (attempt-count is not mechanically enforced in POC). Exceeding either = stop, report to
  `@freehold`/the operator what was tried (the audit trail is the record — presentation, not
  new logging), and OFFER — never unilaterally perform — a rebuild for anything touching
  durable state.
* **LiteLLM is in Chunk 3** (one chunk earlier than originally planned — was MVP/k8s):
  `buzz-acp`-spawned agents need a model credential now. **This is the locked pod model, not
  a new shape** — ARCHITECTURE already locks "per-attempt envFrom Secret" for deterministic
  pods (harness reads its own env credential). POC realization: the CPA spawner injects the
  per-agent LiteLLM key into the agent process env at spawn; the key is minted by the CP via
  LiteLLM's admin API and stored ciphertext-only in the CP store, surfaced at spawn through
  the harness-LXC colocated runner (in-memory, never on disk). LiteLLM's OWN provider keys
  live in LiteLLM's config — operator-managed one-time setup for POC (F2 decision: manual,
  not CP-driven; CP-driven-as-skill is the post-POC path).

## Goal (one sentence)

Prove that real reasoning agents, running under the capability-brokering and inbound-gating
model above, install and operate services end to end via skills — every side effect either
brokered through an audited, classed runner or contained to a disposable environment.

---

## Phase 0 — Capability & risk research (the security-posture gate)

Same spirit as Chunk 2's `BUZZ_SURFACE.md`: resolve the open design questions before writing
agent-facing schema against assumptions.
**Status: RESOLVED (2026-08-23) — deliverable at `roadmap/POC_CHUNK3_SURFACE.md`.**
Items 01–06 below are the decision record; each carries its resolution + note section.
Phases A–F consume the note; where a phase line still says "decide/confirm", the note wins.

* [ ] 01. Enumerate Chunk 3's actual capability needs (github, websearch, package-manager
      fetch, DNS lookups — whatever tailscale/pihole installs require) and classify each as a
      runner: safe (API-scoped) or risky (exec/SSH-shaped). Note: the `hetzner` api-target
      flavor already exists alongside ssh/vultr/b2 — the list starts there, not from zero.
      **(RESOLVED → SURFACE §1: github, websearch, install-ssh, PVE-host ssh, litellm —
      list + classes + credential/env conventions in the note.)**
* [x] 02. RESOLVED → SURFACE §1: `RunnerRecord.risk_level: Option<String>` in
      `control-plane/src/state.rs` (`"safe" | "risky-install" | "risky-host"`; None =
      unknown), set at provision time — kind-based default (api kinds = safe, ssh =
      risky-install) with an explicit `--risk` override at provision (the PVE-host runner is
      marked `risky-host` that way) — rendered in the console overview.
* [x] 03. RESOLVED → SURFACE §2: **STARTUP-ONLY.** `--respond-to` /
      `--respond-to-allowlist` (env `BUZZ_ACP_RESPOND_TO[_ALLOWLIST]`) are spawn-time
      CLI/env, validated once at startup (config.rs:460-481, lib.rs:1945); NO hot-reload, no
      signal, no file watch — allowlist changes REQUIRE a buzz-acp process restart. Owner is
      always implicitly allowlisted; the flag is required for `allowlist` mode. Mention =
      kind 9 with `#p` == agent pubkey; channel reads are `#h`-scoped (non-member 403
      transparent). BONUS: DMs are owner-only regardless of allowlist (buzz-acp's own DM
      hardening) — the allowlist governs channel mentions only.
* [x] 04. RESOLVED → SURFACE §3: master-key `POST /key/generate` mints per-agent keys
      (`agent_id`, `key_alias`, `max_budget`; `models` optional = all models; keys mintable
      BEFORE provider models exist). Auth = master key ONLY (no scoped admin keys).
      `DATABASE_URL` REQUIRED at startup; proxy :4000; lifecycle via `PATCH /key/update` +
      `POST /key/delete`. No shared-key fallback needed. F3 wires a `litellm` api-runner that
      holds the master key as ciphertext.
* [x] 05. RESOLVED → SURFACE §5: operational audit stays **kind-48001 + the local spool**;
      kind-9 redacted channel receipts (G-B) DEFERRED (duplicate path, no Chunk-3 consumer).
      A4 implements the existing 48001 + spool path — no new kind, no channel posting.
* [x] 06. RECORDED → SURFACE §6: private channels default to operator + CPA membership, so
      the injection surface IS operator prompts; buzz-acp's DM hardening (owner-only DMs)
      extends it. Multi-member channels later re-open content-level defense as future work.

**Deliverable (DONE):** `roadmap/POC_CHUNK3_SURFACE.md` — capability runner list +
safe/risky classification, the --respond-to resolution (startup-only → restart-sync), the
LiteLLM key path, the harness tool-disable matrix (goose ✓, claude-code ✓, codex ✗), and
the audit-surface decision.

## Phase A — Capability runners (outbound default-deny, generalized)

* [ ] A1. Generalize the runner abstraction (already generic `exec` + api-target flavor) to
      the Phase 0 capability list — github and websearch as the first two beyond the existing
      SSH/Vultr/B2/Hetzner set.
* [ ] A2. Safe/risky classification lands in runner metadata and renders on the console
      (per Phase 0.02).
* [ ] A3. Grants work identically regardless of class (same NIP-29 channel-membership model
      from 2.6.1) — no special-casing the mechanism, only the label.
* [ ] A4. Audit captures capability-runner calls per Phase 0.05's decision (48001 + spool
      today; kind-9 receipts only if 0.05 picked them).
* [ ] A5. Harness-native web tools disabled at spawn for every expert (base tools only) —
      the enforcement guarantee. (RESOLVED → SURFACE §4: goose ✓ — `extensions[].enabled:
      false` + `available_tools`; claude-code ✓ — `permissions.deny: ["Bash","WebSearch",
      "Write"]`; codex ✗ — NO tool-deny mechanism — see A5b.)
* [ ] A5b. **Harness-native shell/exec/file-write tools are disabled — the actual
      enforcement, not the web-tools clause.** goose/claude-code/codex ship a native
      shell/bash tool as a BASE tool (not a web tool), so A5's web-only wording would
      leave every expert with direct, ungoverned shell access ON the harness LXC — no
      MCP call, no signature, no audit event, no capability check — quietly invalidating
      the outbound default-deny thesis regardless of A6/A7/Phase A, because the agent
      would never need a runner for anything reachable from that shell (including the
      colocated runner's in-memory credential store and every other agent's env).
      Spawn config therefore disables shell/exec/file-write tools ALONGSIDE web tools;
      MCP runner tools remain the ONLY surface that affects anything outside the agent
      process. **Verification (the A5/A5b gate):** spawn an expert with its production
      config and ENUMERATE its tools — assert no tool can run a command or write a file
      on the harness LXC directly; only MCP calls to granted runners survive. Any native
      shell/exec/file-write tool present = A5/A5b FAILS.
      **Harness verdict (SURFACE §4): goose and claude-code comply; CODEX IS EXCLUDED** —
      it has no tool-deny mechanism (approval_policy/sandbox_mode gates only), so a codex
      expert would retain ungoverned shell access on the harness LXC and fail this gate by
      construction. Codex returns only inside an OS-level container sandbox (future work,
      not POC).
* [ ] A6. **Install runners — how an expert reaches the service LXC it installs into.** One
      SSH runner per disposable service LXC (`<service>-install`): the process is colocated
      on the harness LXC (in-memory ssh key — the ARCHITECTURE colocated-runner shape), the
      TARGET is the service LXC over ssh. Granted ONLY to that service's expert. A shared
      install runner reaching every disposable service LXC is REJECTED: one grant would
      reach every install env + key at once — a different, larger blast shape. The PVE-host
      runner stays risky-host, CPA-only (locked decisions). The team-runner precedent does
      NOT stand in for this — it was credential-helpers for git/registry; install is exec
      into another box.
* [ ] A7. **Runner timeout kills the process group (closes the Chunk-1 known gap).** The
      spawner's wall-clock deadline makes client-dies-mid-exec a FIRST-CLASS case. The
      runner's per-call `timeout` exists, but it kills the shell, NOT descendants (no
      setsid/killpg — a Chunk-1 known gap): an in-flight exec can orphan a command ON THE
      SERVICE LXC after the agent dies. Implement setsid/killpg so the deadline truly
      cleans up the target; add an explicit orphan-case test.

## Phase B — Inbound gating (private channels per service agent)

* [ ] B1. Onboarding a service agent creates its private channel (the 9007-create primitive
      from 2.6.1, already live-verified) with default membership = operator + CPA.
* [ ] B2. Adding a person/agent to that channel is the ONLY way to gain talk access — same
      shape as a runner grant (9000 put-user), same revoke shape (9001 remove-user), both
      live-proven on stock buzz in 2.6.1.
* [x] B3. RESOLVED → SURFACE §2 (startup-only): the allowlist is SPAWN-TIME STATE. The CPA
      derives `--respond-to-allowlist` from the channel roster at every (re)spawn and
      RESTARTS the agent's buzz-acp unit on membership change — relay-side 9000/9001
      membership lands instantly; the reply-gate lags one process lifetime (documented).
      CPA START reconciles unit params ← current roster idempotently (the
      `control-plane rebuild` shape), so a crash between the membership write and the sync
      settles on next start — no permanent drift, no manual fix.
* [x] B4. RESOLVED → SURFACE §2. Verify BOTH layers: a non-member's @mention write is
      REFUSED (403 at ingest — assert the 403, not just "no response"); a member added to
      the channel gets replies AFTER the CPA-applied buzz-acp restart (the restart-free
      property is asserted for the RELAY side — 9000 lands live — and for the channel READ
      path; the reply-gate legitimately lags one process lifetime, documented); a member
      absent from the allowlist sees the channel but gets no reply (documented failure
      mode). DM hardening (owner-only DMs) is buzz-acp's own — assert it as a bonus.

## Phase C — Skill framework v1 (agent-improvised, disposability-bounded)

* [ ] C1. Skill schema as sketched in ARCHITECTURE.md: declarative inputs/needs
      (`target: lxc | pod | either`, `needs: {database, volume, mount, workspace_runner}`),
      NO fixed check→apply step list — the agent reasons over intent + actual target state
      (the locked supersession of POC.md's "skills instead of free-form shell").
* [ ] C2. Every skill install runs in the service's dedicated, disposable LXC: `/srv/nobackup`
      for everything reproducible, `/srv/data/<service>` mounted for durable data (two mpN
      entries, backup flags per the locked storage-split). Destroy-and-rebuild = the recovery
      path; durable side effects elsewhere are a known, documented limit.
* [ ] C3. First skills: tailscale, pihole (POC.md:111).
* [ ] C4. Skill install status reported as readiness (🟢/🟡/🔴), reusing the existing
      runner-readiness console surface — no parallel status model.
* [ ] C5. **Independent postcondition per skill — the ground-truth check.** Readiness 🟢
      requires MORE than the agent's own report: each skill declares ONE minimal external
      postcondition (`verify: tailscale status` exit code; `curl -sf` the pihole admin
      port) executed by the CPA via a runner AFTER the agent reports done. No deterministic
      step list is reintroduced — one check, outside the agent's say-so — but the
      confidently-wrong path (agent *believes* success) fails it. C4's readiness is
      postcondition-gated, not agent-reported.

## Phase D — Retry/escalation budget

* [ ] D1. IMPLEMENT the locked decision: wall-clock hard cap via spawner-side process
      deadline (`timeout`/systemd-run) + attempt guidance as strong prompt instruction.
* [ ] D2. On budget exceeded: agent stops, reports to `@freehold`/operator what was tried
      (audit trail is the record — presentation), and OFFERS rather than performs a rebuild
      for anything touching durable state.
* [ ] D3. Recorded named limit: rebuild resets the disposable environment only; side effects
      on other runners/targets (risky SSH exec elsewhere) are NOT undone — and for
      risky-host runners (the PVE host) there is no rebuild at all: that blast is undiluted
      with no disposability backstop (locked decisions — sub-classification).

## Phase E — Spawn per-service expert agents (relay-scoped, real reasoning)

* [ ] E1. ONE dedicated agent-harness LXC on the PVE host (per the locked placement decision);
      `@tailscale`, `@pihole` spawned as real buzz-acp-harnessed agents on **goose or
      claude-code** (the Chunk-3+ path per BUZZ_SURFACE §6; CODEX EXCLUDED — no tool-deny
      mechanism, SURFACE §4), one process per identity; each harness runs with NATIVE WEB
      AND SHELL TOOLS DISABLED (A5/A5b).
* [ ] E2. Onboarding pattern from ARCHITECTURE executes for real: CPA provisions the service
      LXC → creates runner(s) (capability + credential, Phase A — including the `<service>-install` SSH runner reaching the service LXC, A6) → validates 🟢 → creates the
      private channel (Phase B) → grants → writes AGENTS.md (POC prompt source: file on the
      harness LXC, CPA-updated, re-pulled at restart — ARCHITECTURE's control_plane-Postgres
      row is the MVP form, divergence recorded) → spawns the agent → tells it to
      install/verify via Buzz.
* [ ] E3. Secrets/capabilities scoped to the management relay, per relay-as-scope — no new
      scope concept. (SECONDARY-relay onboarding — a user's existing relay as a service —
      remains explicitly out of this chunk; recorded, not omitted.)

## Phase F — LiteLLM

* [ ] F1. Deploy LiteLLM (LXC, no k8s in POC) as the model-routing layer for spawned agents.
* [ ] F2. Operator-configures LiteLLM (provider keys, routing) as a ONE-TIME manual setup for
      POC (locked; CP-driven-as-skill is post-POC). LiteLLM's own keys never enter the CP
      store.
* [ ] F3. Per-agent model credential: minted via a **`litellm` api-runner** (safe class)
      holding the master key as ciphertext (`litellm-admin-key` → `LITELLM_ADMIN_KEY`,
      address `http://<litellm-lxc>:4000`) — the spawn flow calls `POST /key/generate`
      (`agent_id=`, `key_alias=`, `max_budget=`) THROUGH the runner; the response key is
      stored ciphertext in the CP store and surfaced at spawn env by the harness-LXC
      colocated runner (never on disk, never in any agent's context — SURFACE §3).

## Phase G — Acceptance / real dogfood UAT

* [ ] G1. The @tailscale and @pihole experts (real reasoning, CPA orchestrating the
      onboarding) install + configure their services via skills, end to end, with capability
      runners + channel gating enforced.
* [ ] G2. **Unscripted UAT:** an operator, without a pre-written script, asks `@tailscale`
      in Buzz to do something and gets a correct result back. **Target: the PVE host test
      world first** (promote flow: VPS → PVE test → home); home dogfood is the recorded
      milestone, gated on the home box being reachable — NOT an acceptance blocker.
* [ ] G3. Verify a non-granted capability is refused (agent attempts a github action without
      that runner granted) and a non-member channel write gets the 403 (B4).
* [ ] G4. Verify a deliberately-broken install scenario: agent hits the wall-clock budget,
      escalates rather than loops, and a rebuild-offer (not an automatic rebuild) is what
      reaches the operator for anything non-ephemeral.
* [ ] G5. **Confidently-wrong acceptance:** the agent reports success on a deliberately
      broken install — readiness stays 🔴 because the C5 postcondition fails. The operator
      sees the failed independent check, not a green lie.

---

## Open items carried forward from Chunk 1–2.6.1 (not blocking Chunk 3, but not to lose track of)

* B2 (Backblaze) live-account leg — still hermetic-only.
* Vultr as a relay-scoped runner identity — proven as host driver only, never separately
  minted under a relay roster.
* Relay-terminated TLS (vs. proxy-terminated) — undocumented variant never built; every
  deployed topology terminates TLS at an operator proxy or the relay LXC's Caddy.
* Consolidated acceptance harness against a real relay — every delta proven ad hoc so far
  (a repeatable `scripts/live-verify.sh` run is a pre-Chunk-3 candidate when time allows).
* CP restart → memory persistence — round-trip proven, restart itself never exercised.
* Audit-as-channel-messages (G-B from 2.6.1) — resolved BY Phase 0.05 (default: 48001 +
  spool stays; kind-9 receipts deferred).
* SECONDARY-relay onboarding — explicitly deferred from this chunk (was named Chunk-3 work
  in POC_CHUNK2's carried-forward); lands post-POC with the ARCHITECTURE "existing relay =
  service" shape.

## Explicitly not in Chunk 3

* Fully solving unbounded SSH/exec risk (flagged, deferred — least-privileged SSH accounts
  is future work; the safe/risky LABEL is the POC mitigation).
* Network-level egress restriction (per-LXC firewall) — future hardening; the POC policy
  model + disabled native tools + disposable env is the enforcement (locked).
* Content-level prompt-injection defense — bounded by private-channel membership (locked);
  re-opened deliberately only when multi-member channels arrive.
* Kubernetes substrate (still MVP/Chunk 4), except LiteLLM which moves earlier per the
  locked decision above.
