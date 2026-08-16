# freehold

*Reclaim the future we were promised.* An open-source appliance — one-command install,
AI-agent-operated — that lands a Proxmox VE / VPS + Kubernetes stack with Buzz Relay as the
control plane and a skill framework that installs and configures self-hosted OSS. The agent
is the wall-breaker: you don't manage servers, you *ask*.

**Status: Chunk 1 (POC) in progress.** The engine room is being proven standalone before
Buzz (Chunk 2) and Kubernetes (MVP) come in. Phases A (workspace, identity, MCP skeleton)
and B (secret provisioner: seal/ship/rotate/revoke — no master key) are merged; phases
C–H (connectors, grants, orchestrator, web UI, acceptance) are next.

## Design in one paragraph

A **control plane** (a web app, admin/ops only — chat is Buzz's job) manages **runners**:
privileged MCP tool servers that own connections and credentials on the target side.
**Agents** are the brain, **runners** are dumb privileged hands — one generic primitive
`exec(cmd, target, stream?)`, no semantic tools. The control plane is a **secret
provisioner, not a vault**: it generates a runner identity, encrypts the credential *to the
runner's key*, ships ciphertext, and injects the runner's private key. **No master key** —
the CP holds only ciphertext + public keys, the runner decrypts locally, uses in memory,
forgets. Host-flexible: Proxmox lead, VPS/cloud first-class, nothing locked to a hypervisor.
See `VISION.md` (the "why"), `ARCHITECTURE.md` (locked decisions), `roadmap/` (chunked plan).

## Repository layout (what things do in the code)

```
Cargo.toml            workspace: core + runner + control-plane
AGENTS.md             agent guidance: locked model, conventions, known Chunk-1 gaps
roadmap/              ROADMAP.md, POC.md, POC_CHUNK1.md (phase checklists A–H)
core/                 freehold-core — shared by both crates, no product logic
  src/identity.rs     Nostr (secp256k1) + X25519 keypairs; env-inject or 0600 file
  src/crypto.rs       sealed box TO a runner's X25519 pubkey: ephemeral X25519 +
                      HKDF-SHA256 + ChaCha20-Poly1305; recipient AND secret-name bound;
                      low-order-point forgery rejected; versioned wire format
  src/secrets.rs      SecretPackage: the runner's on-disk secrets.json (name → ciphertext)
  src/futil.rs        atomic file discipline: unique 0600-at-birth temp + fsync + rename;
                      0700 state dirs — used everywhere secret material touches disk
runner/               freehold-runner — the privileged connector bridge
  src/mcp.rs          MCP-over-HTTP tool server (JSON-RPC 2.0): initialize / ping /
                      tools/list / tools/call. Contract tools: list, exec, config,
                      status, snapshot. exec/status/snapshot are pending (A4/A5/C).
  src/registry.rs     targets the runner can reach (empty until Phase C connectors)
  src/main.rs         CLI: `runner keys init`, `runner serve`
control-plane/        freehold-control-plane — the engine room
  src/state.rs        runners + secrets store (atomic 0600 JSON; pubkeys + ciphertext only)
  src/provisioner.rs  B1 provision (generate identity → seal → ship → record pubkeys),
                      B2 rotate-secret (re-seal a NEW credential), B3 revoke (cut-off +
                      removes shipped secrets.json), with save-failure rollback
  src/main.rs         CLI: provision / rotate-secret / revoke / list / serve
```

## Security model (no master key)

- The CP never holds a private key (runner identities are dropped and zeroized) and never
  holds plaintext (credentials are read from stdin, sealed, forgotten).
- The runner holds ciphertext + its own injected private key; only that key opens its
  blobs, and a blob only opens under the secret name it was sealed with.
- Rotation re-seals a NEW credential (the erase lever for your copies); revocation blocks
  provision/rotate and deletes the shipped credential. Honest limits are written down in
  `AGENTS.md` (no remote revocation of a capability someone else kept; re-keying and
  epoch/staleness land in A4 + Chunk 2).

## Getting started (current Chunk-1 state)

Prereqs: Rust 1.94+ (workspace declares `rust-version = "1.94"`).

```sh
cargo test --workspace        # 53 tests across core / runner / control-plane
cargo clippy --workspace --all-targets -- -D warnings   # must be clean
cargo fmt --all --check       # CI gate
```

### Runner: identity + MCP server

```sh
cargo run -p freehold-runner -- keys init          # writes ./.freehold/identity.json (0600)
cargo run -p freehold-runner -- serve              # MCP over HTTP on 127.0.0.1:8787
```

Manual handshake (session-id is per connection):

```sh
curl -s -X POST http://127.0.0.1:8787/mcp -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"smoke","version":"0"}}}'
curl -s -X POST http://127.0.0.1:8787/mcp -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}'   # → list, exec, config, status, snapshot
```

The runner refuses non-loopback binds and non-loopback Origins (DNS-rebinding guard). Real
`exec` lands in Phase A4; until then exec/status/snapshot answer with a typed "pending
phase" result.

### Control plane: provision a service (B1 happy path)

```sh
echo -n 'vultr-api-key-9876' | cargo run -p freehold-control-plane -- provision vultr \
  --kind vultr --address api.vultr.com --state-dir ./.freehold/control-plane
```

What just happened (verify it yourself):

- `.freehold/runner/vultr/identity.json` — the runner's injected private keys (0600)
- `.freehold/runner/vultr/secrets.json` — the API key as sealed ciphertext only
- `.freehold/control-plane/state.json` — pubkeys + ciphertext only; grep for the API key
  and for `nostr_secret`/`enc_secret`: **zero matches** (B4's no-master-key proof)

```sh
# rotate the credential (reads the NEW value from stdin; re-seals to the same runner key)
echo -n 'vultr-api-key-5544' | cargo run -p freehold-control-plane -- rotate-secret vultr --state-dir ./.freehold/control-plane

# revoke a runner: blocks provision/rotate, deletes the shipped secrets.json
cargo run -p freehold-control-plane -- revoke vultr --state-dir ./.freehold/control-plane

# service-at-a-glance (no plaintext in output, ever)
cargo run -p freehold-control-plane -- list --state-dir ./.freehold/control-plane
```

Provision refuses to clobber: a name that exists, or a `--runner-dir` that already holds a
package, errors instead of destroying a runner's key.

## Roadmap

`roadmap/` holds the chunked plan: POC chunks 1–3 (engine room → Buzz relay scope → skills)
then MVP chunks 4–6 (k8s, console, installer). Phase checklists in `roadmap/POC_CHUNK1.md`
are ticked as work lands.

## Contributing / review

Every PR runs two gates:

- **CI** (`ci.yml`): `cargo fmt --check`, `build`, `test`, `clippy -D warnings` on the
  workspace (toolchain pinned to the declared `rust-version`). Green/red, no exceptions.
- **AI review** (`claude.yml`): reviews for real problems only. Findings are tiered in the
  top-level comment — BLOCKING (must fix) / IMPORTANT (should fix) / DEFER (named
  follow-up, never re-raised) / NIT (stays silent). Inline comments appear only for
  BLOCKING/IMPORTANT, on the exact lines. Every review ends with a one-line verdict:
  `MERGE-READY: <reason>` or `NEEDS WORK: <n> BLOCKING, <m> IMPORTANT`. The reviewer cites
  the CI status rather than re-running cargo.

Known quirk: the **first PR that introduces a new workflow file** (one that doesn't exist
on the default branch yet) skips the AI review — the review GitHub App refuses to issue a
token for workflows it has never validated on `main` (`workflow_not_found_on_default_branch`).
The check reports green anyway, because the skip is a graceful no-op. This is by design:
such a PR can't be AI-reviewed until merged, so CI + a human read carry it. Editing an
*existing* workflow file (e.g. `claude.yml`) is fine and reviews normally.

Read `AGENTS.md` before changing code: the locked model (relay-as-scope, generic exec, no
master key, host flexibility) is not open for reinterpretation. Never commit secrets,
private keys, or plaintext credentials.
