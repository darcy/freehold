# Plan: Unified capability grants — LiteLLM MCP (option 2) + door hardening

## Decisions locked

- **Option 2 — one MCP key per agent, grants live on the key.** The CP mints a
  dedicated MCP-only LiteLLM key per agent (`mcptool-<pod>`, no model access),
  whose `object_permission` is the union of the agent's granted MCP
  capabilities. Grant = CP updates the key. Revoke = CP updates or deletes it.
  The key rides the pod's bridge config like any other capability credential.
  This is a convenience and consistency win (same mint/seal/revoke flow as
  every other capability), not extra security: the boundary is what the key
  can reach, and a held key is a bearer credential.
- **LiteLLM is the MCP broker.** The door/exec plane stays out of LiteLLM.
- **The root-exec door fix lands first** as its own PR (see "Door hardening").
  It was a blocker for option 3 and a standing gap regardless of this plan.

## Non-goals

- Do **not** route runner/exec through LiteLLM. The runner stays the dumb
  privileged hands; the door remains the out-of-band repair path.
- Do **not** put root/infra credentials into LiteLLM.
- Do **not** rebuild the chat/UI surface; Buzz remains the chat.

## Current state (relevant)

- `control-plane/api/cpbuild/terraform/litellm.tf` runs LiteLLM with
  `STORE_MODEL_IN_DB=True`; no MCP config, no config.yaml, image `main-stable`.
- `ensureAgentLitellmKey` (cpbuild/departments.go) mints per-agent model keys
  and persists them sealed in the CP litellm store.
- The pod bridge (`api/cmd/freehold-agent-tools/mcp.go`) spawns buzz-dev-mcp and
  proxies freehold tools + runner exec. Its config rides a conf FILE
  (`/tmp/freehold-agent-tools.conf`), not env — the harness env-clears before
  spawning MCP servers (agent/agent.go), so env-based keys never reach it.
- `stageDepartmentRunners` + `startCapabilityRunner` stage door units on the
  CP guest; ssh/api doors now run as a per-door unprivileged user with a
  systemd sandbox (landed in the door-hardening PR); `local` doors stay root.
- `provision_runner` kube slots carve a namespace whose Role is full within
  the namespace (including Secrets); `mcp` is not a reserved namespace name.
- `secret-management` owns the seal/rotate paths used by both cpbuild and the
  console.

## Target architecture

```
agent pod ── bridge ── MCP key (mcptool-<pod>) ──► LiteLLM /mcp  (broker)
                                                      │
      grant/revoke = CP writes key object_permission  ├─► google-sheets-a-ro (SA-A-RO, Viewer on A)
                                                      ├─► google-sheets-a-rw (SA-A-RW, Editor on A)
                                                      └─► google-sheets-b-ro (SA-B-RO, Viewer on B)

runner doors (PVE, kube, host repair) stay exec-only, roster-granted  [unchanged]
```

- LiteLLM routes and enforces per-key server/tool access; it holds NO Google
  keys. Each MCP service holds its own least-privilege service account.
- Google ACL is the hard bound: one SA per (sheet, mode), shared only on that
  sheet as Viewer or Editor, Sheets-only scope (no Drive unless list/create
  is required).
- The CP is the single grant authority: one registry, one verb pair
  (`grant_capability` / `revoke_capability`), two enforcement backends
  (relay roster for runner doors, LiteLLM key permissions for MCP).

## Security posture (review findings folded in)

1. **Default-deny MCP.** LiteLLM's default is open when no entity lists
   servers. Every MCP server joins an access group; `allow_all_keys` is
   forbidden; `general_settings.require_key_mcp_access_defined: true` is set;
   the build's reconcile sweep sets `no-mcp-servers` on EVERY LiteLLM key
   without an explicit list — agent keys, UI-created keys, team keys — not
   just the keys the CP mints. **Proven in Phase 0** (a team-less, list-less
   key sees `"tools":[]`).
2. **The LiteLLM master key is a second grant authority.** Whoever holds it
   (the AI department's `litellm-api-admin` door, a UI admin) can rewrite
   key permissions and register servers. Mitigations: every build/reconcile
   OVERWRITES key permissions from CP state and reports any drift it found;
   MCP server registration rides the build like model aliases. Phase 0
   checks whether a scoped admin role can be barred from editing
   `object_permission`; if not, the drift-overwrite + report is the control.
3. **CP-derived endpoints.** `provision_mcp` takes NO caller-supplied URL:
   the CP derives `http://<name>.mcp.svc.cluster.local:<port>` from the
   capability record (the kube-slot rule: an agent-stated address would hand
   the broker token to a server the agent controls). `pinned_tools` freezes
   the tool catalog at registration.
4. **Broker token, operator-free.** The CP generates the per-service broker
   token, seals it into the slot runner's package itself via
   `provisioner.AddSecret`, and sets the LiteLLM side (it holds the master
   key). No human ever handles the token.
5. **SA key delivery needs a form, not a shell.** `add-secret --secret-env`
   on the CP guest would put a multi-line JSON key in a shell (history +
   process list). Console work item: a web fill form for a NAMED secret on a
   slot runner — the same door-page pattern credential fills use today.
6. **The `mcp` slot is near node-root.** A full-namespace Role can schedule
   privileged pods → root on the k3s node. The carve labels the namespace
   `pod-security.kubernetes.io/enforce=restricted` (ideally every slot
   carve). `kube-api-mcp` is granted to the AI department, never a custom
   agent.
7. **Secrets at rest.** k8s Secrets are plaintext on the k3s guest's disk
   unless k3s runs with `--secrets-encryption`. Enable it at carve time or
   record the gap in docs/AI.md — decide in Phase 4.
8. **Revocation latency.** measured in Phase 0: a revoke lands in **~20 ms** — effectively instant.
9. **Logs are not the security audit.** LiteLLM logs may retain tool
   request/response bodies (sheet contents) and are readable/editable by the
   master-key holder. Content logging for MCP is disabled if LiteLLM
   supports it, else accepted and documented. The authoritative grant record
   is CP state; the authoritative exec audit for the runner plane is the
   runner channel (unchanged).
10. **Sheet contents are untrusted input.** Pinned tools stop tool-poisoning,
    not poisoned cells. Don't grant one agent sheet-write AND powerful
    runner doors unless that combination is intended (the granting skill's
    containment rule applies).
11. **Tool-name shadowing.** The bridge routes its own names first and DROPS
    any upstream tool whose name collides with `exec`, `list`, `gate_fetch`,
    or a freehold tool name.
12. **Department containment still applies.** No MCP server may duplicate a
    department-owned capability (exposure, DNS, backup, compute, models);
    the granting skill says so.

## Data model

Add to `control-plane/state/state.go`:

```go
type MCPCapabilityRecord struct {
    ServerName string   // LiteLLM server name / pinned server_id
    Namespace  string   // kube namespace (the mcp slot)
    ServicePort int    // service port; URL derives http://<name>.mcp.svc.cluster.local:<port>
    Transport  string   // "http" | "sse"
    Scope      string   // e.g. "spreadsheet:<id>:readonly"
    Tools      []string // allowed tools (pinned at registration)
    Grantees   []string // agent names
    Origin     string   // "agent" | "operator"
    SecretRef  string   // broker-token secret name sealed into the slot runner package
    CreatedAt  uint64
}
```

- `MCPCapabilities`, `RetiredMCPCapabilities` maps in `ControlPlaneState`;
  init in `defaultState`/`ensureMaps`; backward-compatible load.
- The per-agent MCP key record rides the CP litellm store
  (`mcptool-<pod>` env key, sealed like the model keys).
- A `CapabilityView` aggregator (runner + MCP) for console/CPA listing.
- Optional unified `GrantEvent` audit list — decide in Phase 1.

## Workstreams

### 1. Door hardening (LANDED — separate PR)

Per-kind privilege for CP-guest door units: ssh/api doors run as a derived
per-door user (`fhd-<name>`, crc32-disambiguated for long names) with
`NoNewPrivileges`, `ProtectSystem=strict`, `ReadWritePaths=<pkgDir>`,
`PrivateTmp`, `ProtectHome`, `RestrictSUIDSGID`; `local` doors stay root (the
documented contract). `provisioner.DoorUser/EnsureRunnerUser/DoorSandbox` are
the shared helpers; `startCapabilityRunner`, `fill_door`'s restart, and the
console's rotate-restart re-assert user + ownership before every restart
(every re-seal re-ships the package root-owned). An injected `RestartDoor`
hook (tests, remote drivers) skips the local exec.

### 2. CP capability registry + unified grant surface

**Files:** `state/state.go`, `agent/tools.go`, `agenttools/server.go`,
`console/server.go`, `cmd/freehold-console/main.go`.

- `ProvisionMCP(args)` — no caller-supplied URL; registers the server in
  LiteLLM (pinned tools), seals the broker token into the slot runner
  package, records the capability, applies grantee key permissions.
- `GrantCapability(args)` / `RevokeCapability(args)` — `name`, `grant_to`,
  `revoke_from`; dispatch by plane:
  - runner → relay roster membership + record + pod re-apply (existing path),
  - MCP → update the grantees' `mcptool-<pod>` key `object_permission`
    (grant = add server/tools to the key; revoke-all = delete the key).
- `provision_runner` / `revoke_runner` stay the runner provisioning verbs;
  `grant_agent` stays operator-only for raw runner grants.
- Scope: `requireGranter` (CPA or operator) + the `agent_grants` kill switch
  govern the MCP tools; ownership guards mirror the runner flow (the CPA
  grants only onto capabilities it provisioned).
- Console/CLI: capabilities list, grant, revoke, MCP register.
- Tests: dispatch by plane, guards, kill switch, no-op/typo behavior.

### 3. LiteLLM key permissions + server registration

**Files:** `cpbuild/departments.go`, new `cpbuild/litellm_mcp.go`,
`cpbuild/terraform/litellm.tf`, tests.

- Mint `mcptool-<pod>` per agent: models restricted to none, `object_permission`
  from the agent's MCP grants (empty → `["no-mcp-servers"]`).
- Reconcile sweep EVERY build: all LiteLLM keys without an explicit list get
  `no-mcp-servers`; key permissions overwritten from CP state with drift
  reported.
- `stageLitellmMCPServers`: idempotent registration from the capability
  records, `server_id` pinned, `pinned_tools`, broker token as a
  `static_headers` Authorization header (finding 7 — NOT `auth_type`).
- LiteLLM config: a ConfigMap with `store_model_in_db: true` +
  `require_key_mcp_access_defined: true`, mounted at `/app/config.yaml`
  (confirm the mount path in Phase 0).
- Tests: payloads, drift overwrite, hermetic fake LiteLLM.

### 4. MCP services (Google Sheets first)

**Files:** `cpbuild/kube_slot.go` (carve labels Pod Security restricted),
MCP-service manifests + a re-apply stage, docs.

- Compute carves `mcp` (`kube-api-mcp`) via `kube-api-root`; AI owns the
  workloads; the operator fills SA keys through the NEW console named-secret
  form (work item above), never a shell or chat.
- One Deployment + Service per (sheet, mode): `mcp-google-sheets` over SSE
  (or a small wrapper) behind a bearer-token auth sidecar; SA key from a
  Secret; image digest-pinned.
- The MCP service manifests live in the CP capability record so a rebuild
  re-applies them from sealed material (the kube-slot pattern) instead of
  relying on a manual AI redeploy.
- Google ACL is the sheet bound: SA shared on exactly one sheet, Viewer or
  Editor; no Drive scope unless list/create is required.

### 5. Agent bridge MCP client

**Files:** `api/cmd/freehold-agent-tools/mcp.go`, `agent/agent.go`, tests.

- Bridge conf gains `mcp_url` + `mcp_key` entries (conf FILE, not env — the
  harness env-clears); the CP renders them at pod apply from the agent's
  `mcptool-<pod>` key; 0600 in the world-writable /tmp, same as today's conf.
- On `tools/list`: merge LiteLLM's tools with buzz-dev-mcp + freehold tools;
  drop colliding names (local names win).
- On `tools/call`: local names first, then LiteLLM; cache the upstream tool
  names, refresh on each list.
- Streamable HTTP/SSE + session handling; graceful degradation when LiteLLM
  is down (buzz/freehold/runner tools keep working; unknown names fail
  loudly).
- The MCP key is revocable: CP deletes/rotates the key and the next pod apply
  rewrites the conf.

### 6. Docs + acceptance

**Files:** `agents/freehold/prompt.md`, `agents/freehold/skills/granting.md`,
`docs/AI.md`, `docs/ARCHITECTURE.md`, `docs/FREEHOLD.md`, `AGENTS.md`.

- Granting skill: two planes, one grant surface; MCP grants are
  CP-written LiteLLM key permissions; the master key is a second authority —
  drift is overwritten on every build; department containment applies to MCP
  servers too; sheet contents are untrusted input.
- Docs state current behavior only (the door privilege fix included).
- PR titles carry no version numbers; release notes at release time.

## Phases

| Phase | Work | Deliverable | Est. |
|---|---|---|---|
| **0. Spikes — DONE** | Findings below; live-verified against LiteLLM `main-stable` running locally (docker: litellm + postgres + a minimal Streamable-HTTP MCP server). | This findings note. | done |
| **1. State + registry** | `MCPCapabilityRecord`, maps, `CapabilityView`. | State + tests; no behavior change. | 2–3 days |
| **2. LiteLLM permissions** | `mcptool-<pod>` mint/update/delete, fleet-wide `no-mcp-servers` sweep + drift overwrite, ConfigMap flag, registration helper. | Default-deny MCP, CP-authoritative permissions. | 2–3 days |
| **3. Unified grants** | `provision_mcp`, `grant_capability`, `revoke_capability`; dispatch, guards, kill switch; console/CLI. | One grant/revoke surface for both planes. | 3–4 days |
| **4. MCP services** | `mcp` slot carve (restricted), Sheets services per SA, console named-secret form, broker tokens CP-sealed, re-apply stage. | Read/write Google Sheets capabilities live. | 3–5 days |
| **5. Bridge** | MCP-over-HTTP upstream keyed from the conf file, merge/route/shadow-drop, SSE parsing, degradation. | Native MCP tools in the agent harness. | 3–5 days |
| **6. Docs + acceptance** | Skill/prompt/docs; e2e; audit/readiness. | Docs match code; acceptance green. | 2–3 days |

### Phase 0 findings (live-verified against LiteLLM `main-stable`, 2026-11-05)

1. **Default-deny works.** `require_key_mcp_access_defined: true` is accepted in
   `general_settings` (config.yaml) and does exactly what the plan needs: a key
   with no `mcp_servers` list sees `"tools":[]` — nothing, not everything.
2. **`object_permission` works at mint AND update.** `/key/generate` accepts
   `mcp_servers` + `mcp_tool_permissions`; `/key/update` rewrites them; the
   next `tools/list` reflects the change immediately. Grant/revoke = key write.
3. **Tool-level enforcement at call time.** A tool outside the key's
   `mcp_tool_permissions` is advertised nowhere, and a hardcoded call is still
   refused with a clear `isError` message ("Tool 'write_thing' is not allowed
   for your key/team on server …").
4. **Revocation is effectively instant: ~20 ms** from `/key/delete` to a
   denied call. No meaningful cache TTL; rapid containment = delete the key.
5. **Key administration is proxy-admin-only.** A non-master LiteLLM key is
   refused on `/key/update` ("Only proxy admin can …"); a UI `app_owner` user
   cannot self-serve. The remaining second authority is exactly the master key
   (the AI department's door + the CP) — the build-time drift-overwrite stays
   the control.
6. **Registration: two paths; DB records SHADOW config.** `POST /v1/mcp/server`
   (DB) and config.yaml `mcp_servers` both work; a DB record with the same
   `server_id` shadows the config entry ("the database server wins"). The CP
   must own ONE path per server. `server_id` pinning is honored; duplicate
   registration is refused loudly.
7. **`auth_type: bearer_token` is BROKEN for http servers in `main-stable`.**
   The DB API stores `auth_value` as None; config.yaml `auth_value:
   os.environ/…` forwards the token but `prepare_mcp_client` still fails with
   "MCPAuth.bearer_token requires a usable upstream credential" → 500.
   **Working alternative (verified end to end): `static_headers:
   {Authorization: "Bearer <token>"}`** — accepted by BOTH the DB API and
   config.yaml, forwarded on every request, calls succeed. The plan's
   broker-token design uses `static_headers`; do NOT use `auth_type`/
   `auth_value`.
8. **Dynamic (DB-API) registration with `static_headers` works** —
   `provision_mcp` can register authed servers at runtime; no rebuild needed.
9. **`/mcp` handshake (bridge-relevant):** authenticate with
   `x-litellm-api-key: Bearer <key>`; `tools/list` works WITHOUT a prior
   `initialize` (stateless-friendly); tool names are server-prefixed
   (`<server_name>-<tool>`); responses are **SSE-framed** (`event: message` +
   `data: {…}`) even when the client accepts JSON — the bridge MUST parse
   SSE `data:` lines; `server_outcomes` metadata rides each result.
10. **`pinned_tools` works** via `POST /v1/mcp/server/$ID/pin` (a snapshot of
    names + descriptions + input schemas).

Verdict: the option-2 design is buildable exactly as planned. The only plan
changes are `static_headers` instead of `auth_type`/`auth_value` for the
broker token (finding 7) and SSE-response parsing in the bridge (finding 9) —
both already reflected in the workstreams above.

**PR strategy (operator-directed):** ALL MCP-plane phases land DIRECTLY on
the `mcp` integration branch (no per-phase PRs); a single PR — #461,
`mcp` → `main` — carries the whole effort and must be MERGE-READY before the
operator merges it; `mcp` merges to `main` once everything is tested. The
`require_key_mcp_access_defined` config lands on `mcp` and reaches the world
on the next build after the merge.

## Testing and acceptance

- **Unit:** state round-trip, grant dispatch, key payloads, registration
  payloads, bridge routing, door sandbox regression (landed).
- **Acceptance (hermetic):** fake LiteLLM MCP server, fake relay roster;
  grant/revoke; drift overwrite; bridge merge/route/shadow-drop.
- **E2E (dev world):**
  - Agent reads Sheet A via native MCP tools; write tools invisible for the
    read grant.
  - Write grant updates Sheet A; Sheet B isolated.
  - Revoke updates/deletes the key; the next call is denied within the cache
    TTL (measured).
  - A UI-created key with no list reaches no MCP server.
  - Drift (a hand-edited key permission) is overwritten at the next build and
    reported.
  - Rebuild re-registers servers, re-applies permissions, re-applies the MCP
    services from sealed material; k3s rebuild re-creates the slot.
  - LiteLLM down: MCP tools fail; buzz/freehold/runner tools work.
  - Runner grants unchanged; door units: api/ssh non-root, local root.
- **Commands:** `mise exec go@1.25.0 -- go build ./... && go vet ./... && go
  test ./...` in `control-plane/`, `agents/`, `contract/`; acceptance needs
  `cargo build --bin runner`; harness needs `-p freehold-harness-oracle`.

## Risks and mitigations

- **LiteLLM API drift:** pin the image digest (spike ran on `main-stable`); pin `server_id`.
- **Default-open MCP:** access groups + the fleet-wide sweep + Phase 0 proof.
- **Second grant authority:** drift overwrite + report every build; scoped
  admin if LiteLLM supports it.
- **MCP service auth:** bearer-token sidecar; digest-pinned images.
- **Secret custody:** SA keys via console form (sealed to the slot runner);
  broker tokens never handled by a human; the AI department sees SA keys in
  its exec env (trusted core), custom agents never do.
- **k3s secrets at rest:** enable `--secrets-encryption` or document the gap.
- **Revocation latency:** measured TTL; delete-the-key for rapid containment.
- **Logs with sheet contents:** disable MCP content logging or accept +
  document; CP state is the grant audit.
- **Prompt injection via cells:** containment rule at grant time.
- **Bridge complexity:** conf-file key delivery (env is cleared), shadow-drop,
  timeouts, degradation.

## Open items

1. Scoped-admin role feasibility in LiteLLM (Phase 0).
2. LiteLLM MCP content-logging disable switch (Phase 0).
3. k3s `--secrets-encryption` on/off decision (Phase 4).
4. Tab-level allowlisting (wrapper) vs per-spreadsheet Google ACL only —
   per-spreadsheet ships in Phase 4; tab-level is a follow-up.
5. LiteLLM image digest pin now vs `main-stable` + named follow-up.
