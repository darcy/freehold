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

### Chunk 5 acceptance

*   CPA-routed request → named peer provisions an LXC workspace → agent
    commits a real change → change is verified durable in Buzz's git and/or
    pushed to a real GitHub repo.

*   The workspace/git-runner credential surface is either adopted (with a
    written carve-out from the generic `exec()` model) or explicitly
    rejected in favor of it.
