package config

import (
	"fmt"
	"net"
)

// HostSpec records the world's HOST and the provider that owns it: which
// HostProvider (providers/registry) created/reaches it, its instance handle
// ("" when the host predates the world — a reached proxmox box), whether the
// HOST itself is the gateway (the hosted shape — no gateway guest), and the
// provider's non-secret answers (region/plan for a re-adopt re-create).
// Secrets are NEVER here: the guided flow prompts no-echo and headless
// reads the env; both keep them in memory for the call they serve.
type HostSpec struct {
	Provider string            `toml:"provider,omitempty"`
	ID       string            `toml:"id,omitempty"`
	Gateway  bool              `toml:"gateway,omitempty"`
	Answers  map[string]string `toml:"answers,omitempty"`
}

// HostGatewayNftConf renders the HOST-as-gateway ruleset (a Vultr world: the
// host owns the one public IP — no gateway guest). Same forward/NAT shape as
// GatewayNftConf (one renderer discipline per ruleset) plus an INPUT policy:
// on a public edge the host itself must not answer anything beyond SSH, the
// edge ports, and its own internal side. DNAT happens before INPUT, so the
// forwarded services keep working without INPUT rules; the internal bridge
// (iifname vmbr0) is trusted exactly like the LAN world's subnet. edgeIP is
// the public IP the DNATs match on (hairpin: internal guests dialing the
// public FQDN arrive on the bridge and are served). No WAN-interface name is
// needed: the policy is drop + explicit accepts, and pveproxy (8006) stays
// dark to the public exactly like the LAN world's PVE UI.
func HostGatewayNftConf(cidr, edgeIP, k3sIP, cpIP, relayIP, bridgeIf string) string {
	return fmt.Sprintf(`flush ruleset
table ip freehold {
	chain input {
		type filter hook input priority 0; policy drop;
		ct state established,related accept
		iifname "lo" accept
		iifname "%s" accept
		tcp dport 22 accept
		tcp dport { 80, 443 } accept
		udp dport { 80, 443 } accept
		icmp type echo-request accept
	}
	chain forward {
		type filter hook forward priority 0; accept;
	}
	chain postrouting {
		type nat hook postrouting priority srcnat;
		ip saddr %s masquerade
	}
	chain prerouting {
		type nat hook prerouting priority dstnat;
		ip daddr %s tcp dport { 80, 443 } dnat to %s
		ip daddr %s udp dport { 80, 443 } dnat to %s
		ip daddr %s tcp dport 6443 dnat to %s:6443
		ip daddr %s tcp dport 8080 dnat to %s:8080
		ip daddr %s tcp dport 3000 dnat to %s:3000
	}
}
`, bridgeIf, cidr, edgeIP, k3sIP, edgeIP, k3sIP, edgeIP, k3sIP, edgeIP, cpIP, edgeIP, relayIP)
}

// HostGatewayDnsmasqConf renders the host's resolver for the freehold subnet:
// bound to the internal bridge, forwarding to the cloud resolver chain (the
// host's own 127.0.0.53 stub does not answer bridged guests — the spike's
// "guest DNS needs a relay on the bridge"). Public fallbacks ride behind.
func HostGatewayDnsmasqConf(bridgeIf string) string {
	return fmt.Sprintf(`interface=%s
bind-interfaces
no-resolv
server=1.1.1.1
server=8.8.8.8
`, bridgeIf)
}

// HostGatewayScript is the host-side gateway stage: the internal bridge
// (vmbr0 holds the subnet's .1 — the guests' default route), ip_forward, the
// nftables ruleset, and the subnet resolver. The public NIC is NEVER touched
// (no bridge-move lockout — the cloud's own addressing stays where the
// cloud put it). Idempotent; persisted via /etc/network/interfaces.d +
// enabled units, so a host reboot keeps the world served (the LAN world's
// unpersisted host route gap does not exist here — the host IS the router).
func HostGatewayScript(cidr, edgeIP, k3sIP, cpIP, relayIP, bridgeIf string) string {
	gw := GatewayInternalIP(cidr)
	return fmt.Sprintf(`set -e
apt-get update -qq >/dev/null 2>&1 || true
apt-get install -y -qq nftables dnsmasq >/dev/null 2>&1 || true
mkdir -p /etc/dnsmasq.d
cat > /etc/network/interfaces.d/freehold <<NET
auto %s
iface %s inet static
	address %s/%d
	bridge-ports none
	bridge-stp off
	bridge-fd 0
NET
ip link show %s >/dev/null 2>&1 || ip link add name %s type bridge
ip addr replace %s/%d dev %s
ip link set %s up
echo net.ipv4.ip_forward=1 > /etc/sysctl.d/90-freehold-gateway.conf
sysctl -p /etc/sysctl.d/90-freehold-gateway.conf >/dev/null
cat > /etc/nftables.conf <<NFT
%sNFT
cat > /etc/dnsmasq.d/freehold.conf <<DNS
%sDNS
systemctl enable nftables dnsmasq >/dev/null 2>&1 || true
systemctl restart nftables dnsmasq >/dev/null 2>&1 || nft -f /etc/nftables.conf
`, bridgeIf, bridgeIf, gw, prefixBitsOf(cidr),
		bridgeIf, bridgeIf, gw, prefixBitsOf(cidr), bridgeIf, bridgeIf,
		HostGatewayNftConf(cidr, edgeIP, k3sIP, cpIP, relayIP, bridgeIf),
		HostGatewayDnsmasqConf(bridgeIf))
}

// prefixBitsOf returns a CIDR's prefix length, 0 when it does not parse
// (the script then renders a bare address — the caller validated first).
func prefixBitsOf(cidr string) int {
	_, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return 0
	}
	bits, _ := ipnet.Mask.Size()
	return bits
}
