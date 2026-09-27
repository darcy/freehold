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

## Grant-giving flow (provision_runner)

Grant-giving runs through `provision_runner` (the freehold CP toolset): it
stands up a NEW capability runner — named, keyed, channel-audited — and grants
the named agents onto its roster live. The code enforces the mechanics below;
you enforce the judgment.

**The confirmation discipline.** A grant is the operator's call. When the
operator's ask is in a thread with you — they told YOU what to make possible —
grant when you have everything you need. When the request arrived second-hand
(a department relaying an ask you did not witness), DM the operator, state
exactly what will be provisioned and granted to whom, and wait for their yes
before calling the tool. When unsure whether you witnessed the ask, ask. The
operator can set `agent_grants: off` on the CP, which disables this flow
entirely; if your call is refused on those grounds, report it plainly.

**Interview before you grant.** Collect everything the tool needs before
calling it — going back to the operator mid-provision is worse than one round
of questions up front:

- **For a guest that holds the runner-client (a resident runner):** Compute
  installed the runner-client on the box (`create-lxc` does this) and ran
  `runner enroll` there — the box minted its OWN identity and printed two
  pubkeys. Enroll it: `provision_runner(name, kind=local, hosted=self,
  host=<the pinned name>, address=<user>@<host>, pubkey=<nostr>,
  enc_pubkey=<enc>, grant_to=[...])`. The custody property is the point: the
  private keys were minted ON the target and never transited anything — the
  control plane records the pubkeys, seals credentials TO them, and starts
  nothing; the box runs its own unit (as `lxcadmin`, never root). The exec
  is native on the guest — no ssh hop, no key on any other box. A
  re-provision must present the SAME pubkeys; a mismatch is a different
  process claiming the name and is refused. The Network check-in applies —
  a resident runner is a new LAN listener the pods dial directly.
- For a box on the network (ssh): address as `user@host[:port]`, and whether
  the account can do the job (least privilege where a scoped account exists).
  The runner mints its OWN keypair — never take the operator's password or
  private key. The tool returns the public line; the operator installs it on
  the target (offer the one-liner), and the runner's self-check goes 🟢 once
  it is in. Until then say the door is waiting, not that it works.
- For a service API (e.g. UniFi): figure out WITH the operator what the
  surface is — does it have an API (its URL, and an account with the right
  role), or does it need an account created first? **Never take the
  credential in chat** — provision the door EMPTY and DM the operator the
  door page link from the tool's report; they fill the credential in the
  console web UI (the console seals it to the door's key and restarts the
  door). You reference the credential by env name only, after the fact. The
  TOOL enforces this: provision_runner takes no credential field at all.
- Know the SHAPE of what the operator filled — it is in the tool's
  description per kind. Unifi doors: the exec env carries UNIFI_API_ADMIN as
  a JSON object with the keys username and password; authenticate with
  `POST <controller>/api/auth/login` (body: the parsed username/password),
  take the session token from the response, and call the API with it. Do NOT
  try the value as an X-API-KEY or similar header — an API-key door would be
  named that way in the description; this one is a login pair.
- Name it `<target>-<protocol>-<identity>` (`rtx3090-ssh-root`,
  `unifi-api-admin`, `freehold-dev-local-lxcadmin`) — what it reaches, how,
  at what level. Never for the consumer.
- `grant_to` is the agent (or agents) that will DO the work — the department
  that owns the capability class, or the custom agent that owns the service.
  You hold no exec yourself; never grant a capability to you.

**Only runners you provisioned.** The tool stages NEW capability runners. You
cannot widen an existing runner's roster (that stays operator-scoped via the
console) — when two agents need the identical capability, provision the
capability's runner once for both, or a fresh identical runner for the second
use; never ask the operator to widen one.

**After granting:** the grantees' pods re-apply automatically (the exec surface
picks the new coords up); an EMPTY door stays 🟡 until the operator fills its
credential via the door page link — for a CP-guest door the console seals +
restarts it; for a SELF-HOSTED one the operator first CONFIRMS the enrollment
on the page (verifying the presented pubkeys against the guest's own
`runner enroll` output — Compute's audited report in the thread; this confirm
is the technical barrier that binds the fill to the key the guest actually
holds, so a compromised provisioning agent cannot seal the credential to its
own key), then fills: the console seals to the runner's own key and returns
the package JSON, and the GRANTEE carries it over — it holds exec on that
guest through this very door — writing `secrets.json` beside the runner's
identity.json and restarting the unit there. The ciphertext transits the
grantee's context: that is the system's normal trust level (only the
runner's key opens it), but on a RESIDENT runner the grantee could also read
the runner's own key (it is on a box the grantee holds sudo on) — nothing
technically stops it, so this is a discipline, not a containment: the agent
uses the sealed credential through the door and does not open the box's
identity file. Say what is actually live after verifying the door's own
self-check / an exec probe. A direct-credential mode (the operator handing
the credential to this agent for freehold to seal) is a possible future
`agent_grants` mode; it is not built, and until it is, chat is never the
credential path.

**Revoking a self-hosted door is a feed-cut, not a stop.** The CP holds no
identity and starts no unit for it, so roster removal + revoke cut the
grants and the coords — but the guest's unit keeps running with its identity
and sealed files intact until the box-side action lands (stop the unit,
remove the state dir — or destroy the guest, which is what `create-lxc`'s
teardown does). Say so plainly when revoking a resident door; a revoked door
that is still up is a known state, not a silent one.

## State of this skill

The flow above is live (`provision_runner`, confirm-mode by default; the
operator's `agent_grants` switch is the kill switch). When the wiring changes,
these guardrails do not.

