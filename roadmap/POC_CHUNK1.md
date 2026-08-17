# Chunk 1 — Detailed Build Plan

Status: locked decisions, ready to execute.  
Scope: POC, pre-Buzz, NO k8s, NO real reasoning agent.

## Locked decisions

*   **Stack:** Rust for the runner core + secret provisioner (the durable engine). Scripted  
    orchestrator for the demo (no real reasoning agent in Chunk 1).
    
*   **Transport:** MCP over HTTP for agent↔runner even though co-located — proves the real shape.
    
*   **Agent:** scripted orchestrator (CPA stand-in) driving the onboarding flow. No real agent.
    
*   **Test targets:** old-laptop Proxmox (SSH target) + real Vultr + real B2.
    
*   **Identity stand-in:** standalone keypairs + local registry (no relay yet). Grants whitelist  
    pubkeys, enforced locally. Ports onto relay membership in Chunk 2.
    
*   **Secret model:** CP = provisioner (encrypt-to-runner-key + ship + inject private key + rotate
    *   membership). NO master key. Runner holds only ciphertext + injected key; decrypts locally,  
        uses in memory, forgets.
        
*   **Runner primitive:** generic `exec(cmd, target, stream?)`. Agent writes commands; runner owns  
    connection, executes verbatim, streams output, signs audit record. NO semantic tools.
    

## Goal (one sentence)

Prove the engine room standalone: a local master agent (orchestrator) can onboard an existing  
service → provision a runner (secret encrypted to its key) → validate (🟢) → grant to an agent  
(stub) → readiness view shows green. Secrets never in agent context.

## Demo that defines done (algolia-style, no LXC creation)

User gives CPA "existing service at IP X + API key Y" → CPA creates runner → encrypts secret to  
runner's key, ships ciphertext, injects runner private key → runner self-checks → 🟢 → CPA grants  
runner to an agent stub → readiness view green. Plus: SSH exec, Vultr create/destroy, B2  
read/write round-trip — all via runner, secrets never in agent context.

* * *

## Ordered steps

### Phase A — Runner core (the engine primitive)

- [x] [x]

A1. Scaffold Rust workspace: `runner` crate + `control-plane` crate (workspace root).

- [x] [x]

A2. Runner identity: generate Nostr keypair + separate encryption keypair. Keypair
generation + storage (private key injected via env var / mounted secret, never committed).

- [x] [x]

A3. Runner MCP tool server skeleton over HTTP: `list`, `exec`, `config`, `status`,
`snapshot` endpoints. JSON-RPC framing.

- [x] [x] 

A4. **Generic** `exec(cmd, target, stream?)` — the heart:  
- [x] takes raw command verbatim (no semantic interpretation)  
- [x] runs on target via the owned connection  
- [x] returns output; streaming mode for long-running/live commands (pull-style chunk  
buffer in the runner; agent polls chunks)

- [x] [x] 

A5. **Self-check → readiness** primitive: runner attempts to reach its service with its  
creds, reports green/yellow/red.

- [x] [x] 

A6. **Audit:** runner signs a Nostr event per executed command (agent pubkey, target,  
command, result). Chunk 1: local audit log (no relay yet).

### Phase B — Secret provisioner (CP-side)

- [x] [x]

B1. Provisioner: given a target + credential, generate runner identity + encryption
keypair, encrypt the credential **to the runner's pubkey**, ship ciphertext to runner  
config, inject runner private key.

- [x] [x]

B2. **Rotation:** re-encrypt a secret to a fresh key / re-issue to remaining runners.

- [x] [x]

B3. **Revocation (cut-off):** revoke a runner's identity at the CP → runner can no longer
be called/act.

- [x] [x]

B4. Verify: NO master key stored anywhere; CP holds only ciphertext. Plaintext never on
disk, never in agent context.

### Phase C — Three connectors (each a runner flavor, same core)

- [x] [x] 

C1. **SSH** to local machine (old-laptop Proxmox): persistent ssh connection pool  
(ControlMaster/ControlPersist) for cheap repeated commands.

- [ ] [ ] 

C2. **Vultr** connector: create/destroy/status a server.

- [ ] [ ] 

C3. **Backblaze B2** connector: read/write round-trip (S3-compatible).

### Phase D — Grants (coarse)

- [x] [x] 

D1. Grant model: agent ↔ runner (whitelist Nostr pubkeys of who may call a runner).  
Enforced locally (identity stand-in; port to relay membership in Chunk 2).

- [x] [x] 

D2. Dedicated runner per service = default. Sharing via grants allowed (coarse; no  
target-scoped permissions yet).

### Phase E — Scripted orchestrator (CPA stand-in)

- [x] [x] 

E1. Orchestrator drives the onboarding flow: given "existing service at IP X + key Y" →  
create runner → provision secret → self-check → 🟢 → grant to agent stub → report readiness.

- [x] [x] 

E2. Scripted demo steps: SSH exec, Vultr create/destroy, B2 round-trip, via the  
orchestrator calling the runner (proves plumbing, not reasoning).

### Phase F — Local web UI

- [x] [x] 

F1. Local web UI (`localhost`): services-at-a-glance + readiness (green/yellow/red).

- [x] [x] 

F2. Manage runners / secrets / grants from the UI. Admin/ops view, NOT chat.

### Phase G — Acceptance script

- [x] [x] 

G1. The algolia-style happy path (existing service → runner → 🟢 → grant → green view).

- [x] [x] 

G2. SSH exec, Vultr create/destroy, B2 round-trip — all via runner.

- [x] [x] 

G3. Verify: secrets never in agent context; runner holds only ciphertext + injected key;  
no master key; revoking membership cuts off; rotation re-encrypts.

### Phase H — Test / promote

- [x] [x] 

H1. Run against old-laptop Proxmox (SSH target) — safe target.

- [ ] [ ] 

H2. Promote to home dogfood once green on the laptop.

## Chunk 1 acceptance (from POC Steps doc)

*   Control plane web UI lists the 3 connectors with readiness states.
    
*   Agent can exec on the SSH machine, create/destroy a Vultr server, and do a B2 read/write  
    round-trip — via the runner, with secrets resolved by the runner from ciphertext provisioned  
    by the CP.
    
*   Secrets never appear in agent context; runner holds only ciphertext + its own injected key,  
    no master key anywhere; revoking a runner's membership at the CP cuts it off; rotating a  
    secret re-encrypts it.
