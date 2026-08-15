# AGENTS.md — freehold

Open-source appliance: one-command install, AI-agent-operated. Lands a Proxmox VE / VPS +
Kubernetes stack with Buzz Relay as the control plane and a skill framework that installs and
configures self-hosted OSS. Narrative: "reclaim the future we were promised."

**Status:** docs + locked Chunk 1 plan; code begins at Phase A (Rust workspace scaffold).

## Navigation

- `VISION.md` — narrative, single source of truth for the "why".
- `ARCHITECTURE.md` — system design, locked decisions, build plan (including host feasibility and
  the two axes: skills × host).
- `roadmap/ROADMAP.md` — chunked roadmap: POC chunks 1–3, MVP chunks 4–6.
- `roadmap/POC.md` — POC scope, goal, acceptance, test/promote flow.
- `roadmap/POC_CHUNK1.md` — detailed Chunk 1 build plan. **Locked, ready to execute.** The
  phase checklists (A–H) are the source of truth for implementation progress.

## Locked model — do not change without an explicit user decision

- **One control plane = exactly ONE relay scope** (relay-as-scope). A user's existing relay is
  onboarded as a service, not a nested scope. No "control plane of control planes".
- **Agent = brain; runner = dumb privileged hands.** ONE generic primitive: `exec(cmd, target,
  stream?)`. NO semantic tools (`tail_log`, `create_server`, … don't exist). Streaming is a
  property of exec. Runner executes the agent's command verbatim on the connection it owns.
- **Runner identity** = Nostr keypair (membership/signing) + a separate encryption keypair
  (env-injected / mounted secret, never committed).
- **CP = secret PROVISIONER, not a vault.** Encrypt-to-runner-key → ship ciphertext → inject
  runner private key → rotate. **NO master key.** Runner holds only ciphertext + its own key;
  decrypts locally, uses in memory, forgets. Plaintext never on disk, never in agent context;
  agents reference secrets BY NAME only.
- **Grants are coarse**: agent ↔ runner (whitelist of Nostr pubkeys). Dedicated runner per
  service = default; sharing via grants allowed. Readiness = the runner's OWN self-check:
  🟢 green / 🟡 yellow / 🔴 red.
- **POC is pre-MVP**: NO Kubernetes, NO real reasoning agent (Chunk 1 = scripted orchestrator,
  CPA stand-in), Buzz required only from Chunk 2. Deterministic k8s pods + LiteLLM = public
  release.
- **Host-FLEXIBLE — not locked to Proxmox.** Proxmox is the lead/default, but VPS/cloud are
  first-class supported options (the business path). K8s layer and everything above the host
  driver run identically regardless of substrate. Installer/runner must target a VPS as easily
  as Proxmox from day one — no Proxmox-only shortcuts.
- **K8s fixed; hosting substrate pluggable.** Two orthogonal axes: SKILLS (what to install) ×
  HOST (where the appliance lives). Anything × anything composes.

## Chunk 1 (current work)

Prove the engine room standalone: local control plane (web UI) + runners as MCP tool servers +
secret provisioner + coarse grants + readiness. Connectors: SSH, Vultr, Backblaze B2. No Buzz,
no k8s.

- Rust workspace: `runner` crate + `control-plane` crate.
- MCP over HTTP for agent↔runner even though co-located — proves the real shape.
- Test targets: VPS (dev/smoke) → old-laptop Proxmox (test/staging, SSH target only) → home
  dogfood. Chunk 1 touches Proxmox only as an SSH target; a VPS or any SSH-able box stands in.

## Build / test

- Workspace: `cargo build`, `cargo test` (once scaffolded in Phase A).
- No formatter/linter config yet — rustfmt + clippy defaults.
- Each phase in `roadmap/POC_CHUNK1.md` has acceptance checkboxes; tick them as work lands.

## Code style

- Rust: follow rustfmt; small crates; keep the runner↔CP contract at the crate boundary and
  language-agnostic (MCP over HTTP).
- **Never** put secrets in code, config, tests, logs, or committed files. Private keys arrive via
  env var / mounted secret. No secret dumps in output or agent context.
- Keep `exec` generic — do not add semantic tools to work around a connector's API.
- No UI/chat surface rebuilds: Buzz provides the chat; the CP console is admin/ops only.
