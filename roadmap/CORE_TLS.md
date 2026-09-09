# Core TLS fronting proxy (Caddy) + embedded-lego cert lifecycle — PLAN & RUNBOOK

Status: APPROVED, implementation in progress by opencode. This file is the single source of
truth for the phase so work survives context compaction. Read `AGENTS.md` / `CHANGELOG.md` /
`roadmap/POC_CHUNK4.md` for the surrounding context (Chunk 4 CPA, D1–D4 drills, PR 143).

## Progress (checkpoint — resume here)
- **F1 DONE** (`3ded298`): resolver dotted FQDNs + `relay.<d>`/`cp.<d>` derivation + dnsmasq
  `address=/.<d>/<ip>` wildcard apex + `dns wildcard` subcommand + `stageDnsWildcard` + TUI row.
- **F2 DONE** (`f5981b6`): core Caddy hostNetwork kube edge — `deploy/caddy.go`
  (`RenderCaddyfile` + `CaddyManifest`: durable PVC, Caddyfile ConfigMap, hostNetwork Deployment
  on 80/443, NodePort svc); `config.CaddySpec`; `stageCaddy`/`caddyManifestScript`/`recordCaddy`;
  TUI `caddy (TLS edge)` row + probe.
- **F3a DONE**: embedded go-acme/lego + `platform/services/certificates/letsencrypt` —
  `providers_gen.go` GENERATED from lego's own registry (`go run ./platform/services/certificates/letsencrypt/genproviders`):
  201 provider names + per-provider env-var table; `Providers()`/`ProviderEnvNames()`/`IsProvider()`;
  `Verify()` (throwaway TXT); `IssueWildcard()` (DNS-01 via `SetDNS01Provider`); `WriteTLS()`;
  `LoadExpiry()`/`ReuseIfValid()`/`ReuseIfValidBytes()` reuse gate. Tests green.
- **F3b DONE**: `platform/services/certificates/letsencrypt/store.go` seals the DNS token to the ops identity
  (AAD-bound, regenerable by freehold which runs lego in-process — never plaintext);
  `stageCert()` right after `stageCaddy`: reuse the durable PVC cert when fresh (>=30d),
  else resolve the token (sealed copy or interactive provider dropdown + lego-derived
  env fields + freeform fallback), pre-verify via lego TXT, issue the wildcard, and
  `installCaddyCert` writes chain/key into the caddy-data PVC via a short-lived helper
  pod (survives Caddy's first-boot crash-loop) + restarts the edge; records expiry/issuer
  into `config.Caddy.CertExpiry/CertIssuer`.
- **F3c PENDING (optional)**: the TUI rebuild form step to collect the DNS provider up
  front (like the litellm key) — today collection happens inline in stageCert.
- **F4 IN PROGRESS**: Certs TUI tab (expiry list from config + issue/renew).
- **F5–F6 PENDING**: CPA pod -> wss://relay.<d> (flip RelayWsURL once the edge truly
  serves TLS live) + live D1; tests/docs/PR.
- Binaries rebuilt + placed in `~/.cargo/bin/{freehold,control-plane}`.




## Why this phase exists (the two hard blockers it fixes)

The CPA pod (buzz-acp harness in `ghcr.io/block/buzz-sprig:main`) could not join the relay:

1. **TLS trust**: buzz-acp uses a **bundled webpki root store** — it ignores `SSL_CERT_FILE`
   and a CA injected into `/etc/ssl` (PROVEN live: sound `curl` with the CA → 200, buzz-acp as
   root with the CA in `/etc/ssl` still `invalid peer certificate: UnknownIssuer`). It can only
   trust **public-CA** (Let's Encrypt etc.) certs. Freehold's local-CA Caddy cert was untrustable.
2. **NIP-42 auth**: the relay anchors NIP-42 auth to a single relay URL (≈ `wss://<host>`).
   buzz-acp uses its `BUZZ_RELAY_URL` for BOTH the connect and the auth `relay` tag, and there is
   **no** separate auth-relay env. A plain-`ws://<host>` connect yields `relay: ws://<host>` →
   relay logs `NIP-42 auth failed: relay url mismatch`. The operator's external clients use
   `wss://` and succeed => the relay can only anchor to ONE scheme; plain-ws and wss are mutually
   exclusive on one relay.

=> Fix: freehold owns a **Caddy fronting proxy** (a core kube service, like litellm) with a
**real Let's Encrypt cert via DNS-01**; the pod dials the canonical `wss://relay.<domain>` ->
both blockers fall (public cert trusted; auth tag matches).

## APPROVED decisions (operator-confirmed)

- **Topology — NO world/base domain.** The relay host and CP host are separate, LITERAL
  user-supplied hostnames (never derived, no `relay.`/`cp.` prefix). Everything sits behind a
  single static **proxy IP** (`Proxy.Ip`, the Caddy/k3s node), which both hosts resolve to.
  Relay + CP LXCs are DHCP, internal, behind the proxy; the proxy is the ONE static address.
- **Caddy placement**: k3s **Deployment on `hostNetwork`** binding 80/443 on the proxy's LAN IP
  + NodePort Service + **durable PVC** (`/srv/data/k8s-volumes`, backed up) holding Caddy config
  AND the issued certs. NOT a normal CNI ClusterIP service (pod->external-LAN egress is blocked
  by kube-router; hostNetwork is required, same as the CPA pod).
- **Cert issuance**: **go-acme/lego EMBEDDED** in the control plane (Go lib; no shipped binary).
  DNS-01 only. **PER HOST**: one single-name cert for the relay host + one for the CP host
  (two orders; no wildcard/base). Provider chosen from a **dropdown populated from lego's full
  provider registry** (not a curated shortlist); per-provider env-var names derived from lego.
  Collect + **pre-verify** (throwaway TXT) before saving.
- **DNS credentials = PER DOMAIN** (each host has its own sealed slot,
  `dns-provider-<slot>.json`), kept SEPARATE — may differ. When a slot is missing, the CP slot
  offers to re-use the relay credential (copied into its own slot without re-entering). Never
  plaintext: sealed to the ops identity (freehold runs lego in-process). `--yes` with a missing
  slot hard-errors (run interactively once to store).
- **Reconcile-always / no re-ask**: credential already sealed -> skipped; valid per-host cert on
  the durable volume (>= 30d) -> reused (DNS-01 skipped). Compute-only teardown/rebuild keeps both.
- **Certs TUI tab**: list both hosts (relay, cp), issuer, **expiration**, status.
- **CPA pod**: relay origin `wss://<relay-host>` (443 via the proxy). hostNetwork pod (done).
- **Domain gate REMOVED**: internal resolution (relay/cp hosts -> proxy IP in the CP resolver)
  is enough for install; no A4 DNS-resolution gate.
- **Naming**: LXC + plane slugs = the RELAY host slug + role suffix (`-relay`/`-cp`/`-k3s`).
- Future (chunk8?) = manual cert import + Tailscale cert; NOT in this phase.

## Sequencing (each = a review pass; branch + PR -> main per AGENTS)

- **F1. Resolver extension + domain-rename derivation.**
  - `control-plane/src/dns.rs`: relax `validate_name` to allow dotted FQDNs (`.` in the middle,
    ≤253, no leading/trailing dot/hyphen, no `..`). Update tests.
  - The CP resolver must serve `relay.<base>`, `cp.<base>`, and future subdomains -> Caddy node IP.
    Add **dnsmasq `address=/freehold-test.darcydev.net/<caddy-ip>`** (wildcard-matches subdomains of
    the base) — this is the mechanism; register it in `stageDnsRegister`/the resolver. The current
    addn-hosts has no wildcard and can't do the 3-label `relay.freehold-test.darcydev.net` —
    `address=` is the clean answer.
  - `contract/config/config.go` + `rebuild.go fromAnswers`: derive RelayURL
    `https://relay.<domain>`, CPURL `https://cp.<domain>`; drop the `cp-` prefix.
- **F2. Core Caddy kube service** (mirror `stageLitellm`/`recordLitellm` shape):
  - Caddy manifest: Deployment hostNetwork, ports 80/443, NodePort Service, durable PVC, readiness.
  - Runtime Caddyfile rendered to reverse-proxy by Host: `relay.<base> -> relay LXC:3000`,
    `cp.<base> -> CP LXC`; `tls` from the durable cert files (presented by freehold, not Caddy-ACME).
  - `recordCaddy` + `managed` entry + Services-row + split-horizon `*.base -> 192.168.30.7` + teardown.
- **F3. Embedded-lego DNS-01 + cert install/reload/reuse**:
  - go-acme/lego import; provider dropdown from lego registry; per-provider env collection;
    pre-verify (TXT round-trip); store as runner secret (ciphertext).
  - New stage: lego DNS-01 for `*.freehold-test.darcydev.net` -> write `fullchain.pem`/`key.pem` into
    the Caddy durable volume -> Caddy reload (cert watch / reload signal).
  - Decision gate: no-token -> collect (or hard-error under `--yes`); cert valid -> reuse.
- **F4. Certs TUI tab** (expiry, issue/renew).
- **F5. CPA pod -> `wss://relay.<domain>` + live D1 verify.**
- **F6. Tests, ARCHITECTURE/AGENTS/CHANGELOG, roadmap phase entry, PR cycle**
  (build -> review -> merge-ready -> teardown/rebuild re-verify).

## Live/infra state to resume from (as of last touched)

- Freehold repo branch: `chunk4-phase-b` (PR 143, at commit `ef2b053`, MERGE-READY at that point;
  review re-runs on push).
- Real appliance `freehold-test.darcydev.net`: relay LXC 100 (192.168.30.8), CP LXC 101 (192.168.30.9),
  k3s LXC 102 (192.168.30.7, node). Operator's workstation at 192.168.10.144; PVE/proxmox-box host
  root@192.168.30.224. Config at `~/.config/freehold/config.toml`; runners under `~/.freehold/runner/`
  (proxmox-box, litellm with sealed provider-key). Runners serve on 127.0.0.1:8787 / 8788.
- Driving the host from THIS box: `freehold exec --addr 127.0.0.1:8787 --agent-dir
  ~/.freehold/control-plane/agent-ops proxmox-box '<pct ...>'`. `freehold` live binary =
  `~/.cargo/bin/freehold` (rebuild via `cd control-plane && mise exec go@1.25.0 -- go build -o
  target/debug/freehold ./cmd/freehold && cp target/debug/freehold ~/.cargo/bin/freehold`).
  Rust siblings = `~/.cargo/bin/{control-plane,runner}` + `~/.cargo/release/*` (already built+placed).
- Backlog we can NOT fully hold a long live rebuild in the sandbox tool (its process-cleanup kills
  long background commands; use targeted `freehold exec` probes + `pct`/`docker compose` for live
  verification, and let the operator run the full `t`/`B` teardown+rebuild in their own terminal).
- ALREADY-COMMITTED fixed-and-verified pieces to keep:
  - split-horizon DNS record for the relay domain -> relay LXC (stageDnsRegister + relayDomainHost).
  - CPA pod `hostNetwork: true` (AgentPodManifest).
  - litellm master sync + registration via the litellm runner + CPA key = gateway master.
  - pod subPath prompt mount + allowlist owner.
- LIVE changes to note/cleanup under this phase:
  - Added relay compose host publish `80:3000` (live, for a now-superseded Path 2a) — REVERT/omit.
  - Created `freehold-ca` Secret in agents ns + local-CA Caddy experiments — superseded, retire.
  - Caddy was brought up on the relay LXC then reverted (relay LXC currently back on :3000 only).
- Current relay: plain ws on :3000 (+ a live :80 publish), NO TLS on LAN, vhost matches bare Host
  only (404 on `host:3000`). Operator external access via their nginx (Tailscale), not on the LAN.

## How the pod finally connects (end state)
1. Pod (hostNetwork) dials `wss://relay.freehold-test.darcydev.net` (443).
2. Resolver `address=/freehold-test.darcydev.net/192.168.30.7` -> Caddy (node).
3. Caddy:443 presents the LE wildcard cert; buzz-acp (bundled webpki) trusts it (no UnknownIssuer).
4. NIP-42 auth relay tag `wss://relay.freehold-test.darcydev.net` matches the relay's anchor.
5. Caddy reverse-proxies to relay LXC:3000 (Host preserved) -> relay serves the `relay.` vhost.
6. D1: operator talks to the CPA in Buzz. D2/D3 drills follow.

## Tests to add
- Resolver: dotted host validation + `address=` wildcard render (static).
- Cert decision: no-token (interactive prompt vs `--yes` hard-error); token present -> skip; cert
  valid -> reuse.
- Caddyfile render + recordCaddy/managed/teardown.
- Certs-tab data (expiry) rendering.
- Embedded-lego gated behind a provider pre-verify seam (hermetic).

## Build / test / review workflow (AGENTS)
- Go: `cd contract && gofmt -l .; go build ./... (and platform/, control-plane/) && go vet ./... && go test ./...`.
  Rebuild live freehold as above.
- Rust (resolver change): `mise exec rust@1.94.0 -- cargo build --bin control-plane --bin runner`
  (debug) + release; re-place `~/.cargo/bin/{control-plane,runner}` and `~/.cargo/release/*`.
  Redeploy the CP (deploy-cp / rebuild) so the new resolver binary runs in the CP LXC.
- Ship each phase step as branch -> PR -> main (AGENTS PR rules); wait for `check`+`review`; fix real
  findings; operator merges.
