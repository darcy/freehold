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
    (`<name>.<the world's own domain>` — a child of the world root: the CP host with a
    literal `cp.`/`admin.` label stripped — but only when a world label remains below
    the zone, so `cp.example.com` keeps its name — else the CP host itself:
    `cp.librem.freehold.technology` → `litellm.librem.freehold.technology`;
    `sandbox.freehold.technology` → `litellm.sandbox.freehold.technology`; the member
    cookie scopes to the same world domain — the world's boundary, never the shared zone;
    an operator who owns the whole zone pins the root explicitly with `world_domain` in
    the profile config — `admin.casaquerencia.net` + `world_domain = 'casaquerencia.net'`
    puts the world AT the zone apex: the portal at the root, every app a child), a cert — ONE SHARED wildcard for the world domain, issued once through the same
    in-process lego chain (an expose after that is DNS + config only — no per-app LE order), the Caddy config rendered + applied live — and the record lands
    in the **apps registry** (`apps.json`, the toolset's durable dir), which the build tail
    re-ensures so teardown/rebuild restores the apps (a row written by an older naming
    shape re-derives to the world-domain shape and its stale public record is removed on
    the next ensure). Two invariants: **cert before
    config** (a vhost whose cert is missing crash-loops the whole edge — an app's block
    enters the rendered Caddyfile only once its cert is installed) and **access narrows to
    groups the requester belongs to** (below). The gate is the console's **member identity
    tier**: NIP-07 Nostr login admitted by the relay's NIP-43 membership list plus
    single-use device-link invites (the no-Nostr path); gated apps verify through
    `/auth/verify` (a browser gets the 302 to the login page, an API client the 401), and
    the operator session passes the same gate. The **gate is the standing default
    posture**: every exposed app is member-gated unless the OPERATOR says otherwise for
    that specific app — gate-off (`auth: app`/`none`, `visibility: public`) is an
    operator-only exception, never an agent's default. Scope is structural: only the **network**
    department's identity or the operator may call the verbs; a custom agent that asks
    directly is refused (`-32003`) and routes through Network in conversation — and
    widening (`visibility: public`, `auth: none`) is refused from agents outright.
    The console is the **portal**: the console's session-aware
    launcher (`/api/my/apps`) answers what the asking session can open — the operator
    sees every exposed app, a member sees what their channel roster admits, a device
    session exactly its bound list — and the console renders it as launcher tiles
    (names + FQDNs only, no targets or owners) above the member's own scheduled jobs.
    One login (`/auth`) serves everyone: a whitelisted key lands the operator session,
    any other relay member key lands the member session — the role falls out of the key.
    The world's tree is the layout — the portal at the world root (`<world>.<zone>`), the
    operator face at `admin.<world>`, the relay at `chat.<world>` — and `freehold hosts`
    renames a world onto it (it rewrites the profile's relay/cp URLs and refuses a rename
    that would move the world's own domain: every exposed app's name anchors there).
    When the console's host IS the world root (an unrenamed world), no separate portal
    vhost exists — the console's own vhost serves that name; every prefixed-shape world
    (the standard `cp.<world>`) has the portal live: its cert and public record ride the
    same build that serves the vhost.
    The operator surface is the TUI + the console's admin routes; the portal is the
    member surface — two front doors over one backend. The origin guard accepts exactly
    two origins — the console's own and the world root (never app children: same-site
    subdomain requests would ride the operator session).
*   **The optional Pangolin public path.** A world whose coords carry a `pangolin` block
    routes exposed apps through a Pangolin VPS's tunnel instead of pointing public DNS at
    home: the app A records point at the VPS (`dns_ip`) — the home IP appears in no public
    DNS and nothing at home accepts unsolicited traffic (Newt dials OUT from the gateway);
    relay/cp keep the direct Caddy edge, and the apps' LAN surface rides it unchanged.
    The verbs drive Pangolin's REST integration server (verified against 1.24.0 — Pangolin
    generates its Traefik config from its own DB, so a file-provider door would fight it):
    a Bearer root key sealed to the console identity (`world-secrets/pangolin.json`)
    calls org/site/resource/target — the resource is created **SSO-gated** (nothing it
    fronts is publicly open by default; the tunnel is proven with a resource access
    token via the `p_token` param) and the target points at the app's freehold-subnet
    address, reachable from the gateway where Newt dials out. A created site's one-time
    Newt secret seals to the console identity immediately (`pangolin-newt.json`), and the
    gateway's Newt client is re-asserted by every build — the binary ensured (downloaded
    on the host, pushed to the guest; a hosted gateway installs bare), the identity and
    the instance endpoint riding a root-only env file, the unit restarted so a re-minted
    secret lands live. `unexpose` removes the resource; a failed removal names the
    recovery (re-expose then unexpose). The coords carry no credential. Still open on
    this path: the freehold member gate's exact shape (today Pangolin's own SSO gates
    it), and the pangolin-compose VPS payload through the created-host lifecycle.
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
    resumes that same order when it exists;     public A records are managed via the
    Cloudflare API when the world opts in, or created by hand and re-checked by every
    build (the report lists what's missing); internal names via
    dnsmasq on the CP guest, with the gateway's dnsmasq as its upstream. The
    resolver serves only its own records (`no-hosts`): the CP-local relay-host
    pin in the guest's /etc/hosts reaches CP-side dials via the guest's own
    resolver chain and must never leak into the subnet's answers, where the
    edge record is the one every pod dial depends on; the addn-hosts deploys
    to /etc/freehold/dnsmasq.addn-hosts (world-readable, outside the
    dnsmasq conf-dir — Debian's CONFIG_DIR parses every file in
    /etc/dnsmasq.d as config, and a hosts-format file there keeps dnsmasq
    from ever starting) so dnsmasq never depends on the 0700 state dir's
    perms.
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
*   **More DNS providers:** the provider registry (`dnsman` — it exists today beside the
    Cloudflare manager, which is its only registration) grows per-provider A-record
    managers — Route53, Google Cloud DNS,
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
