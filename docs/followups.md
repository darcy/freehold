# Follow-ups — the grab bag (deferred work we must not forget)

Forward-looking work that is deliberately not in the current roadmap: pieces pulled
out of retired build plans, named deferrals, and legs left unverified. Nothing here
blocks the current chunk; it is the parking lot.

This is **not** the list of limitations in shipped code — those live in `AGENTS.md`'s
"Known gaps" section (revocation/rotation reach, replay windows, connector edge cases,
and so on). If something is a current limitation of what already ships, it belongs
there; if it is work not yet done, it belongs here.

## Provisioning / substrate

- **Vultr provider.** The `providers/` seam exists (`providers/proxmox/`), but no
  `providers/vultr/` or `api-vultr` access mode. Same orchestration, second provider;
  Hetzner follows the same pattern later. (The install-access plan's last unshipped leg.)
- **Relay Buzz stack as Terraform.** The relay LXC is adopted by the substrate module, but
  the Buzz stack *inside* it (postgres, redis, minio, git + the relay) is deployed by the
  Go driver `platform/services/relay/buzz/deploy_relay.go` running `docker compose`. Make
  it declarative via a `relay.tf` (docker provider) or fold `DeployRelay` into a
  terraform `local-exec` — needs a decision (docker-provider vs. fold-DeployRelay).
- **Substrate via a real proxmox/bpg provider.** The substrate (durable plane + cp/relay/
  k3s LXCs + k3s bring-up) stays exec-first (`null_resource` + shell scripts) because
  **bpg 0.66+** dropped the tarball-create path the live substrate was born from (PVE
  9.2.2). Options: clone-based gold template (recommended), `telmate/proxmox`, or pin a
  pre-0.66 bpg. Detection+allocation (free vmid, thin-pool/ZFS, `backup=1` mounts) keeps
  some Go/shell regardless.
- **Fresh-box vmid allocation.** The LXC create path still lives in Go + exec script
  (terraform adopts via `null_resource`), so allocating a vmid for a genuinely fresh box
  is still a follow-up.

## CPA / agents

- **Chunk 4 Phase F — resource baseline.** With the CPA and at least one created agent
  running concurrently, capture idle and active CPU/RAM footprint per agent process and
  record it as a decision input for sleep/wake work. This is the current focus.
- **Pod-prompt durable wiring.** An agent pod reads its prompt from a `<pod>-prompt`
  ConfigMap seeded from the control plane's embedded `agents/freehold/prompt.md`. A
  compute-only teardown/rebuild re-seeds from the *embedded* bytes, so a prompt edit made
  only in the CP's `/srv/data/cp` copy doesn't survive a rebuild until re-deployed. Wire
  the pod to the CP's durable mount.
- **More capability runners land with their capabilities.** The
  capability-runner surface (`stageDepartmentRunners`) is built and the
  departments' current grants are live (`pve-ssh-root`, the kube doors,
  `litellm-api-admin`, `cloudflare-api-<zone>`, `dnsmasq-local-root`);
  GitHub (read/repo state/releases/actions) and web search (self-hosted
  SearXNG) runners are not built yet — each lands as its own capability-named
  runner with its own grant.
- **Capability tooling beyond the runners.** Backup scheduling, monitoring
  dashboards, and AI hardware bring-up still have no tooling — the raw
  grants/exec paths exist; the per-capability tooling lands with each
  capability.
- **Secondary-relay onboarding.** A user's pre-existing/separate Buzz relay is locked to
  be onboarded as a service (via a relay runner), not a nested scope. Not built.

## Relay / Buzz

- **Audit exec receipts as channel messages.** Designed (grant implies read access to a
  runner's exec history; receipts redacted before posting, same discipline as the
  shipped-package flow) but not posted. The operational audit path stays kind-48001 +
  local spool.
- **Relay-mode runners need an explicit community-membership step — automated for
  department runners.** A relay-mode runner cannot read its roster until it is a relay
  COMMUNITY member (`buzz-admin add-member`); `stageDepartmentRunners` now does this for
  each department runner. The generic `provision`/`adopt` CLI path still does not
  auto-member — minting a non-department relay runner needs the step run by hand.
- **Private channels vs. the activity feed.** "Private channels are excluded from any
  server-wide activity feed, not just direct-read gated" was not separately verified. The
  live deployment showed no exposure; re-check if a Buzz version changes the feed surface.
- **Emergency-repair drill.** The relay-down case re-invokes the same dormant local
  provisioning expert against the same target. The path is exercised on every operational
  teardown/rebuild, but a dedicated relay-down drill is later, pre-MVP work.

## Verification / harness

- **Live Backblaze leg.** The B2 connector is hermetic-verified only (mock API +
  acceptance round-trip); a live Backblaze-account leg needs real credentials.
- **One real-relay acceptance run.** The Chunk-2 per-leg deltas (non-member denied,
  member-but-ungranted denied, no session unreachable) were each proven live ad hoc, but
  not yet consolidated into a single `freehold-acceptance` run against the real relay.
- **Vultr live runner identity.** Vultr ran live as a HOST path (real account, PVE-on-cloud
  spike), but a distinct vultr-API runner identity under a relay roster was never minted.

## Console / CLI

- **Go console residual port gaps.** The deleted Rust console carried surfaces the Go
  console never picked up; none is on a live path:
  - no `freehold-console rebuild` verb (the relay-fold primitives exist but nothing calls
    `RebuildFrom` from the runtime);
  - CLI verbs the Go console lacks: `rotate-secret`, `revoke-grant`, `list` (web/API
    equivalents exist), and `dns rm`/`dns list`/`dns sync` (`dns add` + `dns apex` exist);
  - agent-presence reads don't send the community host (LAN-IP dial; the dual-URL
    `QueryEventsAuth` form exists but isn't used there);
  - no 256 KiB request-body cap (the Rust `web.rs` had one);
  - `serve` flag defaults dropped the env-var bindings (`FREEHOLD_CP_ADDR`,
    `FREEHOLD_RELAY_URL`, …) — the Go flags are literals.
- **Certs UI follow-ups.** A TUI rebuild step to collect the DNS provider up front (today
  collection is inline in the cert stage), and a Certs tab for expiry + issue/renew — confirm
  what already landed before building.
- **Manual cert import + Tailscale certs.** Named future work for the TLS edge.

## Dropped (kept here so they aren't re-raised)

- **Migrations epoch-name consolidation** — informational only; a long-lived world that ran
  the old names re-runs them once, and both migrations are idempotent. No action.
- **Release-workflow dry run** — `.github/workflows/release.yml` was untested until a real
  `v*` tag; `v0.7.0` has since shipped, so the assets/checksums path has run. Done.
- **Caddy cert/DNS issuance staying the CP's in-process overlay** — this is the current
  design, not a gap; certs are event-driven (`worldCert`) while `caddy.tf` defines only the
  static shape.
