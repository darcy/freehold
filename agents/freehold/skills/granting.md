# The granting skill — how capabilities are given away

This is freehold's first skill. It binds whoever grants capability in this
system — the operator today, the freehold agent (CPA) as grant-giving unlocks
— and it binds the rule set for EVERY future service, runner, and agent. It is
read-on-boot material: re-check it against the repo as the system evolves.

## The unit of grant is the RUNNER

A capability is one runner: a process with its own identity, one credential,
and its own relay channel (the audit stream). A grant is a roster membership —
the agent's pubkey on the runner's relay-signed roster, re-read per call,
fail-closed on relay outage. From this, everything else follows:

- **One runner per capability.** Never widen a runner's package or target to
  admit a second use — a new capability is a NEW runner. Widening bundles
  revocations and muddies the audit.
- **Runners are named `<target>-<protocol>-<identity>`** — what it reaches
  (`pve`, `kube`, `cloudflare`, `litellm`, `dnsmasq`), how (`ssh`, `api`,
  `local`), and at what credential level (`root`, `admin`, `<name>sa`,
  `<domain>` for per-zone doors). Runners are NEVER named for the consumer:
  `pve-ssh-root`, not `network-pve`. When two agents need the identical
  capability, they share one runner (two pubkeys, one roster, one audit
  stream) — `pve-ssh-root` serves network, compute, and data.
- **Least privilege on the identity.** Where a scoped credential exists, the
  door carries it, not root: namespace-scoped ServiceAccounts for kube
  surfaces (`kube-api-caddysa` over a cluster-admin token for a caddy edit),
  API tokens for external services. Root doors are for root-on-a-box work
  (`pve-ssh-root`), where root IS the job.
- **Every grant stays revocable and auditable.** Revoke = remove the roster
  entry — it lands live, no restart. The audit is the runner's channel. A
  grant that could not be revoked by roster removal alone must not be made.
- **Know what a door is worth.** Doors are intent + audit boundaries, not hard
  containment: a local door is root on the runner's own host, and a root door
  reaches every guest it can `pct exec` into. Grant the SCOPED identity where
  one exists (a namespace-scoped ServiceAccount over a cluster-admin token for
  a single-namespace job); grant root-on-a-box only where root IS the job —
  and say so plainly when the door is as wide as it is.

## Who may hold what

- **Department-owned capability classes never attach to a custom agent.**
  Exposure → Network; backup/DR → Data; compute → Compute; models, providers,
  AI hardware → AI. A custom agent that self-serves a department-owned
  capability is a containment failure even when a grant would technically
  allow it. The department's prompt is the first line of defense; the grant
  model is the second.
- **Service lifecycle is not a department.** Whichever agent created a service
  owns its install/config/operation — and holds the grants FOR THAT SERVICE
  (a dedicated runner per service by default, named for the service's target).
- **The granting agent never sees plaintext.** Credentials are sealed to the
  runner's key by the control plane's provisioner; agents reference secrets by
  name only. A grant flow that would put plaintext into an agent's context is
  designed wrong.
- **New service = the check-in questions fire.** Data: "should this be backed
  up?" Network: "should this be reachable outside the network?" — the grant
  flow raises them; "no" is a valid, final answer. A silent gap is a failure.

## Honest status

Never claim a grant that is not live. Verify against the runner's own
self-check (🟢 all checks / 🟡 some missing / 🔴 none) and the roster, and
report what you actually verified. When access you need is missing, say so
loudly — to freehold and the operator — instead of routing around the audited
surfaces.

## State of this skill

Grant-giving is operator-scoped today (`grant_agent` refuses agent callers —
server-enforced). This skill is the rule set that binding captures: when
grants unlock for the CPA, they unlock UNDER these rules — the wiring changes,
the guardrails do not.
