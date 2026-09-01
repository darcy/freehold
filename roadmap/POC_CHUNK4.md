# Chunk 4 — Detailed Build Plan (v1)

Status: current work.

## What Chunk 4 actually builds (the CPA)

Scope: a real reasoning agent arrives for the first time — this is new relative
to Chunks 1–3, which shipped without one by design. Still no 
skill execution, no workspace/git, no retry/escalation budgets — those are
Chunk 5/6 concerns.

### Locked decisions

*   **CPA runs on the same buzz-acp/goose-class harness as expert agents** —
    its own identity, a dedicated create/grant/manage-agent toolset instead of
    a service-specific one, and its purpose defined by `CPA_SYSTEM_PROMPT.md`
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

- [ ] A1. Add an interactive step to `freehold bootstrap`: ask the operator
      what to call their agent, with a default offered (e.g. `freehold`) 
      and store the value.
- [ ] A2. Stand up the CPA's identity on a buzz-acp/goose-class harness — the
      same class of harness expert agents use — rather than a scripted NIP-42
      client.
- [ ] A3. Use stored name as the CPA's Buzz handle / profile
      display name — not a fixed brand name baked into the product.
- [ ] A4. Give it a dedicated toolset (create-agent, grant, manage) in place
      of a service-specific one. No skill-execution tools yet.
- [ ] A5. Wire it into the existing agent registry (already live: named agents
      register and report ●/○ availability from relay presence) so CPA shows
      up the same way any agent does.

#### Phase B — CPA system prompt

- [ ] B1. Write `CPA_SYSTEM_PROMPT.md` at the repo root (sibling to
      `AGENTS.md`): purpose, tone, and explicit tool/scope boundaries.
- [ ] B2. Bootstrap loads this file into the harness config at first spawn.
- [ ] B3. Every restart re-reads the current file from disk (never cached) —
      editing the prompt and redeploying is the only way CPA's purpose
      changes.

#### Phase D — Live durability proof

- [ ] D1. Open a real Buzz room/DM with the named CPA and carry an actual
      conversation (not the scripted demo class Chunks 1–2 used).
- [ ] D2. Restart the CPA's harness process mid-relationship; confirm the
      conversation/memory continues unbroken — proving the existing
      relay-persisted-memory mechanism (kind 30174) through the live harness,
      not the CLI.
- [ ] D3. Full compute-only teardown + rebuild of the CPA's LXC (Phase 0.12's
      reattach-by-reference); confirm identity, memory, and agent-registry
      roster all survive.

#### Phase E — Agent-creates-agent

- [ ] E1. Give CPA a create-agent tool: given a name and a one-line purpose,
      it stands up a new buzz-acp-class identity with its own durable,
      relay-scoped memory.
- [ ] E2. The created agent ships with no skill and no target — purely
      conversational, holding its own memory, same as CPA at this stage.
- [ ] E3. Verify the created agent is directly reachable in Buzz (not only
      reachable through CPA) and survives the same restart/rebuild drill as
      D2/D3.

#### Phase F — Resource baseline

- [ ] F1. With CPA and at least one created agent running concurrently,
      capture idle and active CPU/RAM footprint per agent process.
- [ ] F2. Record the numbers as a decision input for Chunk 5's sleep/wake
      work — this chunk measures, it does not build a watcher/reaper.

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

### Chunk 4 acceptance (from `roadmap/POC.md`)

*   A human opens a room/DM with the named CPA in Buzz and gets real, reasoned
    responses.
*   Memory survives both a process restart and a full LXC teardown/rebuild,
    demonstrated live in Buzz.
*   CPA, asked in Buzz, creates a second agent (name + purpose only) that gets
    its own durable identity and is directly talkable — also surviving a
    rebuild.
*   CPA's purpose is defined by `CPA_SYSTEM_PROMPT.md`; changing the file and
    redeploying changes CPA's behavior.
*   Baseline resource numbers recorded for one CPA + one created agent, idle
    and active.
