# Chunk 4 — Detailed Build Plan (v1)

Status: current work.

## What Chunk 4 actually builds (the CPA)

Scope: a real reasoning agent arrives for the first time — this is new relative
to Chunks 1–3, which shipped without one by design. Still no skill
execution, no workspace/git — those are Chunk 5's concerns — and no
budgets, no postcondition-gated readiness, no Terraform deploys — those
wait for Chunk 6.

### Locked decisions

*   **CPA runs on the same buzz-acp/goose-class harness as expert agents** —
    its own identity, a dedicated create/grant/manage-agent toolset instead of
    a service-specific one, and its purpose defined by `prompts/CPA_SYSTEM_PROMPT.md`
    rather than improvised per spawn.
*   **The deterministic runner/CP layer is unchanged.** Provisioning, grants,
    secret handling, and teardown/rebuild stay exactly as they are — CPA's
    reasoning decides *what* to do, that layer still does it auditably. This
    chunk does not touch that boundary.
*   **Durability rides infrastructure that already exists.** Relay-persisted
    encrypted memory (kind 30174) and per-tenant compute-only
    teardown/reattach (Phase 0.12) are both already implemented — this chunk
    proves them live through a real conversational agent, it doesn't build
    them.
*   **The created agent (Phase E) gets no skill or target in this chunk.** It
    only needs to hold a conversation and remember it — proving the
    relationship works, not the capability.

### Goal (one sentence)

A person opens Buzz, talks to a real reasoning CPA that knows its own purpose
and remembers the conversation tomorrow even after a full rebuild — and can ask
it to create a second agent, with the same guarantees, that they can talk to
directly.

### Demo that defines done

An operator finishes bootstrap, having named their CPA at install time. They
open a room/DM with it in Buzz and have a real conversation — not a scripted
response. They restart the CPA's process; the conversation picks up where it
left off. They tear down and rebuild the CPA's LXC entirely; same result.
They ask the CPA, in Buzz, to create a new agent for some stated purpose; it
does, and that new agent is directly reachable in Buzz, remembers its own
conversation, and survives the same restart/rebuild drill. Alongside all this,
the team has real numbers on what one of these agent processes actually costs
to run, idle and active.

### Ordered steps

#### Phase A — Bootstrap naming step & CPA on a real-agent harness

- [x] A1. Add an interactive step to `freehold bootstrap`: ask the operator
      what to call their agent, with a default offered (e.g. `freehold`) 
      and store the value.
- [x] A2. Stand up the CPA's identity on a buzz-acp/goose-class harness — the
      same class of harness expert agents use — rather than a scripted NIP-42
      client.
- [x] A3. Use stored name as the CPA's Buzz handle / profile
      display name — not a fixed brand name baked into the product.
- [x] A4. Give it a dedicated toolset (create-agent, grant, manage) in place
      of a service-specific one: `freehold-agent-tools`, a real MCP server on
      the CP exposing `create_agent` / `grant_agent` / `manage_agent`, handlers
      calling `internal/agent/tools.go` in-process, authorized per call against
      the server's own relay roster (NIP-29 channel + 39002 membership,
      fail-closed), seeded at bootstrap and dogfooded by the build. No skill-execution tools yet.
- [x] A5. Wire it into the existing agent registry (already live: named agents
      register and report ●/○ availability from relay presence) so CPA shows
      up the same way any agent does.

> **D1 model wiring:** the CPA's reasoning rides the litellm gateway as an
> OpenAI-compatible endpoint — the pod sets `BUZZ_AGENT_PROVIDER=openai-compat`,
> `OPENAI_COMPAT_BASE_URL=http://litellm.litellm:4000/v1`,
> `OPENAI_COMPAT_MODEL=ControlPlaneAgent` (alias → deepseek), with the API key
> from the `<pod>-litellm-key` Secret (`secretKeyRef`). D1 is the first live
> proof the harness honours these env vars end to end.

#### Phase B — CPA system prompt

- [x] B1. Write `orchestrator/prompts/CPA_SYSTEM_PROMPT.md` (the single
      prompts directory, inside the `freehold/orchestrator` Go module so the
      `//go:embed` can reach it): purpose, tone, and explicit tool/scope
      boundaries.
- [x] B2. Bootstrap loads this file into the harness config at first spawn
      (the orchestrator embeds it — `//go:embed CPA_SYSTEM_PROMPT.md` in the
      `orchestrator/prompts` package — and the CPA/agent pod mounts it as a
      `<pod>-prompt` ConfigMap at `/srv/freehold/CPA_SYSTEM_PROMPT.md`).
- [x] B3. Every restart re-reads the current file from disk (never cached) —
      editing the prompt and redeploying is the only way CPA's purpose
      changes.

> **Phase B deferral (named, not lost):** the pod reads its prompt from a
> `<pod>-prompt` ConfigMap (mounted read-only at
> `/srv/freehold/CPA_SYSTEM_PROMPT.md`), seeded at apply time from the
> orchestrator's embedded `orchestrator/prompts/CPA_SYSTEM_PROMPT.md`. A host restart of that
> pod re-reads the mounted copy, so editing the prompt and redeploying changes
> CPA's behavior (B3 as planned). A compute-only teardown/rebuild (Phase
> 0.12) re-seeds the pod from the *embedded* bytes, so a prompt edit made
> only in the CP's `/srv/data/cp` copy doesn't survive a rebuild until
> re-deployed — wiring the pod to the CP's durable mount is a named
> follow-up.

#### Phase D — Live durability proof

- [x] D1. Open a real Buzz room/DM with the named CPA and carry an actual
      conversation (not the scripted demo class Chunks 1–2 used).
- [x] D2. Restart the CPA's harness process mid-relationship; confirm the
      conversation/memory continues unbroken — proving the existing
      relay-persisted-memory mechanism (kind 30174) through the live harness,
      not the CLI. (Verified 2026-09-05: the pod is stateless-local ($HOME empty,
      no engram/db files), memory persists as kind-30174 engrams on the relay,
      whose Postgres lives on the durable /var/lib/docker LV; a process restart
      left the engram + the 10-message #freehold thread intact on the relay.)
- [x] D3. Full compute-only teardown + rebuild of the CPA's LXC (Phase 0.12's
      reattach-by-reference); confirm identity, memory, and agent-registry
      roster all survive. (Verified 2026-09-05: full world teardown + rebuild;
      the CPA pubkey stayed 6626e5af…, the kind-30174 memory engram and the
      10-message #freehold thread both persisted on the rebuilt relay's durable
      Postgres.)
- [x] D4. **Rebuild reconciles the full desired world (cleanup).** Replace the
      opt-*in* `--with-k3s`/`--with-litellm` flags with opt-*out*
      `--no-k3s`/`--no-litellm`: a default `rebuild` brings up relay/cp/k3s/
      litellm/CPA idempotently (stages skip what's already present; teardown
      what you want replaced and rebuild to resurrect only the missing piece).
      The CPA rides litellm (opt out of either and the CPA is skipped), the
      litellm gateway reuses its already-sealed fireworks key on rebuilt (only
      a truly cold world prompts/hard-errors for a fresh one), and the TUI `B`
      form gained a litellm step (blank = on). D1–D3 drills (above) are the
      live proof this reconcile behaves end to end.

#### Phase E — Agent-creates-agent

- [x] E1. Give CPA a create-agent tool: given a name and a one-line purpose,
      it stands up a new buzz-acp-class identity with its own durable,
      relay-scoped memory — the CP's `freehold-agent-tools` `create_agent`
      (mints a durable identity under the CP's durable plane, adds the pubkey
      as a relay member, seats it in #freehold + publishes its profile, applies
      its pod through the CP's co-located runner, and registers it). The build
      dogfoods this exact call to bring the CPA up; the CPA pod's harness
      attaches the toolset via the stdio `mcp` bridge (fetched at boot), and
      this is verified live — a CPA asked in Buzz created an agent end-to-end.
- [x] E2. The created agent ships with no skill and no target — purely
      conversational, holding its own memory, same as CPA at this stage.
- [x] E3. Verify the created agent is directly reachable in Buzz (not only
      reachable through CPA) and survives the same restart/rebuild drill as
      D2/D3. The CP registry is the durable source of truth: a rebuild
      reconciles it, re-creating agents idempotently with the same durable
      pubkeys.

#### Phase F — Resource baseline

- [ ] F1. With CPA and at least one created agent running concurrently,
      capture idle and active CPU/RAM footprint per agent process.
- [ ] F2. Record the numbers as a decision input for Chunk 6's sleep/wake
      work — this chunk measures, it does not build a watcher/reaper.

### Phase G — operator-box ⇄ CP decoupling (Chunk 4 wrap)

Goal: the operator box stops being the single root of trust, so a fresh box can
`login` and rejoin a world whose CP survives. Shipped (0.4.7, PRs #158–#162):

- [x] G1. **`freehold login`, root-free** (#158): CP address + CP pubkey +
      operator nsec → NIP-98 authorize → end; seeds a local connection/desire
      profile from the CP's `/api/world`, so a fresh box recovers with nothing
      from a lost one. The operator-supplied CP pubkey is cross-checked against
      the CP's report (`resolveCPPubkey` — mismatch aborts, blank falls back),
      so a wrong/hijacked CP address never seeds a bogus trust anchor.
- [x] G2. **The CP serves `/api/world` from real state** (#159): a session-gated
      console endpoint returns relay + CP coords + who the operator is — the
      recovery source of truth, not a mock. Now served WITH the agent-tools
      coords (`agent_tools_url`/`agent_tools_pubkey`) a fresh box needs for the
      Agents view, and the deployed CP reliably records its relay scope: the
      build resolves the relay signing pubkey deterministically from the
      relay's own compose `.env` (no NIP-11 best-effort), and `serve` accepts
      `--relay-url` alone (the pubkey pairing is a soft guard, not a start
      blocker).
- [x] G3. **`freehold logout`** (#158) clears the local operator ledger only
      (CP/world untouched, idempotent). All login prompts share ONE buffered
      stdin reader (AGENTS.md discipline).
- [x] G4. **Login materializes the box's provisioning identity** (#160):
      `control-plane/agent-ops` (first-run-wins, the identity `build`/`teardown`
      sign with); login does NOT fabricate a `[runner]` block.
- [x] G5. **`freehold teardown` is CP-first** (#161): a whole-world teardown asks
      the CP (`POST /api/teardown`) to remove what it manages (runners+secrets,
      agents, DNS) before the box destroys the CP LXC — best-effort when the CP
      is down.
- [x] G6. **A CP world-action surface** (#162): roster-gated `world_status` /
      `world_teardown` on the agent-tools toolset so the box can "login +
      trigger" the stateful half of the CP. The CPA harness deliberately does
      NOT get `world_teardown` (locked conversation+create-only).

Deferred (need a live PVE/CP/relay world to verify — not this chunk's clean
handover): durable world-state under `/srv/data/cp` (world_build currently
learns coords by hostname + the box re-records post-trigger; a durable
world-state makes the CP self-sufficient for a truly cold world), and the
final docs close-out. **Resolved live (PRs #172–#181):** slim `freehold build`
to CP-bring-up with in-memory secret capture + hand-off + trigger, the CP
`world_build` infra provisioning (relay/k3s boot + install, storage, DNS,
litellm, Caddy + cert issue/install — the deferred "CP build/upgrade" items),
and the cert install as a CP step (durable-reuse gate → in-process DNS-01 →
file-transit install).

### Open decisions to make explicitly at kickoff (not pre-decided by this plan)

*   **How literally CPA reuses the expert harness.** Two shapes were on the
    table: (a) CPA runs on the exact same harness as any expert, just with a
    different prompt/toolset — simplest, and the default assumption in Phase A
    above; or (b) CPA stays a thin deterministic front end that delegates
    reasoning to a harness-backed layer it manages separately — more moving
    parts, but keeps agent-creation itself fully deterministic and auditable
    end-to-end. Phase A assumes (a); revisit explicitly if reasoning during
    implementation surfaces a reason to prefer (b).
*   **How far to take the created agent in E2.** "Purely conversational" is
    the floor this plan asks for. Resist giving it real tools/capabilities
    before Chunk 5 even if it seems easy in the moment — the point of this
    chunk is proving the identity/memory/rebuild pattern generalizes, not
    shipping a second capability surface early.
*   **Budgets and the postcondition-gated readiness view** (the
    budget-exceeded stop/report/offer path and the `verify:`-checked
    🟢/🟡/🔴 gate) land with Chunk 6's provisioning work — no agent
    provisions anything in this chunk.

### Chunk 4 acceptance (from `roadmap/POC.md`)

*   A human opens a room/DM with the named CPA in Buzz and gets real, reasoned
    responses.
*   Memory survives both a process restart and a full LXC teardown/rebuild,
    demonstrated live in Buzz.
*   CPA, asked in Buzz, creates a second agent (name + purpose only) that gets
    its own durable identity and is directly talkable — also surviving a
    rebuild.
*   CPA's purpose is defined by `prompts/CPA_SYSTEM_PROMPT.md`; changing the file and
    redeploying changes CPA's behavior.
*   Baseline resource numbers recorded for one CPA + one created agent, idle
    and active.
