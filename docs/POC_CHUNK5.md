# Chunk 5 — Detailed Build Plan

Status: current chunk. The deliverables below are the current-scope copy from
`docs/POC.md`; this file is where the build plan and acceptance checkboxes live.

## Chunk 5's own deliverables (from `docs/POC.md`)

*   A named peer provisions the workspace LXC on request, routed through CPA
    (CPA resolves which peer, gates cost/irreversibility, delegates; the agent
    never learns which peer it landed on).

*   A commit made from the workspace is verified durable in Buzz's
    relay-hosted git — not just present on the disposable workspace LXC.

*   An agent pushes a commit to a real GitHub repo via a GitHub grant.

*   Minimal skills, scoped to only what this needs (clone/edit/commit/push
    reliably) — the fuller skill-schema/readiness/verify-harness design is
    pulled in only as later chunks need it.

*   The first named skills are tailscale + pihole — deliberately *not*
    LiteLLM, which rides the k8s path (the `terraform/` plans land in
    Chunk 6).

*   The workspace/git credential surface (credential-helper vs. generic
    `exec()`) is decided explicitly here, not assumed.

*   Proof point: an agent deploys a service to an LXC using what it
    committed.

*   **Department capability runners (built).** The grant unit is the runner: one runner
    per capability, named `<target>-<protocol>-<identity>` — `pve-ssh-root` (root@<host>
    SSH, shared by Network/Compute/Data: one capability, one audit stream), SA-token kube
    doors (`kube-api-root`, `kube-api-caddysa`, `kube-api-litellmsa`), `litellm-api-admin`
    (the gateway's master + provider keys), `cloudflare-api-<zone>` per DNS zone, and
    `dnsmasq-local-root` local on the CP guest. Each department's pod reaches its runners
    by a scoped `exec`/`list` signing as the department's own key; the CPA and custom
    agents hold no coords and never see exec. Data uses its grant to verify every
    LXC/kube volume lands on a backed-up mount; the granting rules + guardrails are the
    CPA's first skill (`agents/freehold/skills/granting.md`).

### Chunk 5 acceptance

*   CPA-routed request → named peer provisions an LXC workspace → agent
    commits a real change → change is verified durable in Buzz's git and/or
    pushed to a real GitHub repo.

*   The workspace/git-runner credential surface is either adopted (with a
    written carve-out from the generic `exec()` model) or explicitly
    rejected in favor of it.
