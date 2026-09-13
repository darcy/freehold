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

## D — Migrations: consolidate migrated worlds under epoch names

The two migrations now carry epoch names (`1799900000`, `1799910000`) instead of
the old `001-…`/`002-…`. On a world that already ran them under the old names,
they re-run under the new names once — safe because both are idempotent +
verify-gated (registry-loadable; import-console additive). A fresh teardown
world (the PR's verification path) is unaffected.
**Status:** informational; no action unless a long-lived world complains.
