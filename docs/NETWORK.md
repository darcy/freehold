# Network — one address in, an internal subnet behind it

**Owner:** the Network department (`agents/network/`). **Scope:** access and exposure —
the gateway, the subnet, DNS, certs, the web edge, and (later) the public path.

## The idea

Every world guest is born on a freehold-managed **internal subnet** behind a **gateway
guest**. The surrounding network sees exactly one address, no router configuration is
needed, and the shape is meant to be identical on every substrate — the gateway is a guest
role everywhere; only the network its public side rides differs.

```
   surrounding network (LAN / VPS public)
                  │
        ONE address: the gateway   (nftables + dnsmasq)
          │ masquerade out
          │ 80/443 → Caddy on k3s     8080 → CP console
          │ 6443 → kube-apiserver     3000 → relay
   ───────┼──────────────────────────────────
     internal subnet:  relay · cp · k3s · (future guests)
```

## How it works

*   **The gateway guest** is created first and torn down last, like any other role. It holds
    the one LAN address, masquerades the subnet out, and DNATs a fixed set of ports to the
    right guest. Guests get derived static addresses (`.11` relay, `.12` cp, `.13` k3s).
    **Every fresh install builds one** — the internal subnet is derived (`10.77.0.0/24`,
    bumped past any overlap with the LAN; `--gateway-cidr` overrides) and the bridge rides
    untagged unless `--gateway-vlan` says a tag. A pre-gateway world re-adopts flat (no
    gateway is force-built mid-life — its live guests hold the LAN addresses).
*   **The edge is Caddy on k3s**, permanently the internal edge: a relay vhost and a CP vhost
    (the CP also serves `/mcp` publicly). Certificates are issued in-process (lego, DNS-01)
    and written into Caddy's volume.
*   **DNS:** public A records via Cloudflare when the world opts in; internal names via
    dnsmasq on the CP guest, with the gateway's dnsmasq as its upstream.
*   **Network's runners:** the PVE host (shared), a `caddy`-scoped kube door, the resolver on
    the CP guest, a Cloudflare door per DNS zone, plus any door the CPA provisions on the fly
    (e.g. a UniFi controller).

## Known gaps

*   The gateway's host route isn't persisted — it is asserted by the box's gateway stage
    (`ip route replace`), lost on a PVE host reboot until the next install/re-adopt. A switch
    reboot lost it once and the world kept serving — only the box-side verbs went dark until
    the route returned.
*   Port 80 reaches Caddy but nothing redirects to 443; no per-guest vhosts; the DNAT list
    is hard-coded.
*   Pre-gateway worlds' guests are DHCP, so a reboot can strand recorded addresses.
*   The gateway's forwarding rides nftables ordering: the freehold ruleset is asserted LAST
    and the gateway installs no docker, so docker's iptables-nft (FORWARD policy drop) can't
    strangle it — but no reconciliation pass re-asserts the freehold table if docker ever
    re-appears on a guest created by an older build.
*   No tailscale or pihole skills exist; Network's prompt doesn't describe the gateway.
*   Gateway acceptance is live-verified on three worlds on one shared host (librem, live,
    rebuild — full uninstall → gateway install → build → agents connected → edges served from
    a LAN box → teardown/build cycles); the remaining unverified leg is the second-box/
    thin-box flow through the gateway, and automated tests cover config and net shapes, not
    the live flows.
*   The "reachable outside your network?" check-in has no trigger yet.

## Future

*   **VPS parity:** the same gateway role on a cloud VPS, full lifecycle identical to Proxmox.
    Needs a VPS provider (`docs/COMPUTE.md`).
*   **Pangolin, the public path:** a Pangolin VPS (Gerbil + Traefik) with **Newt** on the
    gateway dialing *out*, so nothing at home accepts unsolicited traffic and the home IP
    never appears in public DNS. Caddy stays the internal edge; both paths coexist.
*   **Agent-operated exposure:** a `pangolin-api` runner so Network creates and verifies
    public resources itself, through the department model.
*   Forward list as config, per-guest vhosts, manual cert import, Tailscale certs, a Certs
    tab in the TUI, and the first Network skills (tailscale, pihole).

## Not building

Router automation; gateway HA (it's a rebuildable guest); Caddy as the long-term public
terminator.

## Where the code is

`contract/config/` (gateway spec, nft/dnsmasq renderers), `platform/provisioning/box/`
(gateway boot), `control-plane/api/cpbuild/` (DNS, certs, gateway re-assert, `terraform/caddy.tf`),
`providers/proxmox/` (guest networking), `agents/network/`.
=======
*   New guest role `gateway` in the boot/teardown ordering (born first, torn
    last) — the same per-role path as relay/cp/k3s.
*   `ProxmoxLxcSpec` net0 for the internal bridge/tag (the plumbing path is
    verified: `drivers.go` net0 string; `cpbuild.go` static-IP threading).
*   `proxy_ip` split in `contract/config` + `cpbuild.Spec` + `DnsRecords`.
*   The gateway stage: nftables config deployed like any other stage.
*   Phase 2's VPS driver path; Phase 3's provisioner stage + the Newt unit.