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

The gateway has TWO shapes, one per substrate:

*   **A LAN world (Proxmox at home):** the gateway is a GUEST — a small
    two-NIC LXC holding the one LAN address (the proxy IP), masquerading the
    internal subnet out and DNATing the fixed port set. Its ruleset is
    asserted at boot and re-asserted by every build; the PVE host needs an
    `ip route` into the subnet via it.
*   **A cloud world (Vultr; the api-vultr access mode):** the HOST is the
    gateway — the public IP belongs to the instance's NIC, so no guest can
    hold it. The host owns the internal bridge (`vmbr0`, the subnet's `.1`),
    renders the same forward/NAT shape plus an INPUT policy (drop; ssh, the
    edge ports, the internal side — a public edge must not answer anything
    else, and pveproxy stays dark to the internet), and runs the subnet's
    resolver. The public NIC is never touched (no bridge-move lockout), the
    shape persists across a reboot, and there is no host route to lose — the
    host IS the router. Same ruleset discipline: one renderer
    (`contract/config`), asserted at boot and re-asserted by every build.

## How it works

*   **The gateway guest** is created first and torn down last, like any other role. It holds
    the one LAN address, masquerades the subnet out, and DNATs a fixed set of ports to the
    right guest. Guests get derived static addresses (`.11` relay, `.12` cp, `.13` k3s).
    **Every fresh install builds one** — the internal subnet is derived (`10.77.0.0/24`,
    bumped past any overlap with the LAN and past any subnet another world on this LAN
    already claims: the mint L2-probes the candidate's world addresses before settling —
    two boxes on one LAN both deriving `10.77.0.0/24` untagged ARP-collide their guests);
    `--gateway-cidr` overrides untouched, and `--gateway-vlan` skips the probe (a tagged L2
    is its own domain). The bridge rides untagged unless `--gateway-vlan` says a tag. A
    pre-gateway world re-adopts flat (no gateway is force-built mid-life — its live guests
    hold the LAN addresses).
*   **The edge is Caddy on k3s**, permanently the internal edge: a relay vhost and a CP vhost
    (the CP also serves `/mcp` publicly). Certificates are issued in-process (lego, DNS-01)
    and written into Caddy's volume.
*   **The launch surface — agents' services, reachable by default.** When an agent stands a
    service up (a homelab agent deploying a family calendar), reaching it is a Network
    capability: the **exposure verbs** (`expose_app` / `unexpose_app` on the agent toolset)
    turn {name, target, group} into a public TLS vhost on the edge — DNS pointed
    (`<name>.<cp host>` — an app sits under the CP host, the domain the member cookie is scoped to — → the proxy), a cert — ONE SHARED wildcard for the cp host's domain, issued once through the same
    in-process lego chain (an expose after that is DNS + config only — no per-app LE order), the Caddy config rendered + applied live — and the record lands
    in the **apps registry** (`apps.json`, the toolset's durable dir), which the build tail
    re-ensures so teardown/rebuild restores the apps. Two invariants: **cert before
    config** (a vhost whose cert is missing crash-loops the whole edge — an app's block
    enters the rendered Caddyfile only once its cert is installed) and **access narrows to
    groups the requester belongs to** (below). The gate is the console's **member identity
    tier**: NIP-07 Nostr login admitted by the relay's NIP-43 membership list plus
    single-use device-link invites (the no-Nostr path); gated apps verify through
    `/auth/verify` (a browser gets the 302 to the login page, an API client the 401), and
    the operator session passes the same gate. Scope is structural: only the **network**
    department's identity or the operator may call the verbs; a custom agent that asks
    directly is refused (`-32003`) and routes through Network in conversation — and
    widening (`visibility: public`, `auth: none`) is refused from agents outright.
*   **Groups are relay channels.** Every exposed app rides a relay channel as its ACL —
    the record stores the channel ID (names change; the display layer resolves the current
    name), `#general` is the everyone-channel and the default, and the expose validates
    the requester is IN the named channel before recording it. The family's group chat in
    Buzz IS the family's app group: one place to manage people. The gate ENFORCES the
    channel per request (`IsMemberAuth` — the same channel-scoped read the runner grants
    use): removing someone from the channel revokes them within the gate's cache TTL.
    Device-link invites bind their APPS at mint (a link opens exactly the named apps —
    there is no all-apps link). By convention every member belongs to `#general`, so
    "available to everyone" = the app rides `#general`; adding that convention to the
    member-onboarding flow is its own item below.
*   **DNS:** certs via DNS-01 with an automated provider (Cloudflare today) or MANUAL
    mode — the build stops and prints the exact TXT record to create, then the re-run
    resumes that same order when it exists; public A records are managed via the
    Cloudflare API when the world opts in, or created by hand and re-checked by every
    build (the report lists what's missing); internal names via
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
*   The subnet collision probe is ping/ARP-based: a DOWN guest (a crashed second host) or a
    non-ICMP squatter evades it, and the collision returns undetected — `--gateway-cidr` /
    `--gateway-vlan` is the override. Permanent ARP pins set by hand on the guests are
    runtime state: a guest reboot drops them.
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

*   **The onboarding convention:** every new relay member is added to `#general` when
    they're onboarded (the add-member flow) — the one-time default that makes
    "the app rides #general" mean "available to everyone". Separate concern from the
    exposure verbs.
*   **The portal.** `cp.domain` becomes the landing page: launcher tiles from the apps
    registry (the apps the agents built and manage) + "talk to your agents" (the relay) +
    login; the operator console stays behind its own operator login.
*   **Pangolin as the default public path.** The public DNS story flips: a wildcard
    `*.cp.domain` points at the Pangolin VPS and per-app names ride the tunnel, so the
    home IP appears nowhere in public DNS and nothing at home accepts unsolicited traffic
    (Newt dials out from the gateway — asserted at boot and re-asserted by every build,
    like the nftables ruleset). Apps go behind it first; relay/cp keep the direct Caddy
    edge. A cloud world uses the same chain (Newt on its gateway dials out) — one shape on
    every substrate. The Pangolin VPS itself is provisioned through the created-host
    lifecycle (the Vultr provider path with a pangolin-compose payload instead of
    PVE-on-Debian) and destroyed by `uninstall` — it is a bill, and the profile records it.
    Per-host certs issue at expose time (HTTP-01 through the tunnel); the VPS never holds
    the DNS provider's credentials.
*   **The advanced tier.** A custom domain is the same machinery with a different hostname:
    the registry takes a full fqdn, certs are per-host, and the user's DNS is theirs to
    point. Verify-before-design: whether Network drives Pangolin's REST API (the
    `pangolin-api` runner) or a Traefik file-provider door decides the expose verb's write
    path.
*   **More DNS providers:** a provider registry (`dnsman`, hoisted out of the cloudflare
    package) with per-provider A-record managers — Route53, Google Cloud DNS,
    DigitalOcean — hand-rolled REST like the Cloudflare one (no SDKs; Route53's SigV4
    signed by hand), plus a shared metadata table (credential fields + manage
    capability) feeding the interactive ask and the doors; generalized
    `<provider>-dns-<zone>` doors with per-provider probes (manual = no door). Cert
    DNS-01 itself already covers every lego provider (201, generated from lego), plus
    the manual mode.
*   **Agent-operated exposure:** a `pangolin-api` runner so Network creates and verifies
    public resources itself, through the department model.
*   Forward list as config, per-guest vhosts, manual cert import, Tailscale certs, a Certs
    tab in the TUI, and the first Network skills (tailscale, pihole).

## Not building

Router automation; gateway HA (it's a rebuildable guest); Caddy as the long-term public
terminator.

## Where the code is

`contract/config/` (gateway spec, nft/dnsmasq renderers — the guest and hosted
variants), `platform/provisioning/box/` (gateway boot + the host-gateway
stage), `control-plane/api/cpbuild/` (DNS, certs, gateway re-assert,
`terraform/caddy.tf`), `providers/proxmox/` (guest networking),
`providers/vultr/` (the api-vultr host lifecycle + the PVE-on-Debian
install), `agents/network/`.
