# CP-Driven Build — operator box ⇄ control-plane decoupling (Phase G)

This file is the **current-state + forward plan** for making `freehold build` on an
operator box create the CP and then hand the rest of the world to the **CP** to
bring up — including the cert install — through the CP's own co-located runner.

The repo hygiene rule (AGENTS.md) holds: this documents what is true *now* and
what is planned, not history. Changelog-style narration lives in `CHANGELOG.md`.

## High level: what has been done

The decoupling foundation and the CP world-action surface are implemented,
merged, and live-proven on `192.168.30.224` (a real Proxmox host; the world is
ephemeral-for-testing):

### Merged (in this session, Phases to get here)

| # | PR | What |
|---|---|---|
| 158 | `chunk4-decouple-login` | root-free `freehold login` (CP addr + pubkey + nsec → NIP-98 → seed config → end) + `logout`; enforced CP-pubkey trust anchor |
| 159 | `chunk4-phaseg-cp-world` | CP serves `GET /api/world` from real state (relay/cp/pubkey, operator) |
| 160 | `chunk4-phaseg-box-identity` | login materializes the box's `agent-ops` provisioning identity (first-run-wins) |
| 161 | `chunk4-phaseg-cpfirst-teardown` | `freehold teardown` is CP-first: `POST /api/teardown` clears the CP's runners/secrets/agents/DNS, then the box destroys the CP LXC |
| 162 | `chunk4-phaseg-world-action` | roster-gated `world_status` / `world_teardown` on the CP toolset |
| 163 | `docs/chunk4-phaseg-tick` | POC_CHUNK4 Phase G ticked |
| 164 | `chunk4-phaseg-migrations` | CP verify-gated migration runner (`internal/migrations` + `world_migrate`) |
| 165 | `chunk4-phaseg-stages` | `internal/stages` — shared world-bring-up command builders (k3s/caddy/litellm scripts, coord + secret helpers), net-zero extraction from `rebuild.go` |
| 166 | `chunk4-phaseg-world-build` | roster-gated `world_build` on the CP toolset; slice 1 = k3s durable local-path via the co-located runner |
| 167 | `fix/deploy-cp-stop-runner` | `deploy-cp` stops the CP's co-located runner before re-shipping its binary (fixes overwrite-failed reconverges) |
| 168 | `chunk4-phaseg-world-build2` | `world_build` also reconverges the relay compose stack |
| 169 | `fix/worldbuild-pipefail` | pipefail the relay-compose reconcile (was masking failure) |
| 170 | `chunk4-phaseg-worldbuild-caddy` | `world_build` re-applies the Caddy TLS edge; agent-tools serve gains `--relay-host/--relay-ip/--cp-host/--cp-ip` |
| 171 | `fix/worldbuild-caddy-exec` | Caddy stage runs on the PVE host (raw), not inside the k3s guest (was exit 127) |

### Live-proven (against the PVE host)

- **`freehold build` reconverges the whole world**: relay / CP / k3s / DNS
  resolver / litellm / Caddy TLS edge / agent-tools / CPA + agent reconcile →
  `Freehold is up`.
- **Durable-plane cert reuse works**: both edge certs recovered
  (`recovering durable cert — no challenge`), no new LE order / TXT.
- **`deploy-cp` stop-before-ship works**: the co-located runner re-ships without
  a manual stop.
- **`world_build` (box → login+trigger → CP → co-located runner) drives the
  world**: reported live
  ```
  k3s durable local-path re-asserted
  relay compose reconverged
  caddy TLS edge re-applied
  ```

## Target architecture

```
operator box (root-gated)              control plane (durable /srv/data/cp)
────────────────────────               ─────────────────────────────────────
freehold build
  1. provision door + doorGate (operator appends pubkey once, out-of-band)
  2. grant ops identity · serve local runner · verify door
  3. capture world inputs IN MEMORY (domains, proxy IP, operator pubkey,
     DNS provider creds, litellm provider key, sizing) — never persisted box-side
  4. boot + record the CP LXC
  5. deploy the CP: control-plane + co-located runner + freehold-agent-tools
  6. hand the world to the CP:
       · world-state/desire profile → /srv/data/cp (durable)
       · re-seal operator secrets (DNS creds, litellm keys) to a CP-held key
       · a CP-held enc key for in-process cert issuance
  7. trigger CP world_build   ─────────►  (roster-gated, signed as the box)

CP world_build (executes through ITS co-located runner via shared internal/stages)
  8. storage plane ensure (relay/cp/k3s)                [move to CP]
  9. boot + reconverge relay + deploy the Buzz stack     [done: relay compose]
  10. boot + install k3s + durable local-path            [done: local-path re-assert]
  11. DNS resolver register + point guests               [move to CP]
  12. litellm: kube workloads + CP-hosted loopback runner registers model      [move]
  13. Caddy TLS edge                                     [done: manifest re-apply]
  14. CERT: durable-reuse gate → DNS-01 issue IN-PROCESS on the CP (sealed DNS
      cred + CP-held enc key + resume state) → seal cert-key into the co-located
      runner → install into the Caddy PVC + durable mirror → rollout restart
      (async, verify-gated)                              [move — the key ask]
  15. CPA create + agent reconcile                        [already CP]
  16. report world status back to the box
```

Then a fresh box only does: `freehold login` (root-free) → `freehold` → trigger;
`build` stays the day-0 CP-bring-up.

## Remaining steps (to make the CP own everything, incl. cert install)

Each lands as a branch → PR → both-checks → review → merge, then re-deployed to
the (ephemeral) live CP and verified by triggering `world_build`. **Don't slim
the box build until the CP owns every post-CP stage**, or a reconverge breaks.

### Step A — DNS onto the CP
- `world_build` (buildWorldApply) runs the CP's own resolver registration +
  guest nameserver pointing, through the co-located runner:
  - `pct exec <cp> -- <bin>/control-plane dns add <name> <ip> <source>` for
    relay/cp/k3s/litellm/proxy records
  - `pct set <vmid> --nameserver <cpIP>` + resolv.conf rewrite + `dig` verify
- Carry the needed coords on the agent-tools serve command (`--resolver-ip`,
  guest vmid/IP table) or read from the CP's durable world-state.
- **Verify**: world_build reports "dns register/point applied"; guests resolve
  `relay.<d>` / `cp.<d>` via the CP resolver.

### Step B — litellm onto the CP
- Move the litellm runner package + a CP-hosted loopback runner (127.0.0.1:8788
  inside the CP) so the model-registration curl runs CP-side and reaches the k3s
  NodePort (`http://<k3sIP>:31400`) — the CP is on the same PVE network.
- `world_build` applies the litellm/postgres kube workloads
  (`stages.LitellmManifestScript`), registers the model
  (`stages.LitellmRegisterScript`), seeds the CPA key.
- The minted master/postgres + the operator's provider key come from the
  **hand-off** (Step 6) — re-sealed to a CP-held key, resolved by name.
- **Verify**: world_build reports "litellm gateway live"; `world_status`/
  a gateway probe confirms the model is registered.

### Step C — storage plane onto the CP
- `world_build` runs `storage resolve`/`ensure` for the relay/cp/k3s tenants
  through the co-located runner (already-remote, no box-side secret).
- **Verify**: world_build reports the plane ensured; the durable mounts present.

### Step D — cert issuance + install ON the CP (the key ask)
- The cert engine (`internal/cert`, in-process lego DNS-01 via Cloudflare) is
  runner-independent and portable. Run it ON the CP:
  - the CP needs the **sealed DNS provider cred** (re-sealed to a CP-held ops/enc
    key at hand-off) + the resume-order state + writable local state + Cloudflare/
    Let's-Encrypt reachability.
  - the CP's `world_build` cert step: durable-reuse gate first (no LE order when
    the durable mirror has a valid cert), else issue, then seal the cert private
    key into the co-located runner package (`$CERT_KEY_<slot>`) and run
    `stages.CaddyCertInstallScript` (PVC + durable mirror + caddy rollout).
- Expose issuance as a **migration-shaped, verify-gated** step (the
  `internal/migrations` pattern) so it's 🟢/🔴 and idempotent.
- **Verify**: world_build reports "cert issued/installed (or already present)";
  `https://relay.<d>` and `https://cp.<d>` serve a valid cert via Caddy; the
  durable mirror holds the chain.

### Step E — box slim
Only after A–D are CP-owned:
- `freehold build` becomes: door → boot CP LXC → deploy CP + agent-tools →
  hand world-state + secrets → trigger `world_build` → report. The box no longer
  runs storage/relay/k3s/DNS/litellm/Caddy/cert box-side.
- Keep `freehold build` idempotent (if the world is already up, it fast-paths);
  keep the day-0 doorGate out-of-band.
- **Verify**: a teardown + `freehold build` reconverges the world with the CP
  doing the bring-up; `world_build` report shows each stage.

### Step F — world-state durability + config slim + docs close-out
- Move the desire profile (coords, plane, DNS, Caddy/litellm) durably under
  `/srv/data/cp`; the box `config.toml` slims to connection coords
  (relay/cp URLs + pubkeys, operator, `[runner]`, `cp_pubkey`).
- Drop stale test-domain records; tick POC_CHUNK4 Phase G; README/ARCHITECTURE
  current-state.

## Design notes / risks

- **Secret discipline (locked):** the CP is a provisioner, not a vault. Operator
  secrets are captured in memory at build, re-sealed to a CP-held key, never
  written box-side; the runner decrypts by name in memory. Cert private keys ride
  the co-located runner package (`$CERT_KEY_<slot>` env), never argv/logs.
- **No-master-key catenary:** moving litellm + cert to the CP means the CP holds
  (sealed-ciphertext + a CP enc key) — the same exposure class as every runner;
  no master key is introduced.
- **Pace:** each step is verified live before the next. **Step E (slim) must come
  only after A–D**, else a reconverge loses DNS/litellm/certs.
- The live world on `192.168.30.224` is ephemeral-for-testing; recomputing it
  (teardown + build) is a normal, safe diagnostic.
