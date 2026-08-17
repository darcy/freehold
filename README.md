# freehold

*Reclaim the future we were promised.* An open-source appliance — one-command install,
AI-agent-operated — that lands a Proxmox VE / VPS + Kubernetes stack with Buzz Relay as the
control plane and a skill framework that installs and configures self-hosted OSS. The agent
is the wall-breaker: you don't manage servers, you *ask*.

**Status: Chunk 1 (POC) implemented — phases A–G merged, H (test/promote) next.**
The engine room is proven standalone before Buzz (Chunk 2) and Kubernetes (MVP) come in:
identity + MCP skeleton (A), secret provisioner — seal/ship/rotate/revoke, no master key
(B), SSH + Vultr + B2 connectors (C), coarse grants — signed calls only (D), the scripted
orchestrator CPA stand-in (E), the local admin/ops web console (F), and the acceptance
script that proves it all hermetic on loopback (G). Phase H is operator work on real
hardware (old-laptop Proxmox as an SSH target, then home dogfood) — no code changes
expected unless the test surfaces fixes.

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
Cargo.toml            workspace: core, runner, control-plane, orchestrator, testkit, acceptance
AGENTS.md             agent guidance: locked model, conventions, known Chunk-1 gaps
roadmap/              ROADMAP.md, POC.md, POC_CHUNK1.md + POC_CHUNK2.md (phase checklists,
                      ticked), BUZZ_SURFACE.md (Chunk 2 Phase-0 deliverable)
core/                 freehold-core — shared by every crate, no product logic
  src/identity.rs     Nostr (secp256k1) + X25519 keypairs; env-inject or 0600 file
  src/auth.rs         the signed-call protocol: BIP-340 signatures over
                      `runner_pubkey|ts|raw_body` — the runner verifies, every
                      client signs with the same primitives
  src/crypto.rs       sealed box TO a runner's X25519 pubkey: ephemeral X25519 +
                      HKDF-SHA256 + ChaCha20-Poly1305; recipient AND secret-name bound;
                      low-order-point forgery rejected; versioned wire format
  src/secrets.rs      SecretPackage: the runner's on-disk secrets.json (name → ciphertext,
                      target metadata, agent grants)
  src/audit.rs        BIP-340-signed audit log (0600), caller pubkey recorded
  src/futil.rs        atomic file discipline: unique 0600-at-birth temp + fsync + rename;
                      0700 state dirs — used everywhere secret material touches disk
runner/               freehold-runner — the privileged connector bridge
  src/mcp.rs          MCP-over-HTTP tool server (JSON-RPC 2.0). Contract tools: list,
                      exec, config, status, snapshot — all live. Every tools/call is
                      signed by a GRANTED agent pubkey or fails closed (D).
  src/exec.rs         the ONE generic primitive: exec(cmd, target, timeout) — local
                      process or an owned connection; secret values resolved BY NAME
                      from ciphertext, redacted from every response, audited
  src/ssh.rs          russh connector: in-memory keys, pooled connections, TOFU host keys
  src/main.rs         CLI: `runner keys init`, `runner serve`
control-plane/        freehold-control-plane — the engine room
  src/state.rs        runners + secrets store (atomic 0600 JSON; pubkeys + ciphertext only)
  src/provisioner.rs  B1 provision (generate identity → seal → ship → record pubkeys),
                      B2 rotate-secret, B3 revoke, D grant/revoke-grant (re-ship package),
                      with save-failure rollback
  src/console.rs      the console AGENT: the CP's own identity (0600) that signs
                      readiness probes against each runner — no side door, the
                      runner still fails closed
  src/web.rs          Phase F: loopback admin/ops web console (axum) — services at a
                      glance with LIVE readiness, and provision/rotate/revoke/grant
                      management. Not chat (Buzz owns conversation).
  src/main.rs         CLI: provision / rotate-secret / revoke / list / grant / agent-create /
                      serve (the web console)
orchestrator/         freehold-orchestrator — the scripted CPA stand-in (E): a signed
                      MCP client + onboard/readiness/exec/demo flows
testkit/              freehold-testkit — hermetic fixtures: mock Vultr/B2 API servers +
                      an in-process russh sshd (shared by the connector tests)
acceptance/           freehold-acceptance — the Chunk-1 acceptance script (G): 9 checks,
                      G1 happy path + G2 three connectors via the runner + G3 security
                      invariants; `cargo run -p freehold-acceptance`
```

## Security model (no master key)

- The CP never holds a private key that decrypts anything, and never holds plaintext
  (credentials are sealed, forgotten). The only key under the CP state dir is the console
  AGENT key — it signs readiness probes and is provably not the encryption recipient of
  any runner (G3.3 checks this).
- The runner holds ciphertext + its own injected private key; only that key opens its
  blobs, and a blob only opens under the secret name it was sealed with.
- Rotation re-seals a NEW credential (the erase lever for your copies); revocation blocks
  provision/rotate and deletes the shipped credential. Honest limits are written down in
  `AGENTS.md` (no remote revocation of a capability someone else kept; re-keying and
  epoch/staleness are named follow-ups; a RUNNING runner keeps its in-memory credential
  until restart — rotate/re-grant reach the next boot).

## Getting started (current Chunk-1 state)

Prereqs: Rust 1.94+ (workspace declares `rust-version = "1.94"`).

```sh
cargo test --workspace        # 110 tests across core / runner / control-plane / orchestrator / acceptance
cargo clippy --workspace --all-targets -- -D warnings   # must be clean
cargo fmt --all --check       # CI gate
cargo run -p freehold-acceptance   # the whole Chunk-1 story, hermetic on loopback (9 checks, exit 0)
```

### Runner: identity + MCP server

```sh
cargo run -p freehold-runner -- keys init     # writes ./.freehold/identity.json (0600)
cargo run -p freehold-runner -- serve         # MCP over HTTP, default 127.0.0.1:8787
                                              # (FREEHOLD_RUNNER_ADDR, loopback only)
```

The runner refuses non-loopback binds and non-loopback Origins (DNS-rebinding guard). Every
`exec`/`config`/`status`/`snapshot` call must be signed by a GRANTED agent pubkey or it
fails closed — the `control-plane grant <runner> <pubkey>` whitelist lives in the shipped
package and is re-read from disk per call.

### The console: provision a service, watch it go green (the F flow)

```sh
cargo run -p freehold-control-plane -- serve --state-dir ./.freehold/control-plane
# open http://127.0.0.1:8080 — admin/ops only (chat is Buzz's job, Chunk 2)
```

Paste a credential into the provision form (or `curl` the API). The console ships the
runner package, registers the runner's MCP address, and the overview shows the runner's OWN
self-check per target — 🟢/🟡/🔴 — probed through the same signed MCP channel an agent
uses. Manage: rotate, revoke, grant/revoke-grant, set MCP addr.

### Control plane CLI: provision a service (B1 happy path)

```sh
cargo run -p freehold-control-plane -- agent-create my-agent --state-dir ./.freehold/control-plane   # the agent identity (0600)

echo -n 'vultr-api-key-9876' | cargo run -p freehold-control-plane -- provision vultr \
  --kind vultr --address api.vultr.com --state-dir ./.freehold/control-plane

# grant the agent, then it may call the runner (everything else fails closed):
cargo run -p freehold-control-plane -- grant vultr <agent-pubkey> --state-dir ./.freehold/control-plane
```

What just happened (verify it yourself):

- `.freehold/runner/vultr/identity.json` — the runner's injected private keys (0600)
- `.freehold/runner/vultr/secrets.json` — the API key as sealed ciphertext only
- `.freehold/control-plane/state.json` — pubkeys + ciphertext only; grep for the API key
  and for `nostr_secret`/`enc_secret`: **zero matches** (the no-master-key proof)

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

### freehold: the CLI (the scripted CPA stand-in)

The binary is `freehold`; the cargo package is `freehold-orchestrator`. Build with the
workspace (`cargo build`, binary at `target/debug/freehold`) or install it into PATH once:

```sh
cargo install --path orchestrator   # -> ~/.cargo/bin/freehold
freehold --help
```

```sh
# onboard an existing service: provision -> ship -> self-check (hard-fails unless green) -> grant -> report
echo -n 'vultr-api-key-9876' | cargo run -p freehold-orchestrator -- onboard blog \
  --kind vultr --address api.vultr.com --agent-dir ./.freehold/control-plane/agent-my-agent \
  --cp-state-dir ./.freehold/control-plane

# drive a RUNNING runner with signed calls:
cargo run -p freehold-orchestrator -- exec --addr 127.0.0.1:8787 \
  --agent-dir ./.freehold/control-plane/agent-my-agent --runner-pubkey <runner-nostr> \
  --target blog --cmd 'curl -sS "$VULTR_URL/v2/instances" -H "Authorization: Bearer $VULTR"'

cargo run -p freehold-orchestrator -- demo --addr 127.0.0.1:8787 \
  --agent-dir ./.freehold/control-plane/agent-my-agent --runner-pubkey <runner-nostr> \
  --steps steps.json   # [{target, cmd, secrets?, timeout_s?}]

# C2/A2: bootstrap-provision a target through a provisioning runner (runner-direct)
#   --vmid is OPTIONAL (the driver picks the next free cluster id); sizing
#   --rootfs-gb 16 (default) / --memory-mb 2048 (default) fit the relay stack.
cargo run -p freehold-orchestrator -- bootstrap --kind proxmox-lxc --name relaybox \
  --addr 127.0.0.1:8787 --agent-dir ./.freehold/control-plane/agent-my-agent \
  --runner-pubkey <runner-nostr>    # arch-matched template ensure (pveam, idempotent) + pct on the PVE host; docker+compose installed in the guest

# C2/B: deploy the Buzz relay onto the target (docker gate -> bundle -> compose -> liveness)
#   --owner-pubkey is REQUIRED (written to RELAY_OWNER_PUBKEY; the relay refuses
#   to start with CHANGE_ME placeholders). --lxc <vmid> deploys INTO the container.
cargo run -p freehold-orchestrator -- deploy-relay --addr 127.0.0.1:8787 \
  --agent-dir ./.freehold/control-plane/agent-my-agent --runner-pubkey <runner-nostr> \
  --target proxmox-box --name relay-box --http-port 3000 \
  --owner-pubkey <64-hex-owner> [--lxc 100]
# C1: deploy the control plane onto the box in OPERATE mode (loopback-only).
# The box GENERATES its own identity (a keypair is never shipped — the
# runner logs every exec verbatim); --binary is a local release build.
cargo run -p freehold-orchestrator -- deploy-cp --addr 127.0.0.1:8787 \
  --agent-dir ./.freehold/control-plane/agent-my-agent --runner-pubkey <runner-nostr> \
  --target proxmox-box --binary target/release/control-plane \
  --relay-url http://relay-box:3000

# C2: add the box's fresh console pubkey as a relay member (via buzz-admin
# in the relay LXC — the CP never holds the relay signing key).
cargo run -p freehold-orchestrator -- relay-member --addr 127.0.0.1:8787 \
  --agent-dir ./.freehold/control-plane/agent-my-agent --runner-pubkey <runner-nostr> \
  --pubkey <fresh-console-pubkey> --lxc 100
```

## Roadmap

`roadmap/` holds the chunked plan: POC chunks 1–3 (engine room → Buzz relay scope → skills)
then MVP chunks 4–6 (k8s, console, installer). Phase checklists in `roadmap/POC_CHUNK1.md` (and `roadmap/POC_CHUNK2.md`, in draft)
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
- **README / ARCHITECTURE drift**: when a PR changes something those docs document (or drifts
  from a locked decision in `ARCHITECTURE.md`), the reviewer adds one `README:` /
  `ARCHITECTURE:` line to the top-level comment — a signal to update it or ignore, never a
  blocker, never nitpicked.

Known quirk: **any PR whose tree changes a workflow file — adding *or* editing, including
`claude.yml` itself — skips the AI review.** The review GitHub App refuses to issue a token
for workflows that don't match the default branch (the error may surface as
`workflow_not_found_on_default_branch`), and the action converts the refusal into a
graceful green no-op. The check reporting green means "nothing reviewed", not "review
passed". This is by design (a PR can't be AI-reviewed under a workflow definition it
defines itself) and it is self-resolving: merge the workflow change and it applies to the
default branch, after which normal PRs review again. Workflow-changing PRs are carried by
CI + a human read. Note: workflow content was only loosely enforced early on, so some early
workflow-editing PRs did get reviewed — that window is closed.

Read `AGENTS.md` before changing code: the locked model (relay-as-scope, generic exec, no
master key, host flexibility) is not open for reinterpretation. Never commit secrets,
private keys, or plaintext credentials.
