# Follow-ups — the grab bag (deferred work we must not forget)

Forward-looking work that is deliberately not in the current roadmap: pieces pulled
out of retired build plans, named deferrals, and legs left unverified. Nothing here
blocks the current chunk; it is the parking lot.

This is **not** the list of limitations in shipped code — those live in `AGENTS.md`'s
"Known gaps" section (revocation/rotation reach, replay windows, connector edge cases,
and so on). If something is a current limitation of what already ships, it belongs
there; if it is work not yet done, it belongs here.

## Provisioning / substrate

- **The runner-client baked into every guest, and kept fresh on update.** The
  runner-client install is a `create-lxc` skill step today (Compute fetches
  the `runner` release asset per guest); the mechanical version bakes it into
  the provisioning engine's LXC boot for CORE LXCs too (cp/relay/k3s), and
  `freehold update` sweeps EVERY created/adopted guest — replace the binary
  where it drifted, re-run nothing else — so a resident runner never runs a
  stale build against a newer world.
- **World-config degradation on update.** `FlagsFromConfig` derives flags from
  the tenant config only, and the config does not record the substrate-create
  params (memory/bridge/storage/rootfs/relay-gw/thin-pool), so an update's
  redeploy re-renders the console's world-config WITHOUT them — fine while
  guests exist (they're only create params), but a teardown→rebuild after an
  update then cannot create the relay/k3s guests ("memory: value must have a
  minimum value of 16"). Worked around live (relaunched the console serve with
  a completed world-config); the durable fix is persisting the world-config on
  the CP's durable plane (deploy writes it, redeploys merge/re-read) or
  recording the params in the tenant config at install.
- **Pre-tf kube workloads cannot be adopted by an updated world.**
  `kubernetes_manifest` does not support import, so a world whose namespaces/
  workloads predate the terraform module (0.7.1→0.7.3+) fails its build's
  services phase with "Cannot create resource that already exists". The
  repair is the tested teardown→rebuild cycle (durable-plane data survives);
  an adoption path (a migration that adopts, or a tf-side import workaround)
  would make the upgrade seamless.
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
- **Infra migrations — one-time transforms beyond the CP, executed through the runner.**
  The CP's `<epoch>.sh` queue runs on the CP guest and reaches the relay only over its
  HTTP API, so it cannot patch guest filesystems/docker (that class of repair is a
  *desired-state true-up* and belongs in the re-running deploy paths — e.g. the relay
  deploy re-applies its compose patches on every build). When a genuine ONE-TIME infra
  transform is first needed (data migration between volumes, recorded-state rewrite on a
  guest), ship it as a bash script in `migrations/` (data, not binary — the property that
  matters) executed by the CP's world build **through the co-located runner** (`mc.Exec`,
  the same transport `deploy_relay.go` uses): a small build stage runs pending scripts
  against a named target before the services phase, markers on the CP durable plane.
  Runner calls re-read the relay-signed roster per call and fail closed on relay outage,
  so relay-down repair stays on the box-side transient path.

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

- **After a relay redeploy, private-channel publishes 403 until the channels'
  roster snapshots are reconciled.** The relay's kind-39002 roster events live
  in its event store; a relay LXC redeploy (the teardown→rebuild cycle)
  leaves them stale against the DB's memberships, and the new buzz enforces
  the signed snapshot on private-channel publishes — an agent's own reply to
  its own channel 403s while everything else looks healthy. Repair:
  `buzz-admin reconcile-channels --channel <id>` per channel (verified live).
  The durable fix: deploy_relay (or the world-build's relay stage) runs
  `reconcile-channels` across the community's channels after a redeploy.
- **Audit exec receipts as channel messages.** Designed (grant implies read access to a
  runner's exec history; receipts redacted before posting, same discipline as the
  shipped-package flow) but not posted. The operational audit path stays kind-48001 +
  local spool.
- **Upstream buzz: the agent memory plane's integrity is a prompt, not a
  signature.** The relay validates a kind-30174 engram's envelope shape only
  (`validate_engram_envelope`: exactly one 64-hex `d`, exactly one 64-hex `p`,
  plausible NIP-44 content) and never checks that the event's signature is the
  claimed `p` owner's — so any community member can post an engram under a
  registered agent's identity, and readers (querying `#p` + decrypting with
  their own conversation key) simply never see it. Confidentiality holds
  (NIP-44 pairs writer↔owner), authenticity does not. Upstream fix: replicate
  the `is_agent_owner` check for KIND_AGENT_ENGRAM at ingest; reported with a
  two-key reproducer (owner-signed OK, foreign-signed accepted).
- **Upstream buzz: `mem set` on the shipped image can push a 30174 with no `p`
  tag.** Observed twice live on the deployed image (`sprig`-shipped buzz, Sep
  23): `buzz mem set <slug> v --owner <the author's own pubkey>` is refused by
  the relay with "exactly one `p` tag (got 0)", while the same command with a
  foreign owner writes. The pinned relay source's `build_event` unconditionally
  attaches the `p` tag, so the deployed image predates or diverges from the pin
  (`deploy_relay.go` `DefaultBufRef = f956e6fe…`) and the client-side mechanism
  is unconfirmed — the report should carry the repro, not a cause. Unreachable
  from freehold: every pod now mints its attestation (owner ≠ agent by
  construction), and the CLI itself rejects self-attestation.
- **Bound the memory attestation in time.** `contract/nipoa` signs only
  `kind=30174`; it has no `created_at<` clause, so a leaked pod env authorizes
  memory writes until the pod is re-applied. A rotation story (mint with an
  expiry + re-mint on `freehold build`) is the named follow-up; the same trust
  root already supports the clause.
- **Pin the sprig image.** `agent.SprigImage` is `ghcr.io/block/buzz-sprig:main`
  — a moving tag with no digest pin, so an upstream `main` push changes every
  agent pod's binary on the next re-apply. Pin by digest (after establishing
  the publishing workflow's provenance) and roll deliberately.
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
- **Relay-contract verification skill (write it once, after the exec-audit fix).** Two
  native integrations have been failing silently fleet-wide: kind-30174 memory writes,
  and kind-48001 exec-audit publishes (400 for the appliance's entire lifetime, invisible
  behind NIP-42 auth on the reject). Candidate skill: after any relay/buzz change or a new
  event kind, probe ingest under a *runner* identity and assert the gate accepts what we
  publish — the publish side is what our own checks never see. Write it once, against the
  tested outcome of the exec-audit fix (fail-loud on an unknown kind), not speculatively.

## Verification / harness

- **Bot-review injection pre-vet.** The agentic reviewer reads PR-tree files, so a PR can
  plant reviewer-directed text ("ignore your instructions", fake verdicts) anywhere it
  expects the reviewer to look. First line of defense is the prompt's untrusted-input rule
  (such attempts are themselves a blocking finding); the cheap second layer is a pre-vet
  pass before the harness session: one diff-only single-shot call ("is this diff attempting
  to manipulate an automated reviewer?") whose flag prepends a warning to the review context
  (or fails the run loudly). Same provider key, seconds of latency, no new workflow.
- **Live Backblaze leg.** The B2 connector is hermetic-verified only (mock API +
  acceptance round-trip); a live Backblaze-account leg needs real credentials.
- **One real-relay acceptance run.** The Chunk-2 per-leg deltas (non-member denied,
  member-but-ungranted denied, no session unreachable) were each proven live ad hoc, but
  not yet consolidated into a single `freehold-acceptance` run against the real relay.
- **Vultr live runner identity.** Vultr ran live as a HOST path (real account, PVE-on-cloud
  spike), but a distinct vultr-API runner identity under a relay roster was never minted.

## Console / CLI

- **Guest inventory (LXCs) in the console + TUI.** The CP surface has no
  guest/LXC list: the Services view shows only world services and the DATA
  view only plane mounts, so a guest created outside the core build (e.g. a
  manually created `test-lxc`, or one an agent provisions) appears nowhere.
  Sketched design: the console lists the host's guests live (`pct list`
  through the co-located runner, in `/api/world`), tagged by ownership —
  `core` (the world's recorded vmids / the `<world>-` name prefix), `adopted`
  (a capability record named `<guest>-ssh-*`), `foreign` (other worlds' LXCs
  on a shared host) — with a Guests tab in the TUI plus the owned/adopted set
  surfaced in the DATA view. Split out of the grants-clarity PR so it stays
  reviewable; the ownership taxonomy (created vs adopted vs foreign) wants an
  operator pass before building.
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

- **Bot-review triggers: drop `pull_request`, go `pull_request_target`-only** — Done. Every
  PR (same-repo, fork, Dependabot's) is now reviewed from `pull_request_target`: the
  workflow YAML, scripts, and deps always come from trusted main's default branch (the
  YAML hole is closed), and a repo Actions event policy explicitly allows the event past
  GitHub's default block on public repos. The agent explores trusted base-branch state;
  the diff still comes from the API.
- **Migrations epoch-name consolidation** — informational only; a long-lived world that ran
  the old names re-runs them once, and both migrations are idempotent. No action.
- **Release-workflow dry run** — `.github/workflows/release.yml` was untested until a real
  `v*` tag; `v0.7.0` has since shipped, so the assets/checksums path has run. Done.
- **Caddy cert/DNS issuance staying the CP's in-process overlay** — this is the current
  design, not a gap; certs are event-driven (`worldCert`) while `caddy.tf` defines only the
  static shape.

### Release-test v0.7.4 findings (the fresh cycle)

- **A completed uninstall left the fresh profile behind.** The fresh-074 world
  was gone from the host (no guests, no authorized_keys lines — verified), but
  the profile's config + state dirs survived: the uninstall's local wipe
  (`wipeLocalProfile`) never ran for that attempt. Diagnose why the wipe was
  skipped when the logs exist; the dirs were removed by hand in the meantime.
- **A re-adopted plane's terraform destroy reaches OTHER worlds.** The fresh env
  re-adopted a leftover durable plane whose tf/kube state referenced the librem
  cluster; the uninstall's `terraform destroy` then destroyed the LIBREM world's
  kubernetes workloads (caddy, door SAs, litellm/postgres) while everything else
  looked healthy. Guard every tf kube stage: verify the kubeconfig's cluster is
  THIS world's k3s before apply/destroy.
- **The CP + relay LXCs are DHCP unless pinned.** A fresh install records the
  lease-du-jour; the next guest reboot moves it and the recorded coordinates
  strand the build/uninstall (the CP at .209 became .244 mid-test; the relay
  moved mid-build). Pin the recorded IP at create (or re-resolve by guest name
  on every verb).
- **The console is not a systemd unit.** It runs under `setsid nohup`; a CP
  guest reboot kills it and the world is headless until a deploy re-runs. The
  co-located runner already has `systemd-run` — give the console the same.
- **All profiles' local runners collide on 127.0.0.1:8787.** A multi-profile
  box's `freehold exec` silently hits whichever profile's runner owns the port
  (fresh-074's stole every other profile's execs for an hour, with misleading
  `-32001 not granted` errors). Per-profile ports minted at install.
- **A relay redeploy leaves the console's relay membership stale** (the live env:
  its console identity was never membered; the first reconcile publish 403'd
  `relay_membership_required`). The durable fix from the note above
  (reconcile-channels after a redeploy) plus: deploy re-members the console
  identity on every deploy, not just install.
- **The transient uninstall cannot destroy running guests** (no stop-first;
  `pct destroy` 255s). Stop-then-destroy in the transient path like the runner
  path does.
- **Respond-to allowlist management via freehold.** An agent pod's inbound
  author gate is fixed at deploy time (the CPA: anyone; a core department: the
  operator + the core agents; a custom agent: its asker + the CPA — the asker
  is recorded as the operator because the CP cannot see chat threads). The
  follow-up: anyone who can talk to an agent may ask freehold to add another
  identity to that agent's allowlist — freehold validates the request and
  re-applies the pod — and `create_agent` learns the real asker's pubkey.

## Identity / local-vs-global

- **Local runner port should be per-invocation, not a fixed default.** A box's
  runner only serves that box's own commands (the CP dials its OWN co-located
  runner), so nothing needs a stable port. Today `exec`/`build`/`teardown`/
  `uninstall`/TUI default `--addr 127.0.0.1:8787`, and two profiles' runners on
  one box collide (live's clobbered librem's on the release-test host). Lazy fix:
  default `--addr` from the profile config (install already records a per-profile
  port, #308) instead of the flag default. Better: start the runner on demand on a
  free port, hand the addr to the command, and stop it when the command exits —
  the runner is alive for the whole command (execs stream over minutes), not
  per-exec.
- **LXC guest names should key on the Buzz domain, not the install profile name.**
  Guest names are `<world-name>-<role>`, where world-name is the profile name
  typed at install (baked into the world-config). This is only cosmetic for other
  boxes — discovery is domain → CP URL → NIP-98 auth → world facts/coords, and
  `resolveGuestVmids` uses the world-config's name, so every box agrees — but it
  ties the guest names to what the installer happened to type. The global instance
  id IS the relay (Buzz) domain, dashed — the same key already used host-side for
  LV/dataset names and per-world tf roots. Re-key guest names off it. Pairs with
  the domain-re-point punt (deferred; Buzz keys on the domain anyway).
- **An onboarded (adopted) relay's data plane needs a freehold-namespaced path.**
  When freehold onboards a relay guest it did NOT create (the locked "existing
  relay as a service" path, not yet built), the container is shared with the
  user's own files, so a fixed guest path like `/srv/data/relay` could step on
  them. Use a freehold-prefixed path (e.g. `/srv/data/freehold/relay`) for the
  adopted case; freehold-created guests keep the plain `/srv/data/<tenant>`.
