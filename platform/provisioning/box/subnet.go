// Subnet-claim probing: the mint's freehold-subnet must not collide with
// another world's on the same L2. Live-verified failure: two PVE boxes on one
// LAN both derived 10.77.0.0/24 untagged — their guests ARP-collided and each
// world's CP dialed the OTHER's litellm (a 401 "token not in DB" that looked
// like a key bug). The derive therefore probes the LAN before settling.
package box

import (
	"fmt"
	"net/netip"
	"strings"

	"freehold/platform/provisioning"
)

// probeWorldAddrs are the deterministic addresses a world claims in its
// subnet: the gateway's own (.1), the static proxy (.5), relay (.11), cp
// (.12), k3s (.13) — probing these finds another freehold world's guests
// without scanning.
var probeWorldAddrs = [...]byte{1, 5, 11, 12, 13}

// probeSubnetClaimed reports whether the candidate /24 already has claimants
// on the bridge's L2, as "ip lladdr mac" evidence lines. A host route for the
// cidr is itself a claim (a world on THIS host asserts one at build). The
// probe pings the world addresses to force ARPs, then reads the neigh table —
// the ENTRY is the signal, not the ping outcome (the reply may die in the
// other world's NAT).
// ponytail: ping/ARP-based — a DOWN guest (a crashed second host) or a
// non-ICMP squatter evades it; --gateway-cidr always overrides, and the
// caller treats undetectable as safe.
func probeSubnetClaimed(prov provisioning.Provider, cidr, bridge string) ([]string, error) {
	prefix, err := netip.ParsePrefix(cidr)
	if err != nil || prefix.Bits() != 24 || !prefix.Addr().Is4() {
		return nil, nil // only /24s are probed — the derive only makes /24s
	}
	base := prefix.Addr().As4()
	var ips []string
	for _, o := range probeWorldAddrs {
		ips = append(ips, fmt.Sprintf("%d.%d.%d.%d", base[0], base[1], base[2], o))
	}
	script := fmt.Sprintf(`added=0
if ip route show | grep -q "^%s "; then echo "ROUTE %s"; fi
ip route add %s dev %s 2>/dev/null && added=1
for ip in %s; do ping -c1 -W1 "$ip" >/dev/null 2>&1; done
ip neigh show dev %s
[ "$added" = 1 ] && ip route del %s dev %s 2>/dev/null
true`, cidr, cidr, cidr, bridge, strings.Join(ips, " "), bridge, cidr, bridge)
	out, err := prov.GuestExec("", script, 90)
	if err != nil {
		return nil, err
	}
	var evidence []string
	for _, line := range strings.Split(out.Stdout, "\n") {
		f := strings.Fields(strings.TrimSpace(line))
		if len(f) == 0 {
			continue
		}
		if f[0] == "ROUTE" {
			evidence = append(evidence, fmt.Sprintf("a host route for %s already exists (a world on this host?)", cidr))
			continue
		}
		// neigh line for a candidate IP: "<ip> [dev <bridge>] lladdr <mac>
		// <state>" (the dev column vanishes under `show dev <bridge>`).
		// lladdr = a live claimant; FAILED/INCOMPLETE rows carry none.
		if strings.HasPrefix(f[0], cidrBase(cidr)) {
			for i, tok := range f {
				if tok == "lladdr" && i+1 < len(f) {
					evidence = append(evidence, fmt.Sprintf("%s answers from %s", f[0], f[i+1]))
					break
				}
			}
		}
	}
	return evidence, nil
}

// cidrBase returns the /24's base address ("10.77.0.") for prefix-matching
// neigh lines.
func cidrBase(cidr string) string {
	prefix, err := netip.ParsePrefix(cidr)
	if err != nil {
		return ""
	}
	b := prefix.Addr().As4()
	return fmt.Sprintf("%d.%d.%d.", b[0], b[1], b[2])
}

// deriveGatewayCIDR picks the mint's freehold-subnet: the LAN-overlap-safe
// candidate first, then L2-probes each candidate — a candidate another world
// on this LAN already claims is bumped to the next /24. Silent when the first
// candidate is clean; prints the bump + its evidence otherwise.
func (e *Engine) deriveGatewayCIDR() (string, error) {
	cidr := DefaultGatewayCIDR(e.F.ProxyIP)
	if cidr == "" {
		return "", nil // no parseable LAN IP: the gateway stages fail loud later
	}
	lan := netip.MustParsePrefix(e.F.ProxyIP)
	first, _ := netip.ParsePrefix(cidr)
	for octet := int(first.Addr().As4()[1]); octet <= 255; octet++ {
		cand := fmt.Sprintf("10.%d.0.0/24", octet)
		if lan.Overlaps(netip.MustParsePrefix(cand)) {
			continue
		}
		evidence, err := probeSubnetClaimed(e.Provider, cand, e.F.Bridge)
		if err != nil {
			return "", fmt.Errorf("subnet collision probe for %s: %w", cand, err)
		}
		if len(evidence) == 0 {
			return cand, nil
		}
		fmt.Fprintf(e.Out, "  · %s is claimed on this LAN (%s) — bumping the freehold-subnet\n",
			cand, strings.Join(evidence, "; "))
	}
	return "", fmt.Errorf("every candidate freehold-subnet (10.77-10.255.0.0/24) is claimed on this LAN — pass --gateway-cidr (or --gateway-vlan) explicitly")
}
