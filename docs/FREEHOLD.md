# freehold — the core platform: gaps, plans, and product direction

Everything that isn't a department: the control plane, runners, grants and secrets, the
relay, the console/CLI/TUI, install/update/migrations, skills, and the product itself. The
**current architecture** is `docs/ARCHITECTURE.md`; this is its forward-looking half.
Departments have the same shape in `docs/AI.md`, `NETWORK.md`, `DATA.md`, `COMPUTE.md`;
the ordering across all of them is `docs/ROADMAP.md`.

## Known gaps

**Runners, grants, secrets**
*   Revocation is a feed-cut: a credential someone already holds still opens, and a
    self-hosted runner keeps running until the box-side stop.
*   Rotate doesn't reach a live runner until it restarts (except the console's fill flow).
*   A live-issued grant didn't reach a running door's relay roster in one live test —
    the relay never re-signed the 39002 roster on the put-user. (The console's
    `/api/revoke-grant` does remove a relay membership; the put-user → re-sign chain is
    the part that flaked.)
*   Replay within a call's 60 s window; `timeout_s` leaves orphans; abandoned streaming
    sessions are never reaped; the SSH and API connectors have a handful of edge cases;
    the state store is single-process.
*   The co-located runner reads package grants, not the relay roster; the runner's audit
    is local-spool only (stock buzz rejects kind 48001).
*   On-the-fly grants are new-door-only and confirm-mode; the confirmation discipline is a
    prompt rule the server can't see. Doors are intent-and-audit boundaries, not hard
    containment on a shared host.
*   The department check-in hook ("back this up?", "reachable outside?") has no trigger —
    the provision path it hangs on is unbuilt.

**Migrations and update**
*   No reverse migrations or downgrade verb.
*   The console executor's migration window can race a `create_agent` on the old serve.
*   Unshipped migration scripts look like a converged world (only the report line shows it).
*   Agent-tools audience drift is detected, not repaired.

**The relay (Buzz)**
*   After a redeploy, private-channel publishes 403 until channel rosters are reconciled,
    and the console's relay membership goes stale — the build should reconcile and re-member.
*   Non-department relay runners need a manual community-membership step.
*   Exec receipts as channel messages are designed, not built; no relay-down drill;
    no relay-contract verification skill.

**Console, CLI, TUI**
*   The console isn't a systemd unit; the runner's crash isn't restarted.
*   All profiles' local runners default to `127.0.0.1:8787` and collide.
*   Go-console port gaps: no `rebuild` verb, a few missing CLI verbs, no request-body cap,
    dropped env-var flag bindings; secrets in `/api/provision` live briefly in memory.

**Verification and CI**
*   No bot-review injection pre-vet; no single real-relay acceptance run; two live legs for
    on-the-fly grants (revoke, empty ssh/unifi doors) await a release test run.

## Plans

*   **Skill framework growth:** `target: lxc|pod|either` as a hint (never authority),
    postcondition-gated readiness, budgets as escalation — skills identical on LXC and k8s.
*   **The department check-in hook** and the **provision path** it needs.
*   **A guest inventory** in the console and TUI, tagged core / adopted / foreign.
*   **Security hardening:** target-scoped grants, approval gates, opt-in on-demand
    decryption, remote revocation, dynamic secrets.
*   Vultr and Backblaze carrying real onboarded services through the CPA loop.
*   DM mirroring between the CP and the relay while the relay is down.

## UI/UX

*   The minimal console is readiness + agent chat (Buzz) + services + skill install; the
    console is admin/ops only — **no chat surface rebuilds**.
*   Later: a unified resource/activity view, deep monitoring, AI-box telemetry, TUI tabs for
    Certs and Guests, a mobile client, a Proxmox-ISO install guide.

## Product direction

Pre-installed box; a local CP that provisions other boxes; base presets (personal / family /
business); a skill marketplace; multi-tenant (relay-as-scope makes it natural); open-core
cloud offering. The release flow stays as in `AGENTS.md`.
