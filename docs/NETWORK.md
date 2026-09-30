# The Network Plane — the freehold-subnet, the gateway, and the public path

The network-surface build plan. Every world guest — relay, CP, k3s, and every
service the agents provision later — is born on a **freehold-managed internal
subnet** behind a **gateway guest**. The surrounding network sees exactly one
address; zero router configuration is the north star; and the shape is
**identical on every substrate** (Proxmox home lab, cloud VPS, later
providers). This is the build plan for the Network department's home ground
("the network surface: access & exposure"). It is a named build plan, not a
numbered POC chunk — it runs alongside the Chunk 5/6/7 work and owns no
version of its own.

## The substrate rule (locked)

**The gateway is a guest role on EVERY substrate.** Only the network its
public interface rides differs. There is no "VPS mode without the gateway" and
no "home mode" — one role, one lifecycle, one answer to "what holds the public
IP."

| Substrate | Gateway's public side | Internal side |
| --- | --- | --- |
| Proxmox / home LAN | One LAN bridge IP (the only LAN-facing address) | Internal tagged bridge on the PVE host (router never sees it) |
| VPS / cloud | The VPS's own public NIC | Internal private bridge on the cloud host |

The security benefit is deliberate: the firewall boundary is a **guest**, not
an accident of which container binds `0.0.0.0`. Uniformity pays twice — the
gateway inherits the whole guest lifecycle (teardown/rebuild/re-adopt) like
its siblings, and Newt (Phase 3) runs in the same slot in both worlds.

## Topology

```
        surrounding network (LAN / VPS public) — untouched
                          │
               ONE address: the gateway (nftables)
                │ masquerade out (guests get internet)
                │ 80  → k3s Caddy (redirects to 443)
                │ 443 → k3s Caddy (relay, CP console, /mcp,
                │       per-guest vhosts)
                │ runner port → CP (TUI, updates, thin boxes)
                │ (forward list is config — extensible)
                │ Newt (Phase 3): dials OUT to the Pangolin VPS
       ─────────┼──────────────────────────────────────
        internal subnet — relay │ cp │ k3s │ (future guests)
```

Two paths to everything, by design:

*   **From the surrounding network (Phase 1):** gateway forwards. Works with
    the internet down; no router/PSP config beyond what already exists.
*   **From the internet (Phase 3):** Pangolin VPS:443 → Traefik → Gerbil →
    Newt → internal service. The home IP never appears in public DNS.

## Phase 1 — the gateway guest (home/Proxmox first)

1.  **New guest role `gateway`** — created *first* in boot order, torn down
    *last* (the existing per-role pattern, so teardown/rebuild/re-adopt come
    free). Holds the one public-side address; ~512MB; no docker — nftables
    rules + config only.
2.  **Relay/CP/k3s born on the internal subnet** — static IPs, freehold-
    recorded (the existing `--relay-ip/--cp-ip/--proxy-ip` path, now pointed
    inward). Guests are born on the internal bridge/tag — the "born on the
    plane, never set post-hoc" rule, applied to network.
3.  **Config split:** `proxy_ip` today means both "the IP DNS points at" and
    "where k3s lives." With the gateway those separate: the *edge* (gateway,
    what DNS points at) and k3s's internal address. `contract/config`,
    `cpbuild.Spec`, and the `DnsRecords` consumers change once; everything
    else rides the recorded values.
4.  **DNS/cert model unchanged:** relay/CP domains point at the gateway IP;
    freehold's lego/Caddy pipeline issues as it does today — from the
    surrounding network, everything resolves exactly as it did against the
    old proxy IP.
5.  **Gateway payload:** masquerade out for the subnet; 80 → k3s (Caddy
    redirects); 443 → k3s (Caddy: relay, CP console, `/mcp`, per-guest
    vhosts like `litellm.freehold.local` — name-keyed, a real domain later);
    the runner port → CP (so `freehold build`/`update`, the TUI, and thin
    boxes keep working unchanged). The forward list is a config list —
    extending it is an edit, not code.
6.  **Wizard:** one new prompt — the gateway's public-side IP + the internal
    subnet CIDR. Rebuild/re-adopt reuse the recorded values.

**Verification gate before calling this done:** `freehold build`, the TUI,
and a thin-box flow exercised from a SECOND box against a subnet world — the
runner-port forward must prove itself, not be assumed.

**Acceptance:**

*   [ ] Fresh install on PVE: gateway + relay/CP/k3s on the internal subnet;
    nothing but the gateway is LAN-addressable.
*   [ ] From a second box: `freehold build`, `freehold update`, the TUI, and
    a thin-box login all work through the gateway unchanged.
*   [ ] Agent pods reply in the relay (agents dial runners inside the subnet);
    `pct exec` still reaches every guest via the PVE host.
*   [ ] Teardown → rebuild → all down: lifecycle identical to today's; the
    gateway is recreated with its recorded IP every time.
*   [ ] Existing non-gateway worlds (and VPS deployments without the role)
    still build and reconcile untouched.

## Phase 2 — VPS parity (the test layer)

Run the SAME gateway role on a cloud VPS (Vultr/Hetzner drivers): the gateway
holds the VPS's public IP; guests ride a private bridge on the cloud host.
Nothing listens publicly except the gateway. The full lifecycle is exercised
end to end — install → world up → agents reply → teardown → rebuild → all
down — identical to the Proxmox path. This phase is what turns "gateway
everywhere" from an intention into a proven rule.

**Acceptance:**

*   [ ] The gateway role deploys on a Vultr/Hetzner VPS via the existing
    drivers; guests ride the internal private bridge.
*   [ ] The full lifecycle passes on the VPS exactly as on PVE (the checklist
    above, VPS edition).
*   [ ] The only substrate difference is the gateway's public interface's
    network — one config value, no role fork.

## Phase 3 — Pangolin (the public path, north star)

*   **Provisioned by freehold by default:** install provisions the Pangolin
    VPS through the existing Vultr/Hetzner drivers and records it as a role
    ("pangolin"); an operator-provided VPS/existing-Pangolin is the fallback
    prompt (the drivers make the fallback cheap). The VPS can boot in any
    order — nothing but certs waits on it.
*   **The stack:** Pangolin (control plane + dashboard), **Gerbil** (the
    WireGuard traffic server on the VPS), **Traefik** (public TLS, bundled),
    and **Newt** (the connector client) on the gateway guest — a systemd
    binary, no docker, one key. **Newt dials out:** nothing at the home
    accepts an unsolicited packet, and the single public port-forward of
    Phase 1 becomes retirable.
*   **Public domains terminate on the VPS** (Traefik + its ACME); Caddy stays
    the internal edge permanently. Both paths coexist by design: a dead
    internet never blocks the LAN.

**Acceptance:**

*   [ ] `install` provisions (or adopts) a Pangolin VPS; Newt is enrolled on
    the gateway and dials out; public traffic reaches an internal service
    through the tunnel.
*   [ ] Public domains resolve to the VPS; their TLS is Traefik/ACME; from
    the LAN the same services still resolve via the gateway (internet-down
    test passes).
*   [ ] Teardown of the Pangolin VPS stops Newt and leaves the gateway and
    the world standing.

## Phase 4 — agent-operated exposure

A `pangolin-api` capability runner for the **Network** department: create and
verify public resources through the department model ("reachable outside your
network?") — the check-in hook and capability-execution rules apply as
locked. Pangolin has a real API, so the dashboard is never required.

**Acceptance:**

*   [ ] Network creates a public resource through its door; the request
    arrives as the department's own identity; the audit stream records it.

## Not building

*   Per-service forward lists beyond the initial three — it's a config list;
    extend by editing one file.
*   Router automation, ever — Phase 3 makes it unnecessary.
*   Gateway HA — it is a rebuildable guest like its siblings; recreate is the
    recovery path.
*   Public-domain termination on Caddy long-term — Phase 3 supersedes it;
    Caddy keeps the internal edge.

## Code-touch map (implementing-agent blast radius)

*   New guest role `gateway` in the boot/teardown ordering (born first, torn
    last) — the same per-role path as relay/cp/k3s.
*   `ProxmoxLxcSpec` net0 for the internal bridge/tag (the plumbing path is
    verified: `drivers.go` net0 string; `cpbuild.go` static-IP threading).
*   `proxy_ip` split in `contract/config` + `cpbuild.Spec` + `DnsRecords`.
*   The gateway stage: nftables config deployed like any other stage.
*   Wizard prompts (gateway IP + subnet); Phase 2's VPS driver path; Phase 3's
    provisioner stage + the Newt unit.
