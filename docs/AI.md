# AI — models, the agent runtime, and AI hardware

**Owner:** the AI department (`agents/ai/`). **Scope:** the LiteLLM gateway, provider and
model setup, AI hardware, the agents themselves, and the **runner flow** — how an LLM
touches real infrastructure without ever holding a secret.

## The idea

Every agent — the CPA, the four departments, and anything the CPA creates — is a pod on the
same Buzz `buzz-acp` harness, and every model call goes through one **LiteLLM gateway**.
AI owns the gateway directly; the runtime is what all agents share.

```
   agent pod (buzz-acp)  ──OpenAI-compatible──►  LiteLLM (k3s, NodePort)  ──►  provider (operator-chosen)
     │  identity · workspace · prompt                 │
     │  memory (relay, kind 30174)                    └─ Postgres (models, keys)
     └─ tool bridge ─► CP toolset (create_agent, …)  ·  department runners (exec)
```

## How it works

*   **The gateway.** LiteLLM and its Postgres run as Deployments in the `litellm` namespace,
    configured by env only. The provider key is operator-supplied and sealed in the CP's
    litellm store — never in argv or Terraform state; the build registers the aliases from
    that store over the gateway's admin API directly, and only the AI department's
    `litellm-api-admin` runner carries the key (for retargeting), injecting it per exec. At
    first provision the build's picker collects ONE provider (from a curated single-key
    table — fireworks_ai, openai, anthropic, gemini, groq, deepseek, mistral, together_ai,
    openrouter, xai) and a model id (the provider's default, editable); the choice rides
    the CP's litellm store (`provider` / `provider-prefix` / `provider-model`) and the
    build registers the default ALIAS set on the gateway, ALL pointing at it
    (`stageLitellmAliases`, straight from the store — terraform deploys the gateway but
    registers no model): `Code` (coding agents), `General` (the default for custom
    agents), `Freehold` (the core agents — the CPA + departments, pinned),
    `ExtraThinking`     (complex architecture / deep thinking). A pod's model resolves by
    class (`litellmModelFor`): core identities run the core alias; a custom agent runs
    its persisted choice, defaulting to `General` — and an agent's choice rides its
    registry row, so a rebuild re-applies the same alias. Retargeting the aliases or
    adding providers is the AI department's `litellm-api-admin` work, never a build side
    effect.
*   **Keys and spend.** Every agent rides its OWN gateway virtual key —
    `key_alias` = its pod name, `models` restricted to the alias set — minted
    by the create flow at first apply and persisted sealed in the CP's
    litellm store (`agentkey-<pod>`), so a rebuild re-seeds the same key
    instead of minting orphans; no pod holds the gateway master key. A
    removed agent's key is revoked by its stored token (the gateway's
    key_alias drifts stale after a rename; spend follows the token); a
    rename re-keys the record
    under the new pod name, so the same key — and its spend history —
    follows the identity. The gateway logs spend per key in its Postgres;
    per-agent spend is visible through the `litellm-api-admin` door
    (`GET /spend/keys`, `GET /key/info`).
*   **The pod.** A bare Pod in the `agents` namespace: its own Nostr identity (Secret), a
    workspace on the durable plane (`/srv/data/k8s-volumes/agent-home/<pod>`), a prompt
    mounted from a ConfigMap and re-read on every spawn, and a tool bridge fetched from the
    CP at boot.
*   **Prompts.** A shared orientation block (repo, "be loud", memory) + the agent's own
    prompt + its skills, composed in `agents/prompt.go`. Custom agents get a bare template.
*   **Who may talk to whom** is the inbound author gate fixed at deploy: the CPA answers
    anyone in the relay; departments answer the operator and core agents; custom agents
    answer their asker and the CPA.
*   **Memory** is relay-persisted and encrypted; each pod carries an operator-signed NIP-OA
    attestation scoped to memory writes. It survives pod re-applies and full rebuilds.
*   **Creation.** `create_agent` mints identity, joins the relay, applies the pod; the build
    reconciles the whole registry (CPA, then departments, then custom agents) every time.
*   **Editing.** `update_agent` rewrites a created agent's purpose (its system prompt re-renders
    from the registry row and lands on the next spawn), switches its litellm model, replaces its
    channel list, or renames it. A rename moves the durable identity dir, workspace, pod objects
    and registry row to the new name while keeping the pubkey — chat history, grants, and memory
    follow. Core identities (the CPA + departments) are repo-defined and refused; `manage_agent
    remove` retires the pod and drops the row (the durable workspace dir is kept).
*   **AI's grants:** `litellm-api-admin` (model registration, key minting) and
    `kube-api-litellmsa` (a `litellm`-namespace kube door, no Secrets). Machines such as a
    GPU box arrive through a door the CPA provisions on the fly.

## Scheduled jobs

Any agent can have a job: a prompt fired on a schedule. "@ai, review the latest local-AI
news daily and post a briefing here at 7am" is a conversation first — the agent confirms
the schedule and channel, then calls `create_job` — and a `jobs.json` row after.

*   **The scheduler lives in the agent-tools process** (the CP's own MCP server, beside the
    agent registry): a `jobs.json` store (0600, durable plane) and one tick loop. The
    console **folds the file read-only** for `/api/jobs` — the same split as the registry
    (agent-writable authoritative store; console read-only view). The console never writes
    jobs.
*   **The fire path is the ordinary mention path.** A due job is a kind-9 mention posted by
    the **console identity** into the job's channel, p-tagging the agent — the pod wakes
    exactly as it does for any member, and its in-channel reply IS the delivery. The
    console identity rides every pod's respond-to allowlist for this (see "Who may talk to
    whom"). Open channels work unconditionally; a private channel needs its owner to add
    the console identity — `create_job` resolves the channel as the console identity, so a
    channel it cannot see is refused at creation with that remedy.
*   **Ownership is the privacy boundary.** A job carries the asker's npub as its owner.
    `/api/jobs` serves an owner's own rows with the prompt and label; every other row is
    metadata only (owner npub, agent npub, channel, schedule, last run, created date) —
    **the operator included**. A **member role** (any relay community member, verified
    against the relay's kind-13534 list at login) may hold a console session that reaches
    exactly this route and sees only their own rows.
*   **Reliability invariants** (each guards a real failure mode, learned from OpenClaw's
    automations and Hermes' cron):
    *   *At-most-once*: `next_run_at` is advanced and persisted **before** dispatch, so a
        crash mid-fire skips a slot rather than double-firing.
    *   *No silent drops*: a missed slot (past the catch-up window — half the period,
        clamped 2m–2h) is recorded as a `missed` run with a reason; a skipped one (the
        agent still has an open run — the busy-pod protection) is recorded as `skipped`.
    *   *Liveness*: a fired run closes `ok` the moment the agent posts in the channel;
        after a 10-minute silence it closes `timeout` and the owner gets an in-channel
        failure note. A long-but-chatty run is never cut short.
    *   *A one-shot reminder that lands on a busy agent re-queues* (2 minutes) until the
        agent is free — a mini-queue, not a lost reminder.
*   **Schedules** are standard 5-field cron (or `@every` descriptors) with an explicit
    IANA timezone per job — the resolving agent asks the asker's clock first. One-shots
    are a unix `at` timestamp.

## Runners and secrets — how agents touch the world

The reason this architecture is safe to point at real infrastructure is that the
*reasoning* and the *reach* are different machines with different privileges. An LLM
decides what to do; it never holds the keys that let it do arbitrary damage, and every
action it causes is signed, authorized, and audited by machinery that cannot reason.

*   **Agent = brain, runner = dumb privileged hands.** The agent pod has no credentials
    for anything real. The **runner** owns the connection to a target (a host, a guest, an
    API) and executes the command the agent writes **verbatim** — one generic
    `exec(cmd, target, stream?)`, no semantic tools, streaming for long ops. A runner has
    no LLM, no router, no key vault, and never caches plaintext: credentials are injected
    per attempt as env vars and redacted in output. Each call is validated (signature
    against the runner's roster, audience, expiry, nonce) and audited.
*   **The agent never sees a secret — by construction.** Agents reference credentials by
    NAME only. A door's credential is sealed (encrypted) to the runner's own encryption
    key; the runner decrypts in memory, uses it for the exec, and forgets it. Plaintext
    never sits on disk and never enters agent context or chat. This is enforced by the
    tool surface itself: the grant/provision tools take no secret field, so a prompt
    injection or a confused agent has nothing to leak.
*   **The CP is a secret provisioner, not a vault.** It mints keypairs, encrypts to a
    runner's key, ships ciphertext, injects runner keys, and rotates. No master key; the
    CP's own state (`secrets.json`) holds ciphertext only. Nothing under the CP state dir
    opens a runner's blobs. Two deliberate exceptions, both sealed to the console identity:
    the DNS/LiteLLM creds (opened in memory at build) and the operator's Nostr signing key
    (it attests agent memory and signs console/relay owner actions) — a CP compromise
    yields operator impersonation, the same class as losing the operator box.
*   **Runner identity.** Each runner is a Nostr keypair (membership/signing) plus a separate
    encryption keypair (env-injected or a mounted secret, never committed). Agents connect
    with their OWN keypair; secrets are encrypted for the runner's key.
*   **Grants are coarse and runner-scoped.** The unit of grant is the runner: one runner
    per capability, named `<target>-<protocol>-<identity>` for what it can reach, never for
    who consumes it. A grant is a whitelist of agent pubkeys on that runner's private
    NIP-29 channel; the runner's live whitelist is the relay's own signed roster (kind
    39002), re-read fresh per call and fail-closed on relay outage. No relay fork, no
    custom kinds. Grant/revoke is channel membership (kinds 9000/9001).
*   **Readiness is the runner's own self-check** — 🟢 all checks pass / 🟡 some missing /
    🔴 none — uniform for runners, services, and departments.
*   **Departments hold the capability runners.** Each department's pod carries coords for
    its own doors (`FREEHOLD_RUNNER_*`) and a scoped `exec`/`list` bridge that routes by
    target and signs as the department's own key. The CPA and custom agents hold no runner
    coords and never see exec — a capability grant attaches to a department identity, never
    to a custom agent that would self-serve an ungoverned path.

    | Runner | Kind | Port | Roster |
    | --- | --- | --- | --- |
    | `pve-ssh-root` | ssh (root@host) | 8791 | network, compute, data |
    | `kube-api-root` | kubernetes (cluster-admin SA) | 8792 | compute |
    | `kube-api-caddysa` | kubernetes (`caddy` ns SA) | 8793 | network |
    | `kube-api-litellmsa` | kubernetes (`litellm` ns SA) | 8794 | ai |
    | `litellm-api-admin` | litellm (master + provider key) | 8795 | ai |
    | `dnsmasq-local-root` | local (CP guest) | 8796 | network |
    | `cp-local-root` | local (CP guest) | 8797 | data |
    | `cloudflare-api-<zone>` | api, one per stored DNS zone | from 8798 | network |
    | dynamic (`provision_runner`) | ssh / any api kind (verify arm as data) / kube slots (`kube-api-<slot>`) / local | from 8800 | per grant |

*   **Grants on the fly.** The CPA can provision capability mid-conversation:
    `provision_runner` stages a NEW runner (keypair, sealed credential, private audit
    channel, a systemd unit on the CP guest), records it as a dynamic capability
    (re-staged on every build — adopt-only, except a kube slot's record, whose slot is
    re-created and whose token is re-sealed), grants the named agents onto its roster live,
    and re-applies the grantees' pods with the new coords. It can also enroll a runner
    RESIDENT on a target the agent itself provisioned (`hosted=self`: the guest holds the
    runner-client, `runner enroll` mints the identity ON the guest, pods dial its LAN
    address; the operator confirms the enrollment on the door page — the barrier that
    stops a compromised CPA sealing to its own key). It never widens an existing runner.
    **Credentials never ride chat:** an ssh door mints its own keypair (the operator
    installs the returned public key once); an api door provisions EMPTY and the agent DMs
    the operator the door's console page, whose kind-aware form seals the credential and
    restarts the door. The api door's **verify arm is data, not code**: the requesting
    agent names the probe (`"<METHOD> <path> [auth] [want] [insecure]"`, e.g. `GET
    /user/tokens/verify bearer`), it ships in the door's package (with its optional
    `probe_body`), and the runner composes its self-check curl
    from it — a new service kind is a probe, never a rebuild. A **kube slot**
    (`kind=kubernetes`, named `kube-api-<slot>`) is the exception that still holds the
    rule: the slot (a namespace, its ns-admin ServiceAccount + Role, an optional quota,
    the token Secret) is carved by COMPUTE through `kube-api-root` — the department's
    audited leg, never the CPA's — and the CP then reads the SA token from the slot and
    seals it CP-side, so the door is live with no console fill; the record re-creates the
    slot and re-seals the token on every rebuild, the same role `doors.tf` plays for the
    static kube doors. Confirmation is governed by
    `agent_grants` on the CP state —
    `confirm` (default: in-thread yes, or a DM), `auto`, or `off` (the server-side kill
    switch, `freehold-console grants-mode`) — but the discipline itself is the granting
    skill's, since the server cannot see Buzz threads. `revoke_runner` is the mirror:
    remove grantees (roster AND the capability record, or the next build re-grants) or
    retire the door outright (channel folded but kept for audit, credential erased and
    re-opened to prove it, unit stopped, record dropped), every leg reporting
    `[verified]`/`[UNVERIFIED]`. A retired name is refused to the agent in both directions
    until an operator re-enables it from the console.
*   **Setting a runner up and granting onto it** (a new capability door, operator- or
    agent-provisioned alike): the CP generates the runner's identity, seals the credential
    TO the runner's key, ships a package with ciphertext + the runner's private key
    (recording only pubkeys + ciphertext — no plaintext, no master key), creates the
    runner's private NIP-29 channel in the relay and members the runner. A grant **adds the
    agent to that channel**; the runner's whitelist is its own relay-signed roster, read per
    call.

    ```mermaid
    sequenceDiagram
        participant OP as operator
        participant CP as control plane
        participant RUN as "runner box (my-runner)"
        participant BR as "box runner (proxmox-box)"
        participant REL as buzz relay
        participant AG as agent

        OP->>CP: provision my-runner --kind ssh<br/>--address root@host (credential pasted)
        CP->>CP: generate runner identity<br/>(Nostr + encryption keypairs)
        CP->>CP: seal credential to the runner's<br/>encryption pubkey — ciphertext only
        CP->>RUN: write the package onto the runner's box<br/>(identity.json + secrets.json — ciphertext, targets)
        CP->>CP: record pubkeys + ciphertext in state.json<br/>(no plaintext, no private keys)
        CP->>REL: create the runner's private channel (9007)<br/>h = sha256(runner pk)[0:16] (uuid form) — owner = the console
        CP->>REL: member the runner itself (9000 put-user)<br/>— the channel layer
        CP->>BR: relay-member add — the runner's pubkey<br/>(community layer)
        BR->>REL: buzz-admin add-member (kind 13534) —<br/>without it, roster reads 403
        CP->>REL: publish the runner profile<br/>(kind-9 fh-profile message)
        RUN->>RUN: runner serve --relay-url<br/>(+ --relay-auth-url / --allow-remote as needed;<br/>whitelist = its own channel roster)
        CP->>RUN: readiness probe (signed MCP)
        RUN-->>CP: green
        OP->>CP: grant my-runner agent-pubkey
        alt relay configured (the channel flow)
            CP->>REL: add the agent to the channel (9000 put-user)
            REL->>REL: re-mint the roster (39002, relay-signed)
            RUN->>REL: read own roster — agent is a member (per call)
        else no relay (shipped-package grants)
            CP->>RUN: re-ship the package with the new grant
            RUN->>RUN: re-reads grants from the package (per call)
        end
        AG->>RUN: tools/call (signed) — first exec
        RUN-->>AG: result (credential injected, output redacted)
    ```

    Without a relay the grant is a package re-ship instead of a channel write — both land
    without a runner restart, and revoke is the inverse (`remove-user` / re-ship minus the
    grant). Membership is TWO layers: the channel (9000) grants the whitelist, but the
    runner also needs COMMUNITY membership (relay-member → `buzz-admin`, kind 13534) before
    ANY of its relay reads work — non-members get `403 relay_membership_required` and the
    runner fails closed. The community add is driven through the box's provisioning runner
    (the relay-admin runner that holds the credential into the relay LXC), not by the new
    runner itself. What lands on disk proves the model:
    `/srv/data/cp/control-plane/runner/<name>/identity.json` (the runner's injected private
    keys, 0600), `…/secrets.json` (the credential as sealed ciphertext only), and the CP's
    `state.json` (pubkeys + ciphertext only — grep for the API key and for
    `nostr_secret`/`enc_secret`: zero matches).
*   **The runner mechanism itself** (call validation, connectors, the Rust runner) is in
    `docs/ARCHITECTURE.md` ("Runners"); the open gaps in the mechanics (replay window,
    rotate reach, revocation) are in `docs/FREEHOLD.md`.

## Known gaps

*   A gateway whose Postgres was wiped (a full teardown of the k3s state)
    invalidates every minted agent key: the pods 401 until the CP store's
    `agentkey-<pod>` records are cleared — the next reconcile then mints
    fresh and the seed rotates every pod's Secret onto the new key, no
    hand-deleted Secrets.
*   One model — the aliases all point at the operator's first-build provider
    choice, so every alias routes to the same underlying model today; adding providers or
    per-alias variety is AI's `litellm-api-admin` work. Aliases are ensured only for the
    default set; managing an agent's choice beyond the registry row is not built.
*   AI's prompt says the provider key rides each exec; the bridge doesn't inject it.
*   The sprig image is a moving tag (no digest pin).
*   Memory attestation has no expiry; upstream buzz doesn't verify engram authorship.
*   Prompt edits in the CP's durable copy don't survive a rebuild (re-seeded from embedded bytes).
*   The respond-to allowlist is fixed at deploy (re-applies deliver changes). The console
    identity rides every allowlist — it is the scheduled-jobs fire identity.
*   Stale agents survive a department rename on rebuild.
*   Job **prompts sit in plaintext** in the agent-tools' `jobs.json` (0600, durable plane)
    — the scheduler must read them to fire. The privacy boundary is the console API's
    owner redaction, not encryption.
*   Job fires and **failure notes are best-effort**: a relay outage during a
    fire records the run as `error` — a failed post provably never went out,
    so a one-shot re-queues (2 minutes) and pauses after 5 consecutive
    failures; a failure note the channel refuses is logged, not retried.
*   **Console jobs management is read-only**: pause/delete run through agents
    (`pause_job`/`delete_job`), since jobs.json is the agent-tools process's own store.
*   Agents read the repo but can't write it.
*   AI hardware, local AI, optimization dashboards, and eval harnesses are prompt claims
    with no tooling. No resource baseline per agent exists.

## Future

*   Per-agent LiteLLM **budgets** (the per-agent keys and spend tracking are
    live; a `max_budget` cap and rotation policy are not); multi-model
    management (the default alias set exists; variety behind the aliases does not).
*   **Local AI hosting:** a GPU box (RTX 3090, DGX Spark) as a LiteLLM upstream, provisioned
    and tuned by AI through a door, with telemetry.
*   **Agent workspaces + git/GitHub:** a workspace LXC per agent, commits verified durable in
    Buzz's git, GitHub pushes via a grant; GitHub and web-search runners as their own
    capabilities.
*   Sleep/wake for ad-hoc agents; secondary-relay onboarding. (The scheduled repo
    re-check is built — it is now just a `create_job` the AI department schedules for
    itself.)

## Where the code is

`control-plane/api/agent/` (pod, identity, tools), `control-plane/api/cpbuild/` (LiteLLM
seeding, create/reconcile, department runners, `terraform/litellm.tf`), `control-plane/api/agenttools/`
(registry, scheduled jobs), `contract/nipoa/`, `contract/client/` (the signed MCP client), `secret-management/`
(the provisioner), `control-plane/runner/` (Rust), `agents/`.
