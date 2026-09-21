# Follow-ups (tracked, not in the current PR)

These are deliberately deferred pieces called out during the terraform/migrations
rework. They are current open work, not history — see AGENTS.md "Known gaps" for
the standing limitations list.

## A — Relay Buzz stack (not Terraform today)

The relay LXC is adopted by the substrate module, but the Buzz stack *inside* it
(postgres, redis, minio, git + the relay itself) is deployed by the Go driver
`platform/services/relay/buzz/deploy_relay.go` running `docker compose` into the
LXC. It is the largest thing NOT defined as infrastructure.

To make it declarative: add a `relay.tf` (a `docker` provider) that models the
compose services as resources, or fold `DeployRelay` into a terraform
`local-exec`. Either way the relay's internal services become a deterministic
`.tf` definition instead of a Go/compose-only deploy. Worth doing next so the
"every service is a static file" property holds for the relay too.
**Status:** deferred; needs a decision (docker-provider vs. fold-DeployRelay).

## B — Substrate via a real proxmox/bpg provider (drop `null_resource` shell)

The substrate (durable volume plane + cp/relay/k3s LXCs + k3s bring-up) stays
exec-first (`null_resource` + `plane.sh`/`lxc.sh`/`k3s-bringup.sh`) because no
available provider cleanly reproduces the home-lab PVE path. Why: **bpg 0.66+**
rewrote away the **tarball-create** path the live substrate was born from (a
`local:vztmpl/*.tar.zst` template), leaving only clone-based. PVE 9.2.2 is that
host.

Escape routes the roadmap already names:
1. **Clone-based (recommended):** create a gold template once, then the provider
   clones it for cp/relay/k3s — a genuinely good fit for declarative Terraform.
2. `telmate/proxmox` — older, battle-tested, still does tarball-create.
3. Pin a pre-0.66 bpg release.

Honest caveat: the substrate also needs detection+allocation (free vmid,
thin-pool/ZFS present, `backup=1` mounts), so the bootstrap Go/shell shrinks to
"make the gold template + let the provider clone," not vanishing entirely.
**Status:** named follow-up; not in this PR.

## C — Caddy cert/DNS issuance stays the CP's in-process overlay

`caddy.tf` defines the static shape (Deployment, ConfigMap, PVC, Service). The
TLS **certs themselves** are event-driven (DNS-01 issue/resume through `lego`,
install into the caddy-data PVC, restart) and remain the CP's owned, on-demand
overlay (`worldCert`), not a terraform resource. The cert-install logic is
Go/in-process (resumable, DNS credential handling) rather than a `tf`-placed
script. If a script-file form is wanted later, the script can be shipped via the
migrations/overlay mechanism and still executed by the CP on demand.
**Status:** aligns with the "events are migrations/overlay, not tf" rule.

## E — Go console: residual port gaps from the Rust console

The Rust console (deleted in 0.5.24) carried a few CLI/behavior surfaces the Go
console never picked up. None is on a live path; all are reachable via the
web/API or were intentionally superseded.

- **No `freehold-console rebuild` verb.** The relay-fold primitives
  (`relay.QueryRunnerMetas` + `state.StateStore.RebuildFrom`) exist and are
  covered by the Go acceptance gate, but nothing in the runtime calls
  `RebuildFrom` — a disposable-CP field rebuild has no CLI entrypoint.
- **CLI verbs the Go console lacks:** `rotate-secret`, `revoke-grant`, `list`
  (all web/API: `/api/rotate`, `/api/revoke-grant`, `/api/overview`), and
  `dns rm`/`dns list`/`dns sync` (web `DELETE`/`GET /api/dns`; `dns add`+`dns
  apex` exist).
- **Agent-presence reads don't send the community host.** `probeAgentPresence`
  calls `relay.QueryEvents(snap.RelayURL, …)`; on a LAN-IP dial the community
  host isn't presented (the dual-URL `QueryEventsAuth` form exists but isn't
  used here).
- **No 256 KiB request-body cap** on the Go console (the Rust `web.rs` had one).
- **`serve` flag defaults dropped env-var bindings** (`FREEHOLD_CP_ADDR`,
  `FREEHOLD_RELAY_URL`, …) — the Go flags are literals.

**Status:** deferred; not on any live path.

## D — Migrations: consolidate migrated worlds under epoch names

The two migrations now carry epoch names (`1799900000`, `1799910000`) instead of
the old `001-…`/`002-…`. On a world that already ran them under the old names,
they re-run under the new names once — safe because both are idempotent
(registry-loadable; import-console additive). A fresh teardown world is
unaffected.
**Status:** informational; no action unless a long-lived world complains.

## E — Release workflow untested until a real tag

`.github/workflows/release.yml` (build the sibling set, package
`migrations.tar.gz`, `checksums.txt`, attach to a draft release) can only run on
a `v*` tag push, so the `mise install …` + `mise exec just -- just build` path
has never executed. Cut a throwaway `vX.Y.Z-rc.N` tag **before** the first real
release to prove the assets + checksums upload and a box can pull + verify them.
**Status:** must do once before the first real release.
