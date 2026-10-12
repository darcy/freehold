package config

import "strings"

// AgentToolsPort is the CP's freehold-agent-tools MCP bind port. Any URL the
// console/install hands to the toolset — and the URL the CPA pod uses to curl
// its stdio bridge binary — is derived from it.
const AgentToolsPort = "8089"

// WorldDomain derives a world's own domain — the per-world subtree of the
// zone every world name hangs off. It is the CP host with a literal "cp." or
// "admin." label stripped (the prefixed house shapes, and only when at least
// one world label remains below the zone — stripping cp.example.com would
// land on a zone this world doesn't own); a host that already IS its world
// domain (sandbox.freehold.technology) comes back unchanged. The world's
// tree hangs off that root: the portal at the root, chat./admin. and every
// exposed app (<name>) as children — so a CP-host rename never moves an app.
// Lives in the contract because every module composes world names from it
// (the CLI's hosts verb, the CP's exposure machinery, the console's cookie
// scope).
func WorldDomain(cpHost string) string {
	labels := strings.Split(cpHost, ".")
	if len(labels) >= 4 && (labels[0] == "cp" || labels[0] == "admin") {
		return strings.Join(labels[1:], ".")
	}
	return cpHost
}

// EffectiveWorldDomain resolves a world's own domain from an explicit
// operator override. WorldDomain's guard refuses to strip a prefixed label
// when the strip would land on a zone apex (cp.example.com — a world must
// not silently claim a zone it may not own). An operator WHO OWNS the zone
// and wants the world AT the apex sets the override: when it is a
// registrable domain (≥2 labels) and the CP host is actually its child, the
// override IS the world root — the zone apex — and every world name
// (admin., the portal at the root, <name>.) hangs off it. Anything else
// (empty, too short, or an override the CP host isn't a child of) falls
// back to the derived shape, so a typo can never move the tree onto names
// this world doesn't serve.
func EffectiveWorldDomain(cpHost, override string) string {
	if override != "" && len(strings.Split(override, ".")) >= 2 && strings.HasSuffix(cpHost, "."+override) {
		return override
	}
	return WorldDomain(cpHost)
}

// Coords is the operator-supplied world configuration the CP needs to own the
// build — the non-secret subset of the build Spec, serialized so a box can hand
// it to the console (route: deploy --world-config) for the console to be the CP
// build executor. It carries the runner + world coordinates and the agents'
// provenance, but NO credential: security material (operator DNS/litellm
// secrets, the runner package) never rides here. Shared by the install CLI (it
// renders these coords at deploy-cp) and the CP console (it consumes them), so
// it lives in the contract, not in either module.
type Coords struct {
	Name           string `json:"name,omitempty"`
	StateDir       string `json:"state_dir,omitempty"`
	RelayURL       string `json:"relay_url,omitempty"`
	RelayAuthURL   string `json:"relay_auth_url,omitempty"`
	RelayWS        string `json:"relay_ws,omitempty"`
	RelayPK        string `json:"relay_pubkey,omitempty"`
	RelayHost      string `json:"relay_host,omitempty"`
	RelayIP        string `json:"relay_ip,omitempty"`
	CpHost         string `json:"cp_host,omitempty"`
	CpIP           string `json:"cp_ip,omitempty"`
	CpLxc          uint32 `json:"cp_lxc,omitempty"`
	ProxyIP        string `json:"proxy_ip,omitempty"`
	LitellmIP      string `json:"litellm_ip,omitempty"`
	PlanePool      string `json:"plane_pool,omitempty"`
	PlaneKind      string `json:"plane_kind,omitempty"`
	ThinPool       string `json:"thin_pool,omitempty"`
	SizeGB         uint64 `json:"size_gb,omitempty"`
	PoolSizeGB     uint64 `json:"pool_size_gb,omitempty"`
	RootfsGB       uint32 `json:"rootfs_gb,omitempty"`
	MemoryMB       uint32 `json:"memory_mb,omitempty"`
	StorageName    string `json:"storage,omitempty"`
	RelayGW        string `json:"relay_gw,omitempty"`
	Bridge         string `json:"bridge,omitempty"`
	RelayLxc       uint32 `json:"relay_lxc,omitempty"`
	RelayCompose   string `json:"relay_compose,omitempty"`
	K3sVmid        uint32 `json:"k3s_vmid,omitempty"`
	RunnerAddr     string `json:"runner_addr,omitempty"`
	RunnerPK       string `json:"runner_pubkey,omitempty"`
	RunnerTarget   string `json:"runner_target,omitempty"`
	CpaName        string `json:"cpa_name,omitempty"`
	OwnerPub       string `json:"owner_pubkey,omitempty"`
	LitellmBaseURL string `json:"litellm_base,omitempty"`
	SelfURL        string `json:"self_url,omitempty"`
	// The freehold-subnet gateway (docs/NETWORK.md). GatewayCIDR is
	// the internal subnet; GatewayVlan the in-host bridge tag (0 = untagged);
	// GatewayLxc the gateway guest's vmid. K3sIP is the k3s node's address —
	// with a gateway the INTERNAL one (ProxyIP is then the gateway's LAN
	// address, the edge); without one it mirrors ProxyIP.
	GatewayCIDR string `json:"gateway_cidr,omitempty"`
	GatewayVlan int    `json:"gateway_vlan,omitempty"`
	GatewayLxc  uint32 `json:"gateway_lxc,omitempty"`
	// HostedGateway: the HOST itself is the gateway (the api-vultr world) —
	// no gateway guest exists, the ruleset is asserted host-side, and the
	// bridge carries the subnet. GatewayLxc is then 0.
	HostedGateway bool   `json:"hosted_gateway,omitempty"`
	K3sIP         string `json:"k3s_ip,omitempty"`
	// OperatorName is the operator's display name in Buzz — published as the
	// operator's kind:0 profile at build (which is what makes the Buzz desktop
	// app skip its first-run onboarding). Empty renders "Operator".
	OperatorName string `json:"operator_name,omitempty"`
	// WorldDomain is the explicit world-root override (EffectiveWorldDomain):
	// for an operator who owns the whole zone and wants the world AT the
	// apex — the portal at the root, admin./<name>. as children. Validated at
	// every consume (the CP host must be the override's child; anything else
	// falls back to the derived shape). Empty = the derived WorldDomain.
	WorldDomain string `json:"world_domain,omitempty"`
	// Pangolin is the optional public path (docs/NETWORK.md): when set,
	// exposed apps ride a Pangolin VPS's tunnel — the app A records point at
	// the VPS and the app vhosts live on Pangolin's edge, so the home IP
	// appears in no public DNS and nothing at home accepts unsolicited
	// traffic. Nil = the direct Caddy edge (the default).
	Pangolin *PangolinCoords `json:"pangolin,omitempty"`
}

// PangolinCoords carries the world's Pangolin public path SHAPE — the
// non-secret parts only (coords ride the console's argv and the update
// flow's world-config scrape; NO credential lives here). The instance's
// address + root integration key are sealed to the console identity at
// world-secrets/pangolin.json (PANGOLIN_BASE + PANGOLIN_KEY), exactly like
// the DNS creds. DNSIP is the public IP the exposed apps' A records point
// at (the VPS); empty keeps them on the proxy IP (a dev/test world where
// the tunnel is exercised without moving the public DNS).
type PangolinCoords struct {
	Org    string `json:"org"`    // the world's organization id
	Site   string `json:"site"`   // the newt site's name (the gateway's dial-out identity)
	Domain string `json:"domain"` // the base domain Pangolin serves (the world domain)
	DNSIP  string `json:"dns_ip,omitempty"`
}
