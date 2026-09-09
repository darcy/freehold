# Login-authorized door — security spec (REFACTOR-PLAN §7/§9 design gate)

The CP cannot bootstrap or tear down itself. The box's CLI holds its own door
(an SSH key authorized on the host) for CP-lifecycle jobs — `bootstrap-cp` and
`teardown-cp`. Today that door is authored once by the first `freehold build`
(the box that deploys the world keeps the key). REFACTOR-PLAN §7 proposes the
**login-authorized door**: a *fresh* box that logs into the CP gets its own
door, so any logged-in operator box can perform CP-lifecycle work, not just the
box that built the world.

This is a new elevated path — the CP appends keys to the host door it manages.
§9 deliberately left it as a design gate: "which keys the CP will authorize,
how it proves the box is a logged-in operator (not a rogue), whether the append
is scoped/removable, and how a revoked operator's key is removed from the host
door." This document is that spec.

## Threat model

- The door grants **root-equivalent exec on the host** (`ssh root@host` through
  the PVE/SSH door the co-located runner uses). A compromised door key is a
  compromised box.
- The CP already holds the highest-privilege host credential: its co-located
  runner's injected SSH key (the same door `world_build` drives). The
  login-authorized door must NOT grant anything beyond what the operator who
  built the world already could do — it extends *who* holds a door, not *what*
  a door can do.
- A rogue actor with a valid operator nsec (NIP-98 login) is already in the
  console admin whitelist — login itself is the gate. The door mechanism must
  not widen "who is an operator."

## Design

### 1. What the CP authorizes

The CP appends exactly ONE key per logged-in operator: the box's own
`~/.freehold/control-plane/agent-ops` SSH key (the identity `login`
materializes — first-run-wins, 0600). It is **the box's own keypair**, minted
locally by `login`; the CP never sees the private half, only the public line.

### 2. How the CP proves the box is a logged-in operator

The append happens ONLY through the CP's co-located runner, scoped to the
already-authorized flow:

1. The box logs into the CP via NIP-98 (the existing `login`), obtaining a
   console session whose pubkey is in the admin whitelist.
2. The box calls the API's `world` toolset with the *public* line of its
   agent-ops key (`world_authorize_door`, operator-scoped like `world_build`).
3. The CP verifies the caller is an operator (NIP-98 session → pubkey in the
   admin whitelist; the same scope class as `world_build` — a registry agent
   cannot call it).
4. The CP runs the append **through its co-located runner** (the same runner
   that already holds the host door and drives `world_build`) as an
   idempotent, single-purpose command: `grep -qxF '<pubkey>' <authorized_keys>
   || echo '<pubkey>' >> <authorized_keys>`.

Only the pubkey the caller presented is appended; the CP appends no other key.

### 3. Scoping

- The append targets ONLY the host the CP manages (the runner's `target`), and
  only the operator's own presented pubkey.
- The door is **authorization-only, not a new privilege**: the box's agent-ops
  key is the SAME key that signs its world API calls. A box that can log in
  can already drive world_*; the door just adds the CP-lifecycle verbs
  (`bootstrap-cp`/`teardown-cp`) a fresh box couldn't run before.

### 4. Removability / revocation

- The box's own door key lives at `~/.freehold/control-plane/agent-ops`
  (box-local, 0600). `freehold logout` clears the login ledger but KEEPS the
  box identity (per 0.4.7) — a logged-out box's door key remains authorized
  until removed.
- **Revocation = removing the pubkey from the host door**, driven by the CP
  through its co-located runner: `world_revoke_door <pubkey>` (operator-scoped)
  runs `sed -i '\|<pubkey>|d' <authorized_keys>`. The operator uses it when a
  box is lost/compromised — the same lever `teardown-cp` uses to unwrap the
  world.
- The append is idempotent (grep-before-append), so re-login/re-append is
  safe; the revoke is exact-line removal.

### 5. Protection of the mechanism

- The door pubkeys are **not secrets** (public keys), so the CP can hold them
  in its durable state (`/srv/data/cp/...`) for auditability. The private
  halves never leave the box.
- The CP's own host door (the co-located runner's injected key) is unchanged —
  this mechanism rides it, it does not replace it. The AGENTS.md discipline
  (runner holds ciphertext + injected key, 0600) is untouched.
- Every authorize/revoke is an audited world op (the same signed-channel audit
  the runner emits).

## Open questions for review

- **Scope of the door**: is `bootstrap-cp` (bring up the CP LXC from the
  recorded coords) in-scope for a fresh box, or only `teardown-cp`? The spec
  allows both (the door reaches the host either way); a conservative first cut
  could restrict the fresh box to `teardown-cp` + read ops.
- **Many boxes, one host**: the door is host-scoped, not world-scoped — every
  logged-in operator box holds a key into the same host. That matches the
  pre-existing "the CP's co-located runner holds one door" posture (one door,
  many operators via the API), just materialized per-box.